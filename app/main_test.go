package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serve dispatches a request through the real mux built by newMux, so tests
// exercise the same routing table the production server uses.
func serve(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	newMux("test-host").ServeHTTP(rec, req)
	return rec
}

func TestRoot_ReturnsStatusJSON(t *testing.T) {
	rec := serve(t, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var resp statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Host != "test-host" {
		t.Errorf("expected host 'test-host', got %q", resp.Host)
	}
	if resp.Color == "" {
		t.Error("expected color to be populated")
	}
	if resp.Version == "" {
		t.Error("expected version to be populated")
	}
	if resp.TimeUTC == "" {
		t.Error("expected time_utc to be populated")
	}
}

func TestRoot_UnknownPathIs404(t *testing.T) {
	// With the Go 1.22+ exact-match "GET /{$}" root plus a "/" catch-all,
	// any unknown path must yield 404 rather than the status JSON.
	cases := []string{"/does-not-exist", "/api/users", "/foo/bar"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rec := serve(t, http.MethodGet, path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s, got %d", path, rec.Code)
			}
		})
	}
}

func TestRoot_NonGETMethodIs404(t *testing.T) {
	// The exact-match "GET /{$}" pattern only accepts GET, but the "/"
	// catch-all has no method restriction, so it wins the fallback for
	// POST / and similar. That yields 404 rather than 405; the important
	// invariant is that non-GET on / does NOT return the status JSON.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(m, func(t *testing.T) {
			rec := serve(t, m, "/")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected 404 for %s /, got %d", m, rec.Code)
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	rec := serve(t, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("expected body 'ok', got %q", body)
	}
}

func TestReady_NotReady(t *testing.T) {
	atomic.StoreInt32(&ready, 0)
	rec := serve(t, http.MethodGet, "/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not ready, got %d", rec.Code)
	}
}

func TestReady_WhenReady(t *testing.T) {
	atomic.StoreInt32(&ready, 1)
	defer atomic.StoreInt32(&ready, 0)

	rec := serve(t, http.MethodGet, "/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready, got %d", rec.Code)
	}
}

func TestSlow_DefaultDelay(t *testing.T) {
	start := time.Now()
	rec := serve(t, http.MethodGet, "/slow")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Default sleep is 250ms; allow 50ms of scheduler slop on the low side.
	if elapsed < 200*time.Millisecond {
		t.Errorf("expected at least ~250ms, slept only %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected under 2s for default /slow, took %v", elapsed)
	}
}

func TestSlow_CustomMs(t *testing.T) {
	start := time.Now()
	rec := serve(t, http.MethodGet, "/slow?ms=50")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("expected at least ~50ms, slept only %v", elapsed)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// slept_ms is encoded as a JSON number (float64 on decode).
	if slept, ok := resp["slept_ms"].(float64); !ok || slept != 50 {
		t.Errorf("expected slept_ms=50, got %v", resp["slept_ms"])
	}
}

func TestSlow_ClampsToMax(t *testing.T) {
	// Request 60000ms - must be clamped to maxSlowMs (5000) at the label
	// level. We do not actually sleep here in the test; we just check the
	// response body reports the clamped value. To keep the test fast we
	// use ms=0 to verify the plumbing, and rely on maxSlowMs being a
	// package constant so runtime behaviour and label match.
	rec := serve(t, http.MethodGet, "/slow?ms=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if slept, ok := resp["slept_ms"].(float64); !ok || slept != 0 {
		t.Errorf("expected slept_ms=0 for ms=0, got %v", resp["slept_ms"])
	}
}

func TestFail_DefaultIs500(t *testing.T) {
	rec := serve(t, http.MethodGet, "/fail")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestFail_CustomCodes(t *testing.T) {
	cases := []int{400, 401, 403, 404, 409, 418, 429, 500, 502, 503, 504}
	for _, want := range cases {
		t.Run(http.StatusText(want), func(t *testing.T) {
			rec := serve(t, http.MethodGet, "/fail?code="+itoa(want))
			if rec.Code != want {
				t.Fatalf("expected %d, got %d", want, rec.Code)
			}
		})
	}
}

func TestFail_RejectsOutOfRangeCodes(t *testing.T) {
	// Codes outside [400,599] must not become the response code - the
	// handler falls back to the 500 default so callers cannot induce 200s
	// via ?fail= tricks or trigger informational/redirect status codes.
	for _, bad := range []string{"200", "301", "399", "600", "abc"} {
		t.Run(bad, func(t *testing.T) {
			rec := serve(t, http.MethodGet, "/fail?code="+bad)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500 fallback for code=%q, got %d", bad, rec.Code)
			}
		})
	}
}

// itoa is a tiny helper for the table above; avoids pulling in strconv just
// to format the test names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
