package stats

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Query values in trends (opt-in, insights_trend_store_query_values). Each
// shape snapshot can keep the literal commands of its explained samples and
// up to trendValuesMaxRetained other retained samples, as canonical MongoDB
// Extended JSON v2 with document key order kept. The comparison of values
// between runs is an annotation only; it never changes trend labels.
const (
	TrendValuesSame        = "same_values"
	TrendValuesOverlapping = "overlapping"
	TrendValuesDifferent   = "different_values"
	TrendValuesNotRecorded = "not_recorded"

	TrendValueRoleRetained = "retained"

	trendValuesMaxSampleBytes = 16 << 10
	trendValuesMaxShapeBytes  = 64 << 10
	trendValuesMaxRunBytes    = 1 << 20
	trendValuesMaxRetained    = 8
	trendValuesMaxSamples     = ExplainMaxSamplesPerShape + trendValuesMaxRetained
	trendValuesMaxNote        = 512

	TrendValuesNote        = "Annotation only: when query values differ between runs, the queries touch different documents and index pages, so cache state may differ. It does not change the improved/regressed labels or the comparability rules."
	TrendQueryValuesNotice = "Contains literal query values (canonical Extended JSON) from runs recorded with insights_trend_store_query_values on. Treat this data like the database contents it was taken from."
	trendValuesMethodNote  = "Query values: each run recorded with insights_trend_store_query_values on keeps, per shape, the commands of its explained samples and up to 8 other retained samples (\"retained, not explained\"). Two samples match when their canonical Extended JSON is identical (same SHA-256 values hash). same_values: both runs stored the same set of commands; overlapping: some commands match; different_values: none match; not_recorded: one side stored no values. The comparison is with the run that supplied the latency baseline (otherwise the efficiency baseline)."
)

var trendHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TrendQueryValueSample is one literal sample command. A Truncated sample was
// larger than the per-sample limit: its command is omitted, its hash is kept.
type TrendQueryValueSample struct {
	Role            string  `json:"role"`
	Explained       bool    `json:"explained"`
	ClientLatencyMs float64 `json:"client_latency_ms"`
	Command         string  `json:"command,omitempty"`
	CommandBytes    int     `json:"command_bytes"`
	ValuesHash      string  `json:"values_hash"`
	Truncated       bool    `json:"truncated,omitempty"`
}

// TrendQueryValues are the stored sample commands of one shape in one run.
// ValuesHash covers the sorted sample hashes. Truncated is set whenever a
// sample or a command was left out, with the count and reason.
type TrendQueryValues struct {
	RunID          string                  `json:"run_id"`
	RecordedAt     string                  `json:"recorded_at"`
	ValuesHash     string                  `json:"values_hash"`
	Samples        []TrendQueryValueSample `json:"samples"`
	Truncated      bool                    `json:"truncated,omitempty"`
	OmittedSamples int                     `json:"omitted_samples,omitempty"`
	TruncationNote string                  `json:"truncation_note,omitempty"`
}

// TrendValuesComparison compares the stored values of a row's current and
// baseline runs. It is an annotation only.
type TrendValuesComparison struct {
	State            string            `json:"state"`
	Label            string            `json:"label"`
	Note             string            `json:"note"`
	BaselineRunID    string            `json:"baseline_run_id,omitempty"`
	BaselineRunLabel string            `json:"baseline_run_label,omitempty"`
	MatchingSamples  int               `json:"matching_samples"`
	Current          *TrendQueryValues `json:"current,omitempty"`
	Baseline         *TrendQueryValues `json:"baseline,omitempty"`
	CurrentReason    string            `json:"current_reason,omitempty"`
	BaselineReason   string            `json:"baseline_reason,omitempty"`
	// Source is TrendValuesSourceGeneratedTraffic or TrendValuesSourceRecording
	// when same_values comes from equal traffic fingerprints or the same
	// replayed recording rather than stored samples.
	Source string `json:"source,omitempty"`
	// ReplayHeaderHash is the recording both runs replayed (source
	// same_recording), so the row can link to its comparison report.
	ReplayHeaderHash string `json:"replay_header_hash,omitempty"`
}

// TrendValuesSourceGeneratedTraffic marks a same_values comparison of two
// deterministic runs with the same traffic fingerprint;
// TrendValuesSourceRecording one of two replays of the same recording.
const (
	TrendValuesSourceGeneratedTraffic = "same_generated_traffic"
	TrendValuesSourceRecording        = "same_recording"
)

// TrendExecutedCommand rebuilds the command a worker sent for a sample (the
// command itself, not an explain), or returns why it cannot.
func TrendExecutedCommand(op, collection string, q *ExecutableQuery) (bson.D, string) {
	if q == nil {
		return nil, "executable_query_not_captured"
	}
	if strings.TrimSpace(collection) == "" {
		return nil, "collection_not_captured"
	}
	var filter interface{} = bson.M{}
	if q.Filter != nil {
		filter = q.Filter
	}
	var cmd bson.D
	switch op {
	case "find":
		cmd = bson.D{{Key: "find", Value: collection}, {Key: "filter", Value: filter}}
		if len(q.Projection) > 0 {
			cmd = append(cmd, bson.E{Key: "projection", Value: q.Projection})
		}
		if q.Limit != 0 {
			cmd = append(cmd, bson.E{Key: "limit", Value: q.Limit})
		}
	case "updateOne", "updateMany":
		if len(q.Update) == 0 {
			return nil, "update_document_not_captured"
		}
		stmt := bson.D{{Key: "q", Value: filter}, {Key: "u", Value: q.Update}, {Key: "multi", Value: op == "updateMany"}, {Key: "upsert", Value: q.Upsert}}
		cmd = bson.D{{Key: "update", Value: collection}, {Key: "updates", Value: bson.A{stmt}}}
	case "deleteOne", "deleteMany":
		limit := 0
		if op == "deleteOne" {
			limit = 1
		}
		cmd = bson.D{{Key: "delete", Value: collection}, {Key: "deletes", Value: bson.A{bson.D{{Key: "q", Value: filter}, {Key: "limit", Value: limit}}}}}
	case "aggregate":
		pipeline := q.Pipeline
		if pipeline == nil {
			pipeline = []interface{}{}
		}
		if HasUnorderedSortSpec(pipeline) {
			return nil, "sort_key_order_undefined"
		}
		cmd = bson.D{{Key: "aggregate", Value: collection}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.D{}}}
	default:
		return nil, "unsupported_operation"
	}
	if q.Database != "" {
		cmd = append(cmd, bson.E{Key: "$db", Value: q.Database})
	}
	return cmd, ""
}

// canonicalTrendValue makes encoding deterministic: maps (whose order Go does
// not define) become documents with sorted keys; ordered documents keep their
// order, so a multi-key bson.D $sort is never alphabetized.
func canonicalTrendValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(bson.D, 0, len(keys))
		for _, k := range keys {
			out = append(out, bson.E{Key: k, Value: canonicalTrendValue(t[k])})
		}
		return out
	case bson.M:
		return canonicalTrendValue(map[string]interface{}(t))
	case bson.D:
		out := make(bson.D, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: canonicalTrendValue(e.Value)}
		}
		return out
	case []interface{}:
		out := make(bson.A, len(t))
		for i, x := range t {
			out[i] = canonicalTrendValue(x)
		}
		return out
	case bson.A:
		return canonicalTrendValue([]interface{}(t))
	default:
		return v
	}
}

// CanonicalTrendCommand encodes cmd as canonical Extended JSON v2.
func CanonicalTrendCommand(cmd bson.D) (string, error) {
	b, err := bson.MarshalExtJSON(canonicalTrendValue(cmd), true, false)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func trendValuesHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newTrendValueSample encodes one sample; the command is dropped (and the
// sample marked truncated) above the per-sample limit.
func newTrendValueSample(role string, explained bool, latencyMs float64, op, collection string, q *ExecutableQuery) (TrendQueryValueSample, string) {
	cmd, reason := TrendExecutedCommand(op, collection, q)
	if reason != "" {
		return TrendQueryValueSample{}, reason
	}
	text, err := CanonicalTrendCommand(cmd)
	if err != nil {
		return TrendQueryValueSample{}, "command_not_encodable"
	}
	s := TrendQueryValueSample{
		Role: role, Explained: explained, ClientLatencyMs: math.Round(latencyMs*100) / 100,
		Command: text, CommandBytes: len(text), ValuesHash: trendValuesHash(text),
	}
	if len(text) > trendValuesMaxSampleBytes {
		s.Command, s.Truncated = "", true
	}
	return s, ""
}

// trendValuesBuilder applies the sample, per-shape, and per-run limits and
// records every sample it leaves out.
type trendValuesBuilder struct {
	runBytes int
}

func (b *trendValuesBuilder) add(v *TrendQueryValues, seen map[string]bool, s TrendQueryValueSample, shapeBytes *int, notes map[string]bool) {
	if seen[s.ValuesHash] {
		return
	}
	size := len(s.Command)
	omit := func(note string) {
		v.OmittedSamples++
		notes[note] = true
	}
	switch {
	case len(v.Samples) >= trendValuesMaxSamples:
		omit(fmt.Sprintf("at most %d samples per shape are stored", trendValuesMaxSamples))
		return
	case *shapeBytes+size > trendValuesMaxShapeBytes:
		omit(fmt.Sprintf("the shape reached its limit of %d bytes of commands", trendValuesMaxShapeBytes))
		return
	case b.runBytes+size > trendValuesMaxRunBytes:
		omit(fmt.Sprintf("the run reached its limit of %d bytes of commands", trendValuesMaxRunBytes))
		return
	}
	if s.Truncated {
		notes[fmt.Sprintf("a command larger than %d bytes is stored as its hash only", trendValuesMaxSampleBytes)] = true
	}
	seen[s.ValuesHash] = true
	*shapeBytes += size
	b.runBytes += size
	v.Samples = append(v.Samples, s)
}

func finishTrendValues(v *TrendQueryValues, notes map[string]bool) {
	hashes := make([]string, 0, len(v.Samples))
	for _, s := range v.Samples {
		hashes = append(hashes, s.ValuesHash)
		if s.Truncated {
			v.Truncated = true
		}
	}
	sort.Strings(hashes)
	v.ValuesHash = trendValuesHash(strings.Join(hashes, ","))
	if v.OmittedSamples > 0 {
		v.Truncated = true
	}
	if len(notes) > 0 {
		list := make([]string, 0, len(notes))
		for n := range notes {
			list = append(list, n)
		}
		sort.Strings(list)
		v.TruncationNote = trendText(strings.Join(list, "; "), trendValuesMaxNote)
	}
}

// BuildTrendQueryValues collects, for every efficiency-view row of rep, the
// commands of its explained samples (role and client latency from explain)
// and then up to trendValuesMaxRetained other retained samples. Rows are
// visited in view order, so the per-run limit keeps the top-ranked shapes.
func BuildTrendQueryValues(rep InsightsReport, retained []RepresentativeSample) map[string]TrendQueryValues {
	out := map[string]TrendQueryValues{}
	if rep.EfficiencyView == nil {
		return out
	}
	explained := map[string]ExplainCoverageShape{}
	for _, cs := range rep.ExplainCoverage.Shapes {
		explained[cs.ShapeID] = cs
	}
	byShape := map[string][]RepresentativeSample{}
	for _, s := range retained {
		byShape[s.ShapeID] = append(byShape[s.ShapeID], s)
	}
	var b trendValuesBuilder
	for _, r := range rep.EfficiencyView.Rows {
		var v TrendQueryValues
		seen, notes, shapeBytes := map[string]bool{}, map[string]bool{}, 0
		skipped := func(reason string) {
			v.OmittedSamples++
			if reason == "sort_key_order_undefined" {
				notes["a multi-key $sort held in an unordered map has no defined key order and is not stored"] = true
			} else {
				notes["a sample without a complete command is not stored ("+reason+")"] = true
			}
		}
		if cs, ok := explained[r.ShapeID]; ok {
			for _, es := range cs.Samples {
				if es.Status != "explained" || es.Query == nil {
					continue
				}
				s, reason := newTrendValueSample(es.Role, true, es.ClientLatencyMs, cs.Operation, cs.Collection, es.Query)
				if reason != "" {
					skipped(reason)
					continue
				}
				b.add(&v, seen, s, &shapeBytes, notes)
			}
		}
		var others []TrendQueryValueSample
		for _, rs := range byShape[r.ShapeID] {
			s, reason := newTrendValueSample(TrendValueRoleRetained, false, rs.DurationMs, rs.Operation, rs.Collection, rs.Query)
			if reason != "" {
				skipped(reason)
				continue
			}
			if !seen[s.ValuesHash] {
				others = append(others, s)
			}
		}
		// The smallest hashes are kept so two runs that retained the same
		// values keep the same subset, whatever their latencies or order.
		sort.Slice(others, func(i, j int) bool { return others[i].ValuesHash < others[j].ValuesHash })
		kept := 0
		for i, s := range others {
			if kept >= trendValuesMaxRetained {
				break
			}
			if i > 0 && s.ValuesHash == others[i-1].ValuesHash {
				continue
			}
			b.add(&v, seen, s, &shapeBytes, notes)
			kept++
		}
		if len(v.Samples) == 0 && v.OmittedSamples == 0 {
			continue
		}
		finishTrendValues(&v, notes)
		out[r.ShapeID] = v
	}
	return out
}

// sanitizeTrendQueryValues re-applies the limits to a stored or imported
// snapshot: commands must be valid JSON within the per-sample limit, hashes
// are recomputed from the commands, and anything left out is counted.
func sanitizeTrendQueryValues(v TrendQueryValues) TrendQueryValues {
	notes := map[string]bool{}
	if v.TruncationNote != "" {
		notes[trendText(v.TruncationNote, trendValuesMaxNote)] = true
	}
	in := v.Samples
	v.Samples, v.ValuesHash, v.Truncated, v.TruncationNote = nil, "", false, ""
	if v.OmittedSamples < 0 {
		v.OmittedSamples = 0
	}
	seen, shapeBytes := map[string]bool{}, 0
	for _, s := range in {
		s.Role = trendText(s.Role, 32)
		if s.Role == "" {
			s.Role = TrendValueRoleRetained
		}
		if math.IsNaN(s.ClientLatencyMs) || math.IsInf(s.ClientLatencyMs, 0) || s.ClientLatencyMs < 0 {
			s.ClientLatencyMs = 0
		}
		switch {
		case s.Command != "" && (len(s.Command) > trendValuesMaxSampleBytes || !json.Valid([]byte(s.Command))):
			v.OmittedSamples++
			notes["a stored command that is invalid or over the per-sample limit was dropped"] = true
			continue
		case s.Command != "":
			s.ValuesHash, s.CommandBytes, s.Truncated = trendValuesHash(s.Command), len(s.Command), false
		case !trendHashPattern.MatchString(s.ValuesHash):
			v.OmittedSamples++
			notes["a stored sample without a command or valid hash was dropped"] = true
			continue
		default:
			s.Truncated = true
			if s.CommandBytes < 0 {
				s.CommandBytes = 0
			}
		}
		if seen[s.ValuesHash] {
			continue
		}
		if len(v.Samples) >= trendValuesMaxSamples || shapeBytes+len(s.Command) > trendValuesMaxShapeBytes {
			v.OmittedSamples++
			notes["stored samples above the per-shape limits were dropped"] = true
			continue
		}
		seen[s.ValuesHash] = true
		shapeBytes += len(s.Command)
		v.Samples = append(v.Samples, s)
	}
	finishTrendValues(&v, notes)
	return v
}

func trendValuesBytes(v *TrendQueryValues) int {
	n := 0
	if v != nil {
		for _, s := range v.Samples {
			n += len(s.Command)
		}
	}
	return n
}

// compareTrendValues classifies the stored sample commands of both runs. An
// empty side is not_recorded.
func compareTrendValues(cur, base *TrendQueryValues) (string, int) {
	if cur == nil || base == nil || len(cur.Samples) == 0 || len(base.Samples) == 0 {
		return TrendValuesNotRecorded, 0
	}
	b := map[string]bool{}
	for _, s := range base.Samples {
		b[s.ValuesHash] = true
	}
	c := map[string]bool{}
	match := 0
	for _, s := range cur.Samples {
		if !c[s.ValuesHash] && b[s.ValuesHash] {
			match++
		}
		c[s.ValuesHash] = true
	}
	switch {
	case match == 0:
		return TrendValuesDifferent, 0
	case match == len(c) && match == len(b):
		return TrendValuesSame, match
	default:
		return TrendValuesOverlapping, match
	}
}

// trendValuesReason says why one side of a values comparison has no samples.
func trendValuesReason(side string, run *TrendRunContext, v *TrendQueryValues) string {
	switch {
	case v != nil && len(v.Samples) > 0:
		return ""
	case run == nil:
		return fmt.Sprintf("values not recorded for the %s run (no baseline run)", side)
	case run.QueryValuesDropped:
		return fmt.Sprintf("values not recorded for the %s run: they were removed to keep the baseline store within its size limit", side)
	case !run.QueryValuesStored:
		return fmt.Sprintf("values not recorded for the %s run (insights_trend_store_query_values was off)", side)
	case v != nil && v.OmittedSamples > 0:
		return fmt.Sprintf("values not recorded for the %s run: %s", side, emptyFallback(v.TruncationNote, "every sample was left out"))
	default:
		return fmt.Sprintf("values not recorded for the %s run: no literal sample was retained for this shape", side)
	}
}

// trendValuesRow builds the values annotation of a row, or nil when neither
// run was recorded with query values (so rows stay unchanged by default).
func trendValuesRow(cur TrendShapeSnapshot, base *TrendShapeSnapshot, ctx trendCompareCtx) *TrendValuesComparison {
	var bv *TrendQueryValues
	baseID := ""
	if base != nil {
		bv = base.QueryValues
		switch {
		case bv != nil:
			baseID = bv.RunID
		case base.Workload != nil:
			baseID = base.Workload.RunID
		case base.Efficiency != nil:
			baseID = base.Efficiency.RunID
		}
	}
	var baseRun *TrendRunContext
	if r, ok := ctx.runs[baseID]; ok && baseID != "" {
		baseRun = &r
	}
	if f := ctx.run.Workload.GeneratedTrafficFingerprint; f != "" && baseRun != nil && f == baseRun.Workload.GeneratedTrafficFingerprint {
		_, match := compareTrendValues(cur.QueryValues, bv)
		return &TrendValuesComparison{
			State: TrendValuesSame, Source: TrendValuesSourceGeneratedTraffic, Note: TrendValuesNote,
			Label:         fmt.Sprintf("Same values: both runs generated the same deterministic traffic (fingerprint %s); no stored literals are needed", trendShortHash(f)),
			BaselineRunID: baseID, BaselineRunLabel: TrendRunLabel(baseRun.Label, baseRun.Server, baseRun.RecordedAt),
			MatchingSamples: match, Current: cur.QueryValues, Baseline: bv,
		}
	}
	if h := ctx.run.Workload.ReplayHeaderHash; h != "" && baseRun != nil && h == baseRun.Workload.ReplayHeaderHash {
		_, match := compareTrendValues(cur.QueryValues, bv)
		return &TrendValuesComparison{
			State: TrendValuesSame, Source: TrendValuesSourceRecording, ReplayHeaderHash: h, Note: TrendValuesNote,
			Label:         fmt.Sprintf("Same values: both runs replayed the same recording (header hash %s); no stored literals are needed", trendShortHash(h)),
			BaselineRunID: baseID, BaselineRunLabel: TrendRunLabel(baseRun.Label, baseRun.Server, baseRun.RecordedAt),
			MatchingSamples: match, Current: cur.QueryValues, Baseline: bv,
		}
	}
	if !ctx.run.QueryValuesStored && (baseRun == nil || !baseRun.QueryValuesStored) {
		return nil
	}
	state, match := compareTrendValues(cur.QueryValues, bv)
	c := &TrendValuesComparison{
		State: state, Note: TrendValuesNote, BaselineRunID: baseID, MatchingSamples: match,
		Current: cur.QueryValues, Baseline: bv,
		CurrentReason:  trendValuesReason("current", &ctx.run, cur.QueryValues),
		BaselineReason: trendValuesReason("baseline", baseRun, bv),
	}
	if baseRun != nil {
		c.BaselineRunLabel = TrendRunLabel(baseRun.Label, baseRun.Server, baseRun.RecordedAt)
	}
	n := 0
	if cur.QueryValues != nil {
		n = len(cur.QueryValues.Samples)
	}
	switch state {
	case TrendValuesSame:
		c.Label = fmt.Sprintf("Same values: all %d sample commands match the baseline", n)
	case TrendValuesOverlapping:
		c.Label = fmt.Sprintf("Overlapping values: %d of %d sample commands match the baseline; cache state may differ", match, n)
	case TrendValuesDifferent:
		c.Label = fmt.Sprintf("Different values: none of the %d sample commands match the baseline; cache state may differ", n)
	default:
		c.Label = "Values not recorded: " + emptyFallback(c.CurrentReason, c.BaselineReason)
	}
	return c
}

// trendValuesForRun returns shape s's values snapshot recorded by run id.
func trendValuesForRun(s TrendShapeBaseline, id string) *TrendQueryValues {
	if id == "" {
		return nil
	}
	for i := range s.QueryValuesHistory {
		if s.QueryValuesHistory[i].RunID == id {
			v := s.QueryValuesHistory[i]
			return &v
		}
	}
	return nil
}

func valuesKey(v TrendQueryValues) (string, string) { return v.RunID, v.RecordedAt }

// TrendDataHasQueryValues reports whether any snapshot in d holds values.
func TrendDataHasQueryValues(d TrendStoreData) bool {
	for _, sh := range d.Shapes {
		if len(sh.QueryValuesHistory) > 0 {
			return true
		}
	}
	return false
}

// stripTrendRunValues removes run id's query values from d and marks the run.
func stripTrendRunValues(d TrendStoreData, id string) TrendStoreData {
	for i := range d.Shapes {
		d.Shapes[i].QueryValuesHistory = filterHistory(d.Shapes[i].QueryValuesHistory, func(v TrendQueryValues) bool { return v.RunID != id })
	}
	for i := range d.Runs {
		if d.Runs[i].RunID == id {
			d.Runs[i].QueryValuesDropped = true
		}
	}
	return d
}
