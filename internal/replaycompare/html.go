package replaycompare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/workloadrec"
)

// OutcomeText summarizes an outcome in one line, without literal values.
func OutcomeText(o *workloadrec.Outcome) string {
	if o == nil {
		return "not run"
	}
	var parts []string
	if o.OK {
		parts = append(parts, "ok")
	} else {
		e := "error"
		switch {
		case o.ErrorCode != 0 && o.ErrorName != "":
			e += fmt.Sprintf(" %d %s", o.ErrorCode, o.ErrorName)
		case o.ErrorCode != 0:
			e += fmt.Sprintf(" %d", o.ErrorCode)
		case o.ErrorName != "":
			e += " " + o.ErrorName
		}
		parts = append(parts, e)
	}
	parts = append(parts, fmt.Sprintf("%.3f ms", float64(o.LatencyUS)/1000))
	for _, c := range []struct {
		name string
		v    *int64
	}{{"returned", o.Returned}, {"inserted", o.Inserted}, {"matched", o.Matched}, {"modified", o.Modified}, {"upserted", o.Upserted}, {"deleted", o.Deleted}} {
		if c.v != nil {
			parts = append(parts, fmt.Sprintf("%s %d", c.name, *c.v))
		}
	}
	if o.Checksum != "" {
		parts = append(parts, "checksum "+short(o.Checksum))
	}
	if o.Transaction != "" {
		parts = append(parts, "transaction "+o.Transaction)
	}
	return strings.Join(parts, ", ")
}

// ErrorsText lists error counts by code, sorted.
func ErrorsText(m map[string]uint64) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// OpKeyText names an operation by its seq or (iteration, worker, seq).
func OpKeyText(d OpDiff) string {
	if d.Seq > 0 {
		return fmt.Sprintf("seq %d (worker %d #%d)", d.Seq, d.Worker, d.WorkerSeq)
	}
	it := d.Iteration
	if it < 1 {
		it = 1
	}
	return fmt.Sprintf("iteration %d, worker %d #%d", it, d.Worker, d.WorkerSeq)
}

func pctText(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *p)
}

// prettyJSON indents canonical Extended JSON without reordering keys.
func prettyJSON(s string) string {
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(s), "", "  "); err != nil {
		return s
	}
	return b.String()
}

var htmlTmpl = template.Must(template.New("compare").Funcs(template.FuncMap{
	"outcome": OutcomeText, "errors": ErrorsText, "opkey": OpKeyText, "pct": pctText, "pretty": prettyJSON,
	"join": strings.Join, "under": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>PLGM replay comparison: {{.A.Label}} vs {{.B.Label}}</title>
<style>
body{font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;margin:24px;color:#1f2937;font-size:14px}
h1{font-size:20px}h2{font-size:16px;margin-top:24px}
table{border-collapse:collapse;width:100%;margin:8px 0}th,td{border:1px solid #d1d5db;padding:4px 6px;text-align:left;vertical-align:top}
th{background:#f3f4f6}.num{text-align:right}.muted{color:#6b7280}.warn{background:#fef3c7;border:1px solid #f59e0b;padding:8px;margin:8px 0}
.slower{color:#b91c1c;font-weight:600}.faster{color:#047857;font-weight:600}pre{background:#f9fafb;border:1px solid #e5e7eb;padding:6px;overflow:auto;font-size:12px}
</style></head><body>
<h1>Replay comparison: {{.A.Label}} vs {{.B.Label}}</h1>
<p class="muted">{{.Format}} · generated {{.GeneratedAt}} · operations matched by {{under .MatchedBy}}{{if .IdentityMatch}} · same {{under .IdentityKind}} {{.Identity}}{{end}}</p>
{{if .ContainsLiteralValues}}<div class="warn"><strong>Contains literal query values.</strong> {{.LiteralNote}}</div>{{end}}
{{range .Warnings}}<div class="warn">⚠ {{.}}</div>{{end}}
{{range .Notes}}<p class="muted">{{.}}</p>{{end}}
<h2>Sides</h2>
<table><tr><th></th><th>A (reference)</th><th>B</th></tr>
<tr><th>Label</th><td>{{.A.Label}}</td><td>{{.B.Label}}</td></tr>
<tr><th>File</th><td>{{.A.File}} ({{.A.Kind}})</td><td>{{.B.File}} ({{.B.Kind}})</td></tr>
<tr><th>Status</th><td>{{.A.Status}}</td><td>{{.B.Status}}</td></tr>
<tr><th>Server version</th><td>{{or .A.ServerVersion "unknown"}}</td><td>{{or .B.ServerVersion "unknown"}}</td></tr>
<tr><th>Topology</th><td>{{or .A.Topology "unknown"}}</td><td>{{or .B.Topology "unknown"}}</td></tr>
<tr><th>Operations</th><td>{{.A.Ops}}</td><td>{{.B.Ops}}</td></tr>
<tr><th>Workers</th><td>{{.A.Workers}}</td><td>{{.B.Workers}}</td></tr>
<tr><th>Result checksums</th><td>{{.A.ResultChecksums}}</td><td>{{.B.ResultChecksums}}</td></tr>
<tr><th>PLGM version</th><td>{{.A.PLGMVersion}}</td><td>{{.B.PLGMVersion}}</td></tr>
</table>
<h2>Overall</h2>
{{with .Overall}}<table>
<tr><th>Ops A / B / matched</th><td>{{.OpsA}} / {{.OpsB}} / {{.Matched}}</td></tr>
<tr><th>Latency (ops ok on both sides)</th><td>{{.Latency.Label}}{{range .Latency.Reasons}} ({{.}}){{end}}</td></tr>
<tr><th>Differing operations</th><td>{{.DifferingOps}}</td></tr>
<tr><th>Result count mismatches</th><td>{{.CountMismatches}}</td></tr>
<tr><th>Checksum mismatches</th><td>{{.ChecksumMismatches}} of {{.ChecksumsCompared}} compared</td></tr>
<tr><th>Errors only on A</th><td>{{errors .ErrorsOnlyA}}</td></tr>
<tr><th>Errors only on B</th><td>{{errors .ErrorsOnlyB}}</td></tr>
<tr><th>Different error codes</th><td>{{.ErrorCodeDifferences}}</td></tr>
<tr><th>Missing on A / on B</th><td>{{.MissingInA}} / {{.MissingInB}}</td></tr>
</table>{{end}}
<p class="muted">{{.LatencyNote}}</p><p class="muted">{{.FactsNote}}</p>
<h2>Per query shape</h2>
<table><thead><tr><th>Namespace / op</th><th>Shape</th><th class="num">Ops A / B</th><th>Avg A → B</th><th>p50</th><th>p95</th><th>p99</th><th>Latency verdict (heuristic)</th><th>Result differences</th><th>Errors only on one side</th><th class="num">Missing A / B</th><th>Plan (annotation)</th></tr></thead><tbody>
{{range .Shapes}}<tr><td>{{.Namespace}}<br><span class="muted">{{.Operation}}</span></td><td>{{or .ShapeSummary .ShapeKey}}<br><span class="muted">{{.ShapeID}}</span></td>
<td class="num">{{.OpsA}} / {{.OpsB}}</td><td>{{printf "%.3f" .Latency.A.AvgMs}} → {{printf "%.3f" .Latency.B.AvgMs}} ms ({{pct .Latency.AvgDeltaPct}})</td>
<td>{{printf "%.3f" .Latency.A.P50Ms}} → {{printf "%.3f" .Latency.B.P50Ms}} ({{pct .Latency.P50DeltaPct}})</td>
<td>{{printf "%.3f" .Latency.A.P95Ms}} → {{printf "%.3f" .Latency.B.P95Ms}} ({{pct .Latency.P95DeltaPct}})</td>
<td>{{printf "%.3f" .Latency.A.P99Ms}} → {{printf "%.3f" .Latency.B.P99Ms}} ({{pct .Latency.P99DeltaPct}})</td>
<td class="{{.Latency.Verdict}}">{{under .Latency.Verdict}}</td>
<td>counts {{.CountMismatches}}, checksums {{.ChecksumMismatches}}/{{.ChecksumsCompared}}, error codes {{.ErrorCodeDifferences}}</td>
<td>A: {{errors .ErrorsOnlyA}}<br>B: {{errors .ErrorsOnlyB}}</td><td class="num">{{.MissingInA}} / {{.MissingInB}}</td>
<td>{{with .Plan}}{{if .Changed}}<strong>changed</strong>: {{end}}{{.Note}}{{else}}<span class="muted">no explain data</span>{{end}}</td></tr>
{{else}}<tr><td colspan="12" class="muted">No operations.</td></tr>{{end}}
</tbody></table>
<h2>Differing operations (first {{.TopN}} by order)</h2>
{{if .DifferingOps}}<table><thead><tr><th>Operation</th><th>Shape</th><th>Differences</th><th>A</th><th>B</th><th>Command (literal values)</th></tr></thead><tbody>
{{range .DifferingOps}}<tr><td>{{opkey .}}<br><span class="muted">{{.Namespace}} {{.Operation}}</span></td><td>{{or .ShapeSummary .ShapeKey}}</td><td>{{join .Kinds ", "}}</td><td>{{outcome .A}}</td><td>{{outcome .B}}</td>
<td>{{if .Command}}<pre>{{pretty .Command}}</pre>{{end}}<span class="muted">{{.CommandNote}}</span></td></tr>{{end}}
</tbody></table>{{else}}<p>No operation differs in results or errors, and none is missing on either side.</p>{{end}}
<p class="muted">{{.MemoryNote}}</p>
</body></html>
`))

// WriteHTML writes the report as a standalone HTML page.
func WriteHTML(w io.Writer, r *Report) error { return htmlTmpl.Execute(w, r) }

// WriteFiles writes the report as JSON and HTML (mode 0600: the report can
// hold literal values).
func WriteFiles(r *Report, jsonPath, htmlPath string) error {
	if jsonPath != "" {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	if htmlPath != "" {
		var buf bytes.Buffer
		if err := WriteHTML(&buf, r); err != nil {
			return err
		}
		if err := os.WriteFile(htmlPath, buf.Bytes(), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// SummaryText is a short plain-text summary for the CLI.
func SummaryText(r *Report) string {
	o := r.Overall
	var b strings.Builder
	fmt.Fprintf(&b, "Replay comparison %s (%s, %s) vs %s (%s, %s), matched by %s\n", r.A.Label, r.A.File, emptyFallback(r.A.ServerVersion, "version unknown"), r.B.Label, r.B.File, emptyFallback(r.B.ServerVersion, "version unknown"), r.MatchedBy)
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "WARNING: %s\n", w)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "NOTE: %s\n", n)
	}
	fmt.Fprintf(&b, "Operations: A %d, B %d, matched %d; differing %d (count mismatches %d, checksum mismatches %d, errors only on A %s, errors only on B %s, different error codes %d, missing on A %d, missing on B %d)\n",
		o.OpsA, o.OpsB, o.Matched, o.DifferingOps, o.CountMismatches, o.ChecksumMismatches, ErrorsText(o.ErrorsOnlyA), ErrorsText(o.ErrorsOnlyB), o.ErrorCodeDifferences, o.MissingInA, o.MissingInB)
	fmt.Fprintf(&b, "Latency (heuristic): %s\n", o.Latency.Label)
	keys := make([]string, 0, len(r.ShapeVerdicts))
	for k := range r.ShapeVerdicts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, r.ShapeVerdicts[k])
	}
	fmt.Fprintf(&b, "Shapes: %d (%s)\n", len(r.Shapes), strings.Join(parts, ", "))
	return b.String()
}
