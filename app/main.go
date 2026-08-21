package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	version = "dev"
	color   = getEnv("DEPLOY_COLOR", "blue")
	ready   int32
)

type statusResponse struct {
	Version  string `json:"version"`
	Color    string `json:"color"`
	Host     string `json:"host"`
	TimeUTC  string `json:"time_utc"`
}

func main() {
	hostname, _ := os.Hostname()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(statusResponse{
			Version: version,
			Color:   color,
			Host:    hostname,
			TimeUTC: time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&ready) == 1 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ready")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "not ready")
		}
	})

	srv := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful startup: mark ready after server is listening
	go func() {
		time.Sleep(2 * time.Second)
		atomic.StoreInt32(&ready, 1)
		log.Printf("ready (version=%s color=%s)", version, color)
	}()

	// Graceful shutdown: drain connections on SIGTERM
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received %s, starting graceful shutdown", sig)

		atomic.StoreInt32(&ready, 0)
		time.Sleep(5 * time.Second) // let readiness probe fail, stop routing traffic

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("starting server on :8080 (version=%s color=%s)", version, color)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Println("server stopped")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
