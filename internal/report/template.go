package report

// reportTemplate is a self-contained HTML document (inline CSS, no JS/external
// assets) designed to read well on screen and when printed to PDF.
const reportTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { --bg:#0f1115; --card:#fff; --ink:#1c2230; --muted:#6b7280; --line:#e5e7eb;
          --p50:#2563eb; --p95:#d97706; --p99:#dc2626; --accent:#7c3aed; }
  * { box-sizing: border-box; }
  body { margin:0; background:#f3f4f6; color:var(--ink);
         font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif; }
  .wrap { max-width: 980px; margin: 0 auto; padding: 24px; }
  header { display:flex; justify-content:space-between; align-items:flex-end; border-bottom:3px solid var(--accent); padding-bottom:12px; margin-bottom:20px; }
  header h1 { margin:0; font-size:22px; }
  header .meta { color:var(--muted); font-size:12px; text-align:right; }
  .cards { display:grid; grid-template-columns: repeat(4, 1fr); gap:12px; margin-bottom:20px; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:14px; }
  .card .label { color:var(--muted); font-size:11px; text-transform:uppercase; letter-spacing:.04em; }
  .card .value { font-size:22px; font-weight:700; margin-top:4px; }
  section { background:var(--card); border:1px solid var(--line); border-radius:10px; padding:16px 18px; margin-bottom:18px; }
  section h2 { margin:0 0 12px; font-size:15px; }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th, td { text-align:left; padding:7px 8px; border-bottom:1px solid var(--line); }
  th { color:var(--muted); font-weight:600; font-size:11px; text-transform:uppercase; letter-spacing:.03em; }
  td.num, th.num { text-align:right; font-variant-numeric: tabular-nums; }
  .kv { display:grid; grid-template-columns: repeat(2, 1fr); gap:6px 24px; }
  .kv div { display:flex; justify-content:space-between; border-bottom:1px dotted var(--line); padding:3px 0; }
  .kv .k { color:var(--muted); }
  .muted { color:var(--muted); }
  .warn { background:#fef3c7; border:1px solid #fde68a; color:#92400e; border-radius:8px; padding:10px 12px; margin:6px 0; }
  .note { background:#eef2ff; border:1px solid #c7d2fe; color:#3730a3; border-radius:8px; padding:10px 12px; margin:6px 0; }
  tr.detail td { border-bottom:1px solid var(--line); font-size:12px; }
  tr.detail .k { color:var(--muted); font-weight:600; }
  tr.detail ul { margin:2px 0 6px 18px; padding:0; }
  .chart { width:100%; height:auto; background:#fff; }
  .chart .axis { stroke:#9ca3af; stroke-width:1; }
  .chart .grid { stroke:#eef0f3; stroke-width:1; }
  .chart .tick { fill:#9ca3af; font-size:10px; }
  .chart polyline { fill:none; stroke-width:2; }
  .chart .p50 { stroke:var(--p50); } .chart .p95 { stroke:var(--p95); } .chart .p99 { stroke:var(--p99); }
  .legend { display:flex; gap:14px; margin-top:6px; font-size:12px; }
  .legend .k::before { content:"\2014  "; font-weight:700; }
  .legend .p50 { color:var(--p50); } .legend .p95 { color:var(--p95); } .legend .p99 { color:var(--p99); }
  footer { color:var(--muted); font-size:11px; text-align:center; margin-top:24px; }
  @media print { body { background:#fff; } section, .card { break-inside: avoid; } .wrap { max-width:none; } }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>{{.Title}}</h1>
    <div class="meta">Generated {{fmtTime .GeneratedAt}}</div>
  </header>

  <div class="cards">
    <div class="card"><div class="label">Duration</div><div class="value">{{if .DurationLabel}}{{.DurationLabel}}{{else}}{{fmtFloat .DurationSeconds}}s{{end}}</div></div>
    <div class="card"><div class="label">Total Ops</div><div class="value">{{if .OpsLabel}}{{.OpsLabel}}{{else}}{{.TotalOps}}{{end}}</div></div>
    <div class="card"><div class="label">Avg Throughput</div><div class="value">{{if .ThroughputLabel}}{{.ThroughputLabel}}{{else}}{{fmtFloat .AvgOpsPerSec}}<span class="muted" style="font-size:12px"> ops/s</span>{{end}}</div></div>
  </div>

  {{if .Warnings}}
  <section>
    <h2>Warnings</h2>
    {{range .Warnings}}<div class="warn">{{.}}</div>{{end}}
  </section>
  {{end}}

  <section>
    <h2>Configuration</h2>
    <div class="kv">
      {{range .ConfigItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}
    </div>
  </section>

  <section>
    <h2>Load Profile</h2>
    <div class="kv">
      {{range .LoadProfileItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{else}}<div class="muted">Fixed concurrency.</div>{{end}}
    </div>
  </section>

  <section>
    <h2>Pacing &amp; Access Pattern</h2>
    <div class="kv">
      {{range .PacingItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}
      {{range .AccessPatternItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}
    </div>
  </section>
  {{if .GeneratedTrafficItems}}
  <section id="generated-traffic">
    <h2>Deterministic Traffic Fingerprint</h2>
    <div class="kv">
      {{range .GeneratedTrafficItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}
    </div>
    <p class="muted">Two complete runs with the same fingerprint sent the same commands, with the same values, in the same order per worker. Cross-worker interleaving, server-generated values, and results on different data can still differ.</p>
  </section>
  {{end}}
  {{if .MultiTargetItems}}
  <section id="multi-target">
    <h2>Multi-Target Replay</h2>
    <div class="kv">
      {{range .MultiTargetItems}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}
    </div>
    {{range .MultiTargetNotes}}<p class="muted">{{.}}</p>{{end}}
  </section>
  {{end}}

  <section>
    <h2>Latency by Operation (ms)</h2>
    <table>
      <thead><tr><th>Type</th><th class="num">Count</th><th class="num">Avg</th><th class="num">Min</th><th class="num">Max</th><th class="num">p95</th><th class="num">p99</th></tr></thead>
      <tbody>
      {{range .Latency}}
        <tr><td>{{.Type}}</td><td class="num">{{.Count}}</td><td class="num">{{fmtFloat .AvgMs}}</td><td class="num">{{fmtFloat .MinMs}}</td><td class="num">{{fmtFloat .MaxMs}}</td><td class="num">{{fmtFloat .P95Ms}}</td><td class="num">{{fmtFloat .P99Ms}}</td></tr>
      {{else}}
        <tr><td colspan="7" class="muted">No latency data.</td></tr>
      {{end}}
      </tbody>
    </table>
  </section>

  <section>
    <h2>Latency Over Time</h2>
    {{latencyChart .Heatmap}}
  </section>

  {{with .Impact}}
  <section>
    <h2>Workload Impact (summed concurrent operation time)</h2>
    <p class="muted">{{.TimeBasisNote}} {{.InterpretationNote}}</p>
    <p>Observed: {{.ObservedOps}} operations, {{fmtFloat .ObservedMs}} ms summed operation time.</p>
    <table>
      <thead><tr><th class="num">#</th><th>Namespace</th><th>Op</th><th>Shape</th><th class="num">Operations</th><th class="num">Summed ms</th><th class="num">Share %</th><th class="num">Avg ms</th><th class="num">Samples</th><th class="num">Sample p95</th></tr></thead>
      <tbody>
      {{range .Rows}}
        <tr title="{{.Explanation}}"><td class="num">{{.Rank}}</td><td>{{.Namespace}}</td><td>{{.Operation}}</td><td>{{.Shape}}</td><td class="num">{{.OperationCount}}</td><td class="num">{{fmtFloat .TotalMs}}</td><td class="num">{{fmtFloat .SharePct}}</td><td class="num">{{fmtFloat .AvgMs}}</td><td class="num">{{.SampleCount}}</td><td class="num">{{.SampleP95}}</td></tr>
      {{else}}
        <tr><td colspan="10" class="muted">No completed operations were observed ({{.Status}}).</td></tr>
      {{end}}
      </tbody>
    </table>
    {{range .Limitations}}<div class="warn">{{.}}</div>{{end}}
  </section>
  {{end}}

  {{with .Efficiency}}
  <section id="efficiency">
    <h2>Query Assessment (status: {{.Status}}{{if .Reason}}, {{.Reason}}{{end}})</h2>
    {{if .Notice}}<p class="warn" id="efficiency-notice">{{.Notice}}</p>{{end}}
    <dl class="kv" id="efficiency-columns">{{range .Columns}}<div><dt class="k">{{.Key}}</dt><dd>{{.Value}}</dd></div>{{end}}</dl>
    <p class="muted">Latency targets: {{.LatencyTargets}}. Rating versions: {{.RatingVersions}}.</p>
    <p class="muted">Order: lowest scan efficiency first. {{.SortNote}}</p>
    <div class="kv">{{range .Coverage}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}</div>
    {{range .Unrecorded}}<div class="warn">Not recorded: {{.}}</div>{{end}}
    <table>
      <thead><tr><th class="num">#</th><th>Namespace</th><th>Op</th><th>Shape</th><th class="num">Scan efficiency</th><th>Band</th><th>Confidence</th><th>Query latency</th><th>Workload impact</th><th>Execution overhead</th><th>$lookup / locks / network</th><th>Coverage</th></tr></thead>
      <tbody>
      {{range .Rows}}
        <tr><td class="num">{{.Order}}</td><td>{{.Namespace}}</td><td>{{.Operation}}</td><td>{{.Shape}}</td><td class="num"><strong>{{.ScoreLabel}}</strong></td><td>{{if .Band}}{{.Band}}{{else}}&ndash;{{end}}</td><td>{{if .Confidence}}{{.Confidence}}{{else}}&ndash;{{end}}</td><td>{{.Latency}}<br><span class="muted">{{.LatencyRating}}{{if .Severity}}; severity {{.Severity}}{{end}}</span></td><td>{{.Impact}}</td><td>{{.Overhead}}{{if .OverheadSummary}}<br><span class="muted">{{.OverheadSummary}}</span>{{end}}</td><td>$lookup: {{.Lookup}}<br>locks: {{.Locks}}<br>network: {{.Network}}</td><td>{{.Coverage}}{{if .CoverageReason}} <span class="muted">({{.CoverageReason}})</span>{{end}}</td></tr>
        <tr class="detail"><td></td><td colspan="11">
          <details><summary>Details: {{.Operation}} {{.Namespace}} ({{.ShapeID}})</summary>
            {{if .Query}}<div><span class="k">Redacted query:</span> <code>{{.Query}}</code></div>{{end}}
            <div><span class="k">Scan efficiency:</span> {{.RatingExplanation}}</div>
            {{if .Components}}<div class="k">Scan efficiency checks</div><ul>{{range .Components}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .Samples}}<div class="k">Measured samples</div><ul>{{range .Samples}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .OverheadEvidence}}<div class="k">Execution overhead evidence</div><ul>{{range .OverheadEvidence}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .OverheadMeasurements}}<div class="k">Sort and spill measurements ({{.OverheadMeasurementStatus}})</div><ul>{{range .OverheadMeasurements}}<li>{{.}}</li>{{end}}</ul>{{if .OverheadNotes}}<div class="k">What these measurements mean</div><ul>{{range .OverheadNotes}}<li>{{.}}</li>{{end}}</ul>{{end}}{{end}}
            {{if .OtherDetails}}<div class="k">$lookup, locks, and network</div><ul>{{range .OtherDetails}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .DataSource}}<div class="k">How the measurements were collected</div><ul>{{range .DataSource}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .PlanSummary}}<div><span class="k">Plan:</span> {{.PlanSummary}}</div>{{end}}
            {{if .Observations}}<div class="k">Plan observations</div><ul>{{range .Observations}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{if .Limitations}}<div class="k">Limitations</div><ul>{{range .Limitations}}<li>{{.}}</li>{{end}}</ul>{{end}}
            {{range .Recommendations}}<div class="k">Recommendation #{{.Rank}}: {{.Title}}</div><ul>
              {{if .CandidateKeys}}<li>Index candidate (ordered keys): <code>{{range $i, $k := .CandidateKeys}}{{if $i}}, {{end}}{{$k}}{{end}}</code></li>{{end}}
              {{if .Rationale}}<li>Rationale: {{.Rationale}}</li>{{end}}
              {{if .Tradeoffs}}<li>Tradeoffs: {{.Tradeoffs}}</li>{{end}}
              {{if .ValidationStep}}<li>Validation: {{.ValidationStep}}</li>{{end}}
              {{range .Limitations}}<li>Limitation: {{.}}</li>{{end}}
            </ul>{{end}}
          </details>
        </td></tr>
      {{else}}
        <tr><td colspan="12" class="muted">No tracked shapes ({{.Status}}).</td></tr>
      {{end}}
      </tbody>
    </table>
    {{range .Limitations}}<div class="warn">{{.}}</div>{{end}}
  </section>
  {{end}}

  {{with .Trends}}
  <section id="trends">
    <h2>Trends vs Baseline ({{.Version}}, status: {{.Status}}{{if .Reason}}, {{.Reason}}{{end}})</h2>
    <p class="note">{{.CausationNote}}</p>
    <p class="muted">{{.MethodNote}}</p>
    <p class="muted">{{.EvidenceNote}}</p>
    <p class="muted">Criteria: {{.CriteriaSummary}} {{.CriteriaNote}}</p>{{if .QueryValuesNotice}}
    <div class="warn" id="trendQueryValuesNotice">{{.QueryValuesNotice}}</div>{{end}}
    <div class="kv">{{range .Store}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}</div>
    <div class="kv">{{range .Counts}}<div><span class="k">{{.Key}}</span><span>{{.Value}}</span></div>{{end}}</div>
    <table>
      <thead><tr><th>Namespace</th><th>Op</th><th>Shape</th><th>Baseline</th><th>Scan efficiency</th><th>Latency</th><th>Impact</th></tr></thead>
      <tbody>
      {{range .Rows}}
        <tr><td>{{.Namespace}}</td><td>{{.Operation}}</td><td>{{.Shape}}</td><td>{{.Comparability}}{{if .BaselineAge}} <span class="muted">({{.BaselineAge}})</span>{{end}}</td><td>{{.Efficiency}}</td><td>{{.Latency}}</td><td>{{.Impact}}</td></tr>
        {{if .Details}}<tr class="detail"><td></td><td colspan="6"><details><summary>Comparison details: {{.Operation}} {{.Namespace}} ({{.ShapeID}})</summary><ul>{{range .Details}}<li>{{.}}</li>{{end}}</ul></details></td></tr>{{end}}{{if .QueryValues}}<tr class="detail trend-values"><td></td><td colspan="6"><details><summary>{{if .QueryValuesFromTraffic}}Query values (same deterministic traffic fingerprint; annotation only){{else if .QueryValuesFromRecording}}Query values (both runs replayed the same recording; annotation only){{else}}Query values (literal, recorded with insights_trend_store_query_values on){{end}}: {{.QueryValues}}</summary><ul>{{range .QueryValueDetails}}<li><code>{{.}}</code></li>{{end}}</ul></details></td></tr>{{end}}
      {{else}}
        <tr><td colspan="7" class="muted">No trend rows ({{.Status}}).</td></tr>
      {{end}}
      </tbody>
    </table>
    {{range .Limitations}}<div class="warn">{{.}}</div>{{end}}
  </section>
  {{end}}

  {{if .Recommendations}}
  <section>
    <h2>Recommendations</h2>
    <table>
      <thead><tr><th class="num">#</th><th>Priority</th><th>Recommendation</th><th>Confidence</th><th>Source</th></tr></thead>
      <tbody>
      {{range .Recommendations}}
        <tr><td class="num">{{.Rank}}</td><td>{{.Priority}}</td><td><strong>{{.Title}}</strong><br>{{.Details}}</td><td>{{.Confidence}}</td><td>{{.Source}}</td></tr>
      {{end}}
      </tbody>
    </table>
  </section>
  {{end}}

  {{if .Insights}}
  <section>
    <h2>Insights</h2>
    <ul>{{range .Insights}}<li>{{.}}</li>{{end}}</ul>
  </section>
  {{end}}

  <footer>Generated by percona-load-generator-mongodb &middot; Print this page to export as PDF.</footer>
</div>
</body>
</html>`
