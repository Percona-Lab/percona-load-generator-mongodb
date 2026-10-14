package stats

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type OperationEvent struct {
	Operation       string   `json:"operation"`
	Database        string   `json:"database,omitempty"`
	Collection      string   `json:"collection"`
	ShapeKey        string   `json:"shape_key"`
	ShapeSummary    string   `json:"shape_summary"`
	QueryLabel      string   `json:"query_label,omitempty"`
	QuerySource     string   `json:"query_source,omitempty"`
	WorkloadName    string   `json:"workload_name,omitempty"`
	QueryFile       string   `json:"query_file,omitempty"`
	QueryDefID      string   `json:"query_definition_id,omitempty"`
	QueryDefIndex   int      `json:"query_definition_index"`
	QuerySummary    string   `json:"representative_query_summary,omitempty"`
	PipelineSummary string   `json:"representative_pipeline_summary,omitempty"`
	FilterFields    []string `json:"filter_fields,omitempty"`
	DurationMs      float64  `json:"duration_ms"`
	Success         bool     `json:"success"`
	Iteration       int      `json:"iteration"`
	TimestampMs     int64    `json:"timestamp_ms"`
}

// ExecutableQuery is the query exactly as an operation sent it to the server,
// including literal values. It exists only to build post-run explain commands
// and must never be serialized into API, UI, or export payloads; those use the
// redacted shape/summary fields instead.
type ExecutableQuery struct {
	Database   string
	Filter     map[string]interface{}
	Projection map[string]interface{}
	Limit      int64
	Update     map[string]interface{}
	Upsert     bool
	Pipeline   []interface{}
}

func cloneExecutableQuery(q *ExecutableQuery) *ExecutableQuery {
	if q == nil {
		return nil
	}
	return &ExecutableQuery{
		Database:   q.Database,
		Filter:     cloneMapDeep(q.Filter),
		Projection: cloneMapDeep(q.Projection),
		Limit:      q.Limit,
		Update:     cloneMapDeep(q.Update),
		Upsert:     q.Upsert,
		Pipeline:   cloneSliceDeep(q.Pipeline),
	}
}

type ExplainDiagnostics struct {
	ReplayDB                string   `json:"replay_db,omitempty"`
	ReplayCollection        string   `json:"replay_collection,omitempty"`
	Verbosity               string   `json:"verbosity,omitempty"`
	ServerMaxTimeMS         int      `json:"server_max_time_ms,omitempty"`
	ClientTimeoutMS         int      `json:"client_timeout_ms,omitempty"`
	StageSummary            string   `json:"stage_summary,omitempty"`
	CommandSummary          string   `json:"command_summary,omitempty"`
	ServerVersion           string   `json:"server_version,omitempty"`
	VerbosityNote           string   `json:"verbosity_note,omitempty"`
	UnfilteredScanExpected  bool     `json:"unfiltered_scan_expected,omitempty"`
	ExecutionStatsAvailable bool     `json:"execution_stats_available"`
	ElapsedMS               int      `json:"elapsed_ms,omitempty"`
	WinningPlanSummary      string   `json:"winning_plan_summary,omitempty"`
	PlanStages              []string `json:"plan_stages,omitempty"`
	IndexesUsed             []string `json:"indexes_used,omitempty"`
	CollectionScanDetected  bool     `json:"collection_scan_detected"`
	IndexScanDetected       bool     `json:"index_scan_detected"`
	FetchDetected           bool     `json:"fetch_detected"`
	GroupDetected           bool     `json:"group_detected"`
	SortDetected            bool     `json:"sort_detected"`
	LimitDetected           bool     `json:"limit_detected"`
	BlockingSortDetected    bool     `json:"blocking_sort_detected"`
	UsedDisk                bool     `json:"used_disk"`
	DocsExamined            int64    `json:"docs_examined,omitempty"`
	KeysExamined            int64    `json:"keys_examined,omitempty"`
	NReturned               int64    `json:"n_returned,omitempty"`
	NMatched                int64    `json:"n_matched,omitempty"`
	CollectionScans         int64    `json:"collection_scans,omitempty"`
	IndexSeeks              int64    `json:"index_seeks,omitempty"`
	ExecutionTimeMillis     int64    `json:"execution_time_millis,omitempty"`
	Spills                  int64    `json:"spills,omitempty"`
	ExaminedToReturnedRatio float64  `json:"examined_to_returned_ratio,omitempty"`
	KeysToReturnedRatio     float64  `json:"keys_to_returned_ratio,omitempty"`
	ShardDetailsSummary     string   `json:"shard_details_summary,omitempty"`
	// Metrics are the normalized counters; the flat counters above read
	// unreported values as 0 and exist for existing consumers.
	Metrics                  *ExplainMetrics `json:"metrics,omitempty"`
	Diagnosis                string          `json:"diagnosis,omitempty"`
	Interpretation           string          `json:"interpretation,omitempty"`
	Recommendation           string          `json:"recommendation,omitempty"`
	RecommendationConfidence string          `json:"recommendation_confidence,omitempty"`
	EvidenceSummary          string          `json:"evidence_summary,omitempty"`
}

// ExplainCounter is one execution counter and the explain field it came from.
// LowerBound marks a substitute field that can only undercount.
type ExplainCounter struct {
	Value      int64  `json:"value"`
	Source     string `json:"source"`
	LowerBound bool   `json:"lower_bound,omitempty"`
}

// ExplainMetrics are execution counters normalized across explain layouts.
// A nil counter was not reported and is never read as zero. Each counter is
// the layout's authoritative total when it has one, otherwise the sum of
// non-overlapping shard counters; parent and child totals are never added.
// Returned is the command's final output; AccessReturned is the documents the
// access stage fed into an aggregation pipeline.
type ExplainMetrics struct {
	Layout            string          `json:"layout"`
	Shards            int             `json:"shards,omitempty"`
	DocsExamined      *ExplainCounter `json:"docs_examined,omitempty"`
	KeysExamined      *ExplainCounter `json:"keys_examined,omitempty"`
	Returned          *ExplainCounter `json:"returned,omitempty"`
	AccessReturned    *ExplainCounter `json:"access_returned,omitempty"`
	Matched           *ExplainCounter `json:"matched,omitempty"`
	UnsupportedReason string          `json:"unsupported_reason,omitempty"`
	Notes             []string        `json:"notes,omitempty"`
	// Lookup is the joined-collection work reported by $lookup pipeline
	// stages; nil when the plan has no separate $lookup stage.
	Lookup *LookupMetrics `json:"lookup,omitempty"`
	// SortSpill are the per-stage sort, spill, memory, and timing fields of
	// the executed plan.
	SortSpill *SortSpillMetrics `json:"sort_spill,omitempty"`
}

// Sort/spill measurement statuses.
const (
	SortSpillMeasured    = "measured"
	SortSpillPartial     = "partial"
	SortSpillUnavailable = "unavailable"
)

// SortSpillMetrics are the sort, spill, memory, and timing fields that the
// stages of one explain's executed plan reported. Stages are listed
// individually and never summed: rejected plans, the queryPlanner tree, the
// echoed command, and router nodes above shard plans are not read.
type SortSpillMetrics struct {
	Status string                `json:"status"`
	Reason string                `json:"reason,omitempty"`
	Source string                `json:"source"`
	Stages []StageResourceMetric `json:"stages,omitempty"`
}

// StageResourceMetric is one plan or pipeline stage that reported sort,
// spill, or memory fields. A nil field was not reported; a zero was reported
// as zero. Each counter keeps the explain field it came from because the
// fields mean different things: SortBytesProcessed is the data the sort
// handled (not peak memory), SpilledBytes is the server's size of the records
// written to temporary files, SpillStorageBytes is the storage they used, and
// MemoryLimitBytes is a limit, not usage. TimeMsEstimate is the stage's
// executionTimeMillisEstimate, which includes its input stages.
type StageResourceMetric struct {
	Path               string          `json:"path"`
	Stage              string          `json:"stage"`
	Sort               bool            `json:"sort"`
	Engine             string          `json:"engine"`
	Namespace          string          `json:"namespace,omitempty"`
	Shard              string          `json:"shard,omitempty"`
	ServerVersion      string          `json:"server_version,omitempty"`
	SortBytesProcessed *ExplainCounter `json:"sort_bytes_processed,omitempty"`
	UsedDisk           *bool           `json:"used_disk,omitempty"`
	Spills             *ExplainCounter `json:"spills,omitempty"`
	SpilledRecords     *ExplainCounter `json:"spilled_records,omitempty"`
	SpilledBytes       *ExplainCounter `json:"spilled_bytes,omitempty"`
	SpillStorageBytes  *ExplainCounter `json:"spill_storage_bytes,omitempty"`
	MemoryLimitBytes   *ExplainCounter `json:"memory_limit_bytes,omitempty"`
	PeakMemory         []MemoryCounter `json:"peak_memory,omitempty"`
	TimeMsEstimate     *ExplainCounter `json:"time_ms_estimate,omitempty"`
	Missing            []string        `json:"missing,omitempty"`
	// Caveats name reported values that must not be read at face value.
	Caveats []string `json:"caveats,omitempty"`
}

// CaveatSBESortBytesWithoutSpill marks a slot-based sort on MongoDB 7.0/8.0
// that reported totalDataSizeSorted 0 without spilling and returned rows (or
// did not report nReturned). Those versions count sort bytes only for runs
// sorted before the field is read, which happens only on a spill, so the 0 is
// kept as reported but is not a measurement. No other field or zero is
// affected.
const CaveatSBESortBytesWithoutSpill = "sbe_sort_bytes_reported_only_after_spill"

// SortBytesUsable reports whether SortBytesProcessed is a measurement.
func (s StageResourceMetric) SortBytesUsable() bool {
	if s.SortBytesProcessed == nil {
		return false
	}
	for _, c := range s.Caveats {
		if c == CaveatSBESortBytesWithoutSpill {
			return false
		}
	}
	return true
}

// MemoryCounter is a reported peak memory value; Scope says whose memory it
// is (the stage, or one named accumulator).
type MemoryCounter struct {
	Scope string `json:"scope"`
	ExplainCounter
}

// LookupMetrics sum the counters of every $lookup stage of one explain. A nil
// counter means at least one $lookup stage did not report it.
type LookupMetrics struct {
	Stages          int             `json:"stages"`
	DocsExamined    *ExplainCounter `json:"docs_examined,omitempty"`
	KeysExamined    *ExplainCounter `json:"keys_examined,omitempty"`
	CollectionScans *ExplainCounter `json:"collection_scans,omitempty"`
}

// ExplainSample is one retained operation chosen for a post-run explain.
// ClientLatencyMs was measured by the client during the run; the diagnostics
// timings were measured later by the server and are not directly comparable.
type ExplainSample struct {
	Role              string  `json:"role"`
	TargetLatencyMs   float64 `json:"target_latency_ms"`
	ClientLatencyMs   float64 `json:"client_latency_ms"`
	Status            string  `json:"status"`
	Reason            string  `json:"reason,omitempty"`
	Attempts          int     `json:"attempts"`
	LatencyComparison string  `json:"latency_comparison,omitempty"`
	// SharedCommand marks a sample whose explain command was identical to an
	// earlier sample's, so it ran once and adds no independent evidence.
	SharedCommand bool                `json:"shared_command,omitempty"`
	Diagnostics   *ExplainDiagnostics `json:"diagnostics,omitempty"`
}

// ExplainSelection describes how explain samples were chosen for a shape and
// which population they represent.
type ExplainSelection struct {
	Method                    string  `json:"method"`
	Scope                     string  `json:"scope"`
	ScopeNote                 string  `json:"scope_note"`
	RetainedSuccessfulSamples int     `json:"retained_successful_samples"`
	ExecutableSamples         int     `json:"executable_samples"`
	MedianMs                  float64 `json:"median_ms"`
	P95Ms                     float64 `json:"p95_ms"`
}

// ExplainFinding is the shape-level diagnosis built from all explained samples.
type ExplainFinding struct {
	Diagnosis           string   `json:"diagnosis"`
	Title               string   `json:"title"`
	Namespace           string   `json:"namespace"`
	Interpretation      string   `json:"interpretation"`
	Recommendation      string   `json:"recommendation"`
	Confidence          string   `json:"confidence"`
	ConfidenceBasis     string   `json:"confidence_basis"`
	Evidence            []string `json:"evidence"`
	IndexesUsed         []string `json:"indexes_used,omitempty"`
	SamplesSelected     int      `json:"samples_selected"`
	SamplesExplained    int      `json:"samples_explained"`
	SamplesInconclusive int      `json:"samples_inconclusive"`
	SamplesDisagree     bool     `json:"samples_disagree"`
	TimingNote          string   `json:"timing_note"`
}

type InsightsMetadata struct {
	Status                string  `json:"status"`
	ShapeIdentityVersion  string  `json:"shape_identity_version"`
	GeneratedAt           string  `json:"generated_at,omitempty"`
	SlowThresholdMs       float64 `json:"slow_threshold_ms"`
	SamplingRate          float64 `json:"sampling_rate"`
	RetainedEvents        int     `json:"retained_events"`
	EligibleEvents        uint64  `json:"eligible_events"`
	SampledInEvents       uint64  `json:"sampled_in_events"`
	DroppedGroupEntries   int     `json:"dropped_group_entries"`
	FilteredByThreshold   int     `json:"filtered_by_threshold"`
	FilteredBySeverity    int     `json:"filtered_by_severity"`
	EvidenceLevel         string  `json:"evidence_level"`
	ExplainEnabled        bool    `json:"explain_enabled"`
	ExplainMode           string  `json:"explain_mode"`
	ExplainVerbosity      string  `json:"explain_verbosity"`
	ExplainSeverityMode   string  `json:"explain_severity_mode"`
	ExplainEfficiencyMode string  `json:"explain_efficiency_mode,omitempty"`
	ExplainWorkers        int     `json:"explain_workers"`
	ExplainRetries        int     `json:"explain_retries"`
	ExplainBackoffMS      int     `json:"explain_backoff_ms"`
	ExplainImpact         string  `json:"explain_impact"`

	ExplainSelectionMethod string `json:"explain_selection_method,omitempty"`
	ExplainSelectionScope  string `json:"explain_selection_scope,omitempty"`
	ExplainSelectionNote   string `json:"explain_selection_note,omitempty"`
	ExplainTimingNote      string `json:"explain_timing_note,omitempty"`
	ExplainMaxSamples      int    `json:"explain_max_samples,omitempty"`
	ExplainSamplesPlanned  int    `json:"explain_samples_planned"`
	ExplainCommandsRun     int    `json:"explain_commands_run"`

	LatencyTargets LatencyTargets `json:"latency_targets"`
	// WorkloadDurationSec is the measured workload window; nil when the start
	// or end of the window was not recorded.
	WorkloadDurationSec *float64 `json:"workload_duration_sec,omitempty"`
}

type InsightsSummary struct {
	TotalSampledEvents int     `json:"total_sampled_events"`
	SlowSampledEvents  int     `json:"slow_sampled_events"`
	SlowSampledRatio   float64 `json:"slow_sampled_ratio"`
	TopSeverity        string  `json:"top_severity"`
}

type SlowQueryInsight struct {
	Rank             int                 `json:"rank"`
	ShapeID          string              `json:"shape_id"`
	Operation        string              `json:"operation"`
	Collection       string              `json:"collection"`
	ShapeKey         string              `json:"shape_key"`
	ShapeSummary     string              `json:"shape_summary"`
	QueryLabel       string              `json:"query_label,omitempty"`
	QuerySource      string              `json:"query_source,omitempty"`
	WorkloadName     string              `json:"workload_name,omitempty"`
	QueryFile        string              `json:"query_file,omitempty"`
	QueryDefID       string              `json:"query_definition_id,omitempty"`
	QueryDefIndex    int                 `json:"query_definition_index"`
	QuerySummary     string              `json:"representative_query_summary,omitempty"`
	PipelineSummary  string              `json:"representative_pipeline_summary,omitempty"`
	QueryRefVariants int                 `json:"query_reference_variants,omitempty"`
	FilterFields     []string            `json:"filter_fields,omitempty"`
	Count            int                 `json:"count"`
	ErrorCount       int                 `json:"error_count"`
	SlowCount        int                 `json:"slow_count"`
	SlowRatio        float64             `json:"slow_ratio"`
	AvgMs            float64             `json:"avg_ms"`
	P95Ms            float64             `json:"p95_ms"`
	P99Ms            float64             `json:"p99_ms"`
	MaxMs            float64             `json:"max_ms"`
	Severity         string              `json:"severity"`
	ExplainStatus    string              `json:"explain_status,omitempty"`
	ExplainReason    string              `json:"explain_reason,omitempty"`
	ExplainDiag      *ExplainDiagnostics `json:"explain_diagnostics,omitempty"`
	ExplainSelection *ExplainSelection   `json:"explain_selection,omitempty"`
	ExplainSamples   []ExplainSample     `json:"explain_samples,omitempty"`
	ExplainFinding   *ExplainFinding     `json:"explain_finding,omitempty"`
}

type CollectionInsight struct {
	Collection string   `json:"collection"`
	Count      int      `json:"count"`
	SlowCount  int      `json:"slow_count"`
	SlowRatio  float64  `json:"slow_ratio"`
	AvgMs      float64  `json:"avg_ms"`
	P95Ms      float64  `json:"p95_ms"`
	P99Ms      float64  `json:"p99_ms"`
	MaxMs      float64  `json:"max_ms"`
	TopOps     []string `json:"top_ops,omitempty"`
}

type IterationInsight struct {
	Iteration int     `json:"iteration"`
	Count     int     `json:"count"`
	SlowCount int     `json:"slow_count"`
	SlowRatio float64 `json:"slow_ratio"`
	AvgMs     float64 `json:"avg_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
}

type TimeSliceInsight struct {
	BucketLabel string  `json:"bucket_label"`
	Count       int     `json:"count"`
	SlowCount   int     `json:"slow_count"`
	AvgMs       float64 `json:"avg_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
}

type IndexIssue struct {
	Rank             int      `json:"rank"`
	ShapeID          string   `json:"shape_id"`
	Collection       string   `json:"collection"`
	Operation        string   `json:"operation"`
	ShapeKey         string   `json:"shape_key"`
	QueryLabel       string   `json:"query_label,omitempty"`
	QuerySource      string   `json:"query_source,omitempty"`
	WorkloadName     string   `json:"workload_name,omitempty"`
	QueryFile        string   `json:"query_file,omitempty"`
	QueryDefID       string   `json:"query_definition_id,omitempty"`
	QueryDefIndex    int      `json:"query_definition_index"`
	QuerySummary     string   `json:"representative_query_summary,omitempty"`
	PipelineSummary  string   `json:"representative_pipeline_summary,omitempty"`
	QueryRefVariants int      `json:"query_reference_variants,omitempty"`
	FilterFields     []string `json:"filter_fields,omitempty"`
	Count            int      `json:"count"`
	AvgMs            float64  `json:"avg_ms"`
	P95Ms            float64  `json:"p95_ms"`
	P99Ms            float64  `json:"p99_ms"`
	MaxMs            float64  `json:"max_ms"`
	EvidenceLevel    string   `json:"evidence_level"`
	Confidence       string   `json:"confidence"`
	ExplainStatus    string   `json:"explain_status,omitempty"`
	ExplainReason    string   `json:"explain_reason,omitempty"`
	Diagnosis        string   `json:"diagnosis,omitempty"`
	Evidence         []string `json:"evidence,omitempty"`
	Message          string   `json:"message"`
	Recommendation   string   `json:"recommendation"`
}

type InsightRecommendation struct {
	Rank            int      `json:"rank"`
	Priority        string   `json:"priority"`
	Title           string   `json:"title"`
	Details         string   `json:"details"`
	Confidence      string   `json:"confidence"`
	Source          string   `json:"source"`
	ShapeID         string   `json:"shape_id,omitempty"`
	Diagnosis       string   `json:"diagnosis,omitempty"`
	Evidence        []string `json:"evidence,omitempty"`
	Namespace       string   `json:"namespace,omitempty"`
	ShapeIDs        []string `json:"shape_ids,omitempty"`
	AdviceKind      string   `json:"advice_kind,omitempty"`
	CandidateKeys   []string `json:"candidate_keys,omitempty"`
	ExistingIndex   string   `json:"existing_index,omitempty"`
	InventoryStatus string   `json:"inventory_status,omitempty"`
	Metrics         []string `json:"metrics,omitempty"`
	WorkloadImpact  string   `json:"workload_impact,omitempty"`
	Rationale       string   `json:"rationale,omitempty"`
	Limitations     []string `json:"limitations,omitempty"`
	Tradeoffs       string   `json:"tradeoffs,omitempty"`
	ValidationStep  string   `json:"validation_step,omitempty"`
}

type InsightsReport struct {
	Summary              InsightsSummary         `json:"summary"`
	Impact               ImpactView              `json:"impact"`
	QueryCapture         QueryCaptureView        `json:"query_capture"`
	ExplainCoverage      ExplainCoverageView     `json:"explain_coverage"`
	EfficiencyCoverage   *EfficiencyCoverageView `json:"efficiency_coverage,omitempty"`
	EfficiencyView       *EfficiencyView         `json:"efficiency_view,omitempty"`
	Trends               *TrendView              `json:"trends,omitempty"`
	IndexInventory       *IndexInventoryView     `json:"index_inventory,omitempty"`
	SlowQueries          []SlowQueryInsight      `json:"slow_queries"`
	AffectedCollections  []CollectionInsight     `json:"affected_collections"`
	QueryShapes          []SlowQueryInsight      `json:"query_shapes"`
	PotentialIndexIssues []IndexIssue            `json:"potential_index_issues"`
	Recommendations      []InsightRecommendation `json:"recommendations"`
	PerIteration         []IterationInsight      `json:"per_iteration,omitempty"`
	TimeSlices           []TimeSliceInsight      `json:"time_slices,omitempty"`
	Metadata             InsightsMetadata        `json:"metadata"`
}

type insightsSettings struct {
	enabled          bool
	samplePermille   int
	sampleRate       float64
	slowThresholdMs  float64
	maxEvents        int
	maxGroups        int
	explainEnabled   bool
	explainTopN      int
	explainMaxMs     int
	explainVerbosity string
	explainSeverity  string
	explainEffMode   string
	explainWorkers   int
	explainRetries   int
	explainBackoff   int
	latencyTargets   LatencyTargets
}

type collectionIndexInfo struct {
	IndexKeys [][]string
}

type groupAgg struct {
	Operation        string
	Collection       string
	ShapeKey         string
	ShapeSummary     string
	QueryLabel       string
	QuerySource      string
	WorkloadName     string
	QueryFile        string
	QueryDefID       string
	QueryDefIndex    int
	QuerySummary     string
	PipelineSummary  string
	QueryRefVariants int
	refCounts        map[string]int
	FilterFields     []string
	Count            int
	ErrorCount       int
	SlowCount        int
	SumMs            float64
	MaxMs            float64
	latencies        []float64
}

func defaultInsightsSettings() insightsSettings {
	return insightsSettings{
		enabled:          true,
		samplePermille:   100,
		sampleRate:       0.10,
		slowThresholdMs:  200.0,
		maxEvents:        5000,
		maxGroups:        300,
		explainEnabled:   false,
		explainTopN:      5,
		explainMaxMs:     1000,
		explainVerbosity: "executionStats",
		explainSeverity:  "high_only",
		explainEffMode:   config.ExplainEfficiencyModeDefault,
		explainWorkers:   1,
		explainRetries:   1,
		explainBackoff:   150,
	}
}

func StableShapeID(operation, collection, shapeKey string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(operation))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(collection))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(shapeKey))
	return fmt.Sprintf("shape_%x", h.Sum64())
}

func (c *Collector) configureInsights(cfg *config.AppConfig) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()

	s := defaultInsightsSettings()
	if cfg != nil {
		s.enabled = cfg.InsightsEnabled
		if cfg.InsightsSamplingRate > 0 {
			if cfg.InsightsSamplingRate > 1 {
				s.sampleRate = 1
			} else {
				s.sampleRate = cfg.InsightsSamplingRate
			}
		}
		if cfg.InsightsSlowThresholdMs > 0 {
			s.slowThresholdMs = float64(cfg.InsightsSlowThresholdMs)
		}
		if cfg.InsightsMaxEvents > 0 {
			s.maxEvents = cfg.InsightsMaxEvents
		}
		if cfg.InsightsMaxGroups > 0 {
			s.maxGroups = cfg.InsightsMaxGroups
		}
		s.explainEnabled = cfg.InsightsExplainEnabled
		if cfg.InsightsExplainTopN > 0 {
			s.explainTopN = cfg.InsightsExplainTopN
		}
		if cfg.InsightsExplainMaxTimeMS > 0 {
			s.explainMaxMs = cfg.InsightsExplainMaxTimeMS
		}
		switch cfg.InsightsExplainVerbosity {
		case "queryPlanner", "executionStats":
			s.explainVerbosity = cfg.InsightsExplainVerbosity
		default:
			s.explainVerbosity = "executionStats"
		}
		s.explainSeverity = normalizeExplainSeverityMode(cfg.InsightsExplainSeverityMode)
		s.explainEffMode = config.NormalizeExplainEfficiencyMode(cfg.InsightsExplainEfficiencyMode)
		if cfg.InsightsExplainWorkers > 0 {
			s.explainWorkers = cfg.InsightsExplainWorkers
		}
		if cfg.InsightsExplainRetries >= 0 {
			s.explainRetries = cfg.InsightsExplainRetries
		}
		if cfg.InsightsExplainBackoffMS >= 0 {
			s.explainBackoff = cfg.InsightsExplainBackoffMS
		}
		s.latencyTargets = NewLatencyTargets(cfg.InsightsLatencyTargetAvgMs, cfg.InsightsLatencyTargetP95Ms, cfg.InsightsLatencyTargetP99Ms)
	}

	s.samplePermille = int(s.sampleRate * 1000)
	if s.samplePermille < 1 && s.sampleRate > 0 {
		s.samplePermille = 1
	}
	if s.samplePermille > 1000 {
		s.samplePermille = 1000
	}

	c.insightsCfg = s
	c.explainCapture.Store(s.enabled && s.explainEnabled)
	captureReason := ""
	switch {
	case !s.enabled:
		captureReason = "insights_disabled"
	case !s.explainEnabled:
		captureReason = "explain_disabled"
	}
	c.capture.configure(s.enabled && s.explainEnabled, captureReason)
	if c.insightEvents == nil || cap(c.insightEvents) != s.maxEvents {
		c.insightEvents = make([]OperationEvent, s.maxEvents)
	}
}

func (c *Collector) SetCollectionsForInsights(cols []config.CollectionDefinition) {
	m := make(map[string]collectionIndexInfo, len(cols))
	for _, col := range cols {
		info := collectionIndexInfo{}
		for _, idx := range col.Indexes {
			// Only field overlap is checked here, so the sorted set is sufficient.
			keys := idx.Keys.Fields()
			sort.Strings(keys)
			if len(keys) > 0 {
				info.IndexKeys = append(info.IndexKeys, keys)
			}
		}
		m[col.Name] = info
	}

	c.insightsMu.Lock()
	c.collectionIndexes = m
	c.finalInsights = nil
	c.insightsMu.Unlock()
}

func (c *Collector) ResetInsights() {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	c.insightWrite = 0
	c.insightCount = 0
	c.insightEligible = 0
	c.insightSampledIn = 0
	c.impact.reset()
	c.capture.reset()
	c.finalInsights = nil
}

// CapturesExecutableQueries reports whether RecordOperationEvent retains the
// executable query, so hot paths can skip building one when it would be dropped.
func (c *Collector) CapturesExecutableQueries() bool {
	return c.explainCapture.Load()
}

func (c *Collector) RecordOperationEvent(op, database, collection, shapeKey, shapeSummary string, filterFields []string, duration time.Duration, success bool, iteration int, query *ExecutableQuery, queryLabel, querySource, workloadName, queryFile, queryDefID, querySummary, pipelineSummary string, queryDefIndex int) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()

	if !c.insightsCfg.enabled {
		return
	}
	g := c.impact.record(op, database, collection, shapeKey, shapeSummary, queryLabel, duration, success, c.insightsCfg.maxGroups)
	c.capture.observe(g, op, success, query, duration.Nanoseconds())
	c.finalInsights = nil
	if c.insightsCfg.maxEvents <= 0 {
		return
	}

	c.insightEligible++
	if !shouldSample(c.insightEligible, c.insightsCfg.samplePermille) {
		return
	}
	c.insightSampledIn++

	if iteration < 1 {
		iteration = 1
	}

	ev := OperationEvent{
		Operation:       op,
		Database:        database,
		Collection:      collection,
		ShapeKey:        shapeKey,
		ShapeSummary:    shapeSummary,
		QueryLabel:      queryLabel,
		QuerySource:     querySource,
		WorkloadName:    workloadName,
		QueryFile:       queryFile,
		QueryDefID:      queryDefID,
		QueryDefIndex:   queryDefIndex,
		QuerySummary:    querySummary,
		PipelineSummary: pipelineSummary,
		FilterFields:    dedupeAndSortStrings(filterFields),
		DurationMs:      float64(duration.Nanoseconds()) / 1e6,
		Success:         success,
		Iteration:       iteration,
		TimestampMs:     time.Now().UnixMilli(),
	}
	c.insightEvents[c.insightWrite] = ev
	c.insightWrite = (c.insightWrite + 1) % c.insightsCfg.maxEvents
	if c.insightCount < c.insightsCfg.maxEvents {
		c.insightCount++
	}
}

func shouldSample(seq uint64, permille int) bool {
	if permille >= 1000 {
		return true
	}
	if permille <= 0 {
		return false
	}
	return int(seq%1000) < permille
}

func (c *Collector) GetFinalInsights() InsightsReport {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()

	if !c.insightsCfg.enabled {
		return InsightsReport{
			Impact:             ImpactView{Status: "disabled", Coverage: ImpactCoverage{TimeBasis: ImpactTimeBasis}, Groups: []WorkloadImpact{}},
			QueryCapture:       disabledCaptureView("insights_disabled", c.insightsCfg.slowThresholdMs, c.insightsCfg.maxGroups),
			ExplainCoverage:    NewExplainCoverage("disabled", "insights_disabled"),
			EfficiencyCoverage: NewEfficiencyCoverage("disabled", "insights_disabled"),
			Metadata: InsightsMetadata{
				Status:               "disabled",
				ShapeIdentityVersion: ShapeIdentityVersion,
				EvidenceLevel:        "none",
				SamplingRate:         c.insightsCfg.sampleRate,
				SlowThresholdMs:      c.insightsCfg.slowThresholdMs,
				ExplainEnabled:       c.insightsCfg.explainEnabled,
				ExplainMode:          "disabled",
				ExplainVerbosity:     c.insightsCfg.explainVerbosity,
				ExplainSeverityMode:  c.insightsCfg.explainSeverity,
				ExplainWorkers:       c.insightsCfg.explainWorkers,
				ExplainRetries:       c.insightsCfg.explainRetries,
				ExplainBackoffMS:     c.insightsCfg.explainBackoff,
				ExplainImpact:        "none",
				LatencyTargets:       c.insightsCfg.latencyTargets,
			},
		}
	}

	if c.finalInsights != nil {
		return *c.finalInsights
	}

	events := c.snapshotInsightEventsLocked()
	report := buildInsightsReport(
		events,
		c.collectionIndexes,
		c.insightsCfg.slowThresholdMs,
		c.insightsCfg.maxGroups,
		c.insightsCfg.sampleRate,
		c.insightEligible,
		c.insightSampledIn,
		c.insightsCfg.explainEnabled,
		c.insightsCfg.explainVerbosity,
		c.insightsCfg.explainSeverity,
		c.insightsCfg.explainWorkers,
		c.insightsCfg.explainRetries,
		c.insightsCfg.explainBackoff,
	)
	report.Impact = buildImpactView(&c.impact, events, c.insightsCfg.maxGroups, c.insightSampledIn)
	report.QueryCapture = buildQueryCaptureView(&c.impact, &c.capture, c.insightsCfg.slowThresholdMs, c.insightsCfg.maxGroups)
	report.Metadata.ExplainEfficiencyMode = c.insightsCfg.explainEffMode
	report.Metadata.LatencyTargets = c.insightsCfg.latencyTargets
	if d, ok := c.WorkloadDuration(); ok {
		sec := roundTo(d.Seconds(), 3)
		report.Metadata.WorkloadDurationSec = &sec
	}
	if c.insightsCfg.explainEnabled {
		report.ExplainCoverage = NewExplainCoverage("pending", "explain_runs_after_the_run_completes")
	} else {
		report.ExplainCoverage = NewExplainCoverage("disabled", "explain_disabled")
	}
	report.EfficiencyCoverage = NewEfficiencyCoverage(report.ExplainCoverage.Status, report.ExplainCoverage.Reason)
	c.finalInsights = &report
	return report
}

func (c *Collector) GetExplainSettings() (enabled bool, topN int, maxTimeMs int, verbosity string, severityMode string, workers int, retries int, backoffMs int) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	return c.insightsCfg.explainEnabled, c.insightsCfg.explainTopN, c.insightsCfg.explainMaxMs, c.insightsCfg.explainVerbosity, c.insightsCfg.explainSeverity, c.insightsCfg.explainWorkers, c.insightsCfg.explainRetries, c.insightsCfg.explainBackoff
}

// GetExplainEfficiencyMode returns the normalized insights_explain_efficiency_mode.
func (c *Collector) GetExplainEfficiencyMode() string {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	return c.insightsCfg.explainEffMode
}

func (c *Collector) SnapshotOperationEvents() []OperationEvent {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	return c.snapshotInsightEventsLocked()
}

func (c *Collector) snapshotInsightEventsLocked() []OperationEvent {
	if c.insightCount == 0 || len(c.insightEvents) == 0 {
		return nil
	}
	out := make([]OperationEvent, 0, c.insightCount)
	start := c.insightWrite - c.insightCount
	if start < 0 {
		start += len(c.insightEvents)
	}
	for i := 0; i < c.insightCount; i++ {
		idx := (start + i) % len(c.insightEvents)
		out = append(out, c.insightEvents[idx])
	}
	return out
}

func buildInsightsReport(
	events []OperationEvent,
	indexes map[string]collectionIndexInfo,
	slowThresholdMs float64,
	maxGroups int,
	samplingRate float64,
	eligible uint64,
	sampledIn uint64,
	explainEnabled bool,
	explainVerbosity string,
	explainSeverity string,
	explainWorkers int,
	explainRetries int,
	explainBackoff int,
) InsightsReport {
	explainSeverity = normalizeExplainSeverityMode(explainSeverity)
	if explainVerbosity != "queryPlanner" {
		explainVerbosity = "executionStats"
	}
	rep := InsightsReport{
		Metadata: InsightsMetadata{
			Status:               "ready",
			ShapeIdentityVersion: ShapeIdentityVersion,
			GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
			SlowThresholdMs:      slowThresholdMs,
			SamplingRate:         samplingRate,
			EligibleEvents:       eligible,
			SampledInEvents:      sampledIn,
			RetainedEvents:       len(events),
			EvidenceLevel:        "heuristic",
			ExplainEnabled:       explainEnabled,
			ExplainMode:          boolToMode(explainEnabled),
			ExplainVerbosity:     explainVerbosity,
			ExplainSeverityMode:  explainSeverity,
			ExplainWorkers:       explainWorkers,
			ExplainRetries:       explainRetries,
			ExplainBackoffMS:     explainBackoff,
			ExplainImpact:        explainImpact(explainEnabled),
		},
	}

	if len(events) == 0 {
		rep.Metadata.Status = "empty"
		rep.Summary.TopSeverity = "none"
		return rep
	}

	groups := make(map[string]*groupAgg, maxGroups)
	byCollection := make(map[string]*groupAgg)
	byIteration := make(map[int]*groupAgg)
	byTimeSlice := make(map[int64]*groupAgg)
	droppedGroupEntries := 0
	slowTotal := 0
	firstTS := events[0].TimestampMs

	for _, ev := range events {
		if ev.DurationMs >= slowThresholdMs {
			slowTotal++
		}

		groupKey := fmt.Sprintf("%s|%s|%s", ev.Operation, ev.Collection, ev.ShapeKey)
		grp, ok := groups[groupKey]
		if !ok {
			if len(groups) >= maxGroups {
				droppedGroupEntries++
			} else {
				grp = &groupAgg{
					Operation:    ev.Operation,
					Collection:   ev.Collection,
					ShapeKey:     ev.ShapeKey,
					ShapeSummary: ev.ShapeSummary,
					FilterFields: ev.FilterFields,
				}
				groups[groupKey] = grp
			}
		}
		if grp != nil {
			updateGroupAgg(grp, ev, slowThresholdMs)
		}

		collKey := ev.Collection
		if collKey == "" {
			collKey = "(unknown)"
		}
		collAgg, ok := byCollection[collKey]
		if !ok {
			collAgg = &groupAgg{Collection: collKey}
			byCollection[collKey] = collAgg
		}
		updateGroupAgg(collAgg, ev, slowThresholdMs)

		iterAgg, ok := byIteration[ev.Iteration]
		if !ok {
			iterAgg = &groupAgg{}
			byIteration[ev.Iteration] = iterAgg
		}
		updateGroupAgg(iterAgg, ev, slowThresholdMs)

		relSec := int64(0)
		if ev.TimestampMs >= firstTS {
			relSec = (ev.TimestampMs - firstTS) / 1000
		}
		tsAgg, ok := byTimeSlice[relSec]
		if !ok {
			tsAgg = &groupAgg{}
			byTimeSlice[relSec] = tsAgg
		}
		updateGroupAgg(tsAgg, ev, slowThresholdMs)
	}

	rep.Metadata.DroppedGroupEntries = droppedGroupEntries
	rep.Summary.TotalSampledEvents = len(events)
	rep.Summary.SlowSampledEvents = slowTotal
	rep.Summary.SlowSampledRatio = ratio(slowTotal, len(events))

	slowGroupsAll := make([]SlowQueryInsight, 0, len(groups))
	for _, g := range groups {
		slowGroupsAll = append(slowGroupsAll, toSlowInsight(g))
	}
	filteredGroups := make([]SlowQueryInsight, 0, len(slowGroupsAll))
	filteredByThreshold := 0
	filteredBySeverity := 0
	for _, g := range slowGroupsAll {
		// Keep only groups that actually crossed slow-threshold at least once.
		if g.SlowCount <= 0 {
			filteredByThreshold++
			continue
		}
		if !isSeverityExplainEligible(g.Severity, explainSeverity) {
			filteredBySeverity++
			continue
		}
		filteredGroups = append(filteredGroups, g)
	}
	rep.Metadata.FilteredByThreshold = filteredByThreshold
	rep.Metadata.FilteredBySeverity = filteredBySeverity

	sort.SliceStable(filteredGroups, func(i, j int) bool {
		if filteredGroups[i].Severity != filteredGroups[j].Severity {
			return severityScore(filteredGroups[i].Severity) > severityScore(filteredGroups[j].Severity)
		}
		if filteredGroups[i].P99Ms != filteredGroups[j].P99Ms {
			return filteredGroups[i].P99Ms > filteredGroups[j].P99Ms
		}
		return filteredGroups[i].Count > filteredGroups[j].Count
	})
	for i := range filteredGroups {
		filteredGroups[i].Rank = i + 1
	}

	limitSlow := 20
	if len(filteredGroups) < limitSlow {
		limitSlow = len(filteredGroups)
	}
	rep.SlowQueries = filteredGroups[:limitSlow]
	applyDefaultExplainStatus(rep.SlowQueries, explainEnabled)

	shapeView := make([]SlowQueryInsight, len(filteredGroups))
	copy(shapeView, filteredGroups)
	sort.SliceStable(shapeView, func(i, j int) bool {
		if shapeView[i].P95Ms != shapeView[j].P95Ms {
			return shapeView[i].P95Ms > shapeView[j].P95Ms
		}
		return shapeView[i].Count > shapeView[j].Count
	})
	if len(shapeView) > 15 {
		shapeView = shapeView[:15]
	}
	rep.QueryShapes = shapeView
	applyDefaultExplainStatus(rep.QueryShapes, explainEnabled)

	allowedCollections := make(map[string]struct{}, len(filteredGroups))
	for _, g := range filteredGroups {
		allowedCollections[g.Collection] = struct{}{}
	}
	colls := make([]CollectionInsight, 0, len(byCollection))
	for _, g := range byCollection {
		if g.SlowCount <= 0 {
			continue
		}
		if _, ok := allowedCollections[g.Collection]; !ok {
			continue
		}
		colls = append(colls, CollectionInsight{
			Collection: g.Collection,
			Count:      g.Count,
			SlowCount:  g.SlowCount,
			SlowRatio:  ratio(g.SlowCount, g.Count),
			AvgMs:      round2(g.SumMs / float64(max(1, g.Count))),
			P95Ms:      round2(percentile(g.latencies, 95)),
			P99Ms:      round2(percentile(g.latencies, 99)),
			MaxMs:      round2(g.MaxMs),
		})
	}
	sort.SliceStable(colls, func(i, j int) bool {
		if colls[i].SlowRatio != colls[j].SlowRatio {
			return colls[i].SlowRatio > colls[j].SlowRatio
		}
		return colls[i].P99Ms > colls[j].P99Ms
	})
	if len(colls) > 10 {
		colls = colls[:10]
	}
	rep.AffectedCollections = colls

	rep.PerIteration = buildPerIteration(byIteration)
	rep.TimeSlices = buildTimeSlices(byTimeSlice)
	rep.PotentialIndexIssues = buildIndexIssues(filteredGroups, indexes, slowThresholdMs)
	applyDefaultIndexExplainStatus(rep.PotentialIndexIssues, explainEnabled)
	rep.Recommendations = buildRecommendations(rep)
	rep.Summary.TopSeverity = "none"
	if len(rep.SlowQueries) > 0 {
		rep.Summary.TopSeverity = rep.SlowQueries[0].Severity
	}
	return rep
}

func buildPerIteration(byIteration map[int]*groupAgg) []IterationInsight {
	keys := make([]int, 0, len(byIteration))
	for k := range byIteration {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	out := make([]IterationInsight, 0, len(keys))
	for _, iter := range keys {
		g := byIteration[iter]
		out = append(out, IterationInsight{
			Iteration: iter,
			Count:     g.Count,
			SlowCount: g.SlowCount,
			SlowRatio: ratio(g.SlowCount, g.Count),
			AvgMs:     round2(g.SumMs / float64(max(1, g.Count))),
			P95Ms:     round2(percentile(g.latencies, 95)),
			P99Ms:     round2(percentile(g.latencies, 99)),
		})
	}
	return out
}

func buildTimeSlices(byTimeSlice map[int64]*groupAgg) []TimeSliceInsight {
	keys := make([]int64, 0, len(byTimeSlice))
	for k := range byTimeSlice {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]TimeSliceInsight, 0, len(keys))
	for _, k := range keys {
		g := byTimeSlice[k]
		out = append(out, TimeSliceInsight{
			BucketLabel: fmt.Sprintf("+%ds", k),
			Count:       g.Count,
			SlowCount:   g.SlowCount,
			AvgMs:       round2(g.SumMs / float64(max(1, g.Count))),
			P95Ms:       round2(percentile(g.latencies, 95)),
			P99Ms:       round2(percentile(g.latencies, 99)),
		})
	}
	if len(out) > 60 {
		out = out[len(out)-60:]
	}
	return out
}

func updateGroupAgg(g *groupAgg, ev OperationEvent, slowThresholdMs float64) {
	g.Count++
	if !ev.Success {
		g.ErrorCount++
	}
	if ev.DurationMs >= slowThresholdMs {
		g.SlowCount++
	}
	g.SumMs += ev.DurationMs
	if ev.DurationMs > g.MaxMs {
		g.MaxMs = ev.DurationMs
	}
	g.latencies = append(g.latencies, ev.DurationMs)
	if len(g.FilterFields) == 0 && len(ev.FilterFields) > 0 {
		g.FilterFields = ev.FilterFields
	}
	if g.ShapeSummary == "" {
		g.ShapeSummary = ev.ShapeSummary
	}
	if g.ShapeKey == "" {
		g.ShapeKey = ev.ShapeKey
	}
	if g.Operation == "" {
		g.Operation = ev.Operation
	}
	if g.Collection == "" {
		g.Collection = ev.Collection
	}
	if g.refCounts == nil {
		g.refCounts = make(map[string]int)
	}
	refKey := buildQueryRefKey(ev)
	if refKey != "" {
		g.refCounts[refKey]++
		if len(g.refCounts) > g.QueryRefVariants {
			g.QueryRefVariants = len(g.refCounts)
		}
		if g.QueryLabel == "" {
			g.QueryLabel = ev.QueryLabel
			g.QuerySource = ev.QuerySource
			g.WorkloadName = ev.WorkloadName
			g.QueryFile = ev.QueryFile
			g.QueryDefID = ev.QueryDefID
			g.QueryDefIndex = ev.QueryDefIndex
			g.QuerySummary = ev.QuerySummary
			g.PipelineSummary = ev.PipelineSummary
		} else if g.QueryLabel != ev.QueryLabel || g.QuerySource != ev.QuerySource || g.QueryFile != ev.QueryFile || g.QueryDefID != ev.QueryDefID {
			g.QueryLabel = "multiple query definitions"
			g.QuerySource = "multiple_sources"
			if g.WorkloadName == "" {
				g.WorkloadName = ev.WorkloadName
			}
			if g.QuerySummary == "" {
				g.QuerySummary = ev.QuerySummary
			}
			if g.PipelineSummary == "" {
				g.PipelineSummary = ev.PipelineSummary
			}
		}
	}
}

func buildQueryRefKey(ev OperationEvent) string {
	parts := []string{
		strings.TrimSpace(ev.QueryLabel),
		strings.TrimSpace(ev.QuerySource),
		strings.TrimSpace(ev.WorkloadName),
		strings.TrimSpace(ev.QueryFile),
		strings.TrimSpace(ev.QueryDefID),
		fmt.Sprintf("%d", ev.QueryDefIndex),
	}
	key := strings.Join(parts, "|")
	if strings.Trim(key, "|0") == "" {
		return ""
	}
	return key
}

func toSlowInsight(g *groupAgg) SlowQueryInsight {
	avg := 0.0
	if g.Count > 0 {
		avg = g.SumMs / float64(g.Count)
	}
	p95 := percentile(g.latencies, 95)
	p99 := percentile(g.latencies, 99)
	slowRatio := ratio(g.SlowCount, g.Count)
	sev := classifySeverity(avg, p95, p99, g.MaxMs, slowRatio, g.Count)
	op := emptyFallback(g.Operation, "(unknown)")
	coll := emptyFallback(g.Collection, "(unknown)")
	shape := emptyFallback(g.ShapeKey, "(shape unavailable)")
	return SlowQueryInsight{
		ShapeID:          StableShapeID(op, coll, shape),
		Operation:        op,
		Collection:       coll,
		ShapeKey:         shape,
		ShapeSummary:     emptyFallback(g.ShapeSummary, "(shape unavailable)"),
		QueryLabel:       g.QueryLabel,
		QuerySource:      g.QuerySource,
		WorkloadName:     g.WorkloadName,
		QueryFile:        g.QueryFile,
		QueryDefID:       g.QueryDefID,
		QueryDefIndex:    g.QueryDefIndex,
		QuerySummary:     g.QuerySummary,
		PipelineSummary:  g.PipelineSummary,
		QueryRefVariants: g.QueryRefVariants,
		FilterFields:     g.FilterFields,
		Count:            g.Count,
		ErrorCount:       g.ErrorCount,
		SlowCount:        g.SlowCount,
		SlowRatio:        slowRatio,
		AvgMs:            round2(avg),
		P95Ms:            round2(p95),
		P99Ms:            round2(p99),
		MaxMs:            round2(g.MaxMs),
		Severity:         sev,
	}
}

func buildIndexIssues(groups []SlowQueryInsight, indexes map[string]collectionIndexInfo, slowThresholdMs float64) []IndexIssue {
	issues := make([]IndexIssue, 0)
	for _, g := range groups {
		if g.Count < 3 {
			continue
		}
		if g.P95Ms < slowThresholdMs && g.AvgMs < slowThresholdMs {
			continue
		}
		if !isIndexSensitiveOp(g.Operation) {
			continue
		}
		if len(g.FilterFields) == 0 {
			continue
		}

		info := indexes[g.Collection]
		overlap := hasIndexFieldOverlap(g.FilterFields, info.IndexKeys)
		evidence := "heuristic"
		confidence := "low"
		msg := "Repeated slow pattern observed. A collection scan is possible, but not confirmed."
		reco := "Investigate index coverage for these filter fields and validate with explain on representative queries."

		if overlap {
			evidence = "heuristic_index_overlap"
			confidence = "low"
			msg = "Repeated slow pattern observed even with partial indexed-field overlap. Index order/selectivity may still be suboptimal."
			reco = "Review compound index order and query predicates; confirm with explain before changing indexes."
		} else if len(info.IndexKeys) > 0 {
			evidence = "heuristic_no_overlap_with_configured_indexes"
			confidence = "medium"
			msg = "Repeated slow pattern observed with no clear overlap against configured index fields. A missing index is possible."
			reco = "Consider a candidate index for these filter fields and validate with explain + workload replay before rollout."
		}

		issues = append(issues, IndexIssue{
			ShapeID:          g.ShapeID,
			Collection:       g.Collection,
			Operation:        g.Operation,
			ShapeKey:         g.ShapeKey,
			QueryLabel:       g.QueryLabel,
			QuerySource:      g.QuerySource,
			WorkloadName:     g.WorkloadName,
			QueryFile:        g.QueryFile,
			QueryDefID:       g.QueryDefID,
			QueryDefIndex:    g.QueryDefIndex,
			QuerySummary:     g.QuerySummary,
			PipelineSummary:  g.PipelineSummary,
			QueryRefVariants: g.QueryRefVariants,
			FilterFields:     g.FilterFields,
			Count:            g.Count,
			AvgMs:            g.AvgMs,
			P95Ms:            g.P95Ms,
			P99Ms:            g.P99Ms,
			MaxMs:            g.MaxMs,
			EvidenceLevel:    evidence,
			Confidence:       confidence,
			Message:          msg,
			Recommendation:   reco,
		})
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if confidenceScore(issues[i].Confidence) != confidenceScore(issues[j].Confidence) {
			return confidenceScore(issues[i].Confidence) > confidenceScore(issues[j].Confidence)
		}
		if issues[i].P99Ms != issues[j].P99Ms {
			return issues[i].P99Ms > issues[j].P99Ms
		}
		return issues[i].Count > issues[j].Count
	})
	if len(issues) > 10 {
		issues = issues[:10]
	}
	for i := range issues {
		issues[i].Rank = i + 1
	}
	return issues
}

func buildRecommendations(rep InsightsReport) []InsightRecommendation {
	recs := make([]InsightRecommendation, 0, 6)

	if len(rep.SlowQueries) > 0 {
		top := rep.SlowQueries[0]
		queryRef := fmt.Sprintf("%s/%s", emptyFallback(top.Operation, "op"), emptyFallback(top.Collection, "collection"))
		if strings.TrimSpace(top.QueryLabel) != "" {
			queryRef = top.QueryLabel
		} else if strings.TrimSpace(top.QueryDefID) != "" {
			queryRef = top.QueryDefID
		}
		if strings.TrimSpace(top.QueryFile) != "" {
			queryRef = fmt.Sprintf("%s from %s", queryRef, top.QueryFile)
		}
		recs = append(recs, InsightRecommendation{
			Priority:   "high",
			Title:      "Address the highest-latency operation group first",
			Details:    fmt.Sprintf("%s shows p99 %.2fms across %d sampled events. Review this query definition first.", queryRef, top.P99Ms, top.Count),
			Confidence: "high",
			Source:     "latency",
			ShapeID:    top.ShapeID,
		})
	}

	explainedShapes := make(map[string]struct{})
	explainRecs := 0
	for _, sq := range rep.SlowQueries {
		f := sq.ExplainFinding
		if f == nil {
			continue
		}
		explainedShapes[sq.ShapeID] = struct{}{}
		if explainRecs >= maxExplainRecommendations {
			continue
		}
		explainRecs++
		details := f.Recommendation + " Evidence: " + strings.Join(f.Evidence, "; ") + "."
		if note := workSignalNote(efficiencyObservations(rep, sq.ShapeID)); note != "" {
			details += " " + note
		}
		recs = append(recs, InsightRecommendation{
			Priority:   explainFindingPriority(f),
			Title:      f.Title,
			Details:    details,
			Confidence: f.Confidence,
			Source:     "explain",
			ShapeID:    sq.ShapeID,
			Diagnosis:  f.Diagnosis,
			Evidence:   append([]string(nil), f.Evidence...),
		})
	}

	recs = append(recs, buildIndexAdvice(&rep)...)

	if len(rep.PotentialIndexIssues) > 0 {
		top := rep.PotentialIndexIssues[0]
		if _, ok := explainedShapes[top.ShapeID]; ok {
			top = IndexIssue{}
			for _, issue := range rep.PotentialIndexIssues {
				if _, ok := explainedShapes[issue.ShapeID]; !ok {
					top = issue
					break
				}
			}
		}
		if top.ShapeID != "" {
			recs = appendIndexIssueRecommendation(recs, top)
		}
	}

	if rep.Summary.SlowSampledRatio > 0.25 {
		recs = append(recs, InsightRecommendation{
			Priority:   "medium",
			Title:      "Reduce global slow-operation ratio",
			Details:    fmt.Sprintf("Slow-sampled ratio is %.1f%%. Focus on top groups before increasing load.", rep.Summary.SlowSampledRatio*100),
			Confidence: "medium",
			Source:     "latency",
		})
	}

	if len(recs) == 0 {
		recs = append(recs, InsightRecommendation{
			Priority:   "low",
			Title:      "No strong slow-query/index signal detected",
			Details:    "Current sampled workload did not expose a clear slow-pattern hotspot. Increase duration or sampling if deeper analysis is needed.",
			Confidence: "low",
			Source:     "heuristic",
		})
	}

	if len(recs) > maxInsightRecommendations {
		recs = recs[:maxInsightRecommendations]
	}
	for i := range recs {
		recs[i].Rank = i + 1
	}
	return recs
}

func appendIndexIssueRecommendation(recs []InsightRecommendation, top IndexIssue) []InsightRecommendation {
	queryCtx := fmt.Sprintf("%s (%s)", top.Collection, top.Operation)
	if strings.TrimSpace(top.QueryLabel) != "" {
		queryCtx = top.QueryLabel
	}
	if strings.TrimSpace(top.QueryFile) != "" {
		queryCtx = fmt.Sprintf("%s in %s", queryCtx, top.QueryFile)
	}
	return append(recs, InsightRecommendation{
		Priority:   "high",
		Title:      "Investigate index coverage for repeated slow filters",
		Details:    fmt.Sprintf("%s shows repeated slow behavior. %s", queryCtx, top.Recommendation),
		Confidence: top.Confidence,
		Source:     "heuristic",
		ShapeID:    top.ShapeID,
	})
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	rank := int(math.Round((p / 100) * float64(len(cp)-1)))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(cp) {
		rank = len(cp) - 1
	}
	return cp[rank]
}

func classifySeverity(avg, p95, p99, maxVal, slowRatio float64, count int) string {
	switch {
	case count >= 10 && (p99 >= 2000 || maxVal >= 3000 || slowRatio >= 0.50):
		return "critical"
	case count >= 6 && (p99 >= 1000 || p95 >= 800 || slowRatio >= 0.30):
		return "high"
	case count >= 3 && (p99 >= 400 || p95 >= 250 || avg >= 150):
		return "medium"
	default:
		return "low"
	}
}

func hasIndexFieldOverlap(filterFields []string, indexes [][]string) bool {
	if len(filterFields) == 0 || len(indexes) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(filterFields))
	for _, f := range filterFields {
		set[f] = struct{}{}
	}
	for _, idx := range indexes {
		for _, f := range idx {
			if _, ok := set[f]; ok {
				return true
			}
		}
	}
	return false
}

func isIndexSensitiveOp(op string) bool {
	switch op {
	case "find", "updateOne", "updateMany", "deleteOne", "deleteMany", "aggregate":
		return true
	default:
		return false
	}
}

func dedupeAndSortStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func ratio(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func emptyFallback(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return v
}

func severityScore(s string) int {
	switch s {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func isSeverityExplainEligible(severity, mode string) bool {
	switch normalizeExplainSeverityMode(mode) {
	case "high_and_low":
		return true
	case "medium_only":
		switch severity {
		case "critical", "high", "medium":
			return true
		default:
			return false
		}
	case "critical_only":
		return severity == "critical"
	default: // high_only
		switch severity {
		case "critical", "high":
			return true
		default:
			return false
		}
	}
}

func normalizeExplainSeverityMode(mode string) string {
	switch mode {
	case "high_and_low", "medium_only", "critical_only":
		return mode
	default:
		return "high_only"
	}
}

func confidenceScore(s string) int {
	switch strings.ToLower(s) {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func boolToMode(enabled bool) string {
	if enabled {
		return "post_run_only"
	}
	return "disabled"
}

func explainImpact(enabled bool) string {
	if !enabled {
		return "none"
	}
	return "post_run_only_does_not_affect_measured_workload_window"
}

func applyDefaultExplainStatus(in []SlowQueryInsight, explainEnabled bool) {
	status := "explain_unavailable"
	reason := "explain_disabled"
	if explainEnabled {
		reason = "explain_not_attempted_yet"
	}
	for i := range in {
		if !isIndexSensitiveOp(in[i].Operation) {
			in[i].ExplainStatus = "not_supported"
			in[i].ExplainReason = fmt.Sprintf("operation_%s_not_supported", in[i].Operation)
			continue
		}
		in[i].ExplainStatus = status
		in[i].ExplainReason = reason
	}
}

func applyDefaultIndexExplainStatus(in []IndexIssue, explainEnabled bool) {
	status := "explain_unavailable"
	reason := "explain_disabled"
	if explainEnabled {
		reason = "explain_not_attempted_yet"
	}
	for i := range in {
		in[i].ExplainStatus = status
		in[i].ExplainReason = reason
	}
}

func cloneMapDeep(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneSliceDeep(in []interface{}) []interface{} {
	if in == nil {
		return nil
	}
	out := make([]interface{}, len(in))
	for i := range in {
		out[i] = cloneAny(in[i])
	}
	return out
}

func cloneAny(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return cloneMapDeep(t)
	case []interface{}:
		return cloneSliceDeep(t)
	case bson.D:
		out := make(bson.D, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: cloneAny(e.Value)}
		}
		return out
	default:
		return t
	}
}
