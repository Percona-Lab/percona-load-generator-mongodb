package stats

// WorkloadRecordingInfo describes the workload recording of a run. It holds
// no literal values; the recording file itself is never part of a report.
type WorkloadRecordingInfo struct {
	Path         string `json:"path"`
	OpsWritten   uint64 `json:"ops_written"`
	Bytes        int64  `json:"bytes"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	CommandsHash string `json:"commands_hash,omitempty"`
	// MaxBytes caps the uncompressed Bytes; 0 is unlimited.
	MaxBytes     int64  `json:"max_bytes"`
	Backpressure string `json:"backpressure,omitempty"`
	Compression  string `json:"compression,omitempty"`
	// FileBytes and CompressionRatio (Bytes / FileBytes) are set once the
	// file is closed.
	FileBytes        int64   `json:"file_bytes,omitempty"`
	CompressionRatio float64 `json:"compression_ratio,omitempty"`
	// Iterations is the number of iterations recorded, when more than one.
	Iterations int `json:"iterations,omitempty"`
	// Waits, WaitTotalMs, and WaitMaxMs report how long workers waited for
	// the writer (backpressure block only).
	Waits       uint64   `json:"waits,omitempty"`
	WaitTotalMs *float64 `json:"wait_total_ms,omitempty"`
	WaitMaxMs   *float64 `json:"wait_max_ms,omitempty"`
	// Outcomes is set when op lines carry their outcome; ResultChecksums
	// when find and aggregate outcomes carry a result checksum.
	Outcomes        bool `json:"outcomes,omitempty"`
	ResultChecksums bool `json:"result_checksums,omitempty"`
}

// WorkloadOutcomeFileInfo describes the outcome file a replay or a
// deterministic run wrote. It holds counts and hashes, never literal values.
type WorkloadOutcomeFileInfo struct {
	// Source is "replay" or "deterministic".
	Source          string `json:"source"`
	Path            string `json:"path"`
	Ops             uint64 `json:"ops"`
	Bytes           int64  `json:"bytes"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	ResultChecksums bool   `json:"result_checksums,omitempty"`
	// RecordingHeaderHash (replay) and TrafficFingerprint (deterministic)
	// identify the operations whose outcomes the file holds.
	RecordingHeaderHash string   `json:"recording_header_hash,omitempty"`
	TrafficFingerprint  string   `json:"traffic_fingerprint,omitempty"`
	Waits               uint64   `json:"waits,omitempty"`
	WaitTotalMs         *float64 `json:"wait_total_ms,omitempty"`
}

// SetWorkloadOutcomeFile stores the outcome file state reported with the
// insights.
func (c *Collector) SetWorkloadOutcomeFile(info *WorkloadOutcomeFileInfo) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	if info != nil {
		cp := *info
		cp.WaitTotalMs = copyFloat(info.WaitTotalMs)
		info = &cp
	}
	c.outcomeFile = info
	c.finalInsights = nil
}

// WorkloadReplayInfo describes a run that replayed a recording. It holds no
// literal values. HeaderHash identifies the recording: two runs with the same
// hash replayed the same operations.
type WorkloadReplayInfo struct {
	FileName     string `json:"file_name"`
	HeaderHash   string `json:"header_hash"`
	Mode         string `json:"mode"`
	Workers      int    `json:"workers"`
	OpsTotal     uint64 `json:"ops_total"`
	OpsReplayed  uint64 `json:"ops_replayed"`
	OpsFailed    uint64 `json:"ops_failed"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	SourceStatus string `json:"source_status"`
	// DurationIgnored is the configured duration, which a replay ignores.
	DurationIgnored string `json:"duration_ignored,omitempty"`
	// CommandsHash covers the commands sent, per worker in order;
	// CommandsHashMatch compares it with the recording's trailer.
	CommandsHash      string `json:"commands_hash,omitempty"`
	CommandsHashMatch *bool  `json:"commands_hash_match,omitempty"`
	// Compression is the recording's compression when it is not none;
	// Iterations is the number of recorded iterations when more than one.
	Compression string   `json:"compression,omitempty"`
	Iterations  int      `json:"iterations,omitempty"`
	LagMaxMs    *float64 `json:"lag_max_ms,omitempty"`
	LagAvgMs    *float64 `json:"lag_avg_ms,omitempty"`
	Mismatches  []string `json:"mismatches,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	// OpsFilter is "reads_only" when the replay skipped the OpsSkipped
	// write operations (workload_replay_ops).
	OpsFilter  string `json:"ops_filter,omitempty"`
	OpsSkipped uint64 `json:"ops_skipped,omitempty"`
	// DataFingerprint compares the target's starting data with the
	// fingerprint stored in the recording, when it has one.
	DataFingerprint *DataFingerprintCheck `json:"data_fingerprint,omitempty"`
	// ImportedFrom lists the captured-traffic sources (mongod_log,
	// system_profile, command) of an imported recording.
	ImportedFrom []string `json:"imported_from,omitempty"`
}

// DataFingerprintCheck is the starting-data comparison of a replay: counts,
// index names, and dbHash results per namespace, never document values.
type DataFingerprintCheck struct {
	Mode        string   `json:"mode"`
	Match       bool     `json:"match"`
	Differences []string `json:"differences,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

func (d *DataFingerprintCheck) clone() *DataFingerprintCheck {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Differences = append([]string(nil), d.Differences...)
	cp.Notes = append([]string(nil), d.Notes...)
	return &cp
}

// WorkloadTargetInfo names the target of one run of a multi-target replay
// or deterministic run (workload_replay_targets). It never holds the URI.
type WorkloadTargetInfo struct {
	Label string `json:"label"`
	// Index is 1-based; target 1 is the reference the others are compared
	// with.
	Index     int    `json:"index"`
	Targets   int    `json:"targets"`
	Mode      string `json:"mode"`
	Reference string `json:"reference"`
}

// SetWorkloadTarget stores the multi-target run identity reported with the
// insights.
func (c *Collector) SetWorkloadTarget(info *WorkloadTargetInfo) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	if info != nil {
		cp := *info
		info = &cp
	}
	c.target = info
	c.finalInsights = nil
}

// SetWorkloadRecording stores the recording state reported with the insights.
func (c *Collector) SetWorkloadRecording(info *WorkloadRecordingInfo) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	if info != nil {
		cp := *info
		cp.WaitTotalMs, cp.WaitMaxMs = copyFloat(info.WaitTotalMs), copyFloat(info.WaitMaxMs)
		info = &cp
	}
	c.recording = info
	c.finalInsights = nil
}

// SetWorkloadReplay stores the replay state reported with the insights.
func (c *Collector) SetWorkloadReplay(info *WorkloadReplayInfo) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	if info != nil {
		cp := *info
		cp.Mismatches = append([]string(nil), info.Mismatches...)
		cp.Warnings = append([]string(nil), info.Warnings...)
		cp.DataFingerprint = info.DataFingerprint.clone()
		info = &cp
	}
	c.replay = info
	c.finalInsights = nil
}

// WorkloadReplay returns a copy of the replay state, or nil.
func (c *Collector) WorkloadReplay() *WorkloadReplayInfo {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	if c.replay == nil {
		return nil
	}
	cp := *c.replay
	cp.DataFingerprint = c.replay.DataFingerprint.clone()
	return &cp
}

func copyFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (c *Collector) workloadRunMetadataLocked(md *InsightsMetadata) {
	if c.recording != nil {
		cp := *c.recording
		cp.WaitTotalMs, cp.WaitMaxMs = copyFloat(c.recording.WaitTotalMs), copyFloat(c.recording.WaitMaxMs)
		md.Recording = &cp
	}
	if c.replay != nil {
		cp := *c.replay
		cp.DataFingerprint = c.replay.DataFingerprint.clone()
		md.Replay = &cp
	}
	if c.target != nil {
		cp := *c.target
		md.Target = &cp
	}
	md.GeneratedTraffic = c.generatedTraffic.clone()
	if c.outcomeFile != nil {
		cp := *c.outcomeFile
		cp.WaitTotalMs = copyFloat(c.outcomeFile.WaitTotalMs)
		md.OutcomeFile = &cp
	}
}

// GeneratedTrafficInfo is the traffic fingerprint of a deterministic run
// (workload_deterministic). It holds hashes and counts, never literal
// values. Two complete runs with the same Fingerprint sent the same
// first pass of commands per worker, in the same order. Later passes
// continue that sequence until duration ends and are not part of the
// fingerprint, so the number of operations sent can differ.
type GeneratedTrafficInfo struct {
	// Fingerprint covers the header fields below and CommandsHash.
	Fingerprint string `json:"fingerprint"`
	// CommandsHash is the sha256 of the per-worker command hashes in
	// worker order.
	CommandsHash        string            `json:"commands_hash"`
	Status              string            `json:"status"`
	Reason              string            `json:"reason,omitempty"`
	PLGMVersion         string            `json:"plgm_version"`
	CollectionsHash     string            `json:"collections_hash"`
	QueriesHash         string            `json:"queries_hash"`
	RandomSeed          int64             `json:"random_seed"`
	Workers             int               `json:"workers"`
	OperationsPerWorker int               `json:"operations_per_worker"`
	Iterations          int               `json:"iterations"`
	OpsPlanned          uint64            `json:"ops_planned"`
	OpsGenerated        uint64            `json:"ops_generated"`
	OpsSent             uint64            `json:"ops_sent"`
	WorkerHashes        map[string]string `json:"worker_hashes"`
	// SafetyTimeout is the configured duration. The run repeats its
	// operation budget until this elapses. It is incomplete only when
	// duration ends before every worker finishes one pass.
	SafetyTimeout string `json:"safety_timeout,omitempty"`
}

func (g *GeneratedTrafficInfo) clone() *GeneratedTrafficInfo {
	if g == nil {
		return nil
	}
	cp := *g
	cp.WorkerHashes = make(map[string]string, len(g.WorkerHashes))
	for k, v := range g.WorkerHashes {
		cp.WorkerHashes[k] = v
	}
	return &cp
}

// SetGeneratedTraffic stores the traffic fingerprint reported with the
// insights.
func (c *Collector) SetGeneratedTraffic(info *GeneratedTrafficInfo) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	c.generatedTraffic = info.clone()
	c.finalInsights = nil
}

// GeneratedTraffic returns a copy of the traffic fingerprint, or nil.
func (c *Collector) GeneratedTraffic() *GeneratedTrafficInfo {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	return c.generatedTraffic.clone()
}
