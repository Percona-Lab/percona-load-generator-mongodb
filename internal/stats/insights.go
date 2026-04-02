package stats

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

type OperationEvent struct {
	Operation      string                 `json:"operation"`
	Database       string                 `json:"database,omitempty"`
	Collection     string                 `json:"collection"`
	ShapeKey       string                 `json:"shape_key"`
	ShapeSummary   string                 `json:"shape_summary"`
	FilterFields   []string               `json:"filter_fields,omitempty"`
	DurationMs     float64                `json:"duration_ms"`
	Success        bool                   `json:"success"`
	Iteration      int                    `json:"iteration"`
	TimestampMs    int64                  `json:"timestamp_ms"`
	FilterSample   map[string]interface{} `json:"-"`
	PipelineSample []interface{}          `json:"-"`
}

type ShapeTrend struct {
	PreviousP95Ms float64 `json:"previous_p95_ms"`
	CurrentP95Ms  float64 `json:"current_p95_ms"`
	DeltaP95Ms    float64 `json:"delta_p95_ms"`
	Direction     string  `json:"direction"`
}

type InsightsMetadata struct {
	Status              string  `json:"status"`
	GeneratedAt         string  `json:"generated_at,omitempty"`
	SlowThresholdMs     float64 `json:"slow_threshold_ms"`
	SamplingRate        float64 `json:"sampling_rate"`
	RetainedEvents      int     `json:"retained_events"`
	EligibleEvents      uint64  `json:"eligible_events"`
	SampledInEvents     uint64  `json:"sampled_in_events"`
	DroppedGroupEntries int     `json:"dropped_group_entries"`
	EvidenceLevel       string  `json:"evidence_level"`
	ExplainEnabled      bool    `json:"explain_enabled"`
	ExplainMode         string  `json:"explain_mode"`
}

type InsightsSummary struct {
	TotalSampledEvents int     `json:"total_sampled_events"`
	SlowSampledEvents  int     `json:"slow_sampled_events"`
	SlowSampledRatio   float64 `json:"slow_sampled_ratio"`
	TopSeverity        string  `json:"top_severity"`
}

type SlowQueryInsight struct {
	Rank         int         `json:"rank"`
	ShapeID      string      `json:"shape_id"`
	Operation    string      `json:"operation"`
	Collection   string      `json:"collection"`
	ShapeKey     string      `json:"shape_key"`
	ShapeSummary string      `json:"shape_summary"`
	FilterFields []string    `json:"filter_fields,omitempty"`
	Count        int         `json:"count"`
	ErrorCount   int         `json:"error_count"`
	SlowCount    int         `json:"slow_count"`
	SlowRatio    float64     `json:"slow_ratio"`
	AvgMs        float64     `json:"avg_ms"`
	P95Ms        float64     `json:"p95_ms"`
	P99Ms        float64     `json:"p99_ms"`
	MaxMs        float64     `json:"max_ms"`
	Severity     string      `json:"severity"`
	Trend        *ShapeTrend `json:"trend,omitempty"`
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
	Rank           int      `json:"rank"`
	ShapeID        string   `json:"shape_id"`
	Collection     string   `json:"collection"`
	Operation      string   `json:"operation"`
	ShapeKey       string   `json:"shape_key"`
	FilterFields   []string `json:"filter_fields,omitempty"`
	Count          int      `json:"count"`
	AvgMs          float64  `json:"avg_ms"`
	P95Ms          float64  `json:"p95_ms"`
	P99Ms          float64  `json:"p99_ms"`
	MaxMs          float64  `json:"max_ms"`
	EvidenceLevel  string   `json:"evidence_level"`
	Confidence     string   `json:"confidence"`
	Message        string   `json:"message"`
	Recommendation string   `json:"recommendation"`
}

type InsightRecommendation struct {
	Rank       int    `json:"rank"`
	Priority   string `json:"priority"`
	Title      string `json:"title"`
	Details    string `json:"details"`
	Confidence string `json:"confidence"`
}

type InsightsReport struct {
	Summary              InsightsSummary         `json:"summary"`
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
	enabled         bool
	samplePermille  int
	sampleRate      float64
	slowThresholdMs float64
	maxEvents       int
	maxGroups       int
	explainEnabled  bool
	explainTopN     int
	explainMaxMs    int
}

type collectionIndexInfo struct {
	IndexKeys [][]string
}

type groupAgg struct {
	Operation    string
	Collection   string
	ShapeKey     string
	ShapeSummary string
	FilterFields []string
	Count        int
	ErrorCount   int
	SlowCount    int
	SumMs        float64
	MaxMs        float64
	latencies    []float64
}

func defaultInsightsSettings() insightsSettings {
	return insightsSettings{
		enabled:         true,
		samplePermille:  100,
		sampleRate:      0.10,
		slowThresholdMs: 200.0,
		maxEvents:       5000,
		maxGroups:       300,
		explainEnabled:  false,
		explainTopN:     5,
		explainMaxMs:    1000,
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
	}

	s.samplePermille = int(s.sampleRate * 1000)
	if s.samplePermille < 1 && s.sampleRate > 0 {
		s.samplePermille = 1
	}
	if s.samplePermille > 1000 {
		s.samplePermille = 1000
	}

	c.insightsCfg = s
	if c.insightEvents == nil || cap(c.insightEvents) != s.maxEvents {
		c.insightEvents = make([]OperationEvent, s.maxEvents)
	}
}

func (c *Collector) SetCollectionsForInsights(cols []config.CollectionDefinition) {
	m := make(map[string]collectionIndexInfo, len(cols))
	for _, col := range cols {
		info := collectionIndexInfo{}
		for _, idx := range col.Indexes {
			keys := make([]string, 0, len(idx.Keys))
			for k := range idx.Keys {
				keys = append(keys, k)
			}
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
	c.finalInsights = nil
}

func (c *Collector) RecordOperationEvent(op, database, collection, shapeKey, shapeSummary string, filterFields []string, duration time.Duration, success bool, iteration int, filterSample map[string]interface{}, pipelineSample []interface{}) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()

	if !c.insightsCfg.enabled || c.insightsCfg.maxEvents <= 0 {
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
		Operation:      op,
		Database:       database,
		Collection:     collection,
		ShapeKey:       shapeKey,
		ShapeSummary:   shapeSummary,
		FilterFields:   dedupeAndSortStrings(filterFields),
		DurationMs:     float64(duration.Nanoseconds()) / 1e6,
		Success:        success,
		Iteration:      iteration,
		TimestampMs:    time.Now().UnixMilli(),
		FilterSample:   nil,
		PipelineSample: nil,
	}
	if c.insightsCfg.explainEnabled {
		ev.FilterSample = cloneMapDeep(filterSample)
		ev.PipelineSample = cloneSliceDeep(pipelineSample)
	}

	c.insightEvents[c.insightWrite] = ev
	c.insightWrite = (c.insightWrite + 1) % c.insightsCfg.maxEvents
	if c.insightCount < c.insightsCfg.maxEvents {
		c.insightCount++
	}
	c.finalInsights = nil
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
			Metadata: InsightsMetadata{
				Status:          "disabled",
				EvidenceLevel:   "none",
				SamplingRate:    c.insightsCfg.sampleRate,
				SlowThresholdMs: c.insightsCfg.slowThresholdMs,
				ExplainEnabled:  c.insightsCfg.explainEnabled,
				ExplainMode:     "disabled",
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
	)
	c.finalInsights = &report
	return report
}

func (c *Collector) GetExplainSettings() (enabled bool, topN int, maxTimeMs int) {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	return c.insightsCfg.explainEnabled, c.insightsCfg.explainTopN, c.insightsCfg.explainMaxMs
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
) InsightsReport {
	rep := InsightsReport{
		Metadata: InsightsMetadata{
			Status:          "ready",
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			SlowThresholdMs: slowThresholdMs,
			SamplingRate:    samplingRate,
			EligibleEvents:  eligible,
			SampledInEvents: sampledIn,
			RetainedEvents:  len(events),
			EvidenceLevel:   "heuristic",
			ExplainEnabled:  explainEnabled,
			ExplainMode:     boolToMode(explainEnabled),
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

	slowGroups := make([]SlowQueryInsight, 0, len(groups))
	for _, g := range groups {
		slowGroups = append(slowGroups, toSlowInsight(g))
	}
	sort.SliceStable(slowGroups, func(i, j int) bool {
		if slowGroups[i].Severity != slowGroups[j].Severity {
			return severityScore(slowGroups[i].Severity) > severityScore(slowGroups[j].Severity)
		}
		if slowGroups[i].P99Ms != slowGroups[j].P99Ms {
			return slowGroups[i].P99Ms > slowGroups[j].P99Ms
		}
		return slowGroups[i].Count > slowGroups[j].Count
	})
	for i := range slowGroups {
		slowGroups[i].Rank = i + 1
	}

	limitSlow := 20
	if len(slowGroups) < limitSlow {
		limitSlow = len(slowGroups)
	}
	rep.SlowQueries = slowGroups[:limitSlow]

	shapeView := make([]SlowQueryInsight, len(slowGroups))
	copy(shapeView, slowGroups)
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

	colls := make([]CollectionInsight, 0, len(byCollection))
	for _, g := range byCollection {
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
	rep.PotentialIndexIssues = buildIndexIssues(slowGroups, indexes, slowThresholdMs)
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
		ShapeID:      StableShapeID(op, coll, shape),
		Operation:    op,
		Collection:   coll,
		ShapeKey:     shape,
		ShapeSummary: emptyFallback(g.ShapeSummary, "(shape unavailable)"),
		FilterFields: g.FilterFields,
		Count:        g.Count,
		ErrorCount:   g.ErrorCount,
		SlowCount:    g.SlowCount,
		SlowRatio:    slowRatio,
		AvgMs:        round2(avg),
		P95Ms:        round2(p95),
		P99Ms:        round2(p99),
		MaxMs:        round2(g.MaxMs),
		Severity:     sev,
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
			ShapeID:        g.ShapeID,
			Collection:     g.Collection,
			Operation:      g.Operation,
			ShapeKey:       g.ShapeKey,
			FilterFields:   g.FilterFields,
			Count:          g.Count,
			AvgMs:          g.AvgMs,
			P95Ms:          g.P95Ms,
			P99Ms:          g.P99Ms,
			MaxMs:          g.MaxMs,
			EvidenceLevel:  evidence,
			Confidence:     confidence,
			Message:        msg,
			Recommendation: reco,
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
		recs = append(recs, InsightRecommendation{
			Priority:   "high",
			Title:      "Address the highest-latency operation group first",
			Details:    fmt.Sprintf("%s on %s shows p99 %.2fms across %d sampled events.", top.Operation, top.Collection, top.P99Ms, top.Count),
			Confidence: "high",
		})
	}

	if len(rep.PotentialIndexIssues) > 0 {
		top := rep.PotentialIndexIssues[0]
		recs = append(recs, InsightRecommendation{
			Priority:   "high",
			Title:      "Investigate index coverage for repeated slow filters",
			Details:    fmt.Sprintf("%s (%s) shows repeated slow behavior. %s", top.Collection, top.Operation, top.Recommendation),
			Confidence: top.Confidence,
		})
	}

	if rep.Summary.SlowSampledRatio > 0.25 {
		recs = append(recs, InsightRecommendation{
			Priority:   "medium",
			Title:      "Reduce global slow-operation ratio",
			Details:    fmt.Sprintf("Slow-sampled ratio is %.1f%%. Focus on top groups before increasing load.", rep.Summary.SlowSampledRatio*100),
			Confidence: "medium",
		})
	}

	if len(recs) == 0 {
		recs = append(recs, InsightRecommendation{
			Priority:   "low",
			Title:      "No strong slow-query/index signal detected",
			Details:    "Current sampled workload did not expose a clear slow-pattern hotspot. Increase duration or sampling if deeper analysis is needed.",
			Confidence: "low",
		})
	}

	for i := range recs {
		recs[i].Rank = i + 1
	}
	if len(recs) > 6 {
		recs = recs[:6]
	}
	return recs
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
		return "optional_post_run"
	}
	return "disabled"
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
	default:
		return t
	}
}
