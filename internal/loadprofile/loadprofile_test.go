package loadprofile

import (
	"testing"
	"time"
)

func TestCompileFixedDefaultsToFallback(t *testing.T) {
	sched, err := Compile(Config{}, 7)
	if err != nil {
		t.Fatalf("Compile(empty) error = %v", err)
	}
	if sched.Kind() != KindFixed {
		t.Fatalf("expected fixed kind, got %q", sched.Kind())
	}
	if sched.MaxWorkers() != 7 {
		t.Fatalf("expected peak 7 from fallback, got %d", sched.MaxWorkers())
	}
	for _, d := range []time.Duration{0, time.Second, time.Hour} {
		if got := sched.TargetAt(d); got != 7 {
			t.Fatalf("fixed TargetAt(%s) = %d, want 7", d, got)
		}
	}
}

func TestCompileFixedExplicitWorkersOverrideFallback(t *testing.T) {
	sched, err := Compile(Config{Kind: "fixed", Workers: 3}, 99)
	if err != nil {
		t.Fatalf("Compile error = %v", err)
	}
	if sched.MaxWorkers() != 3 || sched.TargetAt(time.Second) != 3 {
		t.Fatalf("expected fixed 3 workers, got peak=%d target=%d", sched.MaxWorkers(), sched.TargetAt(time.Second))
	}
}

func TestCompileRamp(t *testing.T) {
	sched, err := Compile(Config{Kind: "ramp", StartWorkers: 1, EndWorkers: 50, RampOver: "30s"}, 4)
	if err != nil {
		t.Fatalf("Compile ramp error = %v", err)
	}
	if sched.MaxWorkers() != 50 {
		t.Fatalf("expected peak 50, got %d", sched.MaxWorkers())
	}
	cases := []struct {
		at   time.Duration
		want int
	}{
		{0, 1},
		{15 * time.Second, 26}, // 1 + 49*0.5 = 25.5 -> 26
		{30 * time.Second, 50},
		{60 * time.Second, 50}, // clamps after ramp completes
	}
	for _, c := range cases {
		if got := sched.TargetAt(c.at); got != c.want {
			t.Fatalf("ramp TargetAt(%s) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestCompileRampDownwards(t *testing.T) {
	sched, err := Compile(Config{Kind: "ramp", StartWorkers: 40, EndWorkers: 10, RampOver: "10s"}, 4)
	if err != nil {
		t.Fatalf("Compile ramp error = %v", err)
	}
	if got := sched.TargetAt(5 * time.Second); got != 25 {
		t.Fatalf("ramp-down TargetAt(5s) = %d, want 25", got)
	}
	if got := sched.TargetAt(20 * time.Second); got != 10 {
		t.Fatalf("ramp-down TargetAt(20s) = %d, want 10", got)
	}
}

func TestCompileStep(t *testing.T) {
	sched, err := Compile(Config{Kind: "step", Steps: []Stage{
		{Workers: 5, Duration: "10s"},
		{Workers: 20, Duration: "10s"},
		{Workers: 2, Duration: "10s"},
	}}, 4)
	if err != nil {
		t.Fatalf("Compile step error = %v", err)
	}
	if sched.MaxWorkers() != 20 {
		t.Fatalf("expected peak 20, got %d", sched.MaxWorkers())
	}
	cases := []struct {
		at   time.Duration
		want int
	}{
		{0, 5},
		{9 * time.Second, 5},
		{10 * time.Second, 20},
		{19 * time.Second, 20},
		{25 * time.Second, 2},
		{500 * time.Second, 2}, // holds last stage after end
	}
	for _, c := range cases {
		if got := sched.TargetAt(c.at); got != c.want {
			t.Fatalf("step TargetAt(%s) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestCompileSpike(t *testing.T) {
	sched, err := Compile(Config{Kind: "spike", BaseWorkers: 4, PeakWorkers: 40, SpikeAt: "10s", SpikeFor: "5s"}, 4)
	if err != nil {
		t.Fatalf("Compile spike error = %v", err)
	}
	if sched.MaxWorkers() != 40 {
		t.Fatalf("expected peak 40, got %d", sched.MaxWorkers())
	}
	cases := []struct {
		at   time.Duration
		want int
	}{
		{0, 4},
		{9 * time.Second, 4},
		{10 * time.Second, 40},
		{14 * time.Second, 40},
		{15 * time.Second, 4},
		{60 * time.Second, 4},
	}
	for _, c := range cases {
		if got := sched.TargetAt(c.at); got != c.want {
			t.Fatalf("spike TargetAt(%s) = %d, want %d", c.at, got, c.want)
		}
	}
}

func TestCompileSpikeAllowsZeroSpikeAt(t *testing.T) {
	sched, err := Compile(Config{Kind: "spike", BaseWorkers: 2, PeakWorkers: 10, SpikeFor: "5s"}, 4)
	if err != nil {
		t.Fatalf("Compile spike error = %v", err)
	}
	if got := sched.TargetAt(0); got != 10 {
		t.Fatalf("spike with default spike_at TargetAt(0) = %d, want 10", got)
	}
	if got := sched.TargetAt(6 * time.Second); got != 2 {
		t.Fatalf("spike TargetAt(6s) = %d, want 2", got)
	}
}

func TestCompileSine(t *testing.T) {
	sched, err := Compile(Config{Kind: "sine", MinWorkers: 10, MaxWorkers: 30, Period: "20s"}, 4)
	if err != nil {
		t.Fatalf("Compile sine error = %v", err)
	}
	if sched.MaxWorkers() != 30 {
		t.Fatalf("expected peak 30, got %d", sched.MaxWorkers())
	}
	// Starts at the trough, rises to the mid then peak across the period.
	if got := sched.TargetAt(0); got != 10 {
		t.Fatalf("sine TargetAt(0) = %d, want trough 10", got)
	}
	if got := sched.TargetAt(5 * time.Second); got != 20 {
		t.Fatalf("sine TargetAt(quarter period) = %d, want mid 20", got)
	}
	if got := sched.TargetAt(10 * time.Second); got != 30 {
		t.Fatalf("sine TargetAt(half period) = %d, want peak 30", got)
	}
	// Bounds hold across many samples.
	for at := time.Duration(0); at <= 40*time.Second; at += 250 * time.Millisecond {
		got := sched.TargetAt(at)
		if got < 10 || got > 30 {
			t.Fatalf("sine TargetAt(%s) = %d out of [10,30]", at, got)
		}
	}
}

func TestCompileValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"unknown_kind", Config{Kind: "bogus"}},
		{"ramp_missing_duration", Config{Kind: "ramp", StartWorkers: 1, EndWorkers: 10}},
		{"ramp_bad_duration", Config{Kind: "ramp", StartWorkers: 1, EndWorkers: 10, RampOver: "notaduration"}},
		{"ramp_negative", Config{Kind: "ramp", StartWorkers: -1, EndWorkers: 10, RampOver: "10s"}},
		{"ramp_all_zero", Config{Kind: "ramp", StartWorkers: 0, EndWorkers: 0, RampOver: "10s"}},
		{"step_empty", Config{Kind: "step"}},
		{"step_bad_duration", Config{Kind: "step", Steps: []Stage{{Workers: 5, Duration: "x"}}}},
		{"step_negative", Config{Kind: "step", Steps: []Stage{{Workers: -2, Duration: "5s"}}}},
		{"step_all_zero", Config{Kind: "step", Steps: []Stage{{Workers: 0, Duration: "5s"}}}},
		{"spike_no_peak", Config{Kind: "spike", BaseWorkers: 2, SpikeFor: "5s"}},
		{"spike_bad_for", Config{Kind: "spike", PeakWorkers: 10, SpikeFor: "-5s"}},
		{"spike_bad_at", Config{Kind: "spike", PeakWorkers: 10, SpikeAt: "abc", SpikeFor: "5s"}},
		{"sine_no_max", Config{Kind: "sine", MinWorkers: 1, Period: "10s"}},
		{"sine_min_gt_max", Config{Kind: "sine", MinWorkers: 30, MaxWorkers: 10, Period: "10s"}},
		{"sine_bad_period", Config{Kind: "sine", MaxWorkers: 10, Period: "0s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Compile(c.cfg, 4); err == nil {
				t.Fatalf("expected validation error for %s", c.name)
			}
		})
	}
}

func TestTargetClampingAndZeroValue(t *testing.T) {
	var zero Schedule
	if !zero.IsZero() {
		t.Fatalf("expected zero-value schedule to report IsZero")
	}
	sched, err := Compile(Config{Kind: "fixed", Workers: 5}, 4)
	if err != nil {
		t.Fatalf("Compile error = %v", err)
	}
	if sched.IsZero() {
		t.Fatalf("compiled schedule should not be zero")
	}
	// Negative elapsed is treated as 0.
	if got := sched.TargetAt(-time.Second); got != 5 {
		t.Fatalf("TargetAt(negative) = %d, want 5", got)
	}
}
