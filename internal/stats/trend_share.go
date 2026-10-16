package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
)

// TrendExport is a shareable file with selected runs and their snapshots. It
// holds the same fields as the store and never the connection URI. Query
// text and literals are included only for runs recorded with
// insights_trend_store_query_values on, and then ContainsQueryValues is set.
type TrendExport struct {
	Format              string               `json:"format"`
	SchemaVersion       string               `json:"schema_version"`
	ExportedAt          string               `json:"exported_at"`
	Source              string               `json:"source,omitempty"`
	ContainsQueryValues bool                 `json:"contains_query_values,omitempty"`
	Runs                []TrendRunContext    `json:"runs"`
	Shapes              []TrendShapeBaseline `json:"shapes"`
}

const trendImportValuesWarning = "this file contains literal query values (insights_trend_store_query_values); they were imported with the runs and are shown in trend details, exports, Download Summary, and the HTML report"

// TrendImportRun is one run of an import file that was not added.
type TrendImportRun struct {
	RunID  string `json:"run_id"`
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason"`
}

// TrendImportResult reports what an import changed.
type TrendImportResult struct {
	Source   string            `json:"source"`
	Added    []TrendRunSummary `json:"added"`
	Skipped  []TrendImportRun  `json:"skipped"`
	NotKept  []TrendImportRun  `json:"not_kept"`
	Warnings []string          `json:"warnings,omitempty"`
	Store    TrendStoreStatus  `json:"store"`
}

// Export returns the chosen runs (all when ids is empty) and their snapshots.
func (s *TrendStore) Export(ids []string, source string, now time.Time) (TrendExport, error) {
	if s == nil || s.disabled {
		return TrendExport{}, ErrTrendStoreDisabled
	}
	s.mu.Lock()
	s.refreshLocked()
	d := cloneTrendData(s.data)
	s.mu.Unlock()
	return BuildTrendExport(d, ids, source, now)
}

// BuildTrendExport selects runs ids (all when empty) from d.
func BuildTrendExport(d TrendStoreData, ids []string, source string, now time.Time) (TrendExport, error) {
	want := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			if trendRunIndex(d, id) < 0 {
				return TrendExport{}, fmt.Errorf("%w: %s", ErrTrendRunNotFound, id)
			}
			want[id] = true
		}
	}
	keep := func(id string) bool { return len(want) == 0 || want[id] }
	out := TrendExport{
		Format: TrendExportFormat, SchemaVersion: TrendStoreSchemaVersion, ExportedAt: now.UTC().Format(time.RFC3339),
		Source: trendText(config.SanitizeTrendRunLabel(source), trendMaxSource), Runs: []TrendRunContext{}, Shapes: []TrendShapeBaseline{},
	}
	runs := append([]TrendRunContext(nil), d.Runs...)
	sortRunsNewestFirst(runs)
	for _, r := range runs {
		if keep(r.RunID) {
			out.Runs = append(out.Runs, r)
		}
	}
	for _, sh := range d.Shapes {
		sh.EfficiencyHistory = filterHistory(sh.EfficiencyHistory, func(e TrendEfficiencySnapshot) bool { return keep(e.RunID) })
		sh.WorkloadHistory = filterHistory(sh.WorkloadHistory, func(w TrendWorkloadSnapshot) bool { return keep(w.RunID) })
		sh.QueryValuesHistory = filterHistory(sh.QueryValuesHistory, func(v TrendQueryValues) bool { return keep(v.RunID) })
		if len(sh.EfficiencyHistory) > 0 || len(sh.WorkloadHistory) > 0 {
			out.Shapes = append(out.Shapes, sh)
		}
	}
	out.ContainsQueryValues = TrendDataHasQueryValues(TrendStoreData{Shapes: out.Shapes})
	return out, nil
}

// ParseTrendImport validates an import file: size, JSON, format, schema
// version, and count limits. Fields outside the trend schema are dropped and
// reported in warnings; text is re-bounded like a stored file.
func ParseTrendImport(b []byte) (TrendExport, []string, error) {
	if len(b) > trendStoreMaxBytes {
		return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("file is %d bytes; the limit is %d", len(b), trendStoreMaxBytes)}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return TrendExport{}, nil, &TrendImportError{"file is empty"}
	}
	var head struct {
		Format        string            `json:"format"`
		SchemaVersion string            `json:"schema_version"`
		Runs          []json.RawMessage `json:"runs"`
		Shapes        []struct {
			EfficiencyHistory  []json.RawMessage `json:"efficiency_history"`
			WorkloadHistory    []json.RawMessage `json:"workload_history"`
			QueryValuesHistory []json.RawMessage `json:"query_values_history"`
		} `json:"shapes"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return TrendExport{}, nil, &TrendImportError{"not a PLGM trend export (invalid JSON or field types)"}
	}
	if head.Format != "" && head.Format != TrendExportFormat {
		return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("format %q is not %s", head.Format, TrendExportFormat)}
	}
	if head.SchemaVersion != TrendStoreSchemaVersion {
		return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("unsupported schema %q; this version imports %s (export the runs again from a current PLGM)", emptyFallback(head.SchemaVersion, "missing"), TrendStoreSchemaVersion)}
	}
	if len(head.Runs) > TrendMaxRuns {
		return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("file has %d runs; the limit is %d", len(head.Runs), TrendMaxRuns)}
	}
	if len(head.Shapes) > TrendMaxShapeBaselines {
		return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("file has %d shapes; the limit is %d", len(head.Shapes), TrendMaxShapeBaselines)}
	}
	for _, sh := range head.Shapes {
		if len(sh.EfficiencyHistory) > TrendMaxRuns || len(sh.WorkloadHistory) > TrendMaxRuns || len(sh.QueryValuesHistory) > TrendMaxRuns {
			return TrendExport{}, nil, &TrendImportError{fmt.Sprintf("a shape has more than %d snapshots", TrendMaxRuns)}
		}
	}
	var warnings []string
	strict := json.NewDecoder(bytes.NewReader(b))
	strict.DisallowUnknownFields()
	var probe TrendExport
	if err := strict.Decode(&probe); err != nil && strings.Contains(err.Error(), "unknown field") {
		warnings = append(warnings, "fields outside the trend schema were ignored ("+trendText(strings.TrimPrefix(err.Error(), "json: "), 128)+")")
	}
	var x TrendExport
	if err := json.Unmarshal(b, &x); err != nil {
		return TrendExport{}, nil, &TrendImportError{"invalid run or snapshot entries"}
	}
	d, dropped := sanitizeTrendData(TrendStoreData{Runs: x.Runs, Shapes: x.Shapes})
	if dropped != "" {
		warnings = append(warnings, dropped)
	}
	x.Format, x.SchemaVersion, x.Runs, x.Shapes = TrendExportFormat, TrendStoreSchemaVersion, d.Runs, d.Shapes
	x.Source = trendText(config.SanitizeTrendRunLabel(x.Source), trendMaxSource)
	if len(x.Runs) == 0 {
		return TrendExport{}, warnings, &TrendImportError{"file has no valid runs"}
	}
	x.ContainsQueryValues = TrendDataHasQueryValues(d)
	if x.ContainsQueryValues {
		warnings = append(warnings, trendImportValuesWarning)
	}
	return x, warnings, nil
}

// Import merges a validated file by run id: runs already stored are skipped,
// new runs are added with their source label, and the history setting p is
// applied. Runs that p prunes immediately are reported as not kept.
func (s *TrendStore) Import(b []byte, fileName string, p TrendPolicy, now time.Time) (TrendImportResult, error) {
	if s == nil || s.disabled {
		return TrendImportResult{}, ErrTrendStoreDisabled
	}
	x, warnings, err := ParseTrendImport(b)
	if err != nil {
		return TrendImportResult{}, err
	}
	res := TrendImportResult{Source: x.Source, Added: []TrendRunSummary{}, Skipped: []TrendImportRun{}, NotKept: []TrendImportRun{}, Warnings: warnings}
	if res.Source == "" {
		res.Source = trendText(config.SanitizeTrendRunLabel(strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))), trendMaxSource)
	}
	if res.Source == "" || res.Source == "." {
		res.Source = "import"
	}
	var added []TrendRunContext
	err = s.mutate(func(d TrendStoreData) (TrendStoreData, error) {
		res.Skipped, res.NotKept, added = res.Skipped[:0], res.NotKept[:0], nil
		pinned := 0
		for _, r := range d.Runs {
			if r.Pinned {
				pinned++
			}
		}
		addedIDs := map[string]bool{}
		for _, r := range x.Runs {
			if trendRunIndex(d, r.RunID) >= 0 {
				res.Skipped = append(res.Skipped, TrendImportRun{RunID: r.RunID, Label: r.Label, Reason: "duplicate: a run with this id is already stored"})
				continue
			}
			r.Source, r.ImportedAt = res.Source, now.UTC().Format(time.RFC3339)
			if r.Pinned {
				if pinned >= TrendMaxPinnedRuns {
					r.Pinned = false
					res.Warnings = append(res.Warnings, fmt.Sprintf("run %s was imported unpinned: at most %d runs can be pinned", r.Label, TrendMaxPinnedRuns))
				} else {
					pinned++
				}
			}
			addedIDs[r.RunID] = true
			added = append(added, r)
		}
		conflicts := 0
		byID := map[string]int{}
		for i, sh := range d.Shapes {
			byID[sh.ShapeID] = i
		}
		for _, sh := range x.Shapes {
			eff := filterHistory(sh.EfficiencyHistory, func(e TrendEfficiencySnapshot) bool { return addedIDs[e.RunID] })
			wl := filterHistory(sh.WorkloadHistory, func(w TrendWorkloadSnapshot) bool { return addedIDs[w.RunID] })
			vals := filterHistory(sh.QueryValuesHistory, func(v TrendQueryValues) bool { return addedIDs[v.RunID] })
			if len(eff) == 0 && len(wl) == 0 {
				continue
			}
			i, ok := byID[sh.ShapeID]
			if !ok {
				sh.EfficiencyHistory, sh.WorkloadHistory, sh.QueryValuesHistory = eff, wl, vals
				byID[sh.ShapeID] = len(d.Shapes)
				d.Shapes = append(d.Shapes, sh)
				continue
			}
			local := &d.Shapes[i]
			if local.Namespace != sh.Namespace || local.Operation != sh.Operation {
				conflicts++
				continue
			}
			local.EfficiencyHistory = append(append([]TrendEfficiencySnapshot(nil), local.EfficiencyHistory...), eff...)
			local.WorkloadHistory = append(append([]TrendWorkloadSnapshot(nil), local.WorkloadHistory...), wl...)
			if len(vals) > 0 {
				local.QueryValuesHistory = append(append([]TrendQueryValues(nil), local.QueryValuesHistory...), vals...)
			}
		}
		if conflicts > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d shape snapshots were skipped: the same shape id is stored for a different namespace or operation", conflicts))
		}
		d.Runs = append(d.Runs, added...)
		return pruneTrendData(d, p), nil
	})
	if err != nil {
		return TrendImportResult{}, err
	}
	s.mu.Lock()
	final := cloneTrendData(s.data)
	res.Store = s.statusLocked()
	s.mu.Unlock()
	summaries := map[string]TrendRunSummary{}
	for _, sum := range trendRunSummaries(final, "", now, p) {
		summaries[sum.RunID] = sum
	}
	for _, r := range added {
		if sum, ok := summaries[r.RunID]; ok {
			res.Added = append(res.Added, sum)
		} else {
			res.NotKept = append(res.NotKept, TrendImportRun{RunID: r.RunID, Label: r.Label, Reason: fmt.Sprintf("older than the newest %d unpinned runs kept (trend_history_runs); raise the limit or import it pinned", p.normalized().KeepRuns)})
		}
	}
	return res, nil
}
