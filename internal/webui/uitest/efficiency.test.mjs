// DOM interaction tests for the efficiency view, recommendation pager, and
// Download Summary in internal/webui/static/index.html. Fixtures are the real
// /api/insights output written by TestUIFixturesMatchAPI in the Go tests.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage, choose } from './harness.mjs';

const tableRows = doc => [...doc.querySelectorAll('#efficiencyRows tbody tr')];
const cells = tr => [...tr.querySelectorAll('td')].map(td => td.textContent.trim());
const COL = { nsop: 0, shape: 1, score: 2, band: 3, conf: 4, latency: 5, impact: 6, overhead: 7, other: 8, coverage: 9 };

test('default efficiency view lists every shape, including fast assessed ones, in API order', async () => {
    const data = fixture('ready');
    const view = data.efficiency_view;
    const { w, doc, errors } = await loadPage(data);
    doc.getElementById('qaTabFindingsBtn').click();
    doc.getElementById('qaTabEfficiencyBtn').click();
    assert.equal(doc.getElementById('queryAnalysisTabEfficiency').style.display, 'block');
    assert.equal(doc.getElementById('queryAnalysisTabFindings').style.display, 'none');
    assert.equal(doc.getElementById('qaTabEfficiencyBtn').getAttribute('aria-pressed'), 'true');

    const rows = tableRows(doc);
    assert.equal(rows.length, view.rows.length);
    assert.deepEqual(rows.map(r => r.dataset.shapeId), view.rows.map(r => r.shape_id));
    rows.forEach((tr, i) => {
        const c = cells(tr);
        const r = view.rows[i];
        assert.equal(c[COL.score], r.score_label);
        assert.equal(tr.querySelector('.eff-ns').textContent, r.namespace);
        assert.equal(tr.querySelector('.eff-op').textContent, r.operation);
        assert.ok(c[COL.coverage].startsWith(r.coverage_label), `coverage ${c[COL.coverage]}`);
        assert.ok(c[COL.latency].startsWith(r.latency.summary) && c[COL.latency].includes(r.latency.rating.label), `latency ${c[COL.latency]}`);
        assert.ok(c[COL.impact].endsWith(r.workload_impact.summary), `impact ${c[COL.impact]}`);
        const summary = (r.execution_overhead.measurements || {}).summary || '';
        assert.equal(c[COL.overhead], r.execution_overhead.label + summary);
        assert.equal(tr.querySelector('.eff-overhead-summary')?.textContent || '', summary);
        assert.ok(c[COL.other].includes(`$lookup: ${r.other_measurements.lookup.label}`) && c[COL.other].includes('locks: Not measured'), `other ${c[COL.other]}`);
        if (r.score === undefined) {
            assert.ok(['Unassessed', 'Not applicable'].includes(c[COL.score]), `missing score shown as ${c[COL.score]}`);
            assert.ok(['no score', 'not applicable'].includes(c[COL.band]));
        }
    });
    const fast = view.rows.find(r => r.shape_summary === 'find on fields: sku');
    const fastRow = rows.find(r => r.dataset.shapeId === fast.shape_id);
    assert.ok(fastRow, 'fast inefficient shape is visible by default');
    assert.equal(cells(fastRow)[COL.score], '0');
    assert.equal(cells(fastRow)[COL.band], 'inefficient');
    assert.ok(cells(fastRow)[COL.latency].includes('Within target; severity below threshold'), cells(fastRow)[COL.latency]);
    assert.ok(cells(fastRow)[COL.overhead].startsWith('No in-memory sort or spill'), cells(fastRow)[COL.overhead]);

    const coverage = doc.getElementById('efficiencyCoverage').textContent;
    for (const label of ['Partial evidence: 2', 'Not explained (budget): 3', 'Unavailable: 1', 'Explain failed: 1', 'Not applicable: 1']) {
        assert.ok(coverage.includes(label), `coverage line missing ${label}: ${coverage}`);
    }
    assert.ok(coverage.includes('failed_transactions') && coverage.includes('raw_injector'), 'unrecorded paths disclosed');
    const notes = [...doc.querySelectorAll('#efficiencyColumnNotes [data-column]')];
    assert.deepEqual(notes.map(n => n.dataset.column), view.columns.map(c => c.key));
    notes.forEach((n, i) => assert.equal(n.querySelector('dd').textContent, view.columns[i].explanation));
    const headers = [...doc.querySelectorAll('#efficiencyRows th[data-column]')];
    assert.deepEqual(headers.map(h => [h.dataset.column, h.textContent, h.title]), view.columns.map(c => [c.key, c.title, c.explanation]));
    assert.ok(coverage.includes('latency targets: avg ≤ 500 ms, p95 ≤ 50 ms, p99 ≤ 100 ms'), coverage);
    assert.ok(coverage.includes(`scan efficiency ${view.rating_versions.scan_efficiency}`), coverage);
    assert.ok(!/qe-1|provenance/i.test(doc.getElementById('queryAnalysisPanel').textContent), 'no internal version name or provenance wording');
    assert.deepEqual(errors, []);
});

test('efficiency and latency filters are independent; sort by impact uses impact rank', async () => {
    const data = fixture('ready');
    const view = data.efficiency_view;
    const { doc } = await loadPage(data);

    choose(doc, 'efficiencyBandFilter', 'inefficient');
    let shown = tableRows(doc).map(r => r.dataset.shapeId);
    assert.deepEqual(shown, view.rows.filter(r => r.band === 'inefficient').map(r => r.shape_id));

    choose(doc, 'efficiencyLatencyFilter', 'critical');
    assert.equal(tableRows(doc).length, 0);
    assert.ok(doc.getElementById('efficiencyEmpty').textContent.includes(`${view.rows.length} tracked shapes are hidden`));

    choose(doc, 'efficiencyBandFilter', 'all');
    shown = tableRows(doc).map(r => r.dataset.shapeId);
    assert.deepEqual(shown, view.rows.filter(r => r.severity === 'critical').map(r => r.shape_id));

    choose(doc, 'efficiencyLatencyFilter', 'all');
    choose(doc, 'efficiencyBandFilter', 'unscored');
    assert.ok(tableRows(doc).every(tr => ['Unassessed', 'Not applicable'].includes(cells(tr)[COL.score])));
    assert.equal(tableRows(doc).length, view.rows.filter(r => r.score === undefined).length);

    choose(doc, 'efficiencyBandFilter', 'all');
    choose(doc, 'efficiencySort', 'impact');
    const byImpact = [...view.rows].sort((a, b) => (a.impact_rank || 1e9) - (b.impact_rank || 1e9) || a.shape_id.localeCompare(b.shape_id));
    assert.deepEqual(tableRows(doc).map(r => r.dataset.shapeId), byImpact.map(r => r.shape_id));
    assert.ok(doc.querySelector('#efficiencyRows caption').textContent.includes('workload impact'));
});

test('detail panel shows query, checks, metrics, each dimension, data source, and ordered candidates', async () => {
    const data = fixture('ready');
    const view = data.efficiency_view;
    const { doc } = await loadPage(data);
    const fast = view.rows.find(r => r.shape_summary === 'find on fields: sku');
    const btn = tableRows(doc).find(r => r.dataset.shapeId === fast.shape_id).querySelector('button');
    btn.click();
    const detail = doc.getElementById('efficiencyDetail');
    assert.equal(detail.style.display, 'block');
    const text = detail.textContent;
    assert.equal(doc.getElementById('efficiencyDetailQuery').textContent, fast.query);
    assert.ok(!text.includes('secret-'), 'no literal values');
    assert.ok(text.includes(fast.rating_explanation));
    assert.ok(text.includes('documents examined per result: value 25000, penalty 1.00'));
    assert.ok(text.includes('documents examined 50000'));
    assert.ok(text.includes(`Lowest-scoring sample: ${fast.determining_sample}`));
    assert.ok(doc.getElementById('efficiencyDetailLatency').textContent.includes('p95 target 50 ms: met'));
    assert.ok(doc.getElementById('efficiencyDetailImpact').textContent.endsWith(fast.workload_impact.summary));
    const rec = data.recommendations.find(r => r.rank === fast.recommendation_ranks[0]);
    const keys = [...detail.querySelectorAll('.eff-candidate-keys li')].map(li => li.textContent.trim());
    assert.deepEqual(keys, rec.candidate_keys);
    assert.ok(text.includes(`validation: ${rec.validation_step}`));
    assert.ok(text.includes(`tradeoffs: ${rec.tradeoffs}`));
    assert.equal(tableRows(doc).find(r => r.dataset.shapeId === fast.shape_id).querySelector('button').getAttribute('aria-expanded'), 'true');

    const slow = view.rows.find(r => r.shape_summary === 'find on fields: email');
    tableRows(doc).find(r => r.dataset.shapeId === slow.shape_id).querySelector('button').click();
    assert.ok(detail.textContent.includes('Scan efficiency 100'));
    const overhead = doc.getElementById('efficiencyDetailOverhead').textContent;
    assert.ok(overhead.includes('In-memory sort') && overhead.includes('median sample: SORT stage'), `sort reported as overhead beside a 100 score: ${overhead}`);
    const m = slow.execution_overhead.measurements;
    assert.ok(m.summary.includes('differs by sample'), `fixture samples must differ: ${m.summary}`);
    assert.ok(doc.getElementById('efficiencyDetailMeasurementsHead').textContent.includes(`Sort and spill measurements (${m.status}): ${m.summary}`));
    const lines = [...doc.querySelectorAll('#efficiencyDetailMeasurements li')].map(li => li.textContent);
    assert.deepEqual(lines, m.details);
    assert.ok(lines.some(l => l.startsWith('Samples differ (median vs p95)')), lines.join('\n'));
    const notes = [...doc.querySelectorAll('#efficiencyDetailMeasurementNotes li')].map(li => li.textContent);
    assert.deepEqual(notes, m.notes);
    const latency = doc.getElementById('efficiencyDetailLatency').textContent;
    assert.ok(latency.includes('Above target (avg 900.1 ms > 500 ms)') && latency.includes('p95 target 50 ms: not_measured'), latency);

    const lookup = view.rows.find(r => r.operation === 'aggregate');
    tableRows(doc).find(r => r.dataset.shapeId === lookup.shape_id).querySelector('button').click();
    assert.ok(detail.textContent.includes('Scan efficiency ≤ 100'));
    assert.ok(doc.getElementById('efficiencyDetailOther').textContent.includes(`$lookup: ${lookup.other_measurements.lookup.label}`));
    assert.equal(lookup.other_measurements.lookup.status, 'measured');

    const excluded = view.rows.find(r => r.coverage === 'budget_excluded');
    tableRows(doc).find(r => r.dataset.shapeId === excluded.shape_id).querySelector('button').click();
    assert.ok(detail.textContent.includes('Scan efficiency Unassessed') && detail.textContent.includes('not_selected_budget'));
    assert.ok(doc.getElementById('efficiencyDetailOverhead').textContent.includes('Not assessed (no explain)'), 'no explain must not read as no overhead');

    detail.querySelector('button').click();
    assert.equal(detail.style.display, 'none');
});

test('clicking a row opens its detail panel and the table explains this', async () => {
    const data = fixture('ready');
    const view = data.efficiency_view;
    const { doc, errors } = await loadPage(data);
    assert.ok(/click any row/i.test(doc.getElementById('efficiencyRowHint').textContent));
    const detail = doc.getElementById('efficiencyDetail');
    assert.equal(detail.style.display, 'none');

    const target = view.rows.find(r => r.shape_summary === 'find on fields: sku');
    tableRows(doc).find(r => r.dataset.shapeId === target.shape_id).querySelector('.eff-col-shape').click();
    assert.equal(detail.style.display, 'block');
    assert.equal(doc.getElementById('efficiencyDetailQuery').textContent, target.query);
    const row = tableRows(doc).find(r => r.dataset.shapeId === target.shape_id);
    assert.ok(row.classList.contains('selected'));
    assert.equal(row.querySelector('button').getAttribute('aria-expanded'), 'true');

    const other = view.rows.find(r => r.shape_summary === 'find on fields: email');
    tableRows(doc).find(r => r.dataset.shapeId === other.shape_id).click();
    assert.equal(doc.getElementById('efficiencyDetailQuery').textContent, other.query);
    assert.deepEqual(errors, []);
});

test('recommendations paginate with a total count instead of a silent cutoff', async () => {
    const data = fixture('ready');
    const total = data.recommendations.length;
    assert.ok(total > 8);
    const { doc } = await loadPage(data);
    const count = () => doc.querySelectorAll('#insightsRecommendations .insights-row').length;
    assert.equal(count(), 8);
    assert.equal(doc.getElementById('recommendationsCount').textContent, `Showing 8 of ${total} recommendations`);
    doc.getElementById('recommendationsShowMore').click();
    assert.equal(count(), Math.min(16, total));
    assert.equal(doc.getElementById('recommendationsCount').textContent, `Showing ${Math.min(16, total)} of ${total} recommendations`);
    if (total <= 16) assert.equal(doc.getElementById('recommendationsShowMore'), null);
    const titles = [...doc.querySelectorAll('#insightsRecommendations .insights-row-title')].map(e => e.textContent);
    data.recommendations.forEach((r, i) => assert.ok(titles[i].startsWith(`#${r.rank} ${r.title}`)));
});

test('Download Summary exports the same efficiency data and the visible view state', async () => {
    const data = fixture('ready');
    const { w, doc } = await loadPage(data);
    w.eval('finalReportData = { config: {}, stats: {} };');
    choose(doc, 'efficiencySort', 'impact');
    choose(doc, 'efficiencyBandFilter', 'inefficient');
    await w.downloadSummaryReport();
    assert.equal(w.downloads.length, 1);
    const { href, name } = w.downloads[0];
    assert.match(name, /^plgm_summary_\d+\.json$/);
    const exported = JSON.parse(decodeURIComponent(href.replace(/^data:text\/json;charset=utf-8,/, '')));
    assert.deepEqual(exported.insights.efficiency_view, data.efficiency_view);
    assert.deepEqual(exported.insights.recommendations, data.recommendations);
    assert.deepEqual(exported.insights.explain_coverage, data.explain_coverage);
    const st = exported.efficiency_view_state;
    assert.equal(st.sort, 'impact');
    assert.equal(st.efficiency_filter, 'inefficient');
    assert.deepEqual(st.visible_shape_ids, tableRows(doc).map(r => r.dataset.shapeId));
    assert.equal(st.total_rows, data.efficiency_view.rows.length);
    assert.equal(st.recommendations_total, data.recommendations.length);
});

for (const mode of ['failed', 'disabled']) {
    test(`${mode} explain state shows every shape as unassessed, never 0 or 100`, async () => {
        const data = fixture(mode);
        const { doc, errors } = await loadPage(data);
        const rows = tableRows(doc);
        assert.equal(rows.length, data.efficiency_view.rows.length);
        assert.ok(rows.length > 0);
        for (const tr of rows) {
            const score = cells(tr)[COL.score];
            assert.ok(['Unassessed', 'Not applicable'].includes(score), score);
        }
        assert.ok(doc.getElementById('efficiencyCoverage').textContent.includes(`Status: ${data.efficiency_view.status}`));
        assert.deepEqual(errors, []);
    });
}

test('empty run renders an explicit empty state', async () => {
    const data = fixture('empty');
    const { doc, errors } = await loadPage(data);
    assert.equal(tableRows(doc).length, 0);
    assert.match(doc.getElementById('efficiencyEmpty').textContent, /No tracked shapes \(not_run/);
    assert.equal(doc.getElementById('insightsRecommendationsPager').textContent, '');
    assert.deepEqual(errors, []);
});

test('insights fetch error renders an unavailable state without numbers', async () => {
    const { doc, errors } = await loadPage(new Error('network down'));
    assert.equal(doc.getElementById('insightsPill').textContent, 'No Data');
    assert.match(doc.getElementById('efficiencyEmpty').textContent, /No efficiency data was returned/);
    assert.equal(tableRows(doc).length, 0);
    assert.deepEqual(errors, []);
});

test('query and index analysis has its own dashboard tab and does not leak onto other tabs', async () => {
    const data = fixture('ready');
    const { w, doc, errors } = await loadPage(data);
    const insights = doc.getElementById('insightsPanel');
    const queries = doc.getElementById('queryAnalysisPanel');
    for (const id of ['efficiencyTable', 'insightsSlowQueries', 'insightsIndexIssues', 'insightsRecommendations']) {
        assert.ok(queries.contains(doc.getElementById(id)), `${id} is in the query & index tab`);
        assert.ok(!insights.contains(doc.getElementById(id)), `${id} is not in the insights tab`);
    }
    for (const id of ['insightsTopFindings', 'insightsImpact', 'insightsCollections', 'insightsTabIteration', 'insightsTabTimeline']) {
        assert.ok(insights.contains(doc.getElementById(id)), `${id} stays in the insights tab`);
    }

    const visible = el => el.style.display !== 'none';
    w.showDashTab('overview');
    w.renderFinalInsights({ metadata: { status: 'pending' }, summary: {} });
    await w.fetchFinalInsights();
    assert.ok(!visible(insights) && !visible(queries), 'post-run panels stay hidden on Live Metrics after results arrive');
    w.showDashTab('latency');
    assert.ok(!visible(insights) && !visible(queries));

    doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').click();
    assert.ok(visible(queries) && !visible(insights));
    assert.ok(doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').classList.contains('active'));
    assert.equal(doc.getElementById('queryAnalysisPill').textContent, doc.getElementById('insightsPill').textContent);
    assert.ok(doc.getElementById('queryAnalysisStatus').textContent.includes('explain coverage'));

    doc.getElementById('qaTabFindingsBtn').click();
    assert.equal(doc.getElementById('queryAnalysisTabFindings').style.display, 'block');
    assert.equal(doc.getElementById('queryAnalysisTabEfficiency').style.display, 'none');
    assert.equal(doc.getElementById('qaTabFindingsBtn').getAttribute('aria-pressed'), 'true');

    doc.querySelector('.dash-tab-btn[data-dashtab="insights"]').click();
    assert.ok(visible(insights) && !visible(queries));
    assert.deepEqual(errors, []);
});

test('existing severity and impact sections still render', async () => {
    const data = fixture('ready');
    const { doc } = await loadPage(data);
    assert.equal(doc.querySelectorAll('#insightsImpact .insights-row').length, data.impact.groups.length);
    assert.equal(doc.querySelectorAll('#insightsSlowQueries .insights-finding-card').length, Math.min(8, data.slow_queries.length));
    assert.ok(doc.getElementById('insightsTopFindings').textContent.includes('Top impact'));
});
