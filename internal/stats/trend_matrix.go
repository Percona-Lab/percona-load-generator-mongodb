package stats

import (
	"errors"
	"fmt"
	"time"
)

// ErrTrendRunNotFound is returned for a run id that is not stored.
var ErrTrendRunNotFound = errors.New("trend run not found")

const trendMatrixNote = "Each column is one stored run compared with this run on its own, using the same compatibility gates and evidence minimums as the Trends view. Values are that run's snapshot; states describe this run relative to that column. A column without a value did not record the shape or is not comparable (see the reasons)."

// TrendMatrixCell is one shape in one run. For a baseline column the states
// compare this run with that column; the current column has values only.
type TrendMatrixCell struct {
	RunID             string   `json:"run_id"`
	Current           bool     `json:"current,omitempty"`
	Recorded          bool     `json:"recorded"`
	Comparability     string   `json:"comparability,omitempty"`
	Reasons           []string `json:"reasons,omitempty"`
	AvgMs             *float64 `json:"avg_ms,omitempty"`
	SampleP95Ms       *float64 `json:"sample_p95_ms,omitempty"`
	Score             *int     `json:"score,omitempty"`
	ScoreIsUpperBound bool     `json:"score_is_upper_bound,omitempty"`
	ImpactShare       *float64 `json:"impact_share,omitempty"`
	Efficiency        string   `json:"efficiency,omitempty"`
	Latency           string   `json:"latency,omitempty"`
	Impact            string   `json:"impact,omitempty"`
}

// TrendMatrixRow is one tracked shape of this run across the chosen runs.
type TrendMatrixRow struct {
	ShapeID      string            `json:"shape_id"`
	Namespace    string            `json:"namespace"`
	Operation    string            `json:"operation"`
	ShapeSummary string            `json:"shape_summary"`
	Cells        []TrendMatrixCell `json:"cells"`
}

// TrendMatrix compares this run with several stored runs at once. Columns[0]
// is the current run; the rest are the chosen runs, newest first.
type TrendMatrix struct {
	Version      string            `json:"version"`
	Status       string            `json:"status"`
	Reason       string            `json:"reason,omitempty"`
	Note         string            `json:"note"`
	Criteria     TrendCriteria     `json:"criteria"`
	CurrentRunID string            `json:"current_run_id"`
	Columns      []TrendRunSummary `json:"columns"`
	Rows         []TrendMatrixRow  `json:"rows"`
}

// BuildTrendMatrix compares rep with each of runIDs (1 to TrendMaxMatrixRuns
// stored runs other than this one).
func BuildTrendMatrix(rep InsightsReport, run TrendRunContext, base TrendStoreData, runIDs []string, now time.Time, p TrendPolicy) (TrendMatrix, error) {
	ctx := newTrendCompareCtx(run, base, now, p)
	ids := make([]string, 0, len(runIDs))
	dup := map[string]bool{}
	for _, id := range runIDs {
		if id == "" || dup[id] {
			continue
		}
		dup[id] = true
		if id == run.RunID {
			return TrendMatrix{}, fmt.Errorf("run %s is the current run; choose earlier runs", id)
		}
		if _, ok := ctx.runs[id]; !ok {
			return TrendMatrix{}, fmt.Errorf("%w: %s", ErrTrendRunNotFound, id)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > TrendMaxMatrixRuns {
		return TrendMatrix{}, fmt.Errorf("choose 1 to %d stored runs for the matrix (got %d)", TrendMaxMatrixRuns, len(ids))
	}
	cur := CurrentTrendBaselines(rep, run)
	m := TrendMatrix{Version: TrendVersion, Status: "ready", Note: trendMatrixNote, Criteria: TrendCriteriaFor(ctx.policy), CurrentRunID: run.RunID, Rows: []TrendMatrixRow{}}
	m.Columns = append(m.Columns, trendRunSummary(run, len(cur), now, ctx.policy))
	for _, s := range trendRunSummaries(base, run.RunID, now, ctx.policy) {
		if dup[s.RunID] {
			m.Columns = append(m.Columns, s)
		}
	}
	if rep.Metadata.Status != "ready" || rep.EfficiencyView == nil {
		m.Status, m.Reason = "not_run", "insights_status_"+emptyFallback(rep.Metadata.Status, "unknown")
		return m, nil
	}
	byID := make(map[string]TrendShapeBaseline, len(base.Shapes))
	for _, s := range base.Shapes {
		byID[s.ShapeID] = s
	}
	for _, c := range cur {
		row := TrendMatrixRow{ShapeID: c.ShapeID, Namespace: c.Namespace, Operation: c.Operation, ShapeSummary: c.ShapeSummary}
		cc := matrixValues(TrendMatrixCell{RunID: run.RunID, Current: true}, &c)
		row.Cells = append(row.Cells, cc)
		for _, col := range m.Columns[1:] {
			var b *TrendShapeSnapshot
			if s, ok := byID[c.ShapeID]; ok {
				b = selectRunBaseline(s, col.RunID)
			}
			sel := TrendSelection{Mode: TrendCompareRun, BaselineRunID: col.RunID, BaselineLabel: col.Label}
			tr := compareTrendShape(c, b, ctx, missingReason(sel))
			cell := matrixValues(TrendMatrixCell{RunID: col.RunID}, b)
			cell.Comparability = tr.Comparability
			cell.Efficiency, cell.Latency, cell.Impact = tr.Efficiency.State, tr.Latency.State, tr.Impact.State
			if tr.Comparability != TrendComparable {
				cell.Reasons = tr.ComparabilityReasons
			}
			row.Cells = append(row.Cells, cell)
		}
		m.Rows = append(m.Rows, row)
	}
	return m, nil
}

func matrixValues(cell TrendMatrixCell, s *TrendShapeSnapshot) TrendMatrixCell {
	if s == nil {
		return cell
	}
	if e := s.Efficiency; e != nil {
		cell.Recorded = true
		if e.Score != nil {
			v := *e.Score
			cell.Score = &v
		}
		cell.ScoreIsUpperBound = e.ScoreIsUpperBound
	}
	if w := s.Workload; w != nil {
		cell.Recorded = true
		cell.AvgMs = floatPtr(w.AvgMs)
		if w.SampleP95Ms != nil {
			cell.SampleP95Ms = floatPtr(*w.SampleP95Ms)
		}
		if w.ImpactShare != nil {
			cell.ImpactShare = floatPtr(*w.ImpactShare)
		}
	}
	return cell
}
