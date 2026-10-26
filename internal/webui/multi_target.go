package webui

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/mongo"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/multitarget"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/replaycompare"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/report"
	mongodrv "go.mongodb.org/mongo-driver/v2/mongo"
)

// multiTargetStatus is the multi_target section of /api/stats: the live
// state of every target (labels, never URIs) and, once finished, the
// comparisons with the first target.
func multiTargetStatus(plan *multitarget.Plan) map[string]interface{} {
	if plan == nil {
		return nil
	}
	states := plan.States()
	for i := range states {
		states[i].OutcomeFile = absPath(states[i].OutcomeFile)
	}
	out := map[string]interface{}{
		"kind": plan.Kind, "mode": plan.Mode, "reference": plan.Targets[0].Label, "targets": states,
	}
	if res := plan.Result(); res != nil {
		out["comparisons"] = res.Comparisons
		out["notes"] = res.Notes
		if res.FingerprintsMatch != nil {
			out["fingerprints_match"] = *res.FingerprintsMatch
		}
	}
	return out
}

// multiTargetReportItems lists the targets for the run report.
func multiTargetReportItems(plan *multitarget.Plan) ([]report.KV, []string) {
	if plan == nil {
		return nil, nil
	}
	states := plan.States()
	items := make([]report.KV, 0, len(states)+1)
	items = append(items, report.KV{Key: "Mode", Value: fmt.Sprintf("%s %s on %d targets; each compared with %s", plan.Mode, plan.Kind, len(states), states[0].Label)})
	for _, st := range states {
		v := fmt.Sprintf("%s; %d operations", st.Status, st.OpsDone)
		if st.OpsTotal > 0 {
			v = fmt.Sprintf("%s; %d of %d operations", st.Status, st.OpsDone, st.OpsTotal)
		}
		if st.OpsSkipped > 0 {
			v += fmt.Sprintf(", %d writes skipped", st.OpsSkipped)
		}
		v += fmt.Sprintf("; %.1f s", st.ElapsedSec)
		if st.LagMaxMs != nil {
			v += fmt.Sprintf("; client-side lag max %.1f ms, avg %.1f ms", *st.LagMaxMs, *st.LagAvgMs)
		}
		if st.DataFingerprintMatch != nil {
			v += fmt.Sprintf("; starting data matches the recording: %t", *st.DataFingerprintMatch)
		}
		if st.Error != "" {
			v += "; error: " + st.Error
		}
		items = append(items, report.KV{Key: fmt.Sprintf("%s (target %d of %d)", st.Label, st.Index, len(states)), Value: v})
	}
	var notes []string
	if res := plan.Result(); res != nil {
		for _, c := range res.Comparisons {
			v := fmt.Sprintf("%d differing operations; latency %s (heuristic)", c.DifferingOps, c.Verdict)
			if c.Error != "" {
				v = "not compared: " + c.Error
			}
			items = append(items, report.KV{Key: c.Reference + " vs " + c.Target, Value: v})
		}
		notes = res.Notes
	} else if plan.Mode == config.WorkloadTargetsConcurrent {
		notes = []string{multitarget.ConcurrentNote}
	}
	return items, notes
}

// startMultiTarget runs a plan over workload_replay_targets. Each target
// gets its own client, collector, outcome file, insights (explain runs on
// that target), and trend run; the comparisons with the first target are
// kept in memory like the Replay Compare reports. The dashboard follows the
// running target (sequential) or the first target (concurrent).
func (s *WebServer) startMultiTarget(w http.ResponseWriter, ctx context.Context, cancel context.CancelFunc, cfg *config.AppConfig, collectionsCfg *config.CollectionsFile, queriesCfg *config.QueriesFile, dbName, replayUpload string, generated []*generatedFile) {
	plan, err := multitarget.NewPlan(cfg, dbName, collectionsCfg.Collections, queriesCfg.Queries)
	if err != nil {
		if replayUpload != "" {
			_ = os.Remove(replayUpload)
		}
		removeEmptyFiles(generatedPaths(generated))
		s.abortRun(err.Error())
		cancel()
		writeStartError(w, http.StatusBadRequest, err.Error())
		return
	}
	concurrent := plan.Mode == config.WorkloadTargetsConcurrent
	plan.Deps.Connect = func(ctx context.Context, tcfg *config.AppConfig, dbName string) (*mongodrv.Database, func(), error) {
		conn, err := connectFn(ctx, tcfg, dbName)
		if err != nil {
			return nil, nil, err
		}
		return conn.Database, func() { disconnectFn(conn, context.Background()) }, nil
	}
	plan.Deps.Prepare = func(ctx context.Context, t *multitarget.Target, database *mongodrv.Database) error {
		s.mu.Lock()
		s.setLifecycleStepLocked(fmt.Sprintf("Preparing target %s", t.Label), t.Index, len(plan.Targets))
		s.mu.Unlock()
		file := &config.CollectionsFile{Collections: t.Collections}
		if err := createCollectionsFn(ctx, database, file, t.Config.DropCollections); err != nil {
			return fmt.Errorf("create collections: %w", err)
		}
		if err := createIndexesFn(ctx, database, file); err != nil {
			return fmt.Errorf("create indexes: %w", err)
		}
		if !t.Config.SkipSeed && t.Config.DocumentsCount > 0 {
			for _, col := range t.Collections {
				if err := insertRandomDocsFn(ctx, database, col, t.Config.DocumentsCount, t.Config); err != nil {
					return fmt.Errorf("seed %s: %w", col.Name, err)
				}
			}
		}
		return nil
	}
	s.mu.Lock()
	s.useTrendStoreLocked(cfg)
	runID := s.RunID
	trendStore := s.TrendStore
	s.mu.Unlock()
	plan.Deps.Run = func(ctx context.Context, t *multitarget.Target, database *mongodrv.Database) error {
		iterations := config.RunIterations(t.Config)
		interval, _ := time.ParseDuration(t.Config.IntervalDelay)
		runCtx, finish := mongo.WithWorkloadRecordingSession(ctx)
		var runErr error
		for i := 1; i <= iterations && ctx.Err() == nil; i++ {
			s.mu.Lock()
			if s.CurrentStats == t.Collector {
				s.CurrentIteration = i
				s.IsWaiting = false
			}
			t.Collector.CurrentIteration = i
			s.mu.Unlock()
			if err := runWorkloadFn(runCtx, database, t.Collections, t.Queries, t.Config, t.Collector); err != nil {
				if err != context.Canceled {
					runErr = err
				}
				break
			}
			if i < iterations && interval > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(interval):
				}
			}
		}
		finish()
		// Insights (with explain against this target) and the trend run are
		// built while the client is open; the reference's report becomes the
		// Insights tab of the run.
		now := trendClock()
		t.TrendRunID = newTrendRunID(now, runID)
		rep, run, policy := s.buildFinalInsights(t.Collector, t.Config, trendStore, t.TrendRunID, now)
		recordTrendRun(trendStore, rep, run, policy)
		if t.Index == 1 {
			s.mu.Lock()
			s.multiTargetRefInsights = &rep
			s.mu.Unlock()
		}
		return runErr
	}
	plan.Deps.CompareOptions = func(ref, t *multitarget.Target, opts *replaycompare.Options) {
		if trendStore == nil {
			return
		}
		d := trendStore.Snapshot()
		opts.PlansA, _ = replaycompare.PlansFromTrendRun(d, ref.TrendRunID)
		opts.PlansB, _ = replaycompare.PlansFromTrendRun(d, t.TrendRunID)
	}
	plan.Deps.OnChange = func(t *multitarget.Target) {
		st := t.State()
		s.mu.Lock()
		defer s.mu.Unlock()
		if st.Status == multitarget.StatusRunning && !concurrent {
			s.CurrentStats = t.Collector
		}
		msg := fmt.Sprintf("Target %s: %s", st.Label, st.Status)
		if st.Error != "" {
			msg += " (" + st.Error + ")"
		}
		s.addLifecycleEventLocked("target", msg)
	}

	s.mu.Lock()
	s.multiTarget = plan
	s.multiTargetRefInsights = nil
	s.CurrentStats = plan.Targets[0].Collector
	s.AppConfig = cfg
	s.ActiveCancel = cancel
	s.LastError = ""
	s.CurrentIteration = 1
	s.TotalIterations = config.RunIterations(cfg)
	s.IntervalStr = cfg.IntervalDelay
	s.IsWaiting = false
	s.setLifecycleStepLocked("Preparing targets", 1, len(plan.Targets))
	s.addLifecycleEventLocked("init", fmt.Sprintf("Multi-target %s: %d targets (%s), each compared with %s", plan.Kind, len(plan.Targets), plan.Mode, plan.Targets[0].Label))
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, startedResponse(generated))

	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		defer s.persistDashboard()
		if replayUpload != "" {
			defer s.removeReplayUpload(replayUpload)
		}
		defer removeEmptyFiles(generatedPaths(generated))
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("Multi-target run crashed unexpectedly: %v", r)
				log.Println("UI Run Panic:", msg)
				s.mu.Lock()
				s.LastError = msg
				s.IsRunning = false
				s.setLifecyclePhaseLocked("failed", msg)
				s.mu.Unlock()
			}
		}()
		res := plan.Run(ctx)
		for i := range res.Comparisons {
			c := &res.Comparisons[i]
			if c.Report == nil {
				continue
			}
			c.Report.ID = newComparisonID()
			c.ReportID = c.Report.ID
			for _, t := range plan.Targets {
				switch t.Label {
				case c.Reference:
					c.Report.A.PlanRunID = t.TrendRunID
				case c.Target:
					c.Report.B.PlanRunID = t.TrendRunID
				}
			}
			s.keepReplayComparison(c.Report)
		}
		failed := ""
		for _, st := range res.Targets {
			if st.Status == multitarget.StatusFailed && failed == "" {
				failed = fmt.Sprintf("target %s failed: %s", st.Label, st.Error)
			}
		}
		ref := plan.Targets[0]
		s.mu.Lock()
		refInsights := s.multiTargetRefInsights
		s.mu.Unlock()
		if refInsights == nil {
			// The reference never ran its workload; its insights must still
			// come from (and explain against) the reference, not uri.
			rep, _, _ := s.buildFinalInsights(ref.Collector, ref.Config, trendStore, newTrendRunID(trendClock(), runID), trendClock())
			refInsights = &rep
		}
		s.mu.Lock()
		s.CurrentStats = ref.Collector
		s.AppConfig = cfg
		s.InsightsCache = refInsights
		if failed != "" {
			s.LastError = failed
			s.setLifecyclePhaseLocked("failed", "Multi-target run: "+failed)
		} else {
			s.setLifecyclePhaseLocked("completed", "Multi-target run completed")
		}
		s.IsRunning = false
		s.mu.Unlock()
	}()
}
