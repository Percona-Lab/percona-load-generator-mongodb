package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	WorkloadTargetsSequential = "sequential"
	WorkloadTargetsConcurrent = "concurrent"

	WorkloadReplayOpsAll       = "all"
	WorkloadReplayOpsReadsOnly = "reads_only"

	DataFingerprintOff    = "off"
	DataFingerprintCounts = "counts"
	DataFingerprintDBHash = "dbhash"

	MinWorkloadReplayTargets = 2
	MaxWorkloadReplayTargets = 8
)

// ReplayTarget is one instance of a multi-target replay. Label names its
// outcome file, run metadata, and trend run; the URI is never reported.
type ReplayTarget struct {
	Label string `yaml:"label" json:"label"`
	URI   string `yaml:"uri" json:"uri"`
}

var replayTargetLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ParseReplayTargets reads "label=uri" entries separated by ";" or new
// lines (PLGM_WORKLOAD_REPLAY_TARGETS, --replay-target). Only the first "="
// splits, so URI options are kept. Empty entries are skipped; malformed ones
// are kept for ValidateWorkloadTargets to report.
func ParseReplayTargets(s string) []ReplayTarget {
	var out []ReplayTarget
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, uri, ok := strings.Cut(part, "=")
		if !ok {
			label, uri = "", part
		}
		out = append(out, ReplayTarget{Label: strings.TrimSpace(label), URI: strings.TrimSpace(uri)})
	}
	return out
}

// CloneQueries deep-copies query definitions. RunWorkload normalizes its
// queries in place, so the targets of a concurrent run each need a copy.
func CloneQueries(defs []QueryDefinition) []QueryDefinition {
	if defs == nil {
		return nil
	}
	out := make([]QueryDefinition, len(defs))
	for i, q := range defs {
		q.Filter, _ = cloneValue(q.Filter).(map[string]interface{})
		q.Pipeline, _ = cloneValue(q.Pipeline).([]interface{})
		q.Projection, _ = cloneValue(q.Projection).(map[string]interface{})
		q.Update, _ = cloneValue(q.Update).(map[string]interface{})
		out[i] = q
	}
	return out
}

// CloneCollections deep-copies collection definitions. RunWorkload
// normalizes their index and shard keys in place, so the targets of a
// concurrent run each need a copy.
func CloneCollections(defs []CollectionDefinition) []CollectionDefinition {
	if defs == nil {
		return nil
	}
	out := make([]CollectionDefinition, len(defs))
	for i, c := range defs {
		c.Fields = cloneFields(c.Fields)
		if c.Indexes != nil {
			idx := make([]IndexDefinition, len(c.Indexes))
			for j, d := range c.Indexes {
				d.Keys = append(IndexKeys(nil), d.Keys...)
				d.PartialFilterExpression = append([]byte(nil), d.PartialFilterExpression...)
				if d.ExpireAfterSeconds != nil {
					v := *d.ExpireAfterSeconds
					d.ExpireAfterSeconds = &v
				}
				idx[j] = d
			}
			c.Indexes = idx
		}
		if c.ShardConfig != nil {
			sc := *c.ShardConfig
			sc.Key = append(IndexKeys(nil), sc.Key...)
			c.ShardConfig = &sc
		}
		out[i] = c
	}
	return out
}

func cloneFields(fields map[string]CollectionField) map[string]CollectionField {
	if fields == nil {
		return nil
	}
	out := make(map[string]CollectionField, len(fields))
	for k, f := range fields {
		f.Enum, _ = cloneValue(f.Enum).([]interface{})
		f.Fields = cloneFields(f.Fields)
		if f.Items != nil {
			items := cloneFields(map[string]CollectionField{"": *f.Items})[""]
			f.Items = &items
		}
		out[k] = f
	}
	return out
}

func cloneValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		if t == nil {
			return t
		}
		m := make(map[string]interface{}, len(t))
		for k, x := range t {
			m[k] = cloneValue(x)
		}
		return m
	case bson.M:
		return bson.M(cloneValue(map[string]interface{}(t)).(map[string]interface{}))
	case bson.D:
		d := make(bson.D, len(t))
		for i, e := range t {
			d[i] = bson.E{Key: e.Key, Value: cloneValue(e.Value)}
		}
		return d
	case []interface{}:
		if t == nil {
			return t
		}
		a := make([]interface{}, len(t))
		for i, x := range t {
			a[i] = cloneValue(x)
		}
		return a
	case bson.A:
		return bson.A(cloneValue([]interface{}(t)).([]interface{}))
	}
	return v
}

// TargetPath is the per-target variant of a file path: the label goes
// before the extension (outcomes.ndjson -> outcomes.mongo8.ndjson).
func TargetPath(path, label string) string {
	if path == "" {
		return ""
	}
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "." + label + ext
}

func normalizeWorkloadTargets(cfg *AppConfig) {
	for i := range cfg.WorkloadReplayTargets {
		t := &cfg.WorkloadReplayTargets[i]
		t.Label, t.URI = strings.TrimSpace(t.Label), strings.TrimSpace(t.URI)
	}
	cfg.WorkloadReplayTargetsMode = strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayTargetsMode))
	if cfg.WorkloadReplayTargetsMode == "" {
		cfg.WorkloadReplayTargetsMode = WorkloadTargetsSequential
	}
	cfg.WorkloadReplayOps = strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayOps))
	if cfg.WorkloadReplayOps == "" {
		cfg.WorkloadReplayOps = WorkloadReplayOpsAll
	}
	cfg.WorkloadDataFingerprint = strings.ToLower(strings.TrimSpace(cfg.WorkloadDataFingerprint))
	if cfg.WorkloadDataFingerprint == "" {
		cfg.WorkloadDataFingerprint = DataFingerprintOff
	}
}

// DataFingerprintMode is the normalized workload_data_fingerprint.
func DataFingerprintMode(cfg *AppConfig) string {
	if cfg == nil {
		return DataFingerprintOff
	}
	switch m := strings.ToLower(strings.TrimSpace(cfg.WorkloadDataFingerprint)); m {
	case "", DataFingerprintOff:
		return DataFingerprintOff
	default:
		return m
	}
}

// ReplayReadsOnly reports whether a replay skips its write operations.
func ReplayReadsOnly(cfg *AppConfig) bool {
	return cfg != nil && cfg.WorkloadReplayEnabled && strings.EqualFold(strings.TrimSpace(cfg.WorkloadReplayOps), WorkloadReplayOpsReadsOnly)
}

// ConcurrentTargets reports whether the targets run at the same time.
func ConcurrentTargets(cfg *AppConfig) bool {
	return cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.WorkloadReplayTargetsMode), WorkloadTargetsConcurrent)
}

// ValidateWorkloadTargets rejects multi-target, replay-ops, and data
// fingerprint settings that cannot run. Targets need a replay or a
// deterministic run, outcome capture with an outcome file path (each
// target writes its own copy, compared with the first target), distinct
// labels and URIs, and no recording.
func ValidateWorkloadTargets(cfg *AppConfig) error {
	if cfg == nil {
		return nil
	}
	const prefix = "invalid multi-target settings: "
	switch strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayTargetsMode)) {
	case "", WorkloadTargetsSequential, WorkloadTargetsConcurrent:
	default:
		return fmt.Errorf(prefix+"workload_replay_targets_mode must be %q or %q (got %q)", WorkloadTargetsSequential, WorkloadTargetsConcurrent, cfg.WorkloadReplayTargetsMode)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.WorkloadReplayOps)) {
	case "", WorkloadReplayOpsAll:
	case WorkloadReplayOpsReadsOnly:
		if !cfg.WorkloadReplayEnabled {
			return fmt.Errorf(prefix + "workload_replay_ops reads_only needs workload_replay_enabled")
		}
	default:
		return fmt.Errorf(prefix+"workload_replay_ops must be %q or %q (got %q)", WorkloadReplayOpsAll, WorkloadReplayOpsReadsOnly, cfg.WorkloadReplayOps)
	}
	switch m := DataFingerprintMode(cfg); m {
	case DataFingerprintOff:
	case DataFingerprintCounts, DataFingerprintDBHash:
		if !cfg.WorkloadRecordEnabled && !cfg.WorkloadReplayEnabled {
			return fmt.Errorf(prefix+"workload_data_fingerprint %q needs workload_record_enabled (the recording stores it) or workload_replay_enabled", m)
		}
	default:
		return fmt.Errorf(prefix+"workload_data_fingerprint must be %q, %q, or %q (got %q)", DataFingerprintOff, DataFingerprintCounts, DataFingerprintDBHash, cfg.WorkloadDataFingerprint)
	}
	targets := cfg.WorkloadReplayTargets
	if len(targets) == 0 {
		return nil
	}
	if len(targets) < MinWorkloadReplayTargets || len(targets) > MaxWorkloadReplayTargets {
		return fmt.Errorf(prefix+"workload_replay_targets needs %d to %d targets (got %d); use uri for a single instance", MinWorkloadReplayTargets, MaxWorkloadReplayTargets, len(targets))
	}
	if !cfg.WorkloadReplayEnabled && !cfg.WorkloadDeterministic {
		return fmt.Errorf(prefix + "workload_replay_targets needs workload_replay_enabled or workload_deterministic")
	}
	if cfg.WorkloadRecordEnabled {
		return fmt.Errorf(prefix + "workload_replay_targets cannot be combined with workload_record_enabled: every target would write the same recording file. Record on one instance first (workload_record_enabled, no targets), then replay that recording on the targets (workload_replay_enabled with workload_replay_targets)")
	}
	labels, uris := map[string]bool{}, map[string]bool{}
	for i, t := range targets {
		label, uri := strings.TrimSpace(t.Label), strings.TrimSpace(t.URI)
		if !replayTargetLabelPattern.MatchString(label) {
			return fmt.Errorf(prefix+"target %d: label %q must be 1-32 letters, digits, '.', '_' or '-', starting with a letter or digit", i+1, label)
		}
		if labels[strings.ToLower(label)] {
			return fmt.Errorf(prefix+"target label %q is used twice", label)
		}
		labels[strings.ToLower(label)] = true
		if !strings.HasPrefix(uri, "mongodb://") && !strings.HasPrefix(uri, "mongodb+srv://") {
			return fmt.Errorf(prefix+"target %q: uri must start with mongodb:// or mongodb+srv://", label)
		}
		if strings.ContainsAny(uri, "\x00\r\n") {
			return fmt.Errorf(prefix+"target %q: uri is not valid", label)
		}
		if uris[uri] {
			return fmt.Errorf(prefix+"target %q: the same uri is listed twice", label)
		}
		uris[uri] = true
	}
	outPath := strings.TrimSpace(cfg.WorkloadReplayOutcomesPath)
	outName := "workload_replay_outcomes_path"
	if !cfg.WorkloadReplayEnabled {
		outPath, outName = strings.TrimSpace(cfg.WorkloadDeterministicOutcomesPath), "workload_deterministic_outcomes_path"
	}
	if !cfg.WorkloadRecordOutcomes || outPath == "" {
		return fmt.Errorf(prefix+"workload_replay_targets needs workload_record_outcomes on and %s set: every target writes its own outcome file (the label goes before the extension) and is compared with the first target", outName)
	}
	return nil
}
