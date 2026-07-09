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
    <div class="card"><div class="label">Duration</div><div class="value">{{fmtFloat .DurationSeconds}}s</div></div>
    <div class="card"><div class="label">Total Ops</div><div class="value">{{.TotalOps}}</div></div>
    <div class="card"><div class="label">Avg Throughput</div><div class="value">{{fmtFloat .AvgOpsPerSec}}<span class="muted" style="font-size:12px"> ops/s</span></div></div>
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
