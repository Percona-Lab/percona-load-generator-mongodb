package stats

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ShapeIdentityVersion labels how shape keys (and therefore shape IDs) are
// derived. si-2 adds find projection and limit, update upsert, and pipeline
// structural values ($sort/$limit/$skip/$sample/$project specs and field
// paths) to the key, so its shape IDs do not match reports without a version.
// si-3 renders multi-key sort specifications ($sort and sortBy) in declared
// key order with their directions, so different precedence gives different
// shape IDs; si-2 rendered them alphabetically.
const ShapeIdentityVersion = "si-3"

const (
	captureSamplesPerShape    = 8
	captureMaxSampleBytes     = 16 << 10
	captureMaxRetainedBytes   = 8 << 20
	captureMaxDescriptorBytes = 1024
	captureViewLimit          = 100
	captureArrayPreview       = 8
	captureRNGSeed1           = 0x9e3779b97f4a7c15
	captureRNGSeed2           = 0x2545f4914f6cdd1d
)

const captureSelectionMethod = "uniform_reservoir_per_shape_plus_slowest"

const captureScopeNote = "Counters cover every recorded operation in the run. Samples are a uniform random reservoir per shape over successful explainable operations, plus that shape's slowest successful operation, independent of the slow threshold. Samples are diagnostic evidence only and are never extrapolated to full-run totals."

var captureUnrecordedPaths = []string{
	"failed_transactions: failed transaction batches do not call the operation recorder and are absent from all counters",
	"raw_injector: the raw injector path does not call the operation recorder and is absent from all counters",
}

type capturedSample struct {
	query       *ExecutableQuery
	durationNs  int64
	timestampMs int64
	bytes       int
}

// shapeCapture holds the bounded diagnostic samples for one tracked shape.
// The reservoir is a uniform sample of the shape's successful eligible
// operations; slowest is the single slowest one and may share a query with a
// reservoir entry.
type shapeCapture struct {
	eligible   uint64
	admitted   uint64
	oversized  uint64
	byteBudget uint64
	reservoir  []capturedSample
	slowest    *capturedSample
}

// captureState is guarded by insightsMu. Per-shape state lives on impactAgg so
// capture reuses the impact group lookup and group limit.
type captureState struct {
	enabled          bool
	disabledReason   string
	samplesPerShape  int
	maxSampleBytes   int
	maxRetainedBytes int
	rng              *rand.Rand

	eligible       uint64
	failed         uint64
	notExplainable uint64
	noQuery        uint64
	untracked      uint64
	admitted       uint64
	evicted        uint64
	oversized      uint64
	byteBudget     uint64
	retainedBytes  int
}

func (s *captureState) configure(enabled bool, disabledReason string) {
	s.enabled = enabled
	s.disabledReason = ""
	if !enabled {
		s.disabledReason = disabledReason
	}
	s.samplesPerShape = captureSamplesPerShape
	s.maxSampleBytes = captureMaxSampleBytes
	s.maxRetainedBytes = captureMaxRetainedBytes
	if s.rng == nil {
		s.rng = rand.New(rand.NewPCG(captureRNGSeed1, captureRNGSeed2))
	}
}

// reset clears counters; the per-shape samples are dropped with the impact groups.
func (s *captureState) reset() {
	s.eligible, s.failed, s.notExplainable, s.noQuery, s.untracked = 0, 0, 0, 0, 0
	s.admitted, s.evicted, s.oversized, s.byteBudget = 0, 0, 0, 0
	s.retainedBytes = 0
	s.rng = rand.New(rand.NewPCG(captureRNGSeed1, captureRNGSeed2))
}

func supportsCaptureOperation(op string) bool {
	switch op {
	case "find", "aggregate", "updateOne", "updateMany", "deleteOne", "deleteMany":
		return true
	}
	return false
}

func (s *captureState) observe(g *impactAgg, op string, success bool, query *ExecutableQuery, durationNs int64) {
	if !s.enabled {
		return
	}
	switch {
	case !supportsCaptureOperation(op):
		s.notExplainable++
		return
	case !success:
		s.failed++
		return
	case query == nil:
		s.noQuery++
		return
	case g == nil:
		s.untracked++
		return
	}
	s.eligible++
	sc := g.capture
	if sc == nil {
		sc = &shapeCapture{}
		g.capture = sc
	}
	sc.eligible++

	slot := -1
	if len(sc.reservoir) < s.samplesPerShape {
		slot = len(sc.reservoir)
	} else if j := s.rng.Uint64N(sc.eligible); j < uint64(s.samplesPerShape) {
		slot = int(j)
	}
	newSlowest := sc.slowest == nil || durationNs > sc.slowest.durationNs
	if slot < 0 && !newSlowest {
		return
	}

	size := estimateQueryBytes(query)
	if size > s.maxSampleBytes {
		sc.oversized++
		s.oversized++
		return
	}
	freed, added, evictions := 0, 0, uint64(0)
	if slot >= 0 {
		added += size
		if slot < len(sc.reservoir) {
			freed += sc.reservoir[slot].bytes
			evictions++
		}
	}
	if newSlowest {
		added += size
		if sc.slowest != nil {
			freed += sc.slowest.bytes
			evictions++
		}
	}
	if s.retainedBytes-freed+added > s.maxRetainedBytes {
		sc.byteBudget++
		s.byteBudget++
		return
	}

	sample := capturedSample{query: cloneExecutableQuery(query), durationNs: durationNs, timestampMs: time.Now().UnixMilli(), bytes: size}
	if slot >= 0 {
		if slot < len(sc.reservoir) {
			sc.reservoir[slot] = sample
		} else {
			sc.reservoir = append(sc.reservoir, sample)
		}
	}
	if newSlowest {
		cp := sample
		sc.slowest = &cp
	}
	s.retainedBytes += added - freed
	s.evicted += evictions
	sc.admitted++
	s.admitted++
}

// retained returns the distinct retained samples; the slowest sample is
// appended only when it is not already in the reservoir.
func (sc *shapeCapture) retained() (samples []capturedSample, slowestIdx int) {
	samples = append(samples, sc.reservoir...)
	slowestIdx = -1
	if sc.slowest == nil {
		return samples, slowestIdx
	}
	for i := range samples {
		if samples[i].query == sc.slowest.query {
			return samples, i
		}
	}
	samples = append(samples, *sc.slowest)
	return samples, len(samples) - 1
}

// RepresentativeSample is a retained executable sample for post-run
// diagnostics. Query holds literal values and must never be serialized.
type RepresentativeSample struct {
	ShapeID     string
	Operation   string
	Database    string
	Collection  string
	ShapeKey    string
	DurationMs  float64
	TimestampMs int64
	InReservoir bool
	Slowest     bool
	Query       *ExecutableQuery `json:"-"`
}

// SnapshotRepresentativeSamples returns deep copies of every retained
// representative sample, ordered by shape ID then latency.
func (c *Collector) SnapshotRepresentativeSamples() []RepresentativeSample {
	c.insightsMu.Lock()
	defer c.insightsMu.Unlock()
	var out []RepresentativeSample
	for k, g := range c.impact.groups {
		if g.capture == nil {
			continue
		}
		samples, slowestIdx := g.capture.retained()
		id := StableShapeID(k.op, k.collection, k.shape)
		for i, smp := range samples {
			out = append(out, RepresentativeSample{
				ShapeID:     id,
				Operation:   k.op,
				Database:    k.database,
				Collection:  k.collection,
				ShapeKey:    k.shape,
				DurationMs:  nsToMs(smp.durationNs),
				TimestampMs: smp.timestampMs,
				InReservoir: i < len(g.capture.reservoir),
				Slowest:     i == slowestIdx,
				Query:       cloneExecutableQuery(smp.query),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ShapeID != out[j].ShapeID {
			return out[i].ShapeID < out[j].ShapeID
		}
		return out[i].DurationMs < out[j].DurationMs
	})
	return out
}

type QueryCaptureLimits struct {
	SamplesPerShape      int `json:"samples_per_shape"`
	SlowestSlotsPerShape int `json:"slowest_slots_per_shape"`
	MaxGroups            int `json:"max_groups"`
	MaxSampleBytes       int `json:"max_sample_bytes"`
	MaxRetainedBytes     int `json:"max_retained_bytes"`
	MaxDescriptorBytes   int `json:"max_descriptor_bytes"`
	ShapesListedLimit    int `json:"shapes_listed_limit"`
}

type QueryCaptureCoverage struct {
	Scope                    string             `json:"scope"`
	ScopeNote                string             `json:"scope_note"`
	SelectionMethod          string             `json:"selection_method"`
	ShapeIdentityVersion     string             `json:"shape_identity_version"`
	SlowThresholdMs          float64            `json:"slow_threshold_ms"`
	RecordedOperations       uint64             `json:"recorded_operations"`
	EligibleOperations       uint64             `json:"eligible_operations"`
	IneligibleFailed         uint64             `json:"ineligible_failed"`
	IneligibleNotExplainable uint64             `json:"ineligible_not_explainable"`
	IneligibleNoQuery        uint64             `json:"ineligible_no_executable_query"`
	UntrackedShapeOperations uint64             `json:"untracked_shape_operations"`
	SampledOperations        uint64             `json:"sampled_operations"`
	EvictedSamples           uint64             `json:"evicted_samples"`
	RetainedSamples          int                `json:"retained_samples"`
	RetainedBytesEstimate    int                `json:"retained_bytes_estimate"`
	DroppedOversized         uint64             `json:"dropped_oversized"`
	DroppedByteBudget        uint64             `json:"dropped_byte_budget"`
	TrackedShapes            int                `json:"tracked_shapes"`
	ShapesWithSamples        int                `json:"shapes_with_samples"`
	ShapesShown              int                `json:"shapes_shown"`
	NotShownShapes           int                `json:"not_shown_shapes"`
	DescriptorsTruncated     int                `json:"descriptors_truncated"`
	Limits                   QueryCaptureLimits `json:"limits"`
	UnrecordedPaths          []string           `json:"unrecorded_paths"`
	Limitations              []string           `json:"limitations,omitempty"`
}

type CapturedShape struct {
	ShapeID                    string  `json:"shape_id"`
	Namespace                  string  `json:"namespace"`
	Database                   string  `json:"database,omitempty"`
	Collection                 string  `json:"collection"`
	Operation                  string  `json:"operation"`
	ShapeKey                   string  `json:"shape_key"`
	Status                     string  `json:"status"`
	UnavailableReason          string  `json:"unavailable_reason,omitempty"`
	RecordedOperations         uint64  `json:"recorded_operations"`
	EligibleOperations         uint64  `json:"eligible_operations"`
	SampledOperations          uint64  `json:"sampled_operations"`
	RetainedSamples            int     `json:"retained_samples"`
	RetainedBelowSlowThreshold int     `json:"retained_below_slow_threshold"`
	SlowestRetained            bool    `json:"slowest_retained"`
	RetainedMinMs              float64 `json:"retained_min_ms,omitempty"`
	RetainedMedianMs           float64 `json:"retained_median_ms,omitempty"`
	RetainedMaxMs              float64 `json:"retained_max_ms,omitempty"`
	FullRunMaxMs               float64 `json:"full_run_max_ms"`
	Descriptor                 string  `json:"descriptor,omitempty"`
	DescriptorTruncated        bool    `json:"descriptor_truncated,omitempty"`
	DescriptorVariants         int     `json:"descriptor_variants,omitempty"`
}

type QueryCaptureView struct {
	Status         string               `json:"status"`
	DisabledReason string               `json:"disabled_reason,omitempty"`
	Coverage       QueryCaptureCoverage `json:"coverage"`
	Shapes         []CapturedShape      `json:"shapes"`
}

func captureLimits(maxGroups int) QueryCaptureLimits {
	return QueryCaptureLimits{
		SamplesPerShape:      captureSamplesPerShape,
		SlowestSlotsPerShape: 1,
		MaxGroups:            maxGroups,
		MaxSampleBytes:       captureMaxSampleBytes,
		MaxRetainedBytes:     captureMaxRetainedBytes,
		MaxDescriptorBytes:   captureMaxDescriptorBytes,
		ShapesListedLimit:    captureViewLimit,
	}
}

func disabledCaptureView(reason string, slowThresholdMs float64, maxGroups int) QueryCaptureView {
	return QueryCaptureView{
		Status:         "disabled",
		DisabledReason: reason,
		Coverage: QueryCaptureCoverage{
			Scope:                "disabled",
			SelectionMethod:      captureSelectionMethod,
			ShapeIdentityVersion: ShapeIdentityVersion,
			SlowThresholdMs:      slowThresholdMs,
			Limits:               captureLimits(maxGroups),
			UnrecordedPaths:      append([]string(nil), captureUnrecordedPaths...),
		},
		Shapes: []CapturedShape{},
	}
}

func buildQueryCaptureView(st *impactState, cs *captureState, slowThresholdMs float64, maxGroups int) QueryCaptureView {
	if !cs.enabled {
		return disabledCaptureView(cs.disabledReason, slowThresholdMs, maxGroups)
	}
	view := QueryCaptureView{
		Status: "ready",
		Coverage: QueryCaptureCoverage{
			Scope:                    "full_run_counters_with_bounded_samples",
			ScopeNote:                captureScopeNote,
			SelectionMethod:          captureSelectionMethod,
			ShapeIdentityVersion:     ShapeIdentityVersion,
			SlowThresholdMs:          slowThresholdMs,
			RecordedOperations:       st.totalOps,
			EligibleOperations:       cs.eligible,
			IneligibleFailed:         cs.failed,
			IneligibleNotExplainable: cs.notExplainable,
			IneligibleNoQuery:        cs.noQuery,
			UntrackedShapeOperations: cs.untracked,
			SampledOperations:        cs.admitted,
			EvictedSamples:           cs.evicted,
			RetainedBytesEstimate:    cs.retainedBytes,
			DroppedOversized:         cs.oversized,
			DroppedByteBudget:        cs.byteBudget,
			Limits:                   captureLimits(maxGroups),
			UnrecordedPaths:          append([]string(nil), captureUnrecordedPaths...),
		},
		Shapes: []CapturedShape{},
	}
	cov := &view.Coverage
	if st.totalOps == 0 {
		view.Status = "empty"
	}

	type entry struct {
		row CapturedShape
		agg *impactAgg
	}
	unorderedSort := false
	all := make([]entry, 0, len(st.groups))
	for k, g := range st.groups {
		if !supportsCaptureOperation(k.op) {
			continue
		}
		row := CapturedShape{
			ShapeID:            StableShapeID(k.op, k.collection, k.shape),
			Namespace:          impactNamespace(k.database, k.collection),
			Database:           k.database,
			Collection:         k.collection,
			Operation:          k.op,
			ShapeKey:           k.shape,
			RecordedOperations: g.count,
			FullRunMaxMs:       round2(nsToMs(g.maxNs)),
		}
		sc := g.capture
		if sc != nil {
			row.EligibleOperations = sc.eligible
			row.SampledOperations = sc.admitted
			samples, slowestIdx := sc.retained()
			row.RetainedSamples = len(samples)
			row.SlowestRetained = slowestIdx >= 0
			fillCapturedSampleStats(&row, sc, samples, slowThresholdMs)
			cov.RetainedSamples += len(samples)
			if hasUnorderedSortSpec(samples) {
				unorderedSort = true
			}
		}
		if row.RetainedSamples > 0 {
			row.Status = "sampled"
			cov.ShapesWithSamples++
		} else {
			row.Status, row.UnavailableReason = "unavailable", captureUnavailableReason(g)
		}
		all = append(all, entry{row: row, agg: g})
	}
	cov.TrackedShapes = len(all)
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].row, all[j].row
		if a.EligibleOperations != b.EligibleOperations {
			return a.EligibleOperations > b.EligibleOperations
		}
		if a.RecordedOperations != b.RecordedOperations {
			return a.RecordedOperations > b.RecordedOperations
		}
		return a.ShapeID < b.ShapeID
	})
	if len(all) > captureViewLimit {
		cov.NotShownShapes = len(all) - captureViewLimit
		all = all[:captureViewLimit]
	}
	for _, e := range all {
		if e.row.DescriptorTruncated {
			cov.DescriptorsTruncated++
		}
		view.Shapes = append(view.Shapes, e.row)
	}
	cov.ShapesShown = len(view.Shapes)

	if cs.untracked > 0 || st.overflowOps > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Group limit %d reached: %d eligible operations belong to untracked shapes and have no samples.", maxGroups, cs.untracked))
	}
	if cs.oversized > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d sample admissions were skipped because the command exceeded the %d-byte sample limit.", cs.oversized, captureMaxSampleBytes))
	}
	if cs.byteBudget > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d sample admissions were skipped because retained samples reached the %d-byte budget; shapes seen later may be under-sampled.", cs.byteBudget, captureMaxRetainedBytes))
	}
	if cs.noQuery > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d successful operations had no executable query and cannot be sampled.", cs.noQuery))
	}
	if cov.NotShownShapes > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Only %d of %d tracked shapes are listed; unlisted shapes keep their samples.", cov.ShapesShown, cov.TrackedShapes))
	}
	if cov.DescriptorsTruncated > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d descriptors were truncated at %d bytes; executable samples are unaffected.", cov.DescriptorsTruncated, captureMaxDescriptorBytes))
	}
	if unorderedSort {
		cov.Limitations = append(cov.Limitations,
			"Some $sort specifications have several keys but were defined as unordered maps, so the key order sent to the server is not defined; descriptors list keys alphabetically.")
	}
	return view
}

// captureUnavailableReason explains why a tracked shape has no retained sample.
func captureUnavailableReason(g *impactAgg) string {
	sc := g.capture
	switch {
	case sc == nil && g.errorCount == g.count:
		return "no_successful_operations"
	case sc == nil:
		return "no_executable_query"
	case sc.oversized > 0:
		return "command_exceeds_sample_byte_limit"
	default:
		return "retained_byte_budget_exhausted"
	}
}

func fillCapturedSampleStats(row *CapturedShape, sc *shapeCapture, samples []capturedSample, slowThresholdMs float64) {
	if len(samples) == 0 {
		return
	}
	lat := make([]float64, 0, len(samples))
	descriptors := make(map[string]struct{}, len(samples))
	for _, smp := range samples {
		ms := nsToMs(smp.durationNs)
		lat = append(lat, ms)
		if ms < slowThresholdMs {
			row.RetainedBelowSlowThreshold++
		}
		d, _ := DescribeExecutableQuery(row.Operation, smp.query)
		descriptors[d] = struct{}{}
	}
	sort.Float64s(lat)
	row.RetainedMinMs = round2(lat[0])
	row.RetainedMaxMs = round2(lat[len(lat)-1])
	res := make([]float64, 0, len(sc.reservoir))
	for _, smp := range sc.reservoir {
		res = append(res, nsToMs(smp.durationNs))
	}
	row.RetainedMedianMs = round2(percentile(res, 50))
	row.Descriptor, row.DescriptorTruncated = DescribeExecutableQuery(row.Operation, samples[0].query)
	row.DescriptorVariants = len(descriptors)
}

func hasUnorderedSortSpec(samples []capturedSample) bool {
	for _, smp := range samples {
		if smp.query != nil && HasUnorderedSortSpec(smp.query.Pipeline) {
			return true
		}
	}
	return false
}

// HasUnorderedSortSpec reports whether a pipeline, including $facet, $lookup,
// and $unionWith sub-pipelines, has a multi-key $sort held in an unordered map.
// The key order such a stage sent to the server is not defined.
func HasUnorderedSortSpec(pipeline []interface{}) bool {
	for _, raw := range pipeline {
		var stage map[string]interface{}
		switch t := raw.(type) {
		case map[string]interface{}:
			stage = t
		case bson.M:
			stage = t
		default:
			continue
		}
		for name, v := range stage {
			switch name {
			case "$sort":
				switch spec := v.(type) {
				case map[string]interface{}:
					if len(spec) > 1 {
						return true
					}
				case bson.M:
					if len(spec) > 1 {
						return true
					}
				}
			case "$facet":
				if m, ok := v.(map[string]interface{}); ok {
					for _, sub := range m {
						if p, ok := sub.([]interface{}); ok && HasUnorderedSortSpec(p) {
							return true
						}
					}
				}
			case "$lookup", "$unionWith":
				if m, ok := v.(map[string]interface{}); ok {
					if p, ok := m["pipeline"].([]interface{}); ok && HasUnorderedSortSpec(p) {
						return true
					}
				}
			}
		}
	}
	return false
}

// estimateQueryBytes approximates the BSON size of an executable query.
func estimateQueryBytes(q *ExecutableQuery) int {
	n := 64 + len(q.Database)
	n += estimateValueBytes(q.Filter) + estimateValueBytes(q.Projection) + estimateValueBytes(q.Update)
	if q.Pipeline != nil {
		n += estimateValueBytes(q.Pipeline)
	}
	return n
}

func estimateValueBytes(v interface{}) int {
	switch t := v.(type) {
	case nil:
		return 0
	case map[string]interface{}:
		n := 5
		for k, x := range t {
			n += len(k) + 2 + estimateValueBytes(x)
		}
		return n
	case bson.M:
		return estimateValueBytes(map[string]interface{}(t))
	case bson.D:
		n := 5
		for _, e := range t {
			n += len(e.Key) + 2 + estimateValueBytes(e.Value)
		}
		return n
	case []interface{}:
		n := 5
		for _, x := range t {
			n += 4 + estimateValueBytes(x)
		}
		return n
	case bson.A:
		return estimateValueBytes([]interface{}(t))
	case string:
		return len(t) + 5
	case []byte:
		return len(t) + 5
	case []int64:
		return 5 + 12*len(t)
	case []int32:
		return 5 + 8*len(t)
	case []int:
		return 5 + 12*len(t)
	case []float64:
		return 5 + 12*len(t)
	case []string:
		n := 5
		for _, s := range t {
			n += 9 + len(s)
		}
		return n
	case bool:
		return 1
	case int32:
		return 4
	case int, int64, float64, float32, uint32, uint64:
		return 8
	default:
		return 16
	}
}

type descriptorContext int

const (
	ctxFilter descriptorContext = iota // every scalar is a literal and is redacted
	ctxSpec                            // projection/sort/limit specs: numbers and booleans are structure
	ctxExpr                            // aggregation expressions: "$field" paths are structure
)

// DescribeExecutableQuery renders a display-safe descriptor of an executable
// query. Literal values become BSON type placeholders; field names, operators,
// limits, and projection/sort specs are kept. The result is bounded by
// captureMaxDescriptorBytes and reports whether it was truncated.
func DescribeExecutableQuery(op string, q *ExecutableQuery) (string, bool) {
	if q == nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(op)
	if op == "aggregate" {
		b.WriteString(" pipeline=")
		writePipelineDescriptor(&b, q.Pipeline)
	} else {
		b.WriteString(" filter=")
		writeDescriptorValue(&b, q.Filter, ctxFilter)
		if len(q.Projection) > 0 {
			b.WriteString(" projection=")
			writeDescriptorValue(&b, q.Projection, ctxSpec)
		}
		if q.Limit > 0 {
			b.WriteString(" limit=")
			b.WriteString(strconv.FormatInt(q.Limit, 10))
		}
		if q.Update != nil {
			b.WriteString(" update=")
			writeDescriptorValue(&b, q.Update, ctxFilter)
		}
		if q.Upsert {
			b.WriteString(" upsert=true")
		}
	}
	return truncateDescriptor(b.String(), captureMaxDescriptorBytes)
}

func truncateDescriptor(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...(truncated)", true
}

func writePipelineDescriptor(b *strings.Builder, pipeline []interface{}) {
	b.WriteByte('[')
	for i, raw := range pipeline {
		if i > 0 {
			b.WriteByte(',')
		}
		stage, ok := raw.(map[string]interface{})
		if !ok {
			writeDescriptorValue(b, raw, ctxFilter)
			continue
		}
		b.WriteByte('{')
		for j, name := range sortedKeys(stage) {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(name)
			b.WriteByte(':')
			writeStageDescriptor(b, name, stage[name])
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
}

func writeStageDescriptor(b *strings.Builder, name string, v interface{}) {
	switch name {
	case "$match":
		writeDescriptorValue(b, v, ctxFilter)
	case "$sort", "$limit", "$skip", "$sample", "$project":
		writeDescriptorValue(b, v, ctxSpec)
	case "$facet":
		m, ok := v.(map[string]interface{})
		if !ok {
			writeDescriptorValue(b, v, ctxExpr)
			return
		}
		b.WriteByte('{')
		for i, k := range sortedKeys(m) {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(k)
			b.WriteByte(':')
			if sub, ok := m[k].([]interface{}); ok {
				writePipelineDescriptor(b, sub)
			} else {
				writeDescriptorValue(b, m[k], ctxExpr)
			}
		}
		b.WriteByte('}')
	default:
		writeDescriptorValue(b, v, ctxExpr)
	}
}

func writeDescriptorValue(b *strings.Builder, v interface{}, ctx descriptorContext) {
	switch t := v.(type) {
	case map[string]interface{}:
		b.WriteByte('{')
		for i, k := range sortedKeys(t) {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(k)
			b.WriteByte(':')
			writeDescriptorChild(b, k, t[k], ctx)
		}
		b.WriteByte('}')
	case bson.M:
		writeDescriptorValue(b, map[string]interface{}(t), ctx)
	case bson.D:
		b.WriteByte('{')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(e.Key)
			b.WriteByte(':')
			writeDescriptorChild(b, e.Key, e.Value, ctx)
		}
		b.WriteByte('}')
	case []interface{}:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case bson.A:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case []int64:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case []int32:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case []int:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case []float64:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	case []string:
		writeDescriptorArray(b, len(t), func(i int) interface{} { return t[i] }, ctx)
	default:
		b.WriteString(describeScalar(v, ctx))
	}
}

func writeDescriptorChild(b *strings.Builder, key string, v interface{}, ctx descriptorContext) {
	switch {
	case key == "$literal":
		writeDescriptorValue(b, v, ctxFilter)
	case ctx == ctxFilter && key == "$expr":
		writeDescriptorValue(b, v, ctxExpr)
	case ctx == ctxExpr && key == "sortBy":
		if d, ok := v.(bson.D); ok {
			writeDescriptorValue(b, d, ctxSpec)
			return
		}
		writeDescriptorValue(b, v, ctx)
	case ctx == ctxExpr && key == "pipeline":
		if sub, ok := v.([]interface{}); ok {
			writePipelineDescriptor(b, sub)
			return
		}
		writeDescriptorValue(b, v, ctx)
	case ctx == ctxSpec:
		// Only direct spec values are structural; nested expressions may hold literals.
		switch v.(type) {
		case map[string]interface{}, bson.M, bson.D, []interface{}, bson.A:
			writeDescriptorValue(b, v, ctxExpr)
		default:
			b.WriteString(describeScalar(v, ctxSpec))
		}
	default:
		writeDescriptorValue(b, v, ctx)
	}
}

func writeDescriptorArray(b *strings.Builder, n int, at func(int) interface{}, ctx descriptorContext) {
	parts := make([]string, n)
	same := n > 1
	for i := 0; i < n; i++ {
		var sb strings.Builder
		writeDescriptorValue(&sb, at(i), ctx)
		parts[i] = sb.String()
		if parts[i] != parts[0] {
			same = false
		}
	}
	if same {
		fmt.Fprintf(b, "[%s x%d]", parts[0], n)
		return
	}
	b.WriteByte('[')
	for i, p := range parts {
		if i == captureArrayPreview {
			fmt.Fprintf(b, ",...(+%d)", n-i)
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p)
	}
	b.WriteByte(']')
}

func describeScalar(v interface{}, ctx descriptorContext) string {
	if ctx == ctxSpec {
		switch t := v.(type) {
		case bool:
			return strconv.FormatBool(t)
		case int:
			return strconv.Itoa(t)
		case int32:
			return strconv.FormatInt(int64(t), 10)
		case int64:
			return strconv.FormatInt(t, 10)
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
	}
	if s, ok := v.(string); ok && ctx != ctxFilter && strings.HasPrefix(s, "$") {
		return strconv.Quote(s)
	}
	return bsonTypePlaceholder(v)
}

func bsonTypePlaceholder(v interface{}) string {
	switch v.(type) {
	case nil:
		return "<null>"
	case string:
		return "<string>"
	case bool:
		return "<bool>"
	case int:
		return "<int>"
	case int8, int16, int32:
		return "<int32>"
	case int64:
		return "<int64>"
	case uint, uint8, uint16, uint32, uint64:
		return "<int>"
	case float32, float64:
		return "<double>"
	case time.Time, bson.DateTime:
		return "<date>"
	case bson.ObjectID:
		return "<objectId>"
	case bson.Decimal128:
		return "<decimal>"
	case bson.Regex:
		return "<regex>"
	case bson.Binary, []byte:
		return "<binData>"
	case bson.Timestamp:
		return "<timestamp>"
	default:
		return "<value>"
	}
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
