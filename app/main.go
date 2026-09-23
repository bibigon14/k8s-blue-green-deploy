package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version = "dev"
	color   = getEnv("DEPLOY_COLOR", "blue")
	ready   int32
)

// maxSlowMs caps /slow?ms=N to protect against pathological input in a
// portfolio deployment where the endpoint is exposed via a NodePort.
const maxSlowMs = 5000

type statusResponse struct {
	Version string `json:"version"`
	Color   string `json:"color"`
	Host    string `json:"host"`
	TimeUTC string `json:"time_utc"`
}

var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests, labelled by method, route pattern, and status.",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, labelled by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being served.",
		},
	)

	appReady = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "app_ready",
			Help: "1 if the app is ready to serve traffic, 0 otherwise.",
		},
	)

	appInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "app_info",
			Help: "Static app metadata. Always 1; version and color are labels.",
		},
		[]string{"version", "color"},
	)
)

// newMux builds the application router. It is a package-level constructor so
// tests can exercise the real handlers instead of duplicating them.
//
// Route patterns use Go 1.22+ syntax so that method and exact-match ("/{$}")
// participate in dispatch. Every route pattern is a fixed string, so the
// method/path label pair used in metrics has bounded cardinality.
func newMux(hostname string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(statusResponse{
			Version: version,
			Color:   color,
			Host:    hostname,
			TimeUTC: time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&ready) == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ready")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, "not ready")
		}
	})

	// /slow sleeps for a caller-controlled duration before returning 200.
	// Populates the p50/p95/p99 histogram with a visible tail.
	// Accepts ?ms=N (clamped to [0, maxSlowMs]) or ?jitter to sleep for a
	// uniformly random duration in [0, ms]. Default is 250ms with no jitter.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms := 250
		if v := r.URL.Query().Get("ms"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				ms = parsed
			}
		}
		if ms < 0 {
			ms = 0
		}
		if ms > maxSlowMs {
			ms = maxSlowMs
		}
		sleep := time.Duration(ms) * time.Millisecond
		if r.URL.Query().Has("jitter") && sleep > 0 {
			// #nosec G404 - not a security-sensitive use.
			sleep = time.Duration(rand.Int63n(int64(sleep) + 1))
		}
		time.Sleep(sleep)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"slept_ms": sleep.Milliseconds(),
		})
	})

	// /fail returns a caller-controlled status code so the non-2xx panel
	// gets a controllable mix of 4xx and 5xx. Accepts ?code=N in [400, 599].
	// Default is 500.
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusInternalServerError
		if v := r.URL.Query().Get("code"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed >= 400 && parsed <= 599 {
				code = parsed
			}
		}
		http.Error(w, http.StatusText(code), code)
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	// Catch-all: any path or method that did not match above yields 404.
	// The pattern label in metrics will read "/" for these, distinct from
	// the exact-match "/{$}" root, so unknown-path noise stays visible in
	// the non-2xx breakdown without inflating cardinality.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	return mux
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("LOG_LEVEL")),
	})).With("version", version, "color", color)
	slog.SetDefault(logger)

	appInfo.WithLabelValues(version, color).Set(1)

	hostname, _ := os.Hostname()

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      instrumentedHandler(logger, newMux(hostname)),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second, // must exceed maxSlowMs
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		time.Sleep(2 * time.Second)
		atomic.StoreInt32(&ready, 1)
		appReady.Set(1)
		logger.Info("ready")
	}()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		logger.Info("received signal, starting graceful shutdown", "signal", sig.String())

		atomic.StoreInt32(&ready, 0)
		appReady.Set(0)
		time.Sleep(5 * time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", "err", err)
		}
	}()

	logger.Info("starting server", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

func instrumentedHandler(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz, /ready, /metrics are intentionally excluded from the
		// RED metrics so kubelet probes and Prometheus scrapes do not
		// distort request rate and latency.
		if r.URL.Path == "/healthz" || r.URL.Path == "/ready" || r.URL.Path == "/metrics" {
			h.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-Version", version)
		w.Header().Set("X-Color", color)

		httpRequestsInFlight.Inc()
		defer httpRequestsInFlight.Dec()

		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		elapsed := time.Since(start)

		// Use the mux route pattern (populated by Go 1.22+ ServeMux during
		// dispatch) as the "path" label instead of the raw URL, so metric
		// cardinality is bounded by the number of registered routes.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		labels := prometheus.Labels{"method": r.Method, "path": route}
		httpRequestsTotal.With(prometheus.Labels{
			"method": r.Method,
			"path":   route,
			"status": strconv.Itoa(rec.status),
		}).Inc()
		httpRequestDuration.With(labels).Observe(elapsed.Seconds())

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"route", route,
			"status", rec.status,
			"duration_ms", elapsed.Milliseconds(),
			"bytes", rec.bytes,
			"remote", clientIP(r),
		)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
