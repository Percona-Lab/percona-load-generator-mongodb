// End-to-end acceptance fixture (TestAcceptanceCaptureToUIConsistency): every
// principal scenario rendered by the real dashboard and exported unchanged.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage, exportedSummary } from './harness.mjs';

const tableRows = doc => [...doc.querySelectorAll('#efficiencyRows tbody tr')];
const cells = tr => [...tr.querySelectorAll('td')].map(td => td.textContent.trim());
const COL = { score: 2, band: 3, coverage: 9 };

const EXPECTED = {
    'find on fields: s': { label: '0', band: 'inefficient' },
    'find on fields: _id': { label: '100', band: 'efficient' },
    'find on fields: a (limit 5)': { label: '100', band: 'efficient' },
    'find on fields: b (covered)': { label: '100', band: 'efficient', observation: 'covered' },
    'find on fields: a, s': { label: '33', band: 'inefficient' },
    'find on fields: a': { label: '100', band: 'efficient', observation: 'zero_result' },
    'aggregate $match a, $group': { label: '100', band: 'efficient', observation: 'aggregate_reduction' },
    'updateMany on fields: s': { label: '38', band: 'inefficient' },
    'deleteOne on fields: a': { label: '100', band: 'efficient' },
    'aggregate $match a, $group, $out': { coverage: 'partial' },
    'find on fields: t': { label: 'Unassessed', coverage: 'failed' },
    'find on fields: legacy': { label: 'Unassessed', coverage: 'unavailable' },
    'insert': { label: 'Not applicable', coverage: 'not_applicable' },
};

test('every acceptance scenario renders with its rating, coverage, and evidence', async () => {
    const data = fixture('acceptance');
    const view = data.efficiency_view;
    const { doc, errors } = await loadPage(data);
    const rows = tableRows(doc);
    assert.equal(rows.length, Object.keys(EXPECTED).length);
    assert.deepEqual(rows.map(r => r.dataset.shapeId), view.rows.map(r => r.shape_id));
    const modal = doc.getElementById('shapeDetailModal');
    const detail = doc.getElementById('shapeDetailBody');
    for (const [summary, want] of Object.entries(EXPECTED)) {
        const r = view.rows.find(x => x.shape_summary === summary);
        assert.ok(r, summary);
        const tr = rows.find(x => x.dataset.shapeId === r.shape_id);
        const c = cells(tr);
        if (want.label) assert.equal(c[COL.score], want.label, summary);
        if (want.band) assert.equal(c[COL.band], want.band, summary);
        if (want.coverage) assert.equal(r.coverage, want.coverage, summary);
        assert.ok(c[COL.coverage].startsWith(r.coverage_label), summary);
        if (want.coverage === 'partial') assert.ok(c[COL.score].startsWith('≤ ') && c[COL.band] !== 'efficient', summary);

        tr.querySelector('button').click();
        assert.equal(modal.style.display, 'flex');
        const text = detail.textContent;
        assert.ok(text.includes(`Scan efficiency ${r.score_label}`), `${summary}: detail shows ${r.score_label}`);
        if (want.observation) {
            assert.ok(r.observations.includes(want.observation), summary);
            for (const note of r.observation_notes || []) assert.ok(text.includes(note), `${summary}: ${note}`);
        }
        if (r.score === undefined && r.coverage_reason) assert.ok(text.includes(r.coverage_reason), `${summary}: reason shown`);
        for (const rank of r.recommendation_ranks || []) {
            const rec = data.recommendations.find(x => x.rank === rank);
            for (const key of rec.candidate_keys || []) assert.ok(text.includes(key), `${summary}: candidate ${key}`);
        }
        doc.getElementById('shapeDetailX').click();
        assert.equal(modal.style.display, 'none');
    }
    assert.deepEqual(errors, []);
});

test('acceptance Download Summary matches the API and severity/impact views are unchanged', async () => {
    const data = fixture('acceptance');
    const { w, doc } = await loadPage(data);
    assert.equal(doc.querySelectorAll('#insightsImpact .insights-row').length, data.impact.groups.length);
    assert.equal(doc.querySelectorAll('#insightsSlowQueries .insights-finding-card').length, Math.min(8, data.slow_queries.length));
    w.eval('finalReportData = { config: {}, stats: {} };');
    await w.downloadSummaryReport();
    const exported = exportedSummary(w);
    for (const key of ['efficiency_view', 'explain_coverage', 'efficiency_coverage', 'recommendations', 'impact', 'slow_queries', 'index_inventory', 'trends']) {
        assert.deepEqual(exported.insights[key], data[key], key);
    }
    assert.equal(exported.efficiency_view_state.total_rows, data.efficiency_view.rows.length);
});
