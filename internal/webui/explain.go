package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/db"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
	"go.mongodb.org/mongo-driver/v2/bson"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var runExplainJobsFn = runExplainJobsConcurrently

type explainEvidence struct {
	status   string
	reason   string
	attempts int
	diag     *stats.ExplainDiagnostics
}

// explainCommandSpec is a non-mutating explain command built from the exact
// captured query. cmd carries literal values and is only sent to the server;
// commandSummary and stageSummary are the redacted forms used in logs and UI.
type explainCommandSpec struct {
	db             string
	cmd            bson.D
	verbosity      string
	verbosityNote  string
	serverMaxTime  int
	clientTimeout  time.Duration
	commandType    string
	stageSummary   string
	commandSummary string
	unfiltered     bool
}

type explainSignals struct {
	planStages         []string
	indexesUsed        []string
	collectionScan     bool
	indexScan          bool
	fetch              bool
	group              bool
	sort               bool
	limit              bool
	blockingSort       bool
	usedDisk           bool
	executionStats     bool
	collectionScans    int64
	indexSeeks         int64
	executionTimeMS    int64
	spills             int64
	winningPlanSummary string
	shardSummary       string
}

type samplePick struct {
	role   string
	target float64
	sample stats.RepresentativeSample
}

type explainJob struct {
	sample stats.RepresentativeSample
}

type sampleRef struct {
	shape  int
	sample int
	job    int
}

// explainParams are the post-run explain limits from the insights settings.
type explainParams struct {
	topN           int
	maxTimeMs      int
	verbosity      string
	severityMode   string
	efficiencyMode string
	workers        int
	retries        int
	backoffMs      int
}

const explainSelectionNote = "Severity slots (insights_explain_top_n) take shapes from the severity-filtered slow view. Efficiency slots (the same number) are chosen from every tracked shape regardless of the slow threshold or severity filter, alternating between the highest remaining workload impact and a round-robin over full-run average latency tiers from fastest to slowest, so fast, low-impact shapes can be assessed. A shape that was not selected is unassessed, not efficient."

func cancelledEvidence(attempts int) explainEvidence {
	return explainEvidence{status: "cancelled", reason: "explain_cancelled", attempts: attempts}
}

func coverageNamespace(database, collection string) string {
	if strings.TrimSpace(database) == "" {
		return collection
	}
	return database + "." + collection
}

func enrichInsightsWithExplain(ctx context.Context, status string, rep *stats.InsightsReport, inv stats.ExplainInventory, cfg *config.AppConfig, p explainParams) {
	if cfg == nil || p.topN <= 0 {
		return
	}
	if p.workers <= 0 {
		p.workers = 1
	}
	if p.retries < 0 {
		p.retries = 0
	}
	if p.backoffMs < 0 {
		p.backoffMs = 0
	}
	if p.maxTimeMs <= 0 {
		p.maxTimeMs = 1000
	}
	if p.verbosity != "queryPlanner" {
		p.verbosity = "executionStats"
	}
	p.efficiencyMode = config.NormalizeExplainEfficiencyMode(p.efficiencyMode)
	effSlots := 0
	if p.efficiencyMode != config.ExplainEfficiencyModeOff {
		effSlots = p.topN
	}

	md := &rep.Metadata
	md.ExplainWorkers = p.workers
	md.ExplainRetries = p.retries
	md.ExplainBackoffMS = p.backoffMs
	md.ExplainVerbosity = p.verbosity
	md.ExplainEfficiencyMode = p.efficiencyMode
	md.ExplainSelectionMethod = stats.ExplainSelectionMethod
	md.ExplainSelectionScope = "per_shape_representative_samples"
	md.ExplainSelectionNote = stats.ExplainSamplingScopeNote()
	md.ExplainTimingNote = stats.ExplainTimingNote
	md.ExplainMaxSamples = (p.topN + effSlots) * stats.ExplainMaxSamplesPerShape

	cov := stats.NewExplainCoverage("ready", "")
	cov.EfficiencyMode = p.efficiencyMode
	cov.SelectionMethod = stats.ExplainSelectionMethod
	cov.SelectionNote = explainSelectionNote
	cov.Budget = stats.ExplainCoverageBudget{
		SeveritySlots: p.topN, EfficiencySlots: effSlots, ImpactSlots: (effSlots + 1) / 2, LatencyTierSlots: effSlots / 2,
		SamplesPerShape: stats.ExplainMaxSamplesPerShape, MaxPlannedSamples: md.ExplainMaxSamples,
		MaxTimeMS: p.maxTimeMs, Workers: p.workers, Retries: p.retries,
	}
	cov.Sampling.CaptureStatus = inv.CaptureStatus
	cov.Sampling.CaptureDisabledReason = inv.CaptureDisabledReason
	cov.Sampling.UntrackedOperations = inv.UntrackedOperations
	if status != "ready" {
		cov.Status, cov.Reason = "not_run", "insights_status_"+status
		rep.ExplainCoverage = cov
		rep.EfficiencyCoverage = stats.NewEfficiencyCoverage(cov.Status, cov.Reason)
		return
	}
	if inv.UntrackedOperations > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"Group limit %d reached: %d operations belong to untracked shapes, which have no samples and cannot be selected for explain.", inv.GroupLimit, inv.UntrackedOperations))
	}
	if inv.UnorderedSortShapes > 0 {
		cov.Limitations = append(cov.Limitations, fmt.Sprintf(
			"%d shapes have a multi-key $sort defined as an unordered map. The key order sent to the server is not defined, so those samples are not explained (sort_key_order_undefined).", inv.UnorderedSortShapes))
	}

	n := len(inv.Shapes)
	cov.Shapes = make([]stats.ExplainCoverageShape, n)
	picksByShape := make([][]samplePick, n)
	tierOf := make([]int, n)
	idxByID := make(map[string]int, n)
	for i, is := range inv.Shapes {
		tier, tierName := stats.ExplainLatencyTier(is.AvgMs)
		tierOf[i] = tier
		idxByID[is.ShapeID] = i
		cs := stats.ExplainCoverageShape{
			ShapeID: is.ShapeID, Namespace: coverageNamespace(is.Database, is.Collection), Collection: is.Collection,
			Operation: is.Operation, ShapeSummary: is.ShapeSummary, ImpactRank: is.ImpactRank,
			OperationCount: is.OperationCount, TotalOperationMs: is.TotalOperationMs, AvgMs: is.AvgMs, MaxMs: is.MaxMs,
			LatencyTier: tierName, BelowSlowThreshold: is.MaxMs < md.SlowThresholdMs, SampleScope: is.SampleScope,
		}
		switch {
		case !supportsExplainOperation(is.Operation):
			cs.Status, cs.Reason = "not_supported", fmt.Sprintf("operation_%s_not_supported", is.Operation)
		case inv.CaptureStatus != "ready":
			cs.Status, cs.Reason = "unavailable", "capture_disabled_"+emptyFallback(inv.CaptureDisabledReason, "unknown")
		case len(is.Samples) == 0:
			cs.Status, cs.Reason = "unavailable", emptyFallback(is.UnavailableReason, "no_representative_samples")
		default:
			picks, sel, reason := selectRepresentativeSamples(is.Samples, p.maxTimeMs, p.verbosity)
			if sel != nil {
				sel.Scope = is.SampleScope
				sel.ScopeNote = stats.ExplainSamplingScopeNote()
				cs.Selection = sel
			}
			if reason != "" {
				cs.Status, cs.Reason = "unavailable", reason
			} else {
				picksByShape[i] = picks
			}
		}
		cov.Shapes[i] = cs
	}

	selected := make([]bool, n)
	order := make([]int, 0, p.topN+effSlots)
	sevSelected := 0
	indexIssueShapes := indexIssueShapeIDs(rep)
	for i := range rep.SlowQueries {
		sq := &rep.SlowQueries[i]
		sq.ExplainDiag, sq.ExplainSelection, sq.ExplainSamples, sq.ExplainFinding = nil, nil, nil, nil
		if !supportsExplainOperation(sq.Operation) {
			sq.ExplainStatus = "not_supported"
			sq.ExplainReason = fmt.Sprintf("operation_%s_not_supported", sq.Operation)
			continue
		}
		if !isExplainCandidatePriorityShape(*sq, indexIssueShapes, md.SlowThresholdMs, p.severityMode) {
			sq.ExplainStatus = "explain_unavailable"
			sq.ExplainReason = "low_value_shape_filtered"
			continue
		}
		if sevSelected >= p.topN {
			sq.ExplainStatus = "explain_unavailable"
			sq.ExplainReason = fmt.Sprintf("not_selected_top_n_%d", p.topN)
			continue
		}
		idx, ok := idxByID[sq.ShapeID]
		if !ok {
			sq.ExplainStatus, sq.ExplainReason = "insufficient_metadata", "shape_not_in_capture_inventory"
			continue
		}
		sq.ExplainSelection = cov.Shapes[idx].Selection
		if picksByShape[idx] == nil {
			sq.ExplainStatus, sq.ExplainReason = "insufficient_metadata", cov.Shapes[idx].Reason
			continue
		}
		sevSelected++
		selected[idx] = true
		cov.Shapes[idx].SelectedBy = "severity"
		order = append(order, idx)
	}

	tiers := len(stats.ExplainLatencyTiers())
	tierCursor := 0
	for slot := 0; slot < effSlots; slot++ {
		pick, by := -1, "impact_rank"
		if slot%2 == 0 {
			for i := 0; i < n && pick < 0; i++ {
				if picksByShape[i] != nil && !selected[i] {
					pick = i
				}
			}
		} else {
			by = "latency_tier"
			for k := 0; k < tiers && pick < 0; k++ {
				t := (tierCursor + k) % tiers
				for i := 0; i < n && pick < 0; i++ {
					if picksByShape[i] != nil && !selected[i] && tierOf[i] == t {
						pick = i
						tierCursor = t + 1
					}
				}
			}
		}
		if pick < 0 {
			break
		}
		selected[pick] = true
		cov.Shapes[pick].SelectedBy = by
		order = append(order, pick)
	}

	excludedReason := "not_selected_budget"
	if effSlots == 0 {
		excludedReason = "not_selected_efficiency_mode_off"
	}
	for i := range cov.Shapes {
		switch {
		case selected[i]:
			cov.Shapes[i].Status, cov.Shapes[i].Reason = "pending", "explain_pending"
		case picksByShape[i] != nil:
			cov.Shapes[i].Status, cov.Shapes[i].Reason = "excluded", excludedReason
		}
	}

	jobs := make([]explainJob, 0, len(order)*stats.ExplainMaxSamplesPerShape)
	jobByKey := make(map[string]int)
	refs := make([]sampleRef, 0, cap(jobs))
	for _, idx := range order {
		cs := &cov.Shapes[idx]
		for _, pk := range picksByShape[idx] {
			key := explainDedupeKey(pk.sample, p.maxTimeMs, p.verbosity)
			jobIdx, shared := jobByKey[key]
			if !shared {
				jobIdx = len(jobs)
				jobByKey[key] = jobIdx
				jobs = append(jobs, explainJob{sample: pk.sample})
			}
			cs.Samples = append(cs.Samples, stats.ExplainSample{
				Role:            pk.role,
				SharedCommand:   shared,
				TargetLatencyMs: round2(pk.target),
				ClientLatencyMs: round2(pk.sample.DurationMs),
				Status:          "explain_unavailable",
				Reason:          "explain_pending",
			})
			refs = append(refs, sampleRef{shape: idx, sample: len(cs.Samples) - 1, job: jobIdx})
		}
	}

	results := runExplainJobsFn(ctx, jobs, p.workers, cfg, p.maxTimeMs, p.verbosity, p.retries, p.backoffMs)
	commands := 0
	for _, r := range results {
		commands += r.attempts
	}
	md.ExplainSamplesPlanned = len(refs)
	md.ExplainCommandsRun = commands
	cov.SamplesPlanned = len(refs)
	cov.CommandsDeduplicated = len(refs) - len(jobs)
	cov.CommandsRun = commands
	for _, ref := range refs {
		ev := results[ref.job]
		s := &cov.Shapes[ref.shape].Samples[ref.sample]
		s.Status = ev.status
		s.Reason = ev.reason
		s.Attempts = ev.attempts
		s.Diagnostics = ev.diag
		if cov.Shapes[ref.shape].Predicates == nil && jobs[ref.job].sample.Query != nil {
			p := stats.ProfileQuery(jobs[ref.job].sample.Query)
			cov.Shapes[ref.shape].Predicates = &p
		}
	}

	for i := range rep.SlowQueries {
		sq := &rep.SlowQueries[i]
		idx, ok := idxByID[sq.ShapeID]
		if !ok || !selected[idx] {
			continue
		}
		cs := cov.Shapes[idx]
		sq.ExplainSelection = cs.Selection
		sq.ExplainSamples = append([]stats.ExplainSample(nil), cs.Samples...)
	}

	inSlow := make(map[string]struct{}, len(rep.SlowQueries))
	for _, sq := range rep.SlowQueries {
		inSlow[sq.ShapeID] = struct{}{}
	}
	for i := range rep.QueryShapes {
		qs := &rep.QueryShapes[i]
		if _, ok := inSlow[qs.ShapeID]; ok {
			continue
		}
		if supportsExplainOperation(qs.Operation) {
			qs.ExplainStatus = "explain_unavailable"
			qs.ExplainReason = "shape_not_in_explain_results"
		} else {
			qs.ExplainStatus = "not_supported"
			qs.ExplainReason = fmt.Sprintf("operation_%s_not_supported", qs.Operation)
		}
	}
	rep.ExplainCoverage = cov
	invView := listNamespaceIndexesFn(ctx, cfg, explainedNamespaces(cov))
	rep.IndexInventory = &invView
	stats.ApplyExplainFindings(rep)
}

func explainedNamespaces(cov stats.ExplainCoverageView) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, s := range cov.Shapes {
		if strings.TrimSpace(s.Namespace) == "" {
			continue
		}
		explained := false
		for _, sm := range s.Samples {
			if sm.Status == "explained" {
				explained = true
				break
			}
		}
		if !explained {
			continue
		}
		if _, ok := seen[s.Namespace]; ok {
			continue
		}
		seen[s.Namespace] = struct{}{}
		out = append(out, s.Namespace)
	}
	return out
}

func indexIssueShapeIDs(rep *stats.InsightsReport) map[string]struct{} {
	out := make(map[string]struct{}, len(rep.PotentialIndexIssues))
	for _, issue := range rep.PotentialIndexIssues {
		out[issue.ShapeID] = struct{}{}
	}
	return out
}

func emptyFallback(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// selectRepresentativeSamples picks the representative samples with an
// executable query nearest the shape's sample median and p95 latency. The
// percentiles are computed over all of the shape's representative samples.
func selectRepresentativeSamples(samples []stats.RepresentativeSample, maxTimeMs int, verbosity string) ([]samplePick, *stats.ExplainSelection, string) {
	if len(samples) == 0 {
		return nil, nil, "no_representative_samples"
	}
	latencies := make([]float64, 0, len(samples))
	executable := make([]stats.RepresentativeSample, 0, len(samples))
	firstReason := ""
	for _, smp := range samples {
		latencies = append(latencies, smp.DurationMs)
		if _, reason := buildExplainCommandSpec(smp, maxTimeMs, verbosity); reason != "" {
			if firstReason == "" {
				firstReason = reason
			}
			continue
		}
		executable = append(executable, smp)
	}
	median := nearestRankPercentile(latencies, 50)
	p95 := nearestRankPercentile(latencies, 95)
	sel := &stats.ExplainSelection{
		Method:                    stats.ExplainSelectionMethod,
		RetainedSuccessfulSamples: len(latencies),
		ExecutableSamples:         len(executable),
		MedianMs:                  round2(median),
		P95Ms:                     round2(p95),
	}
	if len(executable) == 0 {
		return nil, sel, firstReason
	}

	picks := make([]samplePick, 0, stats.ExplainMaxSamplesPerShape)
	pickedIdx := make([]int, 0, stats.ExplainMaxSamplesPerShape)
	pickedKey := make([]string, 0, stats.ExplainMaxSamplesPerShape)
	for _, t := range []struct {
		role   string
		target float64
	}{{"median", median}, {"p95", p95}} {
		best := 0
		for i := 1; i < len(executable); i++ {
			if math.Abs(executable[i].DurationMs-t.target) < math.Abs(executable[best].DurationMs-t.target) {
				best = i
			}
		}
		key := explainDedupeKey(executable[best], maxTimeMs, verbosity)
		merged := false
		for j := range picks {
			if pickedIdx[j] == best || pickedKey[j] == key {
				picks[j].role += "+" + t.role
				merged = true
				break
			}
		}
		if merged {
			continue
		}
		picks = append(picks, samplePick{role: t.role, target: t.target, sample: executable[best]})
		pickedIdx = append(pickedIdx, best)
		pickedKey = append(pickedKey, key)
	}
	return picks, sel, ""
}

// nearestRankPercentile matches the percentile definition used by insights.
func nearestRankPercentile(values []float64, p float64) float64 {
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

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// explainDedupeKey identifies identical explain commands so they run once.
// Map keys are sorted because Go map order is random; ordered documents keep
// their order.
func explainDedupeKey(smp stats.RepresentativeSample, maxTimeMs int, verbosity string) string {
	spec, reason := buildExplainCommandSpec(smp, maxTimeMs, verbosity)
	if reason != "" {
		return "invalid|" + reason
	}
	b, err := bson.MarshalExtJSON(canonicalCommandValue(spec.cmd), true, false)
	if err != nil {
		return fmt.Sprintf("unmarshalable|%p", &smp)
	}
	return spec.db + "|" + string(b)
}

func canonicalCommandValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(bson.D, 0, len(keys))
		for _, k := range keys {
			out = append(out, bson.E{Key: k, Value: canonicalCommandValue(t[k])})
		}
		return out
	case bson.M:
		return canonicalCommandValue(map[string]interface{}(t))
	case bson.D:
		out := make(bson.D, len(t))
		for i, e := range t {
			out[i] = bson.E{Key: e.Key, Value: canonicalCommandValue(e.Value)}
		}
		return out
	case []interface{}:
		out := make(bson.A, len(t))
		for i, x := range t {
			out[i] = canonicalCommandValue(x)
		}
		return out
	case bson.A:
		return canonicalCommandValue([]interface{}(t))
	default:
		return v
	}
}

// runExplainJobsConcurrently runs each job once on at most workers goroutines.
// After ctx is cancelled no further commands are sent and the remaining jobs
// are reported as cancelled.
func runExplainJobsConcurrently(ctx context.Context, jobs []explainJob, workers int, cfg *config.AppConfig, maxTimeMs int, explainVerbosity string, retries int, backoffMs int) []explainEvidence {
	results := make([]explainEvidence, len(jobs))
	if len(jobs) == 0 {
		return results
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conns := make(map[string]*db.Connection)
			defer func() {
				for _, c := range conns {
					disconnectFn(c, context.Background())
				}
			}()
			for idx := range work {
				if ctx.Err() != nil {
					results[idx] = cancelledEvidence(0)
					continue
				}
				smp := jobs[idx].sample
				dbName := smp.Query.Database
				conn, ok := conns[dbName]
				if !ok {
					ctxConn, cancel := context.WithTimeout(ctx, 5*time.Second)
					newConn, err := connectFn(ctxConn, cfg, dbName)
					cancel()
					if err != nil {
						if ctx.Err() != nil {
							results[idx] = cancelledEvidence(0)
						} else {
							results[idx] = explainEvidence{status: "execution_failed", reason: "connect_failed"}
						}
						continue
					}
					conn = newConn
					conns[dbName] = conn
				}
				results[idx] = runExplainForEventWithRetry(ctx, conn.Database, smp, maxTimeMs, explainVerbosity, retries, backoffMs)
			}
		}()
	}
	dispatched := 0
dispatch:
	for i := range jobs {
		select {
		case <-ctx.Done():
			break dispatch
		case work <- i:
			dispatched++
		}
	}
	close(work)
	wg.Wait()
	for i := dispatched; i < len(jobs); i++ {
		results[i] = cancelledEvidence(0)
	}
	return results
}

func runExplainForEventWithRetry(ctx context.Context, database *mongodrv.Database, smp stats.RepresentativeSample, baseMaxTimeMs int, explainVerbosity string, retries int, backoffMs int) explainEvidence {
	return retryExplainEvidence(ctx, baseMaxTimeMs, retries, backoffMs, func(maxTimeMs int) explainEvidence {
		return runExplainForEvent(ctx, database, smp, maxTimeMs, explainVerbosity)
	})
}

// retryExplainEvidence retries only timeouts, doubling the server time budget
// up to 60s. attempts counts every command sent; cancellation stops retries.
func retryExplainEvidence(ctx context.Context, baseMaxTimeMs int, retries int, backoffMs int, run func(maxTimeMs int) explainEvidence) explainEvidence {
	if baseMaxTimeMs <= 0 {
		baseMaxTimeMs = 1000
	}
	if retries < 0 {
		retries = 0
	}
	maxTime := baseMaxTimeMs
	var last explainEvidence
	for attempt := 0; attempt <= retries; attempt++ {
		if ctx.Err() != nil {
			return cancelledEvidence(attempt)
		}
		last = run(maxTime)
		last.attempts = attempt + 1
		if last.status != "timed_out" || attempt == retries {
			return last
		}
		if backoffMs > 0 {
			select {
			case <-ctx.Done():
				return cancelledEvidence(attempt + 1)
			case <-time.After(time.Duration(backoffMs*(attempt+1)) * time.Millisecond):
			}
		}
		if maxTime < 60000 {
			maxTime *= 2
			if maxTime > 60000 {
				maxTime = 60000
			}
		}
	}
	return last
}

func runExplainForEvent(ctx context.Context, database *mongodrv.Database, smp stats.RepresentativeSample, maxTimeMs int, explainVerbosity string) explainEvidence {
	spec, reason := buildExplainCommandSpec(smp, maxTimeMs, explainVerbosity)
	if reason != "" {
		if reason == "unsupported_operation" {
			return explainEvidence{status: "not_supported", reason: reason}
		}
		return explainEvidence{status: "insufficient_metadata", reason: reason}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, spec.clientTimeout)
	defer cancel()
	start := time.Now()
	diag := &stats.ExplainDiagnostics{
		ReplayDB:               spec.db,
		ReplayCollection:       smp.Collection,
		Verbosity:              spec.verbosity,
		VerbosityNote:          spec.verbosityNote,
		ServerMaxTimeMS:        spec.serverMaxTime,
		ClientTimeoutMS:        int(spec.clientTimeout / time.Millisecond),
		StageSummary:           spec.stageSummary,
		CommandSummary:         spec.commandSummary,
		UnfilteredScanExpected: spec.unfiltered,
	}
	logResult := func(result string) {
		log.Printf("[Insights Explain] op=%s db=%s coll=%s shape=%s mode=%s server_max_ms=%d client_timeout_ms=%d command=%q result=%s elapsed_ms=%d",
			smp.Operation, spec.db, smp.Collection, smp.ShapeID,
			spec.verbosity, spec.serverMaxTime, diag.ClientTimeoutMS, spec.commandSummary, result, diag.ElapsedMS)
	}

	var out bson.M
	if err := database.Client().Database(spec.db).RunCommand(cmdCtx, spec.cmd, options.RunCmd()).Decode(&out); err != nil {
		diag.ElapsedMS = int(time.Since(start) / time.Millisecond)
		if ctx.Err() != nil {
			logResult("explain_cancelled")
			ev := cancelledEvidence(0)
			ev.diag = diag
			return ev
		}
		reason := classifyExplainCommandError(err)
		logResult(reason)
		if reason == "max_time_exceeded" || reason == "client_deadline_exceeded" {
			return explainEvidence{status: "timed_out", reason: reason, diag: diag}
		}
		return explainEvidence{status: "execution_failed", reason: reason, diag: diag}
	}
	diag.ElapsedMS = int(time.Since(start) / time.Millisecond)
	diag.ServerVersion = explainServerVersion(out)
	applyExplainSignalsToDiagnostics(diag, parseExplainSignals(out))
	applyExplainMetricsToDiagnostics(diag, normalizeExplainMetrics(out, smp.Operation), smp.Operation)
	stats.DescribeExplainSample(diag, smp.Operation)
	logResult("explained")
	return explainEvidence{status: "explained", reason: diag.Diagnosis, diag: diag}
}

// buildExplainCommandSpec wraps the captured query in an explain command with
// the same semantics the worker used. Writes are explained through their own
// update/delete commands; explain never applies the modifications.
func buildExplainCommandSpec(ev stats.RepresentativeSample, maxTimeMs int, explainVerbosity string) (explainCommandSpec, string) {
	if maxTimeMs <= 0 {
		maxTimeMs = 1000
	}
	if explainVerbosity != "queryPlanner" {
		explainVerbosity = "executionStats"
	}
	if !supportsExplainOperation(ev.Operation) {
		return explainCommandSpec{}, "unsupported_operation"
	}
	q := ev.Query
	if q == nil {
		return explainCommandSpec{}, "executable_query_not_captured"
	}
	if strings.TrimSpace(q.Database) == "" {
		return explainCommandSpec{}, "database_not_captured"
	}
	if strings.TrimSpace(ev.Collection) == "" {
		return explainCommandSpec{}, "collection_not_captured"
	}
	spec := explainCommandSpec{
		db:            q.Database,
		verbosity:     explainVerbosity,
		serverMaxTime: maxTimeMs,
		clientTimeout: time.Duration(maxTimeMs+2000) * time.Millisecond,
		commandType:   ev.Operation,
	}
	if spec.clientTimeout < 3*time.Second {
		spec.clientTimeout = 3 * time.Second
	}

	var filter interface{} = bson.M{}
	if q.Filter != nil {
		filter = q.Filter
	}
	filterSummary := summarizeFilterForLog(q.Filter)

	var inner bson.D
	switch ev.Operation {
	case "find":
		inner = bson.D{{Key: "find", Value: ev.Collection}, {Key: "filter", Value: filter}}
		parts := []string{"find", "filter" + filterSummary}
		if len(q.Projection) > 0 {
			inner = append(inner, bson.E{Key: "projection", Value: q.Projection})
			parts = append(parts, "projection"+summarizeFilterForLog(q.Projection))
		}
		if q.Limit != 0 {
			inner = append(inner, bson.E{Key: "limit", Value: q.Limit})
			parts = append(parts, fmt.Sprintf("limit=%d", q.Limit))
		}
		spec.commandSummary = strings.Join(parts, " ")
		spec.stageSummary = filterSummary
		spec.unfiltered = len(q.Filter) == 0
	case "updateOne", "updateMany":
		if len(q.Update) == 0 {
			return explainCommandSpec{}, "update_document_not_captured"
		}
		multi := ev.Operation == "updateMany"
		stmt := bson.D{
			{Key: "q", Value: filter},
			{Key: "u", Value: q.Update},
			{Key: "multi", Value: multi},
			{Key: "upsert", Value: q.Upsert},
		}
		inner = bson.D{{Key: "update", Value: ev.Collection}, {Key: "updates", Value: bson.A{stmt}}}
		spec.commandSummary = fmt.Sprintf("update q%s u%s multi=%t upsert=%t", filterSummary, summarizeFilterForLog(q.Update), multi, q.Upsert)
		spec.stageSummary = filterSummary
		spec.unfiltered = len(q.Filter) == 0
	case "deleteOne", "deleteMany":
		limit := 0
		if ev.Operation == "deleteOne" {
			limit = 1
		}
		inner = bson.D{{Key: "delete", Value: ev.Collection}, {Key: "deletes", Value: bson.A{bson.D{{Key: "q", Value: filter}, {Key: "limit", Value: limit}}}}}
		spec.commandSummary = fmt.Sprintf("delete q%s limit=%d", filterSummary, limit)
		spec.stageSummary = filterSummary
		spec.unfiltered = len(q.Filter) == 0
	case "aggregate":
		pipeline := q.Pipeline
		if pipeline == nil {
			pipeline = []interface{}{}
		}
		if stats.HasUnorderedSortSpec(pipeline) {
			return explainCommandSpec{}, "sort_key_order_undefined"
		}
		if pipelineWritesOutput(pipeline) && spec.verbosity != "queryPlanner" {
			spec.verbosity = "queryPlanner"
			spec.verbosityNote = "pipeline_has_out_or_merge_explained_with_queryPlanner"
		}
		inner = bson.D{{Key: "aggregate", Value: ev.Collection}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.M{}}}
		spec.stageSummary = summarizePipelineForLog(pipeline)
		spec.commandSummary = "aggregate " + spec.stageSummary
		spec.unfiltered = !pipelineStartsWithMatch(pipeline)
	}
	inner = append(inner, bson.E{Key: "maxTimeMS", Value: maxTimeMs})
	spec.cmd = bson.D{{Key: "explain", Value: inner}, {Key: "verbosity", Value: spec.verbosity}}
	return spec, ""
}

func pipelineStageNames(stage interface{}) []string {
	switch t := stage.(type) {
	case map[string]interface{}:
		out := make([]string, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	case bson.M:
		return pipelineStageNames(map[string]interface{}(t))
	case bson.D:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, e.Key)
		}
		return out
	}
	return nil
}

func pipelineWritesOutput(pipeline []interface{}) bool {
	for _, st := range pipeline {
		for _, name := range pipelineStageNames(st) {
			if name == "$out" || name == "$merge" {
				return true
			}
		}
	}
	return false
}

func pipelineStartsWithMatch(pipeline []interface{}) bool {
	if len(pipeline) == 0 {
		return false
	}
	names := pipelineStageNames(pipeline[0])
	return len(names) == 1 && names[0] == "$match"
}

// explainServerVersion reads serverInfo.version from the explain output, or
// from the first shard's serverInfo when the router omits it.
func explainServerVersion(out bson.M) string {
	version := func(v interface{}) string {
		if m, ok := explainDoc(v); ok {
			if s, ok := m["version"].(string); ok {
				return s
			}
		}
		return ""
	}
	if v := version(out["serverInfo"]); v != "" {
		return v
	}
	qp, _ := explainDoc(out["queryPlanner"])
	wp, _ := explainDoc(qp["winningPlan"])
	shards, _ := explainArray(wp["shards"])
	for _, sh := range shards {
		if m, ok := explainDoc(sh); ok {
			if v := version(m["serverInfo"]); v != "" {
				return v
			}
		}
	}
	return ""
}

func classifyExplainCommandError(err error) string {
	if err == nil {
		return "run_command_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "client_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "client_context_canceled"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not authorized"):
		return "not_authorized"
	case strings.Contains(msg, "ns does not exist"), strings.Contains(msg, "namespace does not exist"):
		return "namespace_not_found"
	case strings.Contains(msg, "command not found"), strings.Contains(msg, "commandnotfound"):
		return "command_not_found"
	case strings.Contains(msg, "max time ms expired"), strings.Contains(msg, "maxtimems"):
		return "max_time_exceeded"
	default:
		return "run_command_failed"
	}
}

func supportsExplainOperation(op string) bool {
	switch op {
	case "find", "aggregate", "updateOne", "updateMany", "deleteOne", "deleteMany":
		return true
	default:
		return false
	}
}

func isExplainCandidatePriorityShape(sq stats.SlowQueryInsight, indexIssueShapes map[string]struct{}, slowThresholdMs float64, explainSeverityMode string) bool {
	switch explainSeverityMode {
	case "high_and_low":
	case "medium_only":
		switch sq.Severity {
		case "critical", "high", "medium":
		default:
			return false
		}
	case "critical_only":
		if sq.Severity != "critical" {
			return false
		}
	default: // high_only
		switch sq.Severity {
		case "critical", "high":
		default:
			return false
		}
	}
	if _, ok := indexIssueShapes[sq.ShapeID]; ok {
		return true
	}
	if sq.P95Ms >= slowThresholdMs || sq.P99Ms >= slowThresholdMs {
		return true
	}
	switch sq.Severity {
	case "critical", "high":
		return true
	default:
		return false
	}
}

func summarizePipelineForLog(pipeline []interface{}) string {
	if len(pipeline) == 0 {
		return "[]"
	}
	stages := make([]string, 0, len(pipeline))
	for _, raw := range pipeline {
		names := pipelineStageNames(raw)
		if len(names) == 0 {
			continue
		}
		stages = append(stages, strings.Join(names, "+"))
	}
	if len(stages) == 0 {
		return "unknown_stages"
	}
	return strings.Join(stages, " -> ")
}

func summarizeFilterForLog(filter map[string]interface{}) string {
	if len(filter) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "{" + strings.Join(keys, ",") + "}"
}

// parseExplainSignals reads winning-plan features. Rejected plans are skipped
// so their stages cannot leak into the diagnosis, and $-prefixed keys count as
// stages only inside an aggregate "stages" array. Examined/returned counters
// come from normalizeExplainMetrics.
func parseExplainSignals(out bson.M) explainSignals {
	s := explainSignals{}
	stageSeen := make(map[string]struct{})
	indexSeen := make(map[string]struct{})
	planSort := false

	addStage := func(stage string) {
		stage = strings.ToUpper(strings.TrimSpace(stage))
		if stage == "" {
			return
		}
		if _, ok := stageSeen[stage]; ok {
			return
		}
		stageSeen[stage] = struct{}{}
		s.planStages = append(s.planStages, stage)
	}
	addIndex := func(idx string) {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			return
		}
		if _, ok := indexSeen[idx]; ok {
			return
		}
		indexSeen[idx] = struct{}{}
		s.indexesUsed = append(s.indexesUsed, idx)
	}
	updateMax := func(dst *int64, v int64) {
		if v > *dst {
			*dst = v
		}
	}
	processKey := func(k string, val interface{}, pipelineStage bool) {
		if pipelineStage && strings.HasPrefix(k, "$") && len(k) > 1 {
			addStage(strings.TrimPrefix(k, "$"))
		}
		switch strings.ToLower(k) {
		case "stage", "stagename", "nodetype":
			if sv, ok := val.(string); ok {
				addStage(sv)
				if strings.EqualFold(sv, "SORT") {
					planSort = true
				}
			}
		case "indexname":
			if sv, ok := val.(string); ok {
				addIndex(sv)
			}
		case "indexesused":
			for _, iv := range toStringSlice(val) {
				addIndex(iv)
			}
		case "executionstats":
			s.executionStats = true
		case "collectionscans":
			updateMax(&s.collectionScans, toInt64(val))
		case "seeks", "indexseeks", "totalseeks":
			updateMax(&s.indexSeeks, toInt64(val))
		case "executiontimemillis", "executiontimemillisestimate":
			updateMax(&s.executionTimeMS, toInt64(val))
		case "useddisk":
			if bv, ok := val.(bool); ok && bv {
				s.usedDisk = true
			}
		case "spills":
			sp := toInt64(val)
			if sp > 0 {
				updateMax(&s.spills, sp)
				s.usedDisk = true
			}
		}
	}
	skip := func(k string) bool {
		switch strings.ToLower(k) {
		case "rejectedplans", "allplansexecution", "command", "parsedquery", "originalcommand":
			return true
		}
		return false
	}

	var walk func(v interface{}, inStages bool)
	visit := func(k string, val interface{}, inStages bool) {
		if skip(k) {
			return
		}
		processKey(k, val, inStages)
		walk(val, strings.EqualFold(k, "stages"))
	}
	walk = func(v interface{}, inStages bool) {
		switch t := v.(type) {
		case bson.M:
			walk(map[string]interface{}(t), inStages)
		case map[string]interface{}:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				visit(k, t[k], inStages)
			}
		case bson.D:
			for _, elem := range t {
				visit(elem.Key, elem.Value, inStages)
			}
		case []interface{}:
			for i := range t {
				walk(t[i], inStages)
			}
		case bson.A:
			for i := range t {
				walk(t[i], inStages)
			}
		}
	}
	walk(out, false)

	s.collectionScan = hasPlanStage(s.planStages, "COLLSCAN") || s.collectionScans > 0
	s.indexScan = hasAnyPlanStage(s.planStages, "IXSCAN", "IXSEEK", "DISTINCT_SCAN", "COUNT_SCAN", "EXPRESS_IXSCAN", "IDHACK", "EXPRESS_UPDATE", "EXPRESS_DELETE") || len(s.indexesUsed) > 0 || s.indexSeeks > 0
	s.fetch = hasPlanStage(s.planStages, "FETCH")
	s.group = hasPlanStage(s.planStages, "GROUP")
	s.sort = hasPlanStage(s.planStages, "SORT")
	s.limit = hasPlanStage(s.planStages, "LIMIT")
	s.blockingSort = planSort

	s.winningPlanSummary = strings.Join(s.planStages, " -> ")
	if s.winningPlanSummary == "" {
		s.winningPlanSummary = "stage_chain_unavailable"
	}
	s.shardSummary = buildShardSummary(out)
	return s
}

func applyExplainSignalsToDiagnostics(diag *stats.ExplainDiagnostics, sig explainSignals) {
	if diag == nil {
		return
	}
	diag.WinningPlanSummary = sig.winningPlanSummary
	diag.PlanStages = sig.planStages
	diag.IndexesUsed = sig.indexesUsed
	diag.CollectionScanDetected = sig.collectionScan
	diag.IndexScanDetected = sig.indexScan
	diag.FetchDetected = sig.fetch
	diag.GroupDetected = sig.group
	diag.SortDetected = sig.sort
	diag.LimitDetected = sig.limit
	diag.BlockingSortDetected = sig.blockingSort
	diag.UsedDisk = sig.usedDisk
	diag.ExecutionStatsAvailable = sig.executionStats
	diag.CollectionScans = sig.collectionScans
	diag.IndexSeeks = sig.indexSeeks
	diag.ExecutionTimeMillis = sig.executionTimeMS
	diag.Spills = sig.spills
	diag.ShardDetailsSummary = sig.shardSummary
}

// applyExplainMetricsToDiagnostics attaches normalized counters and fills the
// flat counters, which read unreported values as 0.
func applyExplainMetricsToDiagnostics(diag *stats.ExplainDiagnostics, m stats.ExplainMetrics, op string) {
	if diag == nil {
		return
	}
	diag.Metrics = &m
	value := func(c *stats.ExplainCounter) int64 {
		if c == nil {
			return 0
		}
		return c.Value
	}
	diag.DocsExamined = value(m.DocsExamined)
	diag.KeysExamined = value(m.KeysExamined)
	diag.NReturned = value(m.Returned)
	diag.NMatched = value(m.Matched)
	diag.ExaminedToReturnedRatio, diag.KeysToReturnedRatio = 0, 0
	if r := stats.ExplainResultCount(diag, op); r > 0 {
		diag.ExaminedToReturnedRatio = float64(diag.DocsExamined) / float64(r)
		diag.KeysToReturnedRatio = float64(diag.KeysExamined) / float64(r)
	}
}

func hasPlanStage(stages []string, target string) bool {
	target = strings.ToUpper(strings.TrimSpace(target))
	for _, s := range stages {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

func hasAnyPlanStage(stages []string, targets ...string) bool {
	for _, target := range targets {
		if hasPlanStage(stages, target) {
			return true
		}
	}
	return false
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case float32:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	default:
		return 0
	}
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case bson.A:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func buildShardSummary(out bson.M) string {
	shardsRaw, ok := out["shards"]
	if !ok {
		return ""
	}
	var shardsMap map[string]interface{}
	switch t := shardsRaw.(type) {
	case map[string]interface{}:
		shardsMap = t
	case bson.M:
		shardsMap = map[string]interface{}(t)
	default:
		return ""
	}
	if len(shardsMap) == 0 {
		return ""
	}
	names := make([]string, 0, len(shardsMap))
	for name := range shardsMap {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		doc, ok := explainDoc(shardsMap[name])
		if !ok {
			continue
		}
		m := normalizeNodeExplain(doc, "aggregate", "")
		parts = append(parts, fmt.Sprintf("%s docs=%s keys=%s access_returned=%s", name,
			counterText(m.DocsExamined), counterText(m.KeysExamined), counterText(m.AccessReturned)))
	}
	return strings.Join(parts, "; ")
}

func counterText(c *stats.ExplainCounter) string {
	if c == nil {
		return "n/a"
	}
	return fmt.Sprintf("%d", c.Value)
}
