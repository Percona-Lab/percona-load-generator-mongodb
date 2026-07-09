// Package accesspattern models how a workload selects which existing record to
// target, so benchmarks can emulate realistic, skewed access instead of always
// hitting documents uniformly at random. Skewed access (a small "hot" subset of
// documents receiving most traffic) is what exposes cache pressure, lock
// contention, and hot-shard behavior that uniform access hides.
//
// The package is dependency-free and deterministic given an *math/rand.Rand, so
// the selection math is trivially unit-testable. A Config compiles into a
// stateless Selector that maps a pool size to an index per call.
package accesspattern

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
)

// Kind enumerates the supported access distributions.
type Kind string

const (
	// KindUniform selects any record with equal probability (historical default).
	KindUniform Kind = "uniform"
	// KindZipfian skews selection toward a few records via a power-law.
	KindZipfian Kind = "zipfian"
	// KindHotspot sends a configurable share of traffic to a small subset.
	KindHotspot Kind = "hotspot"
)

const (
	defaultZipfianExponent     = 1.2
	defaultHotspotPercent      = 20
	defaultHotspotTrafficShare = 80
)

// Config is the user-facing, serializable access-pattern description embedded in
// the application config. An empty Kind means uniform, preserving prior behavior.
type Config struct {
	Kind string `yaml:"kind" json:"kind"`

	// ZipfianExponent controls zipfian skew; higher concentrates more traffic on
	// the hottest records. Must be >= 1.0 (1.0 is effectively uniform).
	ZipfianExponent float64 `yaml:"zipfian_exponent" json:"zipfian_exponent"`

	// HotspotPercent is the share of records considered "hot" (1..99).
	// HotspotTrafficPercent is the share of operations directed at the hot set
	// (1..100). For example 20/80 means 20% of records receive 80% of traffic.
	HotspotPercent        int `yaml:"hotspot_percent" json:"hotspot_percent"`
	HotspotTrafficPercent int `yaml:"hotspot_traffic_percent" json:"hotspot_traffic_percent"`
}

// NormalizedKind returns the lower-cased kind, defaulting to uniform.
func (c Config) NormalizedKind() Kind {
	k := Kind(strings.ToLower(strings.TrimSpace(c.Kind)))
	if k == "" {
		return KindUniform
	}
	return k
}

// Selector maps a pool size to a selected index using the configured
// distribution. Implementations are stateless and safe for concurrent use; the
// caller supplies a per-goroutine *rand.Rand.
type Selector interface {
	// Index returns an index in [0, n). For n <= 0 it returns 0.
	Index(n int, rng *rand.Rand) int
	// Kind reports the distribution kind for reporting/UI.
	Kind() Kind
	// Summary returns a short human-readable description.
	Summary() string
}

// Compile validates a Config and returns a stateless Selector.
func Compile(cfg Config) (Selector, error) {
	switch cfg.NormalizedKind() {
	case KindUniform:
		return uniformSelector{}, nil
	case KindZipfian:
		exp := cfg.ZipfianExponent
		if exp == 0 {
			exp = defaultZipfianExponent
		}
		if exp < 1.0 {
			return nil, fmt.Errorf("zipfian access pattern requires zipfian_exponent >= 1.0, got %g", exp)
		}
		return zipfianSelector{exponent: exp}, nil
	case KindHotspot:
		hot := cfg.HotspotPercent
		if hot == 0 {
			hot = defaultHotspotPercent
		}
		traffic := cfg.HotspotTrafficPercent
		if traffic == 0 {
			traffic = defaultHotspotTrafficShare
		}
		if hot < 1 || hot > 99 {
			return nil, fmt.Errorf("hotspot access pattern requires hotspot_percent in 1..99, got %d", hot)
		}
		if traffic < 1 || traffic > 100 {
			return nil, fmt.Errorf("hotspot access pattern requires hotspot_traffic_percent in 1..100, got %d", traffic)
		}
		return hotspotSelector{hotPercent: hot, trafficPercent: traffic}, nil
	default:
		return nil, fmt.Errorf("unknown access pattern kind %q (expected one of uniform, zipfian, hotspot)", cfg.Kind)
	}
}

type uniformSelector struct{}

func (uniformSelector) Index(n int, rng *rand.Rand) int {
	if n <= 0 {
		return 0
	}
	if rng == nil {
		return 0
	}
	return rng.Intn(n)
}
func (uniformSelector) Kind() Kind      { return KindUniform }
func (uniformSelector) Summary() string { return "uniform access" }

type zipfianSelector struct{ exponent float64 }

// Index maps a uniform sample u in [0,1) to idx = floor(n * u^exponent). For
// exponent > 1 this concentrates probability mass on low indices (the hottest
// records), giving an n-independent power-law skew without per-call setup.
func (z zipfianSelector) Index(n int, rng *rand.Rand) int {
	if n <= 0 || rng == nil {
		return 0
	}
	if n == 1 {
		return 0
	}
	u := rng.Float64()
	idx := int(float64(n) * math.Pow(u, z.exponent))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}
func (z zipfianSelector) Kind() Kind      { return KindZipfian }
func (z zipfianSelector) Summary() string { return fmt.Sprintf("zipfian (exponent %.2f)", z.exponent) }

type hotspotSelector struct {
	hotPercent     int
	trafficPercent int
}

// Index sends trafficPercent of selections into the hot set (the first
// hotPercent of indices) and the remainder into the cold set.
func (h hotspotSelector) Index(n int, rng *rand.Rand) int {
	if n <= 0 || rng == nil {
		return 0
	}
	if n == 1 {
		return 0
	}
	hotN := (n * h.hotPercent) / 100
	if hotN < 1 {
		hotN = 1
	}
	if hotN >= n {
		hotN = n - 1
	}
	if rng.Intn(100) < h.trafficPercent {
		return rng.Intn(hotN)
	}
	coldN := n - hotN
	if coldN <= 0 {
		return rng.Intn(hotN)
	}
	return hotN + rng.Intn(coldN)
}
func (h hotspotSelector) Kind() Kind { return KindHotspot }
func (h hotspotSelector) Summary() string {
	return fmt.Sprintf("hotspot (%d%% of records take %d%% of traffic)", h.hotPercent, h.trafficPercent)
}
