package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// Trend comparisons (tc-2) compare this run with persisted per-shape snapshots
// of earlier runs. Efficiency, latency, and workload impact are compared
// separately; every threshold below is a PLGM heuristic, not a significance test.
const (
	TrendVersion             = "tc-2"
	TrendStoreSchemaVersion  = "tb-2"
	trendLegacySchemaVersion = "tb-1"
	TrendExportFormat        = "plgm-trend-export"

	TrendImproved         = "improved"
	TrendRegressed        = "regressed"
	TrendIncreased        = "increased"
	TrendDecreased        = "decreased"
	TrendNoMaterialChange = "no_material_change"
	TrendInsufficient     = "insufficient_evidence"
	TrendNotComparable    = "not_comparable"
	TrendNoBaseline       = "no_baseline"

	TrendComparable   = "comparable"
	TrendPartial      = "partial"
	TrendMissing      = "missing"
	TrendIncompatible = "incompatible"
	TrendOutdated     = "outdated"

	TrendMaxBaselineAge            = 30 * 24 * time.Hour
	TrendMinEfficiencyCommands     = 2
	TrendEfficiencyConfidence      = "high"
	TrendMaterialScoreDelta        = 15
	TrendMinOperations             = 30
	TrendLatencyMaterialAbsMs      = 1.0
	TrendLatencyMaterialRelative   = 0.20
	TrendImpactMaterialShareAbs    = 0.05
	TrendImpactMaterialRelative    = 0.25
	TrendWorkloadCountRelative     = 0.25
	TrendMaxShapeBaselines         = 500
	TrendMaxRuns                   = 64
	TrendMaxPinnedRuns             = 10
	TrendMaxMatrixRuns             = 8
	TrendCompareRun                = "run"
	trendMaxRunID                  = 80
	trendMaxSource                 = 128
	trendMaxText                   = 256
	trendMaxPlans                  = 4
	trendMaxIndexesUsed            = 8
	trendMaxNamespaceIndexes       = 32
	trendMaxServerVersions         = 4
	trendFutureTolerance           = 5 * time.Minute
	trendCriteriaNote              = "These thresholds are PLGM heuristics set on synthetic fixtures. They are not statistical significance tests and have not been calibrated against real customer workloads."
	trendMethodNote                = "Each tracked shape is compared with its stored snapshot from an earlier run, matched by shape id and namespace. By default the baseline is the previous compatible run: for each shape and metric, the newest earlier snapshot that passes the compatibility gates (efficiency prefers assessed evidence). A chosen run or pinned reference run is used for every shape instead. Scan efficiency score, latency (full-run average), and workload impact (share of observed operation time) are compared separately and are never combined. A label is given only when the baseline is compatible (same shape identity and score version, not outdated) and both runs meet the evidence minimums; otherwise the state says why there is no label."
	trendCausationNote             = "Annotations list what changed between the baseline and this run (plans, indexes, server, sampling, workload). They are not evidence of cause: data distribution, cache state, concurrent load, or any annotated change can move these metrics, and PLGM does not attribute a change to any of them."
	trendEvidenceNote              = "Operation counts and average latency cover every completed operation of the run. p95 uses retained latency samples only. Efficiency uses the distinct explained representative commands (at most 2 per shape); a large operation count does not add explain evidence."
	trendServerTopologyNote        = "Topology comes from the explain layout: mongod means a mongod answered directly (standalone or replica-set member, not distinguished); sharded_cluster means a mongos layout."
	trendStorePathEnv              = "PLGM_TREND_BASELINE_STORE"
	trendStoreDisabledValue        = "off"
	trendNoEfficiencyBaselineLabel = "No efficiency baseline"
)

// TrendSampling are the run settings that decide which evidence exists.
type TrendSampling struct {
	SamplingRate          float64 `json:"sampling_rate"`
	SlowThresholdMs       float64 `json:"slow_threshold_ms"`
	GroupLimit            int     `json:"group_limit"`
	ExplainEnabled        bool    `json:"explain_enabled"`
	ExplainVerbosity      string  `json:"explain_verbosity,omitempty"`
	ExplainEfficiencyMode string  `json:"explain_efficiency_mode,omitempty"`
	ExplainMaxSamples     int     `json:"explain_max_samples"`
	CaptureStatus         string  `json:"capture_status,omitempty"`
}

// TrendServer is the server evidence seen by this run's explains.
type TrendServer struct {
	Versions []string `json:"versions,omitempty"`
	Topology string   `json:"topology"`
	Source   string   `json:"source"`
}

// TrendWorkload is the workload configuration summary. It never includes the
// connection URI, credentials, query files, or query content.
type TrendWorkload struct {
	Concurrency        int    `json:"concurrency"`
	Duration           string `json:"duration,omitempty"`
	Iterations         int    `json:"iterations,omitempty"`
	DocumentsCount     int    `json:"documents_count,omitempty"`
	OperationMix       string `json:"operation_mix,omitempty"`
	LoadProfile        string `json:"load_profile,omitempty"`
	AccessPattern      string `json:"access_pattern,omitempty"`
	ThinkTimeMs        int    `json:"think_time_ms,omitempty"`
	ObservedOperations uint64 `json:"observed_operations"`
}

// TrendRunContext is the run-level context (settings, server, workload) stored
// with baselines. Label is user-editable; Pinned runs are reference runs that
// do not count toward the history limit, never expire, and are never pruned.
// Source and ImportedAt are set for runs imported from another machine.
type TrendRunContext struct {
	RunID                string        `json:"run_id"`
	RecordedAt           string        `json:"recorded_at"`
	Label                string        `json:"label,omitempty"`
	Pinned               bool          `json:"pinned,omitempty"`
	Source               string        `json:"source,omitempty"`
	ImportedAt           string        `json:"imported_at,omitempty"`
	ShapeIdentityVersion string        `json:"shape_identity_version"`
	ScoreVersion         string        `json:"score_version"`
	Sampling             TrendSampling `json:"sampling"`
	Server               TrendServer   `json:"server"`
	Workload             TrendWorkload `json:"workload"`
}

// TrendPolicy holds the history settings: how many unpinned runs to keep and
// when an unpinned baseline is outdated (MaxAge 0 = never).
type TrendPolicy struct {
	KeepRuns          int
	MaxAge            time.Duration
	DefaultComparison string
}

// DefaultTrendPolicy keeps 5 runs, outdates after 30 days, and compares with
// the previous compatible run.
func DefaultTrendPolicy() TrendPolicy {
	return TrendPolicy{KeepRuns: config.DefaultTrendHistoryRuns, MaxAge: TrendMaxBaselineAge, DefaultComparison: config.TrendComparisonPrevious}
}

// TrendPolicyFromConfig reads the normalized trend settings of cfg.
func TrendPolicyFromConfig(cfg *config.AppConfig) TrendPolicy {
	if cfg == nil {
		return DefaultTrendPolicy()
	}
	c := *cfg
	if c.TrendMaxAgeDays != nil {
		v := *c.TrendMaxAgeDays
		c.TrendMaxAgeDays = &v
	}
	config.NormalizeTrendSettings(&c)
	return TrendPolicy{
		KeepRuns: c.TrendHistoryRuns, MaxAge: time.Duration(*c.TrendMaxAgeDays) * 24 * time.Hour,
		DefaultComparison: c.TrendDefaultComparison,
	}
}

func (p TrendPolicy) normalized() TrendPolicy {
	d := DefaultTrendPolicy()
	if p.KeepRuns < config.MinTrendHistoryRuns {
		p.KeepRuns = d.KeepRuns
	}
	if p.KeepRuns > config.MaxTrendHistoryRuns {
		p.KeepRuns = config.MaxTrendHistoryRuns
	}
	if p.MaxAge < 0 {
		p.MaxAge = d.MaxAge
	}
	p.DefaultComparison = config.NormalizeTrendComparison(p.DefaultComparison)
	return p
}

// DefaultTrendRunLabel is "mongo-<server version>" when explain saw a server
// version, otherwise "run-<UTC timestamp>".
func DefaultTrendRunLabel(server TrendServer, recordedAt string) string {
	if len(server.Versions) > 0 {
		return config.SanitizeTrendRunLabel("mongo-" + strings.Join(server.Versions, "+"))
	}
	if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
		return "run-" + t.UTC().Format("20060102T150405Z")
	}
	return "run-unknown-time"
}

// TrendRunLabel returns the sanitized label, or the default when it is empty.
func TrendRunLabel(label string, server TrendServer, recordedAt string) string {
	if l := config.SanitizeTrendRunLabel(label); l != "" {
		return l
	}
	return DefaultTrendRunLabel(server, recordedAt)
}

// TrendEfficiencySnapshot is a redacted scan efficiency summary of one shape in one run.
type TrendEfficiencySnapshot struct {
	RunID                string   `json:"run_id"`
	RecordedAt           string   `json:"recorded_at"`
	ShapeIdentityVersion string   `json:"shape_identity_version"`
	ScoreVersion         string   `json:"score_version"`
	Coverage             string   `json:"coverage"`
	Status               string   `json:"status"`
	Reason               string   `json:"reason,omitempty"`
	Score                *int     `json:"score,omitempty"`
	ScoreIsUpperBound    bool     `json:"score_is_upper_bound,omitempty"`
	Band                 string   `json:"band,omitempty"`
	Confidence           string   `json:"confidence,omitempty"`
	ExplainedCommands    int      `json:"explained_commands"`
	Evidence             string   `json:"evidence"`
	SamplesDisagree      bool     `json:"samples_disagree,omitempty"`
	LimitingDimension    string   `json:"limiting_dimension,omitempty"`
	PlanSignatures       []string `json:"plan_signatures,omitempty"`
	IndexesUsed          []string `json:"indexes_used,omitempty"`
	NamespaceIndexStatus string   `json:"namespace_index_status,omitempty"`
	NamespaceIndexes     []string `json:"namespace_indexes,omitempty"`
}

// TrendWorkloadSnapshot is the latency and impact summary of one shape in one
// run. Counts, averages, and totals are full-run; p95 is from retained samples.
type TrendWorkloadSnapshot struct {
	RunID                 string   `json:"run_id"`
	RecordedAt            string   `json:"recorded_at"`
	ShapeIdentityVersion  string   `json:"shape_identity_version"`
	OperationCount        uint64   `json:"operation_count"`
	AvgMs                 float64  `json:"avg_ms"`
	MaxMs                 float64  `json:"max_ms"`
	RetainedSamples       int      `json:"retained_samples"`
	SampleP95Ms           *float64 `json:"sample_p95_ms,omitempty"`
	TotalOperationMs      float64  `json:"total_operation_ms"`
	ImpactShare           *float64 `json:"impact_share,omitempty"`
	GroupCoverageComplete bool     `json:"group_coverage_complete"`
}

// TrendShapeSnapshot is one shape in one comparison: this run's summaries, or
// the baseline chosen for it. Efficiency and workload baselines can come from
// different runs; each carries its own run id and time.
type TrendShapeSnapshot struct {
	ShapeID      string                   `json:"shape_id"`
	Namespace    string                   `json:"namespace"`
	Operation    string                   `json:"operation"`
	ShapeSummary string                   `json:"shape_summary,omitempty"`
	Efficiency   *TrendEfficiencySnapshot `json:"efficiency,omitempty"`
	Workload     *TrendWorkloadSnapshot   `json:"workload,omitempty"`
}

// TrendShapeBaseline is one persisted shape with at most one snapshot per
// kept run in each history, newest first.
type TrendShapeBaseline struct {
	ShapeID           string                    `json:"shape_id"`
	Namespace         string                    `json:"namespace"`
	Operation         string                    `json:"operation"`
	ShapeSummary      string                    `json:"shape_summary,omitempty"`
	EfficiencyHistory []TrendEfficiencySnapshot `json:"efficiency_history,omitempty"`
	WorkloadHistory   []TrendWorkloadSnapshot   `json:"workload_history,omitempty"`
}

// TrendStoreData is the persisted baseline file.
type TrendStoreData struct {
	SchemaVersion string               `json:"schema_version"`
	Runs          []TrendRunContext    `json:"runs"`
	Shapes        []TrendShapeBaseline `json:"shapes"`
}

// TrendRunSummary describes one stored run for run lists and pickers.
type TrendRunSummary struct {
	RunID          string   `json:"run_id"`
	Label          string   `json:"label"`
	RecordedAt     string   `json:"recorded_at"`
	ServerVersions []string `json:"server_versions,omitempty"`
	Topology       string   `json:"topology,omitempty"`
	Pinned         bool     `json:"pinned"`
	Source         string   `json:"source,omitempty"`
	ImportedAt     string   `json:"imported_at,omitempty"`
	Shapes         int      `json:"shapes"`
	Outdated       bool     `json:"outdated"`
	AgeSeconds     *int64   `json:"age_seconds,omitempty"`
	Age            string   `json:"age,omitempty"`
}

// TrendSelection says which baseline the view compares with.
type TrendSelection struct {
	Mode          string `json:"mode"`
	RequestedMode string `json:"requested_mode"`
	BaselineRunID string `json:"baseline_run_id,omitempty"`
	BaselineLabel string `json:"baseline_label,omitempty"`
	Note          string `json:"note,omitempty"`
}

// TrendOptions choose the baseline. Mode is "previous", "pinned", or "run"
// (with BaselineRunID); empty uses Policy.DefaultComparison.
type TrendOptions struct {
	Policy        TrendPolicy
	Mode          string
	BaselineRunID string
}

// TrendCriteria publishes the heuristics so every consumer shows the same rules.
type TrendCriteria struct {
	MaxBaselineAgeHours            int     `json:"max_baseline_age_hours"`
	EfficiencyMinExplainedCommands int     `json:"efficiency_min_explained_commands"`
	EfficiencyRequiredConfidence   string  `json:"efficiency_required_confidence"`
	EfficiencyMaterialScoreDelta   int     `json:"efficiency_material_score_delta"`
	MinOperations                  int     `json:"min_operations"`
	LatencyMaterialAbsMs           float64 `json:"latency_material_abs_ms"`
	LatencyMaterialRelative        float64 `json:"latency_material_relative"`
	LatencyP95MinSamples           int     `json:"latency_p95_min_samples"`
	ImpactMaterialShareAbs         float64 `json:"impact_material_share_abs"`
	ImpactMaterialRelative         float64 `json:"impact_material_relative"`
	WorkloadCountChangeRelative    float64 `json:"workload_count_change_relative"`
	Summary                        string  `json:"summary"`
	Note                           string  `json:"note"`
}

// TrendStoreStatus describes the baseline store as loaded for this report.
type TrendStoreStatus struct {
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Persistent       bool   `json:"persistent"`
	Path             string `json:"path,omitempty"`
	Baselines        int    `json:"baselines"`
	MaxBaselines     int    `json:"max_baselines"`
	Runs             int    `json:"runs"`
	PinnedRuns       int    `json:"pinned_runs"`
	MaxPinnedRuns    int    `json:"max_pinned_runs"`
	OldestRecordedAt string `json:"oldest_recorded_at,omitempty"`
	NewestRecordedAt string `json:"newest_recorded_at,omitempty"`
}

// TrendComparison is one metric's comparison. State is the only label; a nil
// value means the side had no usable value.
type TrendComparison struct {
	State          string   `json:"state"`
	Label          string   `json:"label"`
	Metric         string   `json:"metric"`
	Baseline       *float64 `json:"baseline,omitempty"`
	Current        *float64 `json:"current,omitempty"`
	Delta          *float64 `json:"delta,omitempty"`
	RelativeChange *float64 `json:"relative_change,omitempty"`
	Reasons        []string `json:"reasons,omitempty"`
	Secondary      string   `json:"secondary,omitempty"`
}

// TrendEvidenceSide separates the three kinds of evidence a run has.
type TrendEvidenceSide struct {
	OperationCount         *uint64 `json:"operation_count,omitempty"`
	RetainedLatencySamples *int    `json:"retained_latency_samples,omitempty"`
	ExplainedCommands      *int    `json:"explained_commands,omitempty"`
	EfficiencyEvidence     string  `json:"efficiency_evidence,omitempty"`
	EfficiencyConfidence   string  `json:"efficiency_confidence,omitempty"`
	Coverage               string  `json:"coverage,omitempty"`
}

type TrendEvidence struct {
	Baseline TrendEvidenceSide `json:"baseline"`
	Current  TrendEvidenceSide `json:"current"`
}

// TrendAnnotation records a change between runs without attributing cause.
type TrendAnnotation struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// TrendBaselineInfo is the age and origin of a row's baselines.
type TrendBaselineInfo struct {
	EfficiencyRunID      string `json:"efficiency_run_id,omitempty"`
	EfficiencyRunLabel   string `json:"efficiency_run_label,omitempty"`
	EfficiencyRecordedAt string `json:"efficiency_recorded_at,omitempty"`
	EfficiencyAgeSeconds *int64 `json:"efficiency_age_seconds,omitempty"`
	EfficiencyAge        string `json:"efficiency_age,omitempty"`
	WorkloadRunID        string `json:"workload_run_id,omitempty"`
	WorkloadRunLabel     string `json:"workload_run_label,omitempty"`
	WorkloadRecordedAt   string `json:"workload_recorded_at,omitempty"`
	WorkloadAgeSeconds   *int64 `json:"workload_age_seconds,omitempty"`
	WorkloadAge          string `json:"workload_age,omitempty"`
	ShapeIdentityVersion string `json:"shape_identity_version,omitempty"`
	ScoreVersion         string `json:"score_version,omitempty"`
}

// TrendRow is one tracked shape of this run and its comparisons.
type TrendRow struct {
	ShapeID              string             `json:"shape_id"`
	Namespace            string             `json:"namespace"`
	Operation            string             `json:"operation"`
	ShapeSummary         string             `json:"shape_summary"`
	Comparability        string             `json:"comparability"`
	ComparabilityReasons []string           `json:"comparability_reasons,omitempty"`
	Baseline             *TrendBaselineInfo `json:"baseline,omitempty"`
	Efficiency           TrendComparison    `json:"efficiency"`
	Latency              TrendComparison    `json:"latency"`
	Impact               TrendComparison    `json:"impact"`
	Evidence             TrendEvidence      `json:"evidence"`
	Interventions        []TrendAnnotation  `json:"interventions,omitempty"`
	WorkloadChanges      []TrendAnnotation  `json:"workload_changes,omitempty"`
}

// TrendView is the report-level trend comparison.
type TrendView struct {
	Version       string                    `json:"version"`
	Status        string                    `json:"status"`
	Reason        string                    `json:"reason,omitempty"`
	MethodNote    string                    `json:"method_note"`
	CausationNote string                    `json:"causation_note"`
	EvidenceNote  string                    `json:"evidence_note"`
	Criteria      TrendCriteria             `json:"criteria"`
	Store         TrendStoreStatus          `json:"store"`
	CurrentRun    TrendRunContext           `json:"current_run"`
	Comparison    TrendSelection            `json:"comparison"`
	AvailableRuns []TrendRunSummary         `json:"available_runs"`
	Counts        map[string]map[string]int `json:"counts"`
	Limitations   []string                  `json:"limitations,omitempty"`
	Rows          []TrendRow                `json:"rows"`
}

// DefaultTrendCriteria returns the published heuristics with the default policy.
func DefaultTrendCriteria() TrendCriteria {
	return TrendCriteriaFor(DefaultTrendPolicy())
}

// TrendCriteriaFor returns the published heuristics with p's baseline age.
func TrendCriteriaFor(p TrendPolicy) TrendCriteria {
	p = p.normalized()
	age := fmt.Sprintf("Unpinned baselines older than %d days are outdated; pinned reference runs never are.", int(p.MaxAge/(24*time.Hour)))
	if p.MaxAge == 0 {
		age = "Baselines are never outdated by age (expiry is off)."
	}
	return TrendCriteria{
		MaxBaselineAgeHours:            int(p.MaxAge / time.Hour),
		EfficiencyMinExplainedCommands: TrendMinEfficiencyCommands,
		EfficiencyRequiredConfidence:   TrendEfficiencyConfidence,
		EfficiencyMaterialScoreDelta:   TrendMaterialScoreDelta,
		MinOperations:                  TrendMinOperations,
		LatencyMaterialAbsMs:           TrendLatencyMaterialAbsMs,
		LatencyMaterialRelative:        TrendLatencyMaterialRelative,
		LatencyP95MinSamples:           minImpactPercentileSamples,
		ImpactMaterialShareAbs:         TrendImpactMaterialShareAbs,
		ImpactMaterialRelative:         TrendImpactMaterialRelative,
		WorkloadCountChangeRelative:    TrendWorkloadCountRelative,
		Summary: fmt.Sprintf("Efficiency: both runs assessed (not partial) with at least %d distinct explained commands and %s scan efficiency confidence; a change of %d or more points is labeled. "+
			"Latency: at least %d operations in both runs; the full-run average must move by at least %.0f ms and %.0f%%, and a p95 from at least %d retained samples on both sides must not move materially the other way. "+
			"Impact: at least %d operations and complete impact group coverage in both runs; the share of observed operation time must move by at least %.0f percentage points and %.0f%%. "+
			"%s",
			TrendMinEfficiencyCommands, TrendEfficiencyConfidence, TrendMaterialScoreDelta,
			TrendMinOperations, TrendLatencyMaterialAbsMs, TrendLatencyMaterialRelative*100, minImpactPercentileSamples,
			TrendMinOperations, TrendImpactMaterialShareAbs*100, TrendImpactMaterialRelative*100,
			age),
		Note: trendCriteriaNote,
	}
}

// TrendWorkloadFromConfig summarizes the workload configuration. Only counts,
// percentages, and kinds are kept.
func TrendWorkloadFromConfig(cfg *config.AppConfig) TrendWorkload {
	if cfg == nil {
		return TrendWorkload{}
	}
	mix := fmt.Sprintf("find %d, update %d, delete %d, insert %d, aggregate %d, transaction %d, bulk_insert %d",
		cfg.FindPercent, cfg.UpdatePercent, cfg.DeletePercent, cfg.InsertPercent, cfg.AggregatePercent, cfg.TransactionPercent, cfg.BulkInsertPercent)
	return TrendWorkload{
		Concurrency: cfg.Concurrency, Duration: trendText(cfg.Duration, 32), Iterations: cfg.Iterations,
		DocumentsCount: cfg.DocumentsCount, OperationMix: mix,
		LoadProfile: trendText(cfg.LoadProfile.Kind, 32), AccessPattern: trendText(cfg.AccessPattern.Kind, 32),
		ThinkTimeMs: cfg.ThinkTimeMs,
	}
}

// NewTrendRunContext builds this run context from the finished report. An
// empty label gets the default (see DefaultTrendRunLabel). recorded_at has a
// fixed-width millisecond part so runs in the same second sort as strings.
func NewTrendRunContext(rep InsightsReport, workload TrendWorkload, runID, label string, now time.Time) TrendRunContext {
	md := rep.Metadata
	workload.ObservedOperations = rep.Impact.Coverage.ObservedOperations
	recordedAt := now.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	server := trendServerFromReport(rep)
	return TrendRunContext{
		RunID: runID, RecordedAt: recordedAt, Label: TrendRunLabel(label, server, recordedAt),
		ShapeIdentityVersion: md.ShapeIdentityVersion, ScoreVersion: EfficiencyVersion,
		Sampling: TrendSampling{
			SamplingRate: md.SamplingRate, SlowThresholdMs: md.SlowThresholdMs, GroupLimit: rep.Impact.Coverage.GroupLimit,
			ExplainEnabled: md.ExplainEnabled, ExplainVerbosity: md.ExplainVerbosity, ExplainEfficiencyMode: md.ExplainEfficiencyMode,
			ExplainMaxSamples: md.ExplainMaxSamples, CaptureStatus: rep.QueryCapture.Status,
		},
		Server:   server,
		Workload: workload,
	}
}

func trendServerFromReport(rep InsightsReport) TrendServer {
	versions := map[string]struct{}{}
	topo := map[string]struct{}{}
	for _, cs := range rep.ExplainCoverage.Shapes {
		for _, s := range cs.Samples {
			d := s.Diagnostics
			if d == nil || s.Status != "explained" {
				continue
			}
			if d.ServerVersion != "" {
				versions[trendText(d.ServerVersion, 32)] = struct{}{}
			}
			switch {
			case d.Metrics != nil && strings.HasPrefix(d.Metrics.Layout, "sharded_"), d.ShardDetailsSummary != "":
				topo["sharded_cluster"] = struct{}{}
			case d.Metrics != nil && (d.Metrics.Layout == "plan" || d.Metrics.Layout == "pipeline"):
				topo["mongod"] = struct{}{}
			}
		}
	}
	out := TrendServer{Topology: "unknown", Source: "none"}
	for v := range versions {
		out.Versions = append(out.Versions, v)
	}
	sort.Strings(out.Versions)
	if len(out.Versions) > trendMaxServerVersions {
		out.Versions = out.Versions[:trendMaxServerVersions]
	}
	switch len(topo) {
	case 0:
	case 1:
		for t := range topo {
			out.Topology = t
		}
	default:
		out.Topology = "mixed"
	}
	if len(versions) > 0 || len(topo) > 0 {
		out.Source = "explain"
	}
	return out
}

// CurrentTrendBaselines summarizes every efficiency-view row of rep. The
// summaries hold no query text, literals, or executable commands.
func CurrentTrendBaselines(rep InsightsReport, run TrendRunContext) []TrendShapeSnapshot {
	if rep.EfficiencyView == nil {
		return nil
	}
	impact := make(map[string]WorkloadImpact, len(rep.Impact.Groups))
	for _, g := range rep.Impact.Groups {
		impact[g.ShapeID] = g
	}
	nsIndexes := map[string]NamespaceIndexInventory{}
	if rep.IndexInventory != nil {
		for _, n := range rep.IndexInventory.Namespaces {
			nsIndexes[n.Namespace] = n
		}
	}
	complete := rep.Impact.Coverage.GroupCoverageComplete
	out := make([]TrendShapeSnapshot, 0, len(rep.EfficiencyView.Rows))
	for _, r := range rep.EfficiencyView.Rows {
		b := TrendShapeSnapshot{
			ShapeID: trendText(r.ShapeID, 64), Namespace: trendText(r.Namespace, trendMaxText),
			Operation: trendText(r.Operation, 32), ShapeSummary: trendText(r.ShapeSummary, trendMaxText),
		}
		e := &TrendEfficiencySnapshot{
			RunID: run.RunID, RecordedAt: run.RecordedAt, ShapeIdentityVersion: run.ShapeIdentityVersion, ScoreVersion: run.ScoreVersion,
			Coverage: r.Coverage, Status: r.Status, Reason: trendText(r.Reason, 128), ScoreIsUpperBound: r.ScoreIsUpperBound,
			Band: r.Band, Confidence: r.Confidence, SamplesDisagree: r.SamplesDisagree, LimitingDimension: r.LimitingDimension,
			Evidence: "none",
		}
		if r.Score != nil {
			v := *r.Score
			e.Score = &v
		}
		plans, idx := map[string]struct{}{}, map[string]struct{}{}
		for _, s := range r.Samples {
			switch s.Evidence {
			case "execution_stats":
				e.Evidence = "execution_stats"
				if !s.SharedCommand {
					e.ExplainedCommands++
				}
			case "planner_only":
				if e.Evidence == "none" {
					e.Evidence = "planner_only"
				}
			}
			if s.ExplainStatus == "explained" {
				if s.WinningPlan != "" {
					plans[trendText(s.WinningPlan, trendMaxText)] = struct{}{}
				}
				for _, n := range s.IndexesUsed {
					idx[trendText(n, 128)] = struct{}{}
				}
			}
		}
		e.PlanSignatures = trendKeys(plans, trendMaxPlans)
		e.IndexesUsed = trendKeys(idx, trendMaxIndexesUsed)
		if inv, ok := nsIndexes[r.Namespace]; ok {
			e.NamespaceIndexStatus = inv.Status
			if inv.Status == "fetched" {
				names := map[string]struct{}{}
				for _, d := range inv.Indexes {
					names[trendText(d.Name, 128)] = struct{}{}
				}
				e.NamespaceIndexes = trendKeys(names, trendMaxNamespaceIndexes)
			}
		}
		b.Efficiency = e

		if r.OperationCount > 0 {
			w := &TrendWorkloadSnapshot{
				RunID: run.RunID, RecordedAt: run.RecordedAt, ShapeIdentityVersion: run.ShapeIdentityVersion,
				OperationCount: r.OperationCount, AvgMs: r.AvgMs, MaxMs: r.MaxMs, TotalOperationMs: r.TotalOperationMs,
				GroupCoverageComplete: complete,
			}
			if r.ImpactShare != nil {
				v := *r.ImpactShare
				w.ImpactShare = &v
			}
			if g, ok := impact[r.ShapeID]; ok {
				w.RetainedSamples = g.SampleCount
				if g.SampleP95Ms != nil {
					v := *g.SampleP95Ms
					w.SampleP95Ms = &v
				}
			}
			b.Workload = w
		}
		out = append(out, b)
	}
	return out
}

// MergeTrendBaselines adds this run's snapshots to the history of every shape
// it saw, replacing any earlier record of the same run id, and prunes with p.
// Every run keeps its own snapshot, so a budget-excluded run does not erase
// earlier efficiency evidence; the comparison chooses among them.
func MergeTrendBaselines(prev TrendStoreData, run TrendRunContext, cur []TrendShapeSnapshot, p TrendPolicy) TrendStoreData {
	d := removeTrendRun(prev, run.RunID)
	byID := make(map[string]int, len(d.Shapes)+len(cur))
	for i, s := range d.Shapes {
		byID[s.ShapeID] = i
	}
	for _, c := range cur {
		i, ok := byID[c.ShapeID]
		if !ok {
			i = len(d.Shapes)
			byID[c.ShapeID] = i
			d.Shapes = append(d.Shapes, TrendShapeBaseline{ShapeID: c.ShapeID, Namespace: c.Namespace, Operation: c.Operation})
		}
		sh := &d.Shapes[i]
		if sh.Namespace != c.Namespace || sh.Operation != c.Operation {
			*sh = TrendShapeBaseline{ShapeID: c.ShapeID, Namespace: c.Namespace, Operation: c.Operation}
		}
		sh.ShapeSummary = c.ShapeSummary
		if c.Efficiency != nil {
			sh.EfficiencyHistory = append([]TrendEfficiencySnapshot{*c.Efficiency}, sh.EfficiencyHistory...)
		}
		if c.Workload != nil {
			sh.WorkloadHistory = append([]TrendWorkloadSnapshot{*c.Workload}, sh.WorkloadHistory...)
		}
	}
	d.Runs = append(d.Runs, run)
	return pruneTrendData(d, p)
}

// removeTrendRun returns d without run id and its snapshots. It never
// modifies d's slices.
func removeTrendRun(d TrendStoreData, id string) TrendStoreData {
	out := TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: make([]TrendRunContext, 0, len(d.Runs)+1), Shapes: make([]TrendShapeBaseline, 0, len(d.Shapes))}
	for _, r := range d.Runs {
		if r.RunID != id {
			out.Runs = append(out.Runs, r)
		}
	}
	for _, s := range d.Shapes {
		s.EfficiencyHistory = filterHistory(s.EfficiencyHistory, func(e TrendEfficiencySnapshot) bool { return e.RunID != id })
		s.WorkloadHistory = filterHistory(s.WorkloadHistory, func(w TrendWorkloadSnapshot) bool { return w.RunID != id })
		if len(s.EfficiencyHistory) > 0 || len(s.WorkloadHistory) > 0 {
			out.Shapes = append(out.Shapes, s)
		}
	}
	return out
}

func filterHistory[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// normalizeHistory keeps one snapshot per kept run, newest first.
func normalizeHistory[T any](in []T, key func(T) (runID, recordedAt string), kept map[string]bool) []T {
	out := make([]T, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		id, _ := key(v)
		if kept[id] && !seen[id] {
			seen[id] = true
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ii, ti := key(out[i])
		ij, tj := key(out[j])
		if ti != tj {
			return ti > tj
		}
		return ii > ij
	})
	return out
}

func effKey(e TrendEfficiencySnapshot) (string, string) { return e.RunID, e.RecordedAt }
func wlKey(w TrendWorkloadSnapshot) (string, string)    { return w.RunID, w.RecordedAt }

func sortRunsNewestFirst(runs []TrendRunContext) {
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].RecordedAt != runs[j].RecordedAt {
			return runs[i].RecordedAt > runs[j].RecordedAt
		}
		return runs[i].RunID > runs[j].RunID
	})
}

func trendSnapshotRunIDs(shapes []TrendShapeBaseline) map[string]bool {
	ids := map[string]bool{}
	for _, s := range shapes {
		for _, e := range s.EfficiencyHistory {
			ids[e.RunID] = true
		}
		for _, w := range s.WorkloadHistory {
			ids[w.RunID] = true
		}
	}
	return ids
}

// pruneTrendData enforces the history and shape limits. Runs are kept newest
// first: every pinned run (up to TrendMaxPinnedRuns) plus the newest
// p.KeepRuns unpinned runs that still have snapshots. Snapshots of dropped
// runs go with them. When more than TrendMaxShapeBaselines shapes remain,
// shapes with a pinned snapshot are kept first, then the most recently
// recorded ones.
func pruneTrendData(d TrendStoreData, p TrendPolicy) TrendStoreData {
	p = p.normalized()
	runs := append([]TrendRunContext(nil), d.Runs...)
	sortRunsNewestFirst(runs)
	referenced := trendSnapshotRunIDs(d.Shapes)
	kept, pinned := map[string]bool{}, map[string]bool{}
	keptRuns := make([]TrendRunContext, 0, len(runs))
	nPinned, nUnpinned := 0, 0
	for _, r := range runs {
		if kept[r.RunID] {
			continue
		}
		switch {
		case r.Pinned && nPinned < TrendMaxPinnedRuns:
			nPinned++
			pinned[r.RunID] = true
		case !referenced[r.RunID] || nUnpinned >= p.KeepRuns:
			continue
		default:
			r.Pinned = false
			nUnpinned++
		}
		kept[r.RunID] = true
		keptRuns = append(keptRuns, r)
	}

	shapes := make([]TrendShapeBaseline, 0, len(d.Shapes))
	seenShape := map[string]bool{}
	for _, s := range d.Shapes {
		if seenShape[s.ShapeID] {
			continue
		}
		s.EfficiencyHistory = normalizeHistory(s.EfficiencyHistory, effKey, kept)
		s.WorkloadHistory = normalizeHistory(s.WorkloadHistory, wlKey, kept)
		if len(s.EfficiencyHistory) == 0 && len(s.WorkloadHistory) == 0 {
			continue
		}
		seenShape[s.ShapeID] = true
		shapes = append(shapes, s)
	}
	hasPinned := func(s TrendShapeBaseline) bool {
		for _, e := range s.EfficiencyHistory {
			if pinned[e.RunID] {
				return true
			}
		}
		for _, w := range s.WorkloadHistory {
			if pinned[w.RunID] {
				return true
			}
		}
		return false
	}
	latest := func(s TrendShapeBaseline) string {
		t := ""
		if len(s.EfficiencyHistory) > 0 {
			t = s.EfficiencyHistory[0].RecordedAt
		}
		if len(s.WorkloadHistory) > 0 && s.WorkloadHistory[0].RecordedAt > t {
			t = s.WorkloadHistory[0].RecordedAt
		}
		return t
	}
	sort.SliceStable(shapes, func(i, j int) bool {
		pi, pj := hasPinned(shapes[i]), hasPinned(shapes[j])
		if pi != pj {
			return pi
		}
		li, lj := latest(shapes[i]), latest(shapes[j])
		if li != lj {
			return li > lj
		}
		return shapes[i].ShapeID < shapes[j].ShapeID
	})
	if len(shapes) > TrendMaxShapeBaselines {
		shapes = shapes[:TrendMaxShapeBaselines]
	}
	referenced = trendSnapshotRunIDs(shapes)
	finalRuns := keptRuns[:0]
	for _, r := range keptRuns {
		if r.Pinned || referenced[r.RunID] {
			finalRuns = append(finalRuns, r)
		}
	}
	return TrendStoreData{SchemaVersion: TrendStoreSchemaVersion, Runs: finalRuns, Shapes: shapes}
}

// trendRunSummaries lists the runs of d newest first, without excludeID.
func trendRunSummaries(d TrendStoreData, excludeID string, now time.Time, p TrendPolicy) []TrendRunSummary {
	p = p.normalized()
	shapes := map[string]int{}
	for _, s := range d.Shapes {
		ids := map[string]bool{}
		for _, e := range s.EfficiencyHistory {
			ids[e.RunID] = true
		}
		for _, w := range s.WorkloadHistory {
			ids[w.RunID] = true
		}
		for id := range ids {
			shapes[id]++
		}
	}
	runs := append([]TrendRunContext(nil), d.Runs...)
	sortRunsNewestFirst(runs)
	out := make([]TrendRunSummary, 0, len(runs))
	for _, r := range runs {
		if r.RunID == excludeID {
			continue
		}
		out = append(out, trendRunSummary(r, shapes[r.RunID], now, p))
	}
	return out
}

func trendRunSummary(r TrendRunContext, shapes int, now time.Time, p TrendPolicy) TrendRunSummary {
	s := TrendRunSummary{
		RunID: r.RunID, Label: TrendRunLabel(r.Label, r.Server, r.RecordedAt), RecordedAt: r.RecordedAt,
		ServerVersions: r.Server.Versions, Topology: r.Server.Topology, Pinned: r.Pinned,
		Source: r.Source, ImportedAt: r.ImportedAt, Shapes: shapes,
	}
	if t, err := time.Parse(time.RFC3339, r.RecordedAt); err == nil {
		age := int64(now.Sub(t) / time.Second)
		s.AgeSeconds, s.Age = &age, trendAgeLabel(&age)
		s.Outdated = !r.Pinned && p.MaxAge > 0 && now.Sub(t) > p.MaxAge
	}
	return s
}

// ApplyTrends attaches the comparison of rep with the stored baselines.
func ApplyTrends(rep *InsightsReport, run TrendRunContext, base TrendStoreData, store TrendStoreStatus, now time.Time, opts TrendOptions) {
	v := BuildTrendView(*rep, run, base, store, now, opts)
	rep.Trends = &v
}

// trendCompareCtx is what every per-shape comparison needs besides the shapes.
type trendCompareCtx struct {
	run    TrendRunContext
	runs   map[string]TrendRunContext
	now    time.Time
	policy TrendPolicy
}

func newTrendCompareCtx(run TrendRunContext, base TrendStoreData, now time.Time, p TrendPolicy) trendCompareCtx {
	runs := make(map[string]TrendRunContext, len(base.Runs))
	for _, r := range base.Runs {
		runs[r.RunID] = r
	}
	return trendCompareCtx{run: run, runs: runs, now: now, policy: p.normalized()}
}

// resolveTrendSelection turns opts into the baseline run to use, falling back
// to the previous compatible run when no pinned run is stored.
func resolveTrendSelection(opts TrendOptions, base TrendStoreData, currentID string) TrendSelection {
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = opts.Policy.normalized().DefaultComparison
	}
	sel := TrendSelection{RequestedMode: mode, Mode: config.TrendComparisonPrevious}
	runs := append([]TrendRunContext(nil), base.Runs...)
	sortRunsNewestFirst(runs)
	switch mode {
	case TrendCompareRun:
		sel.Mode, sel.BaselineRunID = TrendCompareRun, opts.BaselineRunID
		for _, r := range runs {
			if r.RunID == opts.BaselineRunID && r.RunID != currentID {
				sel.BaselineLabel = TrendRunLabel(r.Label, r.Server, r.RecordedAt)
				return sel
			}
		}
		sel.Note = "The requested baseline run is not stored; no shape is compared."
	case config.TrendComparisonPinned:
		for _, r := range runs {
			if r.Pinned && r.RunID != currentID {
				sel.Mode, sel.BaselineRunID, sel.BaselineLabel = config.TrendComparisonPinned, r.RunID, TrendRunLabel(r.Label, r.Server, r.RecordedAt)
				sel.Note = "Compared with the newest pinned reference run."
				return sel
			}
		}
		sel.Note = "No pinned reference run is stored; compared with the previous compatible run instead."
	default:
		sel.RequestedMode = config.TrendComparisonPrevious
		sel.Note = "Compared with the previous compatible run of each shape."
	}
	return sel
}

// BuildTrendView compares every efficiency-view row of rep with the baseline
// chosen by opts. base may contain this run; its snapshots are never a baseline.
func BuildTrendView(rep InsightsReport, run TrendRunContext, base TrendStoreData, store TrendStoreStatus, now time.Time, opts TrendOptions) TrendView {
	ctx := newTrendCompareCtx(run, base, now, opts.Policy)
	v := TrendView{
		Version: TrendVersion, Status: "ready", MethodNote: trendMethodNote, CausationNote: trendCausationNote,
		EvidenceNote: trendEvidenceNote, Criteria: TrendCriteriaFor(ctx.policy), Store: store, CurrentRun: run,
		Comparison:    resolveTrendSelection(opts, base, run.RunID),
		AvailableRuns: trendRunSummaries(base, run.RunID, now, ctx.policy),
		Counts:        map[string]map[string]int{"efficiency": {}, "latency": {}, "impact": {}, "comparability": {}},
		Rows:          []TrendRow{},
	}
	v.Limitations = append(v.Limitations,
		trendCriteriaNote,
		fmt.Sprintf("Efficiency evidence is at most %d explained representative commands per shape per run, chosen from retained samples; it is not a statistical sample of the shape's operations.", ExplainMaxSamplesPerShape),
		"Only full-run sums are kept for latency, so there is no variance estimate; latency labels are a magnitude rule, not a statistical test.",
		"Data distribution, collection size, cache state, and load from other clients are not recorded; any of them can change these metrics without an annotation.",
		fmt.Sprintf("History keeps one snapshot per shape per run for the newest %d unpinned runs plus up to %d pinned reference runs. With the previous-run comparison, latency and impact use the newest compatible earlier snapshot, and efficiency prefers the newest compatible snapshot with assessed evidence, so the two can come from different runs.", ctx.policy.KeepRuns, TrendMaxPinnedRuns),
		trendServerTopologyNote,
	)
	if store.Status == "disabled" {
		v.Status, v.Reason = "disabled", emptyFallback(store.Reason, "trend_store_disabled")
		return v
	}
	if rep.Metadata.Status != "ready" || rep.EfficiencyView == nil {
		v.Status, v.Reason = "not_run", "insights_status_"+emptyFallback(rep.Metadata.Status, "unknown")
		return v
	}
	if store.Reason != "" && store.Status != "ok" && store.Status != "empty" {
		v.Limitations = append(v.Limitations, fmt.Sprintf("Baseline store %s: %s.", store.Status, store.Reason))
	}
	baseByID := make(map[string]TrendShapeBaseline, len(base.Shapes))
	others := 0
	for _, s := range base.Shapes {
		if trendShapeHasOtherRun(s, run.RunID) {
			baseByID[s.ShapeID] = s
			others++
		}
	}
	seen := map[string]bool{}
	for _, c := range CurrentTrendBaselines(rep, run) {
		seen[c.ShapeID] = true
		row := compareTrendShape(c, selectForView(baseByID, c.ShapeID, v.Comparison, ctx), ctx, missingReason(v.Comparison))
		v.Counts["efficiency"][row.Efficiency.State]++
		v.Counts["latency"][row.Latency.State]++
		v.Counts["impact"][row.Impact.State]++
		v.Counts["comparability"][row.Comparability]++
		v.Rows = append(v.Rows, row)
	}
	unseen := 0
	for id := range baseByID {
		if !seen[id] {
			unseen++
		}
	}
	if unseen > 0 {
		v.Limitations = append(v.Limitations, fmt.Sprintf("%d baseline shapes were not seen in this run and are not listed.", unseen))
	}
	if others == 0 {
		v.Status, v.Reason = TrendNoBaseline, "no_stored_baselines"
	}
	return v
}

func trendShapeHasOtherRun(s TrendShapeBaseline, currentID string) bool {
	for _, e := range s.EfficiencyHistory {
		if e.RunID != currentID {
			return true
		}
	}
	for _, w := range s.WorkloadHistory {
		if w.RunID != currentID {
			return true
		}
	}
	return false
}

func selectForView(baseByID map[string]TrendShapeBaseline, shapeID string, sel TrendSelection, ctx trendCompareCtx) *TrendShapeSnapshot {
	s, ok := baseByID[shapeID]
	if !ok {
		return nil
	}
	if sel.Mode == config.TrendComparisonPrevious {
		return selectPreviousBaseline(s, ctx)
	}
	return selectRunBaseline(s, sel.BaselineRunID)
}

func missingReason(sel TrendSelection) string {
	if sel.Mode == config.TrendComparisonPrevious {
		return ""
	}
	return fmt.Sprintf("not_in_baseline_run: this shape was not recorded in the baseline run %s (%s)", emptyFallback(sel.BaselineLabel, "unknown"), sel.BaselineRunID)
}

// selectRunBaseline returns the snapshots of shape s recorded by run id.
func selectRunBaseline(s TrendShapeBaseline, id string) *TrendShapeSnapshot {
	out := &TrendShapeSnapshot{ShapeID: s.ShapeID, Namespace: s.Namespace, Operation: s.Operation, ShapeSummary: s.ShapeSummary}
	for i := range s.EfficiencyHistory {
		if s.EfficiencyHistory[i].RunID == id {
			e := s.EfficiencyHistory[i]
			out.Efficiency = &e
			break
		}
	}
	for i := range s.WorkloadHistory {
		if s.WorkloadHistory[i].RunID == id {
			w := s.WorkloadHistory[i]
			out.Workload = &w
			break
		}
	}
	return out
}

// selectPreviousBaseline picks, per metric, the newest earlier snapshot that
// passes the compatibility gates; efficiency prefers assessed over partial
// over other coverage. When none passes, the newest earlier snapshot is used
// so the row reports why it is not comparable.
func selectPreviousBaseline(s TrendShapeBaseline, ctx trendCompareCtx) *TrendShapeSnapshot {
	out := &TrendShapeSnapshot{ShapeID: s.ShapeID, Namespace: s.Namespace, Operation: s.Operation, ShapeSummary: s.ShapeSummary}
	rank := func(cov string) int {
		switch cov {
		case EffCoverageAssessed:
			return 2
		case EffCoveragePartial:
			return 1
		}
		return 0
	}
	best := -1
	for i := range s.EfficiencyHistory {
		e := s.EfficiencyHistory[i]
		if e.RunID == ctx.run.RunID {
			continue
		}
		if out.Efficiency == nil {
			out.Efficiency = &e
		}
		if g := ctx.efficiencyGate(&e); g.ok && rank(e.Coverage) > best {
			best = rank(e.Coverage)
			out.Efficiency = &e
		}
	}
	var newest *TrendWorkloadSnapshot
	for i := range s.WorkloadHistory {
		w := s.WorkloadHistory[i]
		if w.RunID == ctx.run.RunID {
			continue
		}
		if newest == nil {
			newest = &w
		}
		if ctx.workloadGate(&w).ok {
			out.Workload = &w
			break
		}
	}
	if out.Workload == nil {
		out.Workload = newest
	}
	return out
}

type trendGate struct {
	ok     bool
	state  string
	reason string
	age    *int64
}

func (c trendCompareCtx) pinned(runID string) bool {
	return c.runs[runID].Pinned
}

func (c trendCompareCtx) efficiencyGate(be *TrendEfficiencySnapshot) trendGate {
	g := gateSnapshot(be.RecordedAt, be.ShapeIdentityVersion, c.run.ShapeIdentityVersion, c.now, c.policy.MaxAge, c.pinned(be.RunID))
	baseVersion, curVersion := CanonicalScoreVersion(be.ScoreVersion), CanonicalScoreVersion(c.run.ScoreVersion)
	if g.ok && baseVersion != curVersion {
		g = trendGate{state: TrendNotComparable, age: g.age, reason: fmt.Sprintf("score_version_changed: baseline %s, current %s; scores from different versions are never compared", emptyFallback(baseVersion, "unknown"), curVersion)}
	}
	return g
}

func (c trendCompareCtx) workloadGate(bw *TrendWorkloadSnapshot) trendGate {
	return gateSnapshot(bw.RecordedAt, bw.ShapeIdentityVersion, c.run.ShapeIdentityVersion, c.now, c.policy.MaxAge, c.pinned(bw.RunID))
}

// gateSnapshot checks shape identity, clock, and age. maxAge 0 or a pinned
// baseline run disables the age check.
func gateSnapshot(recordedAt, baseSI, curSI string, now time.Time, maxAge time.Duration, pinned bool) trendGate {
	t, err := time.Parse(time.RFC3339, recordedAt)
	if err != nil {
		return trendGate{state: TrendNotComparable, reason: "baseline_timestamp_invalid"}
	}
	age := int64(now.Sub(t) / time.Second)
	g := trendGate{age: &age}
	switch {
	case baseSI != curSI:
		g.state, g.reason = TrendNotComparable, fmt.Sprintf("shape_identity_version_changed: baseline %s, current %s; shape ids from different identity versions are never matched", emptyFallback(baseSI, "unknown"), emptyFallback(curSI, "unknown"))
	case now.Sub(t) < -trendFutureTolerance:
		g.state, g.reason = TrendNotComparable, "baseline_recorded_in_future: the baseline time is after this run (clock change?)"
	case !pinned && maxAge > 0 && now.Sub(t) > maxAge:
		g.state, g.reason = TrendNotComparable, fmt.Sprintf("baseline_outdated: recorded %s ago (maximum %d days; pin the run to keep it as a reference)", trendAgeText(age), int(maxAge/(24*time.Hour)))
	default:
		g.ok = true
	}
	return g
}

// compareTrendShape compares cur with base. missing replaces the default
// reason when base has no snapshot.
func compareTrendShape(cur TrendShapeSnapshot, base *TrendShapeSnapshot, ctx trendCompareCtx, missing string) TrendRow {
	run, runs := ctx.run, ctx.runs
	row := TrendRow{ShapeID: cur.ShapeID, Namespace: cur.Namespace, Operation: cur.Operation, ShapeSummary: cur.ShapeSummary}
	ce, cw := cur.Efficiency, cur.Workload
	row.Evidence.Current = trendEvidenceSide(ce, cw)
	if base == nil || (base.Efficiency == nil && base.Workload == nil) {
		row.Comparability = TrendMissing
		row.ComparabilityReasons = []string{emptyFallback(missing, "no_baseline: this shape has no stored baseline; this run becomes its baseline")}
		row.Efficiency = trendNoBaseline("scan efficiency score")
		row.Latency = trendNoBaseline("full-run average latency (ms)")
		row.Impact = trendNoBaseline("share of observed operation time")
		return row
	}
	if base.Namespace != cur.Namespace || base.Operation != cur.Operation {
		row.Comparability = TrendIncompatible
		row.ComparabilityReasons = []string{fmt.Sprintf("shape_mismatch: baseline is %s on %s", base.Operation, base.Namespace)}
		nc := func(metric string) TrendComparison {
			return TrendComparison{State: TrendNotComparable, Label: "Not comparable", Metric: metric, Reasons: row.ComparabilityReasons}
		}
		row.Efficiency, row.Latency, row.Impact = nc("scan efficiency score"), nc("full-run average latency (ms)"), nc("share of observed operation time")
		return row
	}
	be, bw := base.Efficiency, base.Workload
	row.Evidence.Baseline = trendEvidenceSide(be, bw)
	info := &TrendBaselineInfo{}
	var reasons []string
	gates := 0
	failed, outdated, siMismatch := 0, 0, false

	var eg, wg trendGate
	runLabel := func(id string) string {
		if r, ok := runs[id]; ok {
			return TrendRunLabel(r.Label, r.Server, r.RecordedAt)
		}
		return ""
	}
	if be != nil {
		gates++
		info.EfficiencyRunID, info.EfficiencyRecordedAt, info.EfficiencyRunLabel = be.RunID, be.RecordedAt, runLabel(be.RunID)
		info.ShapeIdentityVersion, info.ScoreVersion = be.ShapeIdentityVersion, CanonicalScoreVersion(be.ScoreVersion)
		eg = ctx.efficiencyGate(be)
		info.EfficiencyAgeSeconds, info.EfficiencyAge = eg.age, trendAgeLabel(eg.age)
		if !eg.ok {
			failed++
			reasons = append(reasons, "efficiency: "+eg.reason)
			outdated += boolInt(strings.HasPrefix(eg.reason, "baseline_outdated"))
			siMismatch = siMismatch || strings.HasPrefix(eg.reason, "shape_identity_version_changed")
		}
	} else {
		reasons = append(reasons, "efficiency: no efficiency baseline")
	}
	if bw != nil {
		gates++
		info.WorkloadRunID, info.WorkloadRecordedAt, info.WorkloadRunLabel = bw.RunID, bw.RecordedAt, runLabel(bw.RunID)
		if info.ShapeIdentityVersion == "" {
			info.ShapeIdentityVersion = bw.ShapeIdentityVersion
		}
		wg = ctx.workloadGate(bw)
		info.WorkloadAgeSeconds, info.WorkloadAge = wg.age, trendAgeLabel(wg.age)
		if !wg.ok {
			failed++
			reasons = append(reasons, "latency and impact: "+wg.reason)
			outdated += boolInt(strings.HasPrefix(wg.reason, "baseline_outdated"))
			siMismatch = siMismatch || strings.HasPrefix(wg.reason, "shape_identity_version_changed")
		}
	} else {
		reasons = append(reasons, "latency and impact: no workload baseline")
	}
	row.Baseline = info
	switch {
	case siMismatch:
		row.Comparability = TrendIncompatible
	case gates > 0 && outdated == gates:
		row.Comparability = TrendOutdated
	case failed > 0 || gates < 2:
		row.Comparability = TrendPartial
	default:
		row.Comparability = TrendComparable
	}
	row.ComparabilityReasons = reasons

	switch {
	case be == nil:
		row.Efficiency = TrendComparison{State: TrendNoBaseline, Label: trendNoEfficiencyBaselineLabel, Metric: "scan efficiency score"}
	case !eg.ok:
		row.Efficiency = TrendComparison{State: TrendNotComparable, Label: "Not comparable", Metric: "scan efficiency score", Reasons: []string{eg.reason}}
	default:
		row.Efficiency = compareTrendEfficiency(be, ce, cw)
	}
	switch {
	case bw == nil:
		row.Latency = trendNoBaseline("full-run average latency (ms)")
		row.Impact = trendNoBaseline("share of observed operation time")
	case !wg.ok:
		row.Latency = TrendComparison{State: TrendNotComparable, Label: "Not comparable", Metric: "full-run average latency (ms)", Reasons: []string{wg.reason}}
		row.Impact = TrendComparison{State: TrendNotComparable, Label: "Not comparable", Metric: "share of observed operation time", Reasons: []string{wg.reason}}
	case cw == nil:
		row.Latency = TrendComparison{State: TrendInsufficient, Label: "Insufficient evidence", Metric: "full-run average latency (ms)", Reasons: []string{"current run recorded no completed operations for this shape"}}
		row.Impact = TrendComparison{State: TrendInsufficient, Label: "Insufficient evidence", Metric: "share of observed operation time", Reasons: row.Latency.Reasons}
	default:
		row.Latency = compareTrendLatency(bw, cw)
		row.Impact = compareTrendImpact(bw, cw)
	}

	var effRun, wlRun *TrendRunContext
	if be != nil {
		if r, ok := runs[be.RunID]; ok {
			effRun = &r
		}
	}
	if bw != nil {
		if r, ok := runs[bw.RunID]; ok {
			wlRun = &r
		}
	}
	row.Interventions = trendInterventions(be, ce, effRun, wlRun, run)
	row.WorkloadChanges = trendWorkloadChanges(bw, cw, wlRun, run)
	return row
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func trendNoBaseline(metric string) TrendComparison {
	return TrendComparison{State: TrendNoBaseline, Label: "No baseline", Metric: metric}
}

func trendEvidenceSide(e *TrendEfficiencySnapshot, w *TrendWorkloadSnapshot) TrendEvidenceSide {
	var s TrendEvidenceSide
	if w != nil {
		n, r := w.OperationCount, w.RetainedSamples
		s.OperationCount, s.RetainedLatencySamples = &n, &r
	}
	if e != nil {
		c := e.ExplainedCommands
		s.ExplainedCommands = &c
		s.EfficiencyEvidence, s.EfficiencyConfidence, s.Coverage = e.Evidence, e.Confidence, e.Coverage
	}
	return s
}

func compareTrendEfficiency(be, ce *TrendEfficiencySnapshot, cw *TrendWorkloadSnapshot) TrendComparison {
	c := TrendComparison{Metric: "scan efficiency score"}
	if be.Score != nil {
		c.Baseline = floatPtr(float64(*be.Score))
	}
	if ce != nil && ce.Score != nil {
		c.Current = floatPtr(float64(*ce.Score))
	}
	insufficient := func(reasons ...string) TrendComparison {
		c.State, c.Label, c.Reasons = TrendInsufficient, "Insufficient evidence", reasons
		return c
	}
	if ce == nil {
		return insufficient("current run has no efficiency summary for this shape")
	}
	if ce.Status == EfficiencyNotApplicable || be.Status == EfficiencyNotApplicable {
		c.State, c.Label, c.Reasons = TrendNotComparable, "Not applicable", []string{"this operation is not explained, so it has no scan efficiency score"}
		return c
	}
	var reasons []string
	side := func(name string, e *TrendEfficiencySnapshot) {
		switch {
		case e.Score == nil:
			reasons = append(reasons, fmt.Sprintf("%s run has no scan efficiency score (coverage %s, %s)", name, e.Coverage, emptyFallback(e.Reason, "unassessed")))
		case e.ScoreIsUpperBound || e.Coverage == EffCoveragePartial:
			reasons = append(reasons, fmt.Sprintf("%s run has partial evidence: ≤ %d is an upper bound, and upper bounds are not compared", name, *e.Score))
		default:
			if e.ExplainedCommands < TrendMinEfficiencyCommands {
				reasons = append(reasons, fmt.Sprintf("%s run has %d distinct explained command(s) with executionStats (minimum %d)", name, e.ExplainedCommands, TrendMinEfficiencyCommands))
			}
			if e.Confidence != TrendEfficiencyConfidence {
				reasons = append(reasons, fmt.Sprintf("%s run scan efficiency confidence is %s (required %s)", name, emptyFallback(e.Confidence, "none"), TrendEfficiencyConfidence))
			}
		}
	}
	side("baseline", be)
	side("current", ce)
	if len(reasons) > 0 {
		if cw != nil && cw.OperationCount > 0 {
			reasons = append(reasons, fmt.Sprintf("the current run's %d operations do not add explain evidence", cw.OperationCount))
		}
		return insufficient(reasons...)
	}
	delta := *c.Current - *c.Baseline
	c.Delta = &delta
	if *c.Baseline > 0 {
		c.RelativeChange = floatPtr(roundTo(delta / *c.Baseline, 4))
	}
	text := fmt.Sprintf("scan efficiency %d → %d (%s)", *be.Score, *ce.Score, signedInt(int(delta)))
	switch {
	case delta >= TrendMaterialScoreDelta:
		c.State, c.Label = TrendImproved, "Improved: "+text
	case delta <= -TrendMaterialScoreDelta:
		c.State, c.Label = TrendRegressed, "Regressed: "+text
	default:
		c.State, c.Label = TrendNoMaterialChange, fmt.Sprintf("No material change: %s; threshold ±%d", text, TrendMaterialScoreDelta)
	}
	return c
}

func compareTrendLatency(bw, cw *TrendWorkloadSnapshot) TrendComparison {
	c := TrendComparison{Metric: "full-run average latency (ms)", Baseline: floatPtr(bw.AvgMs), Current: floatPtr(cw.AvgMs)}
	var reasons []string
	if bw.OperationCount < TrendMinOperations {
		reasons = append(reasons, fmt.Sprintf("baseline run has %d operations (minimum %d)", bw.OperationCount, TrendMinOperations))
	}
	if cw.OperationCount < TrendMinOperations {
		reasons = append(reasons, fmt.Sprintf("current run has %d operations (minimum %d)", cw.OperationCount, TrendMinOperations))
	}
	if bw.AvgMs <= 0 && len(reasons) == 0 {
		reasons = append(reasons, "baseline average latency is zero")
	}
	if len(reasons) > 0 {
		c.State, c.Label, c.Reasons = TrendInsufficient, "Insufficient evidence", reasons
		return c
	}
	delta := roundTo(cw.AvgMs-bw.AvgMs, 3)
	rel := roundTo(delta/bw.AvgMs, 4)
	c.Delta, c.RelativeChange = &delta, &rel
	material := math.Abs(delta) >= TrendLatencyMaterialAbsMs && math.Abs(rel) >= TrendLatencyMaterialRelative
	text := fmt.Sprintf("avg %.2f → %.2f ms (%+.2f ms, %+.0f%%)", bw.AvgMs, cw.AvgMs, delta, rel*100)

	p95Material, p95Dir := false, 0.0
	switch {
	case bw.SampleP95Ms == nil || cw.SampleP95Ms == nil || bw.RetainedSamples < minImpactPercentileSamples || cw.RetainedSamples < minImpactPercentileSamples:
		c.Secondary = fmt.Sprintf("p95 not compared: fewer than %d retained samples on one side (baseline %d, current %d)", minImpactPercentileSamples, bw.RetainedSamples, cw.RetainedSamples)
	default:
		pd := *cw.SampleP95Ms - *bw.SampleP95Ms
		pr := 0.0
		if *bw.SampleP95Ms > 0 {
			pr = pd / *bw.SampleP95Ms
		}
		p95Material = math.Abs(pd) >= TrendLatencyMaterialAbsMs && math.Abs(pr) >= TrendLatencyMaterialRelative
		p95Dir = pd
		c.Secondary = fmt.Sprintf("retained-sample p95 %.2f → %.2f ms (%+.2f ms) from %d and %d samples", *bw.SampleP95Ms, *cw.SampleP95Ms, pd, bw.RetainedSamples, cw.RetainedSamples)
	}
	switch {
	case material && p95Material && (delta > 0) != (p95Dir > 0):
		c.State, c.Label = TrendInsufficient, "Insufficient evidence"
		c.Reasons = []string{"the full-run average and the retained-sample p95 moved materially in opposite directions"}
	case material && delta > 0:
		c.State, c.Label = TrendRegressed, "Regressed: "+text
	case material:
		c.State, c.Label = TrendImproved, "Improved: "+text
	default:
		c.State = TrendNoMaterialChange
		c.Label = fmt.Sprintf("No material change: %s; a label needs at least %.0f ms and %.0f%%", text, TrendLatencyMaterialAbsMs, TrendLatencyMaterialRelative*100)
	}
	return c
}

func compareTrendImpact(bw, cw *TrendWorkloadSnapshot) TrendComparison {
	c := TrendComparison{Metric: "share of observed operation time"}
	if bw.ImpactShare != nil {
		c.Baseline = floatPtr(*bw.ImpactShare)
	}
	if cw.ImpactShare != nil {
		c.Current = floatPtr(*cw.ImpactShare)
	}
	var reasons []string
	if !bw.GroupCoverageComplete {
		reasons = append(reasons, "baseline impact group coverage was incomplete, so its share omits untracked operations")
	}
	if !cw.GroupCoverageComplete {
		reasons = append(reasons, "current impact group coverage is incomplete, so its share omits untracked operations")
	}
	if bw.OperationCount < TrendMinOperations || cw.OperationCount < TrendMinOperations {
		reasons = append(reasons, fmt.Sprintf("operations: baseline %d, current %d (minimum %d)", bw.OperationCount, cw.OperationCount, TrendMinOperations))
	}
	if c.Baseline == nil || c.Current == nil {
		reasons = append(reasons, "share of observed operation time is unknown on one side")
	}
	if len(reasons) > 0 {
		c.State, c.Label, c.Reasons = TrendInsufficient, "Insufficient evidence", reasons
		return c
	}
	delta := roundTo(*c.Current-*c.Baseline, 4)
	c.Delta = &delta
	material := math.Abs(delta) >= TrendImpactMaterialShareAbs
	if *c.Baseline > 0 {
		rel := roundTo(delta / *c.Baseline, 4)
		c.RelativeChange = &rel
		material = material && math.Abs(rel) >= TrendImpactMaterialRelative
	}
	text := fmt.Sprintf("%.1f%% → %.1f%% of observed operation time (%+.1f points)", *c.Baseline*100, *c.Current*100, delta*100)
	switch {
	case material && delta > 0:
		c.State, c.Label = TrendIncreased, "Larger share: "+text
	case material:
		c.State, c.Label = TrendDecreased, "Smaller share: "+text
	default:
		c.State = TrendNoMaterialChange
		c.Label = fmt.Sprintf("No material change: %s; a label needs at least %.0f points and %.0f%%", text, TrendImpactMaterialShareAbs*100, TrendImpactMaterialRelative*100)
	}
	c.Secondary = "A share also moves when other shapes change; it is not a per-operation cost."
	return c
}

func trendInterventions(be, ce *TrendEfficiencySnapshot, effRun, wlRun *TrendRunContext, run TrendRunContext) []TrendAnnotation {
	var out []TrendAnnotation
	if be != nil && ce != nil {
		if bothExplained := len(be.PlanSignatures) > 0 && len(ce.PlanSignatures) > 0; bothExplained {
			if !equalStrings(be.PlanSignatures, ce.PlanSignatures) {
				out = append(out, TrendAnnotation{Kind: "plan_changed", Detail: fmt.Sprintf("winning plan %s → %s", strings.Join(be.PlanSignatures, " | "), strings.Join(ce.PlanSignatures, " | "))})
			}
			if !equalStrings(be.IndexesUsed, ce.IndexesUsed) {
				out = append(out, TrendAnnotation{Kind: "indexes_used_changed", Detail: fmt.Sprintf("indexes used %s → %s", listOrNone(be.IndexesUsed), listOrNone(ce.IndexesUsed))})
			}
		}
		if be.NamespaceIndexStatus == "fetched" && ce.NamespaceIndexStatus == "fetched" {
			added, removed := diffStrings(be.NamespaceIndexes, ce.NamespaceIndexes)
			if len(added) > 0 || len(removed) > 0 {
				out = append(out, TrendAnnotation{Kind: "index_inventory_changed", Detail: fmt.Sprintf("deployed indexes on the namespace: added %s; removed %s", listOrNone(added), listOrNone(removed))})
			}
		}
	}
	ref := effRun
	if ref == nil {
		ref = wlRun
	}
	if ref == nil {
		return append(out, TrendAnnotation{Kind: "run_context_unavailable", Detail: "the baseline run's sampling and server settings were not stored, so their changes cannot be checked"})
	}
	if ref.Server.Source != "none" && run.Server.Source != "none" {
		if !equalStrings(ref.Server.Versions, run.Server.Versions) {
			out = append(out, TrendAnnotation{Kind: "server_version_changed", Detail: fmt.Sprintf("server version %s → %s", listOrNone(ref.Server.Versions), listOrNone(run.Server.Versions))})
		}
		if ref.Server.Topology != run.Server.Topology {
			out = append(out, TrendAnnotation{Kind: "topology_changed", Detail: fmt.Sprintf("topology %s → %s", ref.Server.Topology, run.Server.Topology)})
		}
	}
	if d := samplingDiff(ref.Sampling, run.Sampling); d != "" {
		out = append(out, TrendAnnotation{Kind: "sampling_settings_changed", Detail: d})
	}
	return out
}

func trendWorkloadChanges(bw, cw *TrendWorkloadSnapshot, wlRun *TrendRunContext, run TrendRunContext) []TrendAnnotation {
	var out []TrendAnnotation
	if bw != nil && cw != nil && bw.OperationCount > 0 {
		rel := (float64(cw.OperationCount) - float64(bw.OperationCount)) / float64(bw.OperationCount)
		if math.Abs(rel) >= TrendWorkloadCountRelative {
			out = append(out, TrendAnnotation{Kind: "operation_count_changed", Detail: fmt.Sprintf("operations of this shape %d → %d (%+.0f%%)", bw.OperationCount, cw.OperationCount, rel*100)})
		}
	}
	if wlRun != nil {
		if d := workloadDiff(wlRun.Workload, run.Workload); d != "" {
			out = append(out, TrendAnnotation{Kind: "workload_config_changed", Detail: d})
		}
	}
	return out
}

func samplingDiff(a, b TrendSampling) string {
	var d []string
	add := func(name string, x, y interface{}) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			d = append(d, fmt.Sprintf("%s %v → %v", name, x, y))
		}
	}
	add("sampling_rate", a.SamplingRate, b.SamplingRate)
	add("slow_threshold_ms", a.SlowThresholdMs, b.SlowThresholdMs)
	add("group_limit", a.GroupLimit, b.GroupLimit)
	add("explain_enabled", a.ExplainEnabled, b.ExplainEnabled)
	add("explain_verbosity", a.ExplainVerbosity, b.ExplainVerbosity)
	add("explain_efficiency_mode", a.ExplainEfficiencyMode, b.ExplainEfficiencyMode)
	add("explain_max_samples", a.ExplainMaxSamples, b.ExplainMaxSamples)
	add("capture_status", a.CaptureStatus, b.CaptureStatus)
	return strings.Join(d, "; ")
}

func workloadDiff(a, b TrendWorkload) string {
	var d []string
	add := func(name string, x, y interface{}) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			d = append(d, fmt.Sprintf("%s %v → %v", name, x, y))
		}
	}
	add("concurrency", a.Concurrency, b.Concurrency)
	add("duration", a.Duration, b.Duration)
	add("iterations", a.Iterations, b.Iterations)
	add("documents_count", a.DocumentsCount, b.DocumentsCount)
	add("operation_mix", a.OperationMix, b.OperationMix)
	add("load_profile", a.LoadProfile, b.LoadProfile)
	add("access_pattern", a.AccessPattern, b.AccessPattern)
	add("think_time_ms", a.ThinkTimeMs, b.ThinkTimeMs)
	return strings.Join(d, "; ")
}

func trendAgeText(sec int64) string {
	unit := func(n int64, name string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", name)
		}
		return fmt.Sprintf("%d %ss", n, name)
	}
	switch {
	case sec < 3600:
		return unit(sec/60, "minute")
	case sec < 48*3600:
		return unit(sec/3600, "hour")
	}
	return unit(sec/86400, "day")
}

func trendAgeLabel(sec *int64) string {
	switch {
	case sec == nil:
		return ""
	case *sec < 0:
		return "recorded in the future"
	case *sec < 60:
		return "less than a minute old"
	}
	return trendAgeText(*sec) + " old"
}

func trendText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

func trendKeys(m map[string]struct{}, max int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffStrings(a, b []string) (added, removed []string) {
	in := func(list []string, s string) bool {
		for _, v := range list {
			if v == s {
				return true
			}
		}
		return false
	}
	for _, s := range b {
		if !in(a, s) {
			added = append(added, s)
		}
	}
	for _, s := range a {
		if !in(b, s) {
			removed = append(removed, s)
		}
	}
	return added, removed
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func signedInt(v int) string {
	if v > 0 {
		return fmt.Sprintf("+%d", v)
	}
	return fmt.Sprint(v)
}
