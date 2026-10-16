package replaycompare

import (
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/stats"
)

// PlansFromTrendRun returns the explain evidence (plan signatures and
// indexes used) that trend run runID stored per shape id, and whether the
// run exists.
func PlansFromTrendRun(d stats.TrendStoreData, runID string) (map[string]PlanInfo, bool) {
	found := false
	for _, r := range d.Runs {
		found = found || r.RunID == runID
	}
	if !found {
		return nil, false
	}
	out := map[string]PlanInfo{}
	for _, s := range d.Shapes {
		for _, e := range s.EfficiencyHistory {
			if e.RunID == runID && len(e.PlanSignatures) > 0 {
				out[s.ShapeID] = PlanInfo{PlanSignatures: append([]string(nil), e.PlanSignatures...), IndexesUsed: append([]string(nil), e.IndexesUsed...)}
				break
			}
		}
	}
	return out, true
}
