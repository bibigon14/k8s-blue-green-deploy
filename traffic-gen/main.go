// Command traffic-gen generates a steady, low-volume workload against the
// demo-app HTTP server so the Grafana blue-green dashboard has continuous
// signal in RED metrics without a human clicking a URL.
//
// It runs as a long-lived systemd service on a small VM (the reference
// deployment sits on an Oracle Cloud Ampere A1 VM that reaches the homelab
// Pi over a WireGuard tunnel). Traffic pattern:
//
//   - Baseline: a weighted random pick from the app's real routes at a
//     configurable steady RPS.
//   - Latency tail: with probability -slow-prob, the request is redirected
//     to /slow with a uniformly random ms in [0, -max-slow-ms].
//   - Error mix: with probability -error-prob, the request is redirected to
//     /fail with a status code sampled from a fixed table of 4xx and 5xx.
//   - Diurnal modulation: RPS is scaled by a slow sinusoid so the graphs
//     show visible highs and lows instead of a flat line.
//
// The generator exposes its own /metrics endpoint so operator dashboards can
// see when the generator itself is broken (target unreachable, DNS failure,
// clock skew on the modulation) rather than assuming a flat request-rate
// panel is an outage of the demo app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
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

// route describes one weighted entry in the traffic mix. Weights are
// relative; they are normalised at pick time.
type route struct {
	method string
	path   string
	weight int
}

// baseRoutes matches paths the demo app actually serves. Weights are set so
// the / handler dominates and the special-purpose routes appear as tail
// activity rather than swamping the graph.
var baseRoutes = []route{
	{method: http.MethodGet, path: "/", weight: 70},
	{method: http.MethodGet, path: "/healthz", weight: 10},
	{method: http.MethodGet, path: "/ready", weight: 10},
	// A pinch of unknown-path noise so the non-2xx panel is never quite
	// zero even without the error-injection branch.
	{method: http.MethodGet, path: "/api/v1/search", weight: 5},
	{method: http.MethodGet, path: "/api/v1/orders", weight: 5},
}

// failCodes is the distribution of status codes injected via /fail. Weights
// are relative; 500 appears the most often because that is the real-world
// shape a 5xx-heavy service tends to have.
var failCodes = []struct {
	code   int
	weight int
}{
	{code: 500, weight: 40},
	{code: 502, weight: 15},
	{code: 503, weight: 15},
	{code: 504, weight: 5},
	{code: 404, weight: 15},
	{code: 429, weight: 10},
}

var (
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "trafficgen_requests_total",
			Help: "Requests issued by the traffic generator, labelled by outcome.",
		},
		[]string{"outcome"}, // ok, http_error, transport_error, timeout
	)
	requestDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "trafficgen_request_duration_seconds",
			Help:    "End-to-end request latency observed by the generator.",
			Buckets: prometheus.DefBuckets,
		},
	)
	targetInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "trafficgen_target_info",
			Help: "Static target metadata. Always 1; target URL is a label.",
		},
		[]string{"target"},
	)
	currentRPS = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "trafficgen_current_rps",
			Help: "Current target RPS after diurnal modulation.",
		},
	)
)

type config struct {
	target       *url.URL
	baseRPS      float64
	slowProb     float64
	maxSlowMs    int
	errorProb    float64
	timeout      time.Duration
	metricsAddr  string
	modulationHz float64 // frequency of the diurnal sinusoid, in cycles/sec
	modulationDB float64 // amplitude of modulation as fraction of baseRPS
}

func parseFlags() (*config, error) {
	var (
		targetStr    = flag.String("target", "http://10.0.0.1:30130", "Base URL of the target service")
		baseRPS      = flag.Float64("rps", 3.0, "Steady-state target requests per second")
		slowProb     = flag.Float64("slow-prob", 0.10, "Probability [0..1] of routing to /slow")
		maxSlowMs    = flag.Int("max-slow-ms", 800, "Upper bound for /slow?ms=N")
		errorProb    = flag.Float64("error-prob", 0.05, "Probability [0..1] of routing to /fail")
		timeout      = flag.Duration("timeout", 10*time.Second, "Per-request HTTP timeout")
		metricsAddr  = flag.String("metrics-listen", "127.0.0.1:9101", "Prometheus /metrics listen address")
		modPeriodMin = flag.Float64("modulation-period-min", 30.0, "Full diurnal cycle length in minutes (0 disables modulation)")
		modAmplitude = flag.Float64("modulation-amplitude", 0.4, "Modulation amplitude as fraction of baseline RPS [0..1)")
	)
	flag.Parse()

	if *baseRPS <= 0 {
		return nil, fmt.Errorf("rps must be > 0, got %v", *baseRPS)
	}
	if *slowProb < 0 || *slowProb > 1 {
		return nil, fmt.Errorf("slow-prob must be in [0,1], got %v", *slowProb)
	}
	if *errorProb < 0 || *errorProb > 1 {
		return nil, fmt.Errorf("error-prob must be in [0,1], got %v", *errorProb)
	}
	if *slowProb+*errorProb > 1 {
		return nil, fmt.Errorf("slow-prob + error-prob must be <= 1, got %v", *slowProb+*errorProb)
	}
	if *modAmplitude < 0 || *modAmplitude >= 1 {
		return nil, fmt.Errorf("modulation-amplitude must be in [0,1), got %v", *modAmplitude)
	}
	u, err := url.Parse(*targetStr)
	if err != nil {
		return nil, fmt.Errorf("parse target: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("target must be http:// or https://, got %q", u.Scheme)
	}

	var modHz float64
	if *modPeriodMin > 0 {
		modHz = 1.0 / (*modPeriodMin * 60.0)
	}

	return &config{
		target:       u,
		baseRPS:      *baseRPS,
		slowProb:     *slowProb,
		maxSlowMs:    *maxSlowMs,
		errorProb:    *errorProb,
		timeout:      *timeout,
		metricsAddr:  *metricsAddr,
		modulationHz: modHz,
		modulationDB: *modAmplitude,
	}, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("LOG_LEVEL")),
	}))
	slog.SetDefault(logger)

	cfg, err := parseFlags()
	if err != nil {
		logger.Error("bad flags", "err", err)
		os.Exit(2)
	}
	targetInfo.WithLabelValues(cfg.target.String()).Set(1)

	logger.Info("starting traffic-gen",
		"target", cfg.target.String(),
		"base_rps", cfg.baseRPS,
		"slow_prob", cfg.slowProb,
		"error_prob", cfg.errorProb,
		"metrics_listen", cfg.metricsAddr,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Metrics server for the generator itself.
	metricsSrv := &http.Server{
		Addr:              cfg.metricsAddr,
		Handler:           promhttp.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "err", err)
		}
	}()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("generator loop stopped", "err", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	logger.Info("stopped")
}

// run is the main loop. It sleeps for the current inter-request interval
// (derived from baseRPS + sinusoidal modulation), then issues one request.
// A one-goroutine loop is sufficient for the ~2-5 RPS this service is sized
// for; HTTP keep-alive on the shared client keeps connection overhead low.
func run(ctx context.Context, cfg *config, logger *slog.Logger) error {
	client := &http.Client{
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
		},
	}
	defer client.CloseIdleConnections()

	start := time.Now()

	// atomic counter to keep the request logs sequential in flight.
	var seq atomic.Uint64

	for {
		rps := effectiveRPS(cfg, time.Since(start))
		currentRPS.Set(rps)
		interval := time.Duration(float64(time.Second) / rps)

		select {
		case <-ctx.Done():
			logger.Info("shutdown requested")
			return nil
		case <-time.After(interval):
		}

		req := pickRequest(cfg)
		id := seq.Add(1)
		if err := do(ctx, client, cfg.target, req, logger, id); err != nil {
			// Errors are already recorded in metrics; keep the loop going.
			continue
		}
	}
}

// pickRequest chooses the next request shape. Order of decisions:
//  1. With slow-prob, hit /slow with a random ms.
//  2. Else with error-prob, hit /fail with a sampled status code.
//  3. Else weighted-pick a route from baseRoutes.
func pickRequest(cfg *config) *http.Request {
	roll := rand.Float64() // #nosec G404 - not security-sensitive.
	switch {
	case roll < cfg.slowProb:
		ms := rand.IntN(cfg.maxSlowMs + 1)
		req, _ := http.NewRequest(http.MethodGet, "/slow?ms="+strconv.Itoa(ms)+"&jitter", nil)
		return req
	case roll < cfg.slowProb+cfg.errorProb:
		code := pickFailCode()
		req, _ := http.NewRequest(http.MethodGet, "/fail?code="+strconv.Itoa(code), nil)
		return req
	default:
		r := pickWeightedRoute()
		req, _ := http.NewRequest(r.method, r.path, nil)
		return req
	}
}

func pickWeightedRoute() route {
	total := 0
	for _, r := range baseRoutes {
		total += r.weight
	}
	n := rand.IntN(total)
	for _, r := range baseRoutes {
		n -= r.weight
		if n < 0 {
			return r
		}
	}
	return baseRoutes[0] // unreachable
}

func pickFailCode() int {
	total := 0
	for _, c := range failCodes {
		total += c.weight
	}
	n := rand.IntN(total)
	for _, c := range failCodes {
		n -= c.weight
		if n < 0 {
			return c.code
		}
	}
	return 500 // unreachable
}

// do executes one request and records outcome + latency. The request's URL
// is relative when constructed by pickRequest; here we resolve it against
// the base target.
func do(ctx context.Context, client *http.Client, base *url.URL, tmpl *http.Request, logger *slog.Logger, id uint64) error {
	rel, err := url.Parse(tmpl.URL.String())
	if err != nil {
		requestsTotal.WithLabelValues("transport_error").Inc()
		return err
	}
	final := base.ResolveReference(rel)
	req, err := http.NewRequestWithContext(ctx, tmpl.Method, final.String(), nil)
	if err != nil {
		requestsTotal.WithLabelValues("transport_error").Inc()
		return err
	}
	req.Header.Set("User-Agent", "traffic-gen/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	requestDuration.Observe(elapsed.Seconds())

	if err != nil {
		outcome := "transport_error"
		if isTimeout(err) {
			outcome = "timeout"
		}
		requestsTotal.WithLabelValues(outcome).Inc()
		logger.Warn("request failed",
			"seq", id,
			"method", req.Method,
			"url", final.String(),
			"err", err,
			"elapsed_ms", elapsed.Milliseconds(),
		)
		return err
	}
	// Drain and close so keep-alive can reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	outcome := "ok"
	if resp.StatusCode >= 400 {
		outcome = "http_error"
	}
	requestsTotal.WithLabelValues(outcome).Inc()

	// Log at debug level for successful requests so a healthy generator is
	// not spammy; anything >=400 gets an info line.
	logFn := logger.Debug
	if resp.StatusCode >= 400 {
		logFn = logger.Info
	}
	logFn("request",
		"seq", id,
		"method", req.Method,
		"url", final.String(),
		"status", resp.StatusCode,
		"elapsed_ms", elapsed.Milliseconds(),
	)
	return nil
}

func isTimeout(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// effectiveRPS applies a slow sinusoidal modulation to the base RPS so
// graphs show visible waves instead of a flat line.
func effectiveRPS(cfg *config, since time.Duration) float64 {
	if cfg.modulationHz == 0 || cfg.modulationDB == 0 {
		return cfg.baseRPS
	}
	phase := 2 * math.Pi * cfg.modulationHz * since.Seconds()
	factor := 1 + cfg.modulationDB*math.Sin(phase)
	// factor is in [1-amp, 1+amp]; amp < 1 guarantees positivity.
	return cfg.baseRPS * factor
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
