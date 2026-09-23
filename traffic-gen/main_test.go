package main

import (
	"math"
	"testing"
	"time"
)

func TestPickWeightedRoute_ReturnsRegisteredRoute(t *testing.T) {
	// Over many draws, every returned route must be one of baseRoutes.
	seen := make(map[string]int)
	for i := 0; i < 5000; i++ {
		r := pickWeightedRoute()
		seen[r.path]++
		found := false
		for _, br := range baseRoutes {
			if br.path == r.path && br.method == r.method {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pickWeightedRoute returned unregistered %s %s", r.method, r.path)
		}
	}

	// Root should be the most common because its weight dominates.
	if seen["/"] <= seen["/api/v1/search"] {
		t.Errorf("expected / to be picked more often than /api/v1/search, got %d vs %d",
			seen["/"], seen["/api/v1/search"])
	}
}

func TestPickFailCode_ReturnsRegisteredCode(t *testing.T) {
	for i := 0; i < 5000; i++ {
		c := pickFailCode()
		found := false
		for _, fc := range failCodes {
			if fc.code == c {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pickFailCode returned unregistered %d", c)
		}
	}
}

func TestEffectiveRPS_NoModulation(t *testing.T) {
	cfg := &config{baseRPS: 3.0, modulationHz: 0, modulationDB: 0.5}
	for _, since := range []time.Duration{0, time.Second, 10 * time.Minute} {
		if got := effectiveRPS(cfg, since); got != 3.0 {
			t.Errorf("no-modulation: expected 3.0 at %v, got %v", since, got)
		}
	}

	// modulationDB=0 should also disable modulation.
	cfg = &config{baseRPS: 3.0, modulationHz: 1.0, modulationDB: 0}
	if got := effectiveRPS(cfg, 5*time.Second); got != 3.0 {
		t.Errorf("zero-amplitude: expected 3.0, got %v", got)
	}
}

func TestEffectiveRPS_WithinBounds(t *testing.T) {
	// With amplitude 0.4 the effective RPS must stay in
	// [baseRPS*(1-0.4), baseRPS*(1+0.4)].
	cfg := &config{baseRPS: 5.0, modulationHz: 1.0 / 30.0, modulationDB: 0.4}
	minAllowed := cfg.baseRPS * (1 - cfg.modulationDB)
	maxAllowed := cfg.baseRPS * (1 + cfg.modulationDB)
	// eps for float rounding.
	const eps = 1e-9
	// Sample across a full period.
	period := 1.0 / cfg.modulationHz
	steps := 200
	for i := 0; i < steps; i++ {
		since := time.Duration(float64(i) / float64(steps) * period * float64(time.Second))
		got := effectiveRPS(cfg, since)
		if got < minAllowed-eps || got > maxAllowed+eps {
			t.Errorf("at %v: got %v, want in [%v, %v]", since, got, minAllowed, maxAllowed)
		}
	}
}

func TestEffectiveRPS_PhaseZeroEqualsBase(t *testing.T) {
	// sin(0) = 0, so at t=0 modulation contributes nothing.
	cfg := &config{baseRPS: 4.0, modulationHz: 1.0 / 60.0, modulationDB: 0.3}
	got := effectiveRPS(cfg, 0)
	if math.Abs(got-4.0) > 1e-9 {
		t.Errorf("expected baseRPS at t=0, got %v", got)
	}
}
