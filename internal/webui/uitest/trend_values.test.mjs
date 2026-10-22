// DOM tests for stored query values in Trends (insights_trend_store_query_values):
// badge column, column notes, filter, side-by-side details, not-recorded rows,
// export confirmation, and the setting. insights_trend_values.json is real API
// output written by TestUIFixturesMatchAPITrendValues.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage, choose, exportedSummary } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));
const rows = doc => [...doc.querySelectorAll('#trendRows tbody tr[data-shape-id]')];
const TEXT = { same_values: 'same values', overlapping: 'overlapping', different_values: 'different values', not_recorded: 'not recorded' };

function openTrends(doc) {
    doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').click();
    doc.getElementById('qaTabTrendsBtn').click();
}

async function valuesPage() {
    const data = fixture('trend_values');
    const p = await loadPage(data);
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    openTrends(p.doc);
    const row = summary => data.trends.rows.find(r => r.shape_summary === summary);
    return { ...p, data, row };
}

test('each trend row shows a query-values badge and the column is explained', async () => {
    const { doc, data, errors } = await valuesPage();
    const head = [...doc.querySelectorAll('#trendRows thead th')].map(th => th.textContent);
    assert.ok(head.includes('Query values'));
    const trs = rows(doc);
    assert.equal(trs.length, data.trends.rows.length);
    trs.forEach((tr, i) => {
        const r = data.trends.rows[i];
        const cell = tr.querySelector('.trend-values');
        assert.equal(cell.querySelector('.insight-badge').textContent, TEXT[r.query_values.state], r.shape_summary);
        assert.ok(cell.textContent.includes(r.query_values.label), r.shape_summary);
    });
    const states = new Set(data.trends.rows.map(r => r.query_values.state));
    assert.deepEqual([...states].sort(), Object.keys(TEXT).sort(), 'the fixture covers every state');

    const notes = doc.getElementById('trendValuesNotes');
    assert.notEqual(notes.style.display, 'none');
    const col = doc.querySelector('#trendColumnNotes [data-column="query_values"]').textContent;
    assert.ok(col.includes('Annotation only') && col.includes('cache state may differ') && col.includes('does not change the improved/regressed labels'));
    for (const k of Object.keys(TEXT)) assert.ok(doc.querySelector(`#trendColumnNotes [data-values-state="${k}"]`), k);
    const sum = doc.getElementById('trendSummary').textContent;
    assert.ok(sum.includes('Query values (annotation only): different_values: 1'));
    assert.ok(sum.includes(data.trends.query_values_notice));
    assert.deepEqual(errors, []);
});

test('without stored values there is no query-values column, note, or count', async () => {
    const data = fixture('trends');
    const { doc, errors } = await loadPage(data);
    openTrends(doc);
    assert.ok(![...doc.querySelectorAll('#trendRows thead th')].some(th => th.textContent === 'Query values'));
    assert.equal(doc.querySelectorAll('#trendRows .trend-values').length, 0);
    assert.equal(doc.getElementById('trendValuesNotes').style.display, 'none');
    assert.ok(!doc.getElementById('trendSummary').textContent.includes('Query values'));
    assert.deepEqual(errors, []);
});

test('the trend filter selects rows by query-values state, missing values count as not recorded', async () => {
    const { doc, data, errors } = await valuesPage();
    for (const state of Object.keys(TEXT)) {
        choose(doc, 'trendStateFilter', `values_${state}`);
        const want = data.trends.rows.filter(r => r.query_values.state === state).map(r => r.shape_id);
        assert.deepEqual(rows(doc).map(tr => tr.dataset.shapeId), want, state);
        assert.equal(want.length, data.trends.counts.query_values[state], state);
    }
    const plain = fixture('trends');
    const p = await loadPage(plain);
    openTrends(p.doc);
    choose(p.doc, 'trendStateFilter', 'values_not_recorded');
    assert.equal(rows(p.doc).length, plain.trends.rows.length);
    choose(p.doc, 'trendStateFilter', 'values_same_values');
    assert.equal(rows(p.doc).length, 0);
    assert.deepEqual(errors, []);
});

test('details show current and baseline samples side by side with matching samples marked', async () => {
    const { doc, row, errors } = await valuesPage();
    const open = r => rows(doc).find(tr => tr.dataset.shapeId === r.shape_id).querySelector('button').click();

    const tag = row('find on fields: tag');
    open(tag);
    const sides = [...doc.querySelectorAll('#trendDetailValues .trend-values-side')];
    assert.deepEqual(sides.map(s => s.dataset.side), ['current', 'baseline']);
    assert.ok(sides[1].textContent.includes(`Baseline run (${tag.query_values.baseline_run_label})`));
    const cur = sides[0].querySelectorAll('.trend-value-sample');
    assert.equal(cur.length, tag.query_values.current.samples.length);
    assert.equal(sides[0].querySelectorAll('.trend-value-match').length, tag.query_values.matching_samples);
    assert.equal(sides[0].querySelectorAll('.trend-value-match-mark').length, tag.query_values.matching_samples);
    const first = tag.query_values.current.samples[0];
    assert.ok(cur[0].textContent.includes(`Role: ${first.role}`) && cur[0].textContent.includes('explained'));
    assert.ok(cur[0].textContent.includes(`client latency ${first.client_latency_ms} ms`));
    assert.ok(sides[0].textContent.includes('retained, not explained'));
    assert.equal(cur[0].querySelector('pre').textContent, doc.defaultView.prettyExtJSON(first.command));
    assert.match(cur[0].querySelector('pre').textContent, /^\{\n {2}"find": "orders",\n {2}"filter": \{\n/);
    assert.ok(doc.getElementById('trendDetailValues').textContent.includes('Annotation only'));
    doc.getElementById('shapeDetailX').click();

    const status = row('find on fields: status');
    open(status);
    assert.equal(doc.querySelector('#trendDetailValues .insight-badge').textContent, 'different values');
    assert.equal(doc.querySelectorAll('#trendDetailValues .trend-value-match').length, 0);
    assert.ok(doc.getElementById('trendDetailValues').textContent.includes('secret-changed-status-'));
    assert.deepEqual(errors, []);
});

test('a not_recorded row explains which run has no values', async () => {
    const { doc, row, errors } = await valuesPage();
    const ins = row('insert');
    const tr = rows(doc).find(t => t.dataset.shapeId === ins.shape_id);
    assert.equal(tr.querySelector('.trend-values .insight-badge').textContent, 'not recorded');
    tr.querySelector('button').click();
    const missing = [...doc.querySelectorAll('#trendDetailValues .trend-values-missing')].map(e => e.textContent);
    assert.deepEqual(missing, [ins.query_values.current_reason, ins.query_values.baseline_reason]);

    const plain = fixture('trends');
    const p = await loadPage(plain);
    openTrends(p.doc);
    rows(p.doc)[0].querySelector('button').click();
    assert.equal(p.doc.getElementById('trendDetailValues'), null, 'no values section when no run stored values');
    assert.deepEqual(errors, []);
});

test('the pretty-printer keeps key order and string contents', async () => {
    const { w } = await loadPage(fixture('trends'));
    const out = w.prettyExtJSON('{"sort":{"b":1,"a":{"$numberInt":"-1"}},"x":[],"y":{},"s":"a,{b\\":c"}');
    assert.equal(out, '{\n  "sort": {\n    "b": 1,\n    "a": {\n      "$numberInt": "-1"\n    }\n  },\n  "x": [],\n  "y": {},\n  "s": "a,{b\\":c"\n}');
});

test('Download Summary marks reports that carry query values', async () => {
    const { w, data } = await valuesPage();
    w.eval('finalReportData = { config: {}, stats: {} };');
    await w.downloadSummaryReport();
    const exported = exportedSummary(w);
    assert.equal(exported.contains_query_values, true);
    assert.deepEqual(exported.insights.trends, data.trends);

    const p = await loadPage(fixture('trends'));
    p.w.eval('finalReportData = { config: {}, stats: {} };');
    await p.w.downloadSummaryReport();
    assert.ok(!('contains_query_values' in exportedSummary(p.w)));
});

async function historyPage(runs, exp) {
    const calls = [];
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true } }),
        '/api/trend-baselines/export': async url => { calls.push(url); return { body: exp }; },
        '/api/trend-baselines': async () => ({ body: { settings: {}, store: { status: 'ok' }, runs } }),
    });
    await settle();
    p.w.revealSetting('trendHistoryRuns', { focus: false });
    await settle();
    return { ...p, calls };
}

test('exporting runs with query values needs a second click to confirm', async () => {
    const exp = { format: 'plgm-trend-export', exported_at: '2026-10-16T09:00:00Z', contains_query_values: true, runs: [{ run_id: 'r1' }], shapes: [] };
    const { w, doc, calls, errors } = await historyPage([{ run_id: 'r1', label: 'with values', query_values_stored: true }, { run_id: 'r2', label: 'plain' }], exp);
    const status = () => doc.getElementById('trendHistoryStatus').textContent;
    const runRow = id => doc.querySelector(`#trendRunsTable tr[data-run-id="${id}"]`);
    assert.ok(runRow('r1').querySelector('.trend-run-values').textContent.includes('contains literal query values'));
    assert.equal(runRow('r2').querySelector('.trend-run-values'), null);

    await w.exportTrendRuns(false);
    assert.equal(calls.length, 0, 'nothing is fetched before confirmation');
    assert.equal(w.downloads.length, 0);
    assert.match(status(), /1 of these runs contain literal query values.*Click Export again to confirm/);
    await w.exportTrendRuns(false);
    assert.equal(w.downloads.length, 1);
    assert.match(status(), /Exported 1 runs\. ⚠ The file contains literal query values \(contains_query_values: true\)/);

    runRow('r2').querySelector('.trend-run-select').checked = true;
    calls.length = 0;
    exp.contains_query_values = false;
    await w.exportTrendRuns(true);
    assert.equal(calls.length, 1, 'runs without values export with one click');
    assert.equal(w.downloads.length, 2);
    assert.match(status(), /redacted/);
    assert.deepEqual(errors, []);
});

test('an export response with values is not downloaded until confirmed, even if the run list missed it', async () => {
    const exp = { format: 'plgm-trend-export', exported_at: '2026-10-16T09:00:00Z', contains_query_values: true, runs: [{ run_id: 'r1' }], shapes: [] };
    const { w, doc, calls, errors } = await historyPage([{ run_id: 'r1', label: 'stale' }], exp);
    await w.exportTrendRuns(false);
    assert.equal(calls.length, 1);
    assert.equal(w.downloads.length, 0);
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /This export contains literal query values.*Click Export again/);
    await w.exportTrendRuns(false);
    assert.equal(w.downloads.length, 1);
    assert.deepEqual(errors, []);
});

test('the Store query values setting is off by default and explains On and Off', async () => {
    const plain = await loadPage(fixture('trends'), { '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }) });
    await settle();
    const box = plain.doc.getElementById('trendStoreQueryValues');
    assert.equal(box.name, 'insights_trend_store_query_values');
    assert.equal(box.checked, false);
    assert.ok(box.closest('#configForm'));
    assert.equal(box.closest('.tab-content').id, 'tab-trends');
    const note = plain.doc.getElementById('trendStoreQueryValuesNote').textContent;
    for (const s of ['On:', 'Off (default):', 'real query values', '16 KiB', 'ordinary generated runs', 'Record & Replay', 'Replay Comparison', 'insights_trend_store_query_values', 'PLGM_INSIGHTS_TREND_STORE_QUERY_VALUES']) {
        assert.ok(note.includes(s), s);
    }
    const on = await loadPage(fixture('trends'), { '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsTrendStoreQueryValues: true } }) });
    await settle();
    assert.equal(on.doc.getElementById('trendStoreQueryValues').checked, true);
});
