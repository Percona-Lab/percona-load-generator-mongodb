// Package loadprofile models dynamic concurrency over the lifetime of a
// benchmark run. Instead of a fixed number of workers for the whole run, a
// profile describes how many workers should be active at any elapsed time so
// the engine can emulate ramp-ups, steps, spikes, and periodic bursts.
//
// The package is intentionally free of any MongoDB or workload dependencies so
// the scheduling math is trivially unit-testable. A Config is compiled into an
// immutable Schedule that the runner samples via TargetAt as the run proceeds.
package loadprofile

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Kind enumerates the supported load-shaping strategies.
type Kind string

const (
	// KindFixed keeps a constant worker count (the historical behavior).
	KindFixed Kind = "fixed"
	// KindRamp linearly moves from StartWorkers to EndWorkers over RampOver.
	KindRamp Kind = "ramp"
	// KindStep holds discrete worker counts for configured stage durations.
	KindStep Kind = "step"
	// KindSpike runs at BaseWorkers and jumps to PeakWorkers during a window.
	KindSpike Kind = "spike"
	// KindSine oscillates worker count between MinWorkers and MaxWorkers.
	KindSine Kind = "sine"
)

// Stage is one segment of a step profile: a worker count held for a duration.
type Stage struct {
	Workers  int    `yaml:"workers" json:"workers"`
	Duration string `yaml:"duration" json:"duration"`
}

// Config is the user-facing, serializable description of a load profile. It is
// embedded in the application config and accepted from the web UI/CLI. An empty
// Kind is treated as "fixed" so existing configs keep working unchanged.
type Config struct {
	Kind string `yaml:"kind" json:"kind"`

	// Fixed
	Workers int `yaml:"workers" json:"workers"`

	// Ramp
	StartWorkers int    `yaml:"start_workers" json:"start_workers"`
	EndWorkers   int    `yaml:"end_workers" json:"end_workers"`
	RampOver     string `yaml:"ramp_over" json:"ramp_over"`

	// Step
	Steps []Stage `yaml:"steps" json:"steps"`

	// Spike
	BaseWorkers int    `yaml:"base_workers" json:"base_workers"`
	PeakWorkers int    `yaml:"peak_workers" json:"peak_workers"`
	SpikeAt     string `yaml:"spike_at" json:"spike_at"`
	SpikeFor    string `yaml:"spike_for" json:"spike_for"`

	// Sine
	MinWorkers int    `yaml:"min_workers" json:"min_workers"`
	MaxWorkers int    `yaml:"max_workers" json:"max_workers"`
	Period     string `yaml:"period" json:"period"`
}

// NormalizedKind returns the lower-cased kind, defaulting to fixed.
func (c Config) NormalizedKind() Kind {
	k := Kind(strings.ToLower(strings.TrimSpace(c.Kind)))
	if k == "" {
		return KindFixed
	}
	return k
}

// Schedule is a compiled, immutable load profile. It is safe to share across
// goroutines because TargetAt is a pure function of elapsed time.
type Schedule struct {
	kind     Kind
	peak     int
	summary  string
	targetFn func(elapsed time.Duration) int
}

// Kind returns the profile kind backing this schedule.
func (s Schedule) Kind() Kind {
	if s.kind == "" {
		return KindFixed
	}
	return s.kind
}

// MaxWorkers returns the peak worker count the schedule can request. The runner
// pre-provisions this many workers and gates how many are active over time.
func (s Schedule) MaxWorkers() int { return s.peak }

// Summary returns a short human-readable description for logs/UI/reports.
func (s Schedule) Summary() string {
	if s.summary == "" {
		return string(s.Kind())
	}
	return s.summary
}

// TargetAt returns the desired number of active workers at the given elapsed
// time, always clamped to [0, MaxWorkers].
func (s Schedule) TargetAt(elapsed time.Duration) int {
	if s.targetFn == nil {
		return s.peak
	}
	if elapsed < 0 {
		elapsed = 0
	}
	t := s.targetFn(elapsed)
	if t < 0 {
		t = 0
	}
	if t > s.peak {
		t = s.peak
	}
	return t
}

// IsZero reports whether the schedule is an uninitialized zero value. The runner
// uses this to fall back to a fixed schedule for directly-constructed configs.
func (s Schedule) IsZero() bool { return s.targetFn == nil && s.peak == 0 }

// Compile validates a Config and returns an immutable Schedule. fallbackWorkers
// is used as the worker count for fixed profiles when Config.Workers is unset,
// preserving the historical AppConfig.Concurrency behavior.
func Compile(cfg Config, fallbackWorkers int) (Schedule, error) {
	if fallbackWorkers < 1 {
		fallbackWorkers = 1
	}
	switch cfg.NormalizedKind() {
	case KindFixed:
		return compileFixed(cfg, fallbackWorkers)
	case KindRamp:
		return compileRamp(cfg)
	case KindStep:
		return compileStep(cfg)
	case KindSpike:
		return compileSpike(cfg)
	case KindSine:
		return compileSine(cfg)
	default:
		return Schedule{}, fmt.Errorf("unknown load profile kind %q (expected one of fixed, ramp, step, spike, sine)", cfg.Kind)
	}
}

func compileFixed(cfg Config, fallbackWorkers int) (Schedule, error) {
	workers := cfg.Workers
	if workers == 0 {
		workers = fallbackWorkers
	}
	if workers < 1 {
		return Schedule{}, fmt.Errorf("fixed load profile requires workers >= 1, got %d", workers)
	}
	w := workers
	return Schedule{
		kind:     KindFixed,
		peak:     w,
		summary:  fmt.Sprintf("fixed %d workers", w),
		targetFn: func(time.Duration) int { return w },
	}, nil
}

func compileRamp(cfg Config) (Schedule, error) {
	if cfg.StartWorkers < 0 || cfg.EndWorkers < 0 {
		return Schedule{}, fmt.Errorf("ramp load profile requires non-negative worker counts (start=%d end=%d)", cfg.StartWorkers, cfg.EndWorkers)
	}
	if cfg.StartWorkers == 0 && cfg.EndWorkers == 0 {
		return Schedule{}, fmt.Errorf("ramp load profile requires at least one of start_workers/end_workers to be > 0")
	}
	over, err := parsePositiveDuration("ramp_over", cfg.RampOver)
	if err != nil {
		return Schedule{}, err
	}
	start := cfg.StartWorkers
	end := cfg.EndWorkers
	peak := maxInt(start, end)
	return Schedule{
		kind:    KindRamp,
		peak:    peak,
		summary: fmt.Sprintf("ramp %d -> %d over %s", start, end, over),
		targetFn: func(elapsed time.Duration) int {
			if elapsed >= over {
				return end
			}
			frac := float64(elapsed) / float64(over)
			return int(math.Round(float64(start) + (float64(end)-float64(start))*frac))
		},
	}, nil
}

func compileStep(cfg Config) (Schedule, error) {
	if len(cfg.Steps) == 0 {
		return Schedule{}, fmt.Errorf("step load profile requires at least one stage")
	}
	type boundary struct {
		until   time.Duration
		workers int
	}
	boundaries := make([]boundary, 0, len(cfg.Steps))
	var cumulative time.Duration
	peak := 0
	for i, st := range cfg.Steps {
		if st.Workers < 0 {
			return Schedule{}, fmt.Errorf("step %d requires non-negative workers, got %d", i+1, st.Workers)
		}
		d, err := parsePositiveDuration(fmt.Sprintf("steps[%d].duration", i+1), st.Duration)
		if err != nil {
			return Schedule{}, err
		}
		cumulative += d
		boundaries = append(boundaries, boundary{until: cumulative, workers: st.Workers})
		peak = maxInt(peak, st.Workers)
	}
	if peak < 1 {
		return Schedule{}, fmt.Errorf("step load profile requires at least one stage with workers >= 1")
	}
	lastWorkers := boundaries[len(boundaries)-1].workers
	return Schedule{
		kind:    KindStep,
		peak:    peak,
		summary: fmt.Sprintf("step profile with %d stages (peak %d)", len(boundaries), peak),
		targetFn: func(elapsed time.Duration) int {
			for _, b := range boundaries {
				if elapsed < b.until {
					return b.workers
				}
			}
			return lastWorkers
		},
	}, nil
}

func compileSpike(cfg Config) (Schedule, error) {
	if cfg.BaseWorkers < 0 || cfg.PeakWorkers < 0 {
		return Schedule{}, fmt.Errorf("spike load profile requires non-negative worker counts (base=%d peak=%d)", cfg.BaseWorkers, cfg.PeakWorkers)
	}
	if cfg.PeakWorkers < 1 {
		return Schedule{}, fmt.Errorf("spike load profile requires peak_workers >= 1, got %d", cfg.PeakWorkers)
	}
	at, err := parseNonNegativeDuration("spike_at", cfg.SpikeAt)
	if err != nil {
		return Schedule{}, err
	}
	dur, err := parsePositiveDuration("spike_for", cfg.SpikeFor)
	if err != nil {
		return Schedule{}, err
	}
	base := cfg.BaseWorkers
	peakWorkers := cfg.PeakWorkers
	spikeEnd := at + dur
	return Schedule{
		kind:    KindSpike,
		peak:    maxInt(base, peakWorkers),
		summary: fmt.Sprintf("spike %d -> %d for %s at %s", base, peakWorkers, dur, at),
		targetFn: func(elapsed time.Duration) int {
			if elapsed >= at && elapsed < spikeEnd {
				return peakWorkers
			}
			return base
		},
	}, nil
}

func compileSine(cfg Config) (Schedule, error) {
	if cfg.MinWorkers < 0 || cfg.MaxWorkers < 0 {
		return Schedule{}, fmt.Errorf("sine load profile requires non-negative worker counts (min=%d max=%d)", cfg.MinWorkers, cfg.MaxWorkers)
	}
	if cfg.MaxWorkers < 1 {
		return Schedule{}, fmt.Errorf("sine load profile requires max_workers >= 1, got %d", cfg.MaxWorkers)
	}
	if cfg.MinWorkers > cfg.MaxWorkers {
		return Schedule{}, fmt.Errorf("sine load profile requires min_workers <= max_workers (min=%d max=%d)", cfg.MinWorkers, cfg.MaxWorkers)
	}
	period, err := parsePositiveDuration("period", cfg.Period)
	if err != nil {
		return Schedule{}, err
	}
	min := float64(cfg.MinWorkers)
	max := float64(cfg.MaxWorkers)
	mid := (min + max) / 2
	amp := (max - min) / 2
	periodSec := period.Seconds()
	return Schedule{
		kind:    KindSine,
		peak:    cfg.MaxWorkers,
		summary: fmt.Sprintf("sine %d..%d over %s", cfg.MinWorkers, cfg.MaxWorkers, period),
		targetFn: func(elapsed time.Duration) int {
			// Start at the trough so the run warms up rather than starting hot.
			phase := (2 * math.Pi * elapsed.Seconds() / periodSec) - math.Pi/2
			return int(math.Round(mid + amp*math.Sin(phase)))
		},
	}, nil
}

func parsePositiveDuration(field, raw string) (time.Duration, error) {
	d, err := parseDurationField(field, raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", field, raw)
	}
	return d, nil
}

func parseNonNegativeDuration(field, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	d, err := parseDurationField(field, raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %q", field, raw)
	}
	return d, nil
}

func parseDurationField(field, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s is required (for example \"30s\" or \"2m\")", field)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration %q: %w", field, raw, err)
	}
	return d, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
