package accesspattern

import (
	"math/rand"
	"testing"
)

func TestCompileDefaultsToUniform(t *testing.T) {
	sel, err := Compile(Config{})
	if err != nil {
		t.Fatalf("Compile(empty) error = %v", err)
	}
	if sel.Kind() != KindUniform {
		t.Fatalf("expected uniform, got %q", sel.Kind())
	}
}

func TestUniformBoundsAndDistribution(t *testing.T) {
	sel, _ := Compile(Config{Kind: "uniform"})
	rng := rand.New(rand.NewSource(1))
	n := 50
	counts := make([]int, n)
	for i := 0; i < 50000; i++ {
		idx := sel.Index(n, rng)
		if idx < 0 || idx >= n {
			t.Fatalf("uniform index %d out of range [0,%d)", idx, n)
		}
		counts[idx]++
	}
	// Every bucket should get a non-trivial share for uniform.
	for i, c := range counts {
		if c == 0 {
			t.Fatalf("uniform bucket %d never selected", i)
		}
	}
}

func TestUniformEdgeCases(t *testing.T) {
	sel, _ := Compile(Config{Kind: "uniform"})
	rng := rand.New(rand.NewSource(1))
	if sel.Index(0, rng) != 0 {
		t.Fatalf("expected 0 for empty pool")
	}
	if sel.Index(1, rng) != 0 {
		t.Fatalf("expected 0 for single-element pool")
	}
	if sel.Index(5, nil) != 0 {
		t.Fatalf("expected 0 for nil rng")
	}
}

func TestZipfianSkewsTowardLowIndices(t *testing.T) {
	sel, err := Compile(Config{Kind: "zipfian", ZipfianExponent: 2.0})
	if err != nil {
		t.Fatalf("Compile zipfian error = %v", err)
	}
	rng := rand.New(rand.NewSource(7))
	n := 100
	firstDecile, lastDecile := 0, 0
	total := 200000
	for i := 0; i < total; i++ {
		idx := sel.Index(n, rng)
		if idx < 0 || idx >= n {
			t.Fatalf("zipfian index %d out of range", idx)
		}
		if idx < 10 {
			firstDecile++
		} else if idx >= 90 {
			lastDecile++
		}
	}
	if firstDecile <= lastDecile*5 {
		t.Fatalf("expected strong zipfian skew: firstDecile=%d lastDecile=%d", firstDecile, lastDecile)
	}
}

func TestZipfianDefaultExponentAndValidation(t *testing.T) {
	if _, err := Compile(Config{Kind: "zipfian"}); err != nil {
		t.Fatalf("expected default exponent to compile, got %v", err)
	}
	if _, err := Compile(Config{Kind: "zipfian", ZipfianExponent: 0.5}); err == nil {
		t.Fatalf("expected exponent < 1.0 to fail validation")
	}
}

func TestHotspotConcentratesTraffic(t *testing.T) {
	sel, err := Compile(Config{Kind: "hotspot", HotspotPercent: 10, HotspotTrafficPercent: 90})
	if err != nil {
		t.Fatalf("Compile hotspot error = %v", err)
	}
	rng := rand.New(rand.NewSource(3))
	n := 100
	hot := 0
	total := 100000
	for i := 0; i < total; i++ {
		idx := sel.Index(n, rng)
		if idx < 0 || idx >= n {
			t.Fatalf("hotspot index %d out of range", idx)
		}
		if idx < 10 {
			hot++
		}
	}
	share := float64(hot) / float64(total)
	if share < 0.80 || share > 0.97 {
		t.Fatalf("expected ~90%% of traffic in hot set, got %.2f", share)
	}
}

func TestHotspotDefaultsAndValidation(t *testing.T) {
	if _, err := Compile(Config{Kind: "hotspot"}); err != nil {
		t.Fatalf("expected hotspot defaults to compile, got %v", err)
	}
	bad := []Config{
		{Kind: "hotspot", HotspotPercent: 0, HotspotTrafficPercent: 80},   // 0 -> default ok, so use 100
		{Kind: "hotspot", HotspotPercent: 100, HotspotTrafficPercent: 80}, // out of range
		{Kind: "hotspot", HotspotPercent: 20, HotspotTrafficPercent: 150}, // out of range
	}
	// First case (0) actually defaults; only the explicit out-of-range cases fail.
	if _, err := Compile(bad[1]); err == nil {
		t.Fatalf("expected hotspot_percent=100 to fail")
	}
	if _, err := Compile(bad[2]); err == nil {
		t.Fatalf("expected hotspot_traffic_percent=150 to fail")
	}
}

func TestHotspotEdgeCases(t *testing.T) {
	sel, _ := Compile(Config{Kind: "hotspot", HotspotPercent: 20, HotspotTrafficPercent: 80})
	rng := rand.New(rand.NewSource(1))
	if sel.Index(0, rng) != 0 {
		t.Fatalf("expected 0 for empty pool")
	}
	if sel.Index(1, rng) != 0 {
		t.Fatalf("expected 0 for single pool")
	}
	// Small pool where hotN would round to 0 must still stay in-range.
	for i := 0; i < 1000; i++ {
		idx := sel.Index(3, rng)
		if idx < 0 || idx >= 3 {
			t.Fatalf("index %d out of range for n=3", idx)
		}
	}
}

func TestUnknownKindFails(t *testing.T) {
	if _, err := Compile(Config{Kind: "bogus"}); err == nil {
		t.Fatalf("expected unknown kind to fail")
	}
}
