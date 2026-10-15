// DOM tests for the Trends & History settings tab (settings, run list, rename,
// pin, delete, export, import, reset) and the Trends view baseline picker and
// multi-run matrix. trend_history.json is real API output for three runs on
// one store, written by TestUIFixturesMatchAPITrendHistory.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFixture, loadPage, choose, exportedSummary } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));
const H = () => readFixture('trend_history.json');

async function historyPage(routes = {}, cfg = {}) {
    const h = H();
    const calls = [];
    const p = await loadPage(h.insights, {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true, ...cfg } }),
        '/api/trend-baselines': async (url, init) => {
            calls.push({ url, method: (init.method || 'GET').toUpperCase(), body: init.body });
            for (const [prefix, fn] of Object.entries(routes)) {
                if (url.startsWith(prefix)) return fn(url, init);
            }
            return { body: h.baselines };
        },
        '/api/trends/compare': async (url, init) => {
            calls.push({ url, method: 'GET' });
            for (const [prefix, fn] of Object.entries(routes)) {
                if (url.startsWith(prefix)) return fn(url, init);
            }
            return url.includes('runs=') ? { body: h.matrix } : { body: h.compare_run };
        },
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return { ...p, h, calls };
}

async function openHistoryTab(doc) {
    doc.querySelector(`.tab-btn[onclick*="'tab-trends'"]`).click();
    await settle();
}

const runRows = doc => [...doc.querySelectorAll('#trendRunsTable tbody tr[data-run-id]')];

test('the Trends & History tab holds every trend setting with its default and config/env key', async () => {
    const { doc, errors } = await historyPage();
    const tabOf = sel => doc.querySelector(sel).closest('.tab-content').id;
    const want = { trend_history_runs: '5', trend_max_age_days: '30', trend_default_comparison: 'previous', trend_run_label: '', trend_baseline_store: '' };
    for (const [name, v] of Object.entries(want)) {
        const el = doc.querySelector(`[name="${name}"]`);
        assert.equal(tabOf(`[name="${name}"]`), 'tab-trends', name);
        assert.equal(el.value, v, name);
        assert.ok(el.closest('#configForm'), `${name} is submitted with the run`);
        const note = el.closest('.form-group').textContent;
        assert.ok(note.includes(name) && note.includes(`PLGM_${name.toUpperCase()}`), `${name} documents its yaml key and env var`);
    }
    const runs = doc.querySelector('[name="trend_history_runs"]');
    assert.equal(runs.min, '1');
    assert.equal(runs.max, '50');
    assert.ok(doc.getElementById('tab-trends').textContent.includes('Default 5, 1–50'));
    assert.ok(doc.getElementById('tab-trends').textContent.includes('0 = never'));
    assert.ok(doc.getElementById('tab-trends').textContent.includes('off'));
    assert.equal(doc.querySelector('.tab-btn[onclick*="tab-trends"]').textContent, 'Trends & History');
    assert.notEqual(doc.getElementById('queryAnalysisTabTrends').closest('#tab-trends'), doc.getElementById('tab-trends'), 'results view is separate');
    assert.deepEqual(errors, []);
});

test('loaded config values fill the trend settings, including 0 = never', async () => {
    const { doc, errors } = await historyPage({}, {
        TrendHistoryRuns: 12, TrendMaxAgeDays: 0, TrendDefaultComparison: 'pinned', TrendRunLabel: 'lab-a', TrendBaselineStore: 'off',
    });
    const v = n => doc.querySelector(`[name="${n}"]`).value;
    assert.deepEqual([v('trend_history_runs'), v('trend_max_age_days'), v('trend_default_comparison'), v('trend_run_label'), v('trend_baseline_store')],
        ['12', '0', 'pinned', 'lab-a', 'off']);
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.equal(fd.get('trend_history_runs'), '12');
    assert.equal(fd.get('trend_baseline_store'), 'off');
    assert.equal(fd.get('trend_run_label'), 'lab-a');
    assert.deepEqual(errors, []);
});

test('invalid trend settings block the run and open the Trends & History tab', async () => {
    const { w, doc, errors } = await historyPage();
    for (const [name, value, msg] of [
        ['trend_history_runs', '0', /Runs to keep/], ['trend_history_runs', '51', /Runs to keep/], ['trend_history_runs', '1.5', /Runs to keep/],
        ['trend_max_age_days', '-1', /Age expiry/], ['trend_max_age_days', '3651', /Age expiry/],
        ['trend_run_label', 'é'.repeat(33), /at most 64 bytes/],
    ]) {
        const el = doc.querySelector(`[name="${name}"]`);
        const before = el.value;
        el.value = value;
        assert.equal(w.validateConfigForm(), false, `${name}=${value}`);
        assert.ok(doc.getElementById('tab-trends').classList.contains('active'));
        assert.match(doc.getElementById('configInlineError').textContent, msg);
        el.value = before;
    }
    doc.querySelector('[name="trend_max_age_days"]').value = '0';
    assert.equal(w.validateTrendSettings(), '', '0 days (never) is valid');
    assert.deepEqual(errors, []);
});

test('opening the tab lists stored runs with label, server, time, source, and pin state', async () => {
    const { doc, h, calls, errors } = await historyPage();
    assert.equal(calls.length, 0, 'runs load only when the tab opens');
    await openHistoryTab(doc);
    assert.equal(calls[0].method, 'GET');
    const trs = runRows(doc);
    assert.deepEqual(trs.map(tr => tr.dataset.runId), h.baselines.runs.map(r => r.run_id));
    trs.forEach((tr, i) => {
        const r = h.baselines.runs[i];
        assert.equal(tr.querySelector('.trend-run-label').value, r.label);
        assert.ok(tr.textContent.includes(r.server_versions[0]));
        assert.equal(tr.querySelector('.trend-run-pin').getAttribute('aria-pressed'), String(!!r.pinned));
        assert.ok(tr.textContent.includes('this machine'));
    });
    const store = doc.getElementById('trendHistoryStore').textContent;
    assert.ok(store.includes('3 runs, 1 of max 10 pinned') && store.includes('Keeping 5 unpinned runs') && store.includes('30 days'), store);
    for (const el of doc.querySelectorAll('#trendRunsTable input')) {
        assert.equal(el.name, '', 'run list inputs are not part of the run form');
    }
    assert.deepEqual(errors, []);
});

test('rename, pin, and delete call the run endpoint and reload the list', async () => {
    const { w, doc, calls, errors } = await historyPage({
        '/api/trend-baselines/runs/': async (url, init) => {
            const b = init.body ? JSON.parse(init.body) : {};
            return { body: { run: { run_id: 'fixture-run-2', label: b.label || 'mongo-8.0.4', pinned: !!b.pinned }, store: {} } };
        },
    });
    await openHistoryTab(doc);
    const row = () => runRows(doc).find(tr => tr.dataset.runId === 'fixture-run-2');
    row().querySelector('.trend-run-label').value = '  after index ';
    row().querySelector('.trend-run-rename').click();
    await settle();
    const patch = calls.find(c => c.method === 'PATCH');
    assert.equal(patch.url, '/api/trend-baselines/runs/fixture-run-2');
    assert.deepEqual(JSON.parse(patch.body), { label: 'after index' });
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Renamed to "after index"/);
    assert.equal(calls.at(-1).method, 'GET', 'list reloads');

    row().querySelector('.trend-run-pin').click();
    await settle();
    assert.deepEqual(JSON.parse(calls.filter(c => c.method === 'PATCH').at(-1).body), { pinned: true });
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /never expires/);

    row().querySelector('.trend-run-delete').click();
    assert.equal(calls.filter(c => c.method === 'DELETE').length, 0, 'first click only arms the delete');
    assert.equal(row().querySelector('.trend-run-delete').textContent, 'Confirm delete');
    await w.deleteTrendRun('fixture-run-2');
    const del = calls.find(c => c.method === 'DELETE');
    assert.equal(del.url, '/api/trend-baselines/runs/fixture-run-2');
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Run deleted/);
    assert.deepEqual(errors, []);
});

test('a rejected change reports the server error and that nothing changed', async () => {
    const { doc, errors } = await historyPage({
        '/api/trend-baselines/runs/': async () => ({ status: 409, body: { error: 'trend run pin limit reached: at most 10 runs can be pinned' } }),
    });
    await openHistoryTab(doc);
    runRows(doc)[0].querySelector('.trend-run-pin').click();
    await settle();
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Change failed: trend run pin limit reached.*not changed/);
    assert.deepEqual(errors, []);
});

test('export selected or all downloads the redacted export file', async () => {
    const exp = { format: 'plgm-trend-export', schema_version: 'tb-2', exported_at: '2026-10-15T09:00:00Z', runs: [{ run_id: 'fixture-run-1' }], shapes: [] };
    const { w, doc, calls, errors } = await historyPage({ '/api/trend-baselines/export': async () => ({ body: exp }) });
    await openHistoryTab(doc);
    await w.exportTrendRuns(true);
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Select at least one run/);
    assert.equal(w.downloads.length, 0);

    runRows(doc).find(tr => tr.dataset.runId === 'fixture-run-1').querySelector('.trend-run-select').checked = true;
    runRows(doc).find(tr => tr.dataset.runId === 'fixture-run-3').querySelector('.trend-run-select').checked = true;
    await w.exportTrendRuns(true);
    assert.equal(calls.at(-1).url, '/api/trend-baselines/export?runs=fixture-run-3,fixture-run-1');
    const dl = w.downloads.at(-1);
    assert.equal(dl.name, 'plgm-trend-runs-20261015T090000Z.json');
    assert.deepEqual(JSON.parse(decodeURIComponent(dl.href.replace(/^data:application\/json;charset=utf-8,/, ''))), exp);
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Exported 1 runs/);

    await w.exportTrendRuns(false);
    assert.equal(calls.at(-1).url, '/api/trend-baselines/export');
    assert.deepEqual(errors, []);
});

function setImportFile(w, doc, text, name = 'lab-b.json') {
    const input = doc.getElementById('trendImportFile');
    const file = new w.File([text], name, { type: 'application/json' });
    Object.defineProperty(input, 'files', { value: [file], configurable: true });
    return file;
}

test('import uploads the file and reports added, skipped, not kept, and warnings', async () => {
    let uploaded = null;
    const result = {
        source: 'lab-b',
        added: [{ run_id: 'b-1', label: 'mongo-7.0.12', source: 'lab-b' }],
        skipped: [{ run_id: 'fixture-run-1', label: 'before index', reason: 'duplicate: a run with this id is already stored' }],
        not_kept: [{ run_id: 'b-0', label: 'old', reason: 'older than the newest 5 unpinned runs kept (trend_history_runs); raise the limit or import it pinned' }],
        warnings: ['fields outside the trend schema were ignored'],
    };
    const { w, doc, calls, errors } = await historyPage({
        '/api/trend-baselines/import': async (url, init) => { uploaded = init.body; return { body: result }; },
    });
    await openHistoryTab(doc);
    await w.importTrendRuns();
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Choose a trend export file/);
    assert.equal(uploaded, null);

    setImportFile(w, doc, '{"schema_version":"tb-2"}');
    await w.importTrendRuns();
    assert.ok(uploaded instanceof w.FormData && uploaded.get('file').name === 'lab-b.json');
    const msg = doc.getElementById('trendHistoryStatus').textContent;
    for (const want of ['Imported from lab-b: 1 added, 1 skipped, 1 not kept.', 'before index (duplicate', 'Not kept: old', 'fields outside the trend schema were ignored']) {
        assert.ok(msg.includes(want), `${want} in ${msg}`);
    }
    assert.equal(calls.at(-1).method, 'GET', 'list reloads after import');
    assert.deepEqual(errors, []);
});

test('an invalid or oversized import is rejected with the reason', async () => {
    const { w, doc, h, errors } = await historyPage({
        '/api/trend-baselines/import': async () => ({ status: 400, body: { error: 'trend import rejected: unsupported schema "tb-1" (this version reads tb-2)' } }),
    });
    await openHistoryTab(doc);
    setImportFile(w, doc, '{"schema_version":"tb-1"}');
    await w.importTrendRuns();
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /Import failed: trend import rejected: unsupported schema "tb-1".*Nothing was imported/);

    const max = h.baselines.settings.limits.max_file_bytes;
    const input = doc.getElementById('trendImportFile');
    Object.defineProperty(input, 'files', { value: [{ name: 'big.json', size: max + 1 }], configurable: true });
    const before = w.fetchCalls.length;
    await w.importTrendRuns();
    assert.equal(w.fetchCalls.length, before, 'an oversized file is not uploaded');
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /larger than the limit/);
    assert.deepEqual(errors, []);
});

test('reset in the history tab needs a second click and clears the list', async () => {
    const { w, doc, h, calls, errors } = await historyPage({
        '/api/trend-baselines': async (url, init) => ((init.method || 'GET') === 'DELETE'
            ? { body: { ...h.baselines, runs: [], store: { ...h.baselines.store, runs: 0, pinned_runs: 0, status: 'empty' } } }
            : { body: h.baselines }),
    });
    await openHistoryTab(doc);
    doc.getElementById('trendHistoryResetBtn').click();
    assert.equal(calls.filter(c => c.method === 'DELETE').length, 0);
    assert.match(doc.getElementById('trendHistoryStatus').textContent, /including pinned and imported runs/);
    await w.resetTrendHistory();
    assert.equal(calls.filter(c => c.method === 'DELETE').length, 1);
    assert.match(doc.getElementById('trendHistoryEmpty').textContent, /No stored runs/);
    assert.deepEqual(errors, []);
});

function openTrends(doc) {
    doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').click();
    doc.getElementById('qaTabTrendsBtn').click();
}

test('the Trends view compares with a chosen run and returns to the default', async () => {
    const { doc, h, calls, errors } = await historyPage();
    openTrends(doc);
    const sel = doc.getElementById('trendCompareSelect');
    const values = [...sel.options].map(o => o.value);
    assert.deepEqual(values, ['default', 'previous', 'pinned', ...h.insights.trends.available_runs.map(r => r.run_id)]);
    assert.match(doc.getElementById('trendSummary').textContent, /Compared with: the previous compatible run of each shape/);
    const skuState = () => {
        const sku = h.insights.trends.rows.find(r => r.shape_summary === 'find on fields: sku');
        return doc.querySelector(`#trendRows tr[data-shape-id="${sku.shape_id}"] .trend-eff .insight-badge`).textContent;
    };
    assert.equal(skuState(), 'regressed', 'default: run 3 vs run 2 (index)');

    choose(doc, 'trendCompareSelect', 'fixture-run-1');
    await settle();
    assert.equal(calls.at(-1).url, '/api/trends/compare?baseline=fixture-run-1');
    assert.equal(skuState(), 'no material change', 'run 3 vs run 1 (no index)');
    assert.match(doc.getElementById('trendSummary').textContent, /Compared with: run before index/);
    assert.ok(doc.getElementById('trendTable').textContent.includes('(before index)'), 'baseline cells name the run');
    assert.equal(doc.getElementById('trendCompareSelect').value, 'fixture-run-1');

    choose(doc, 'trendCompareSelect', 'default');
    await settle();
    assert.equal(skuState(), 'regressed');
    assert.deepEqual(errors, []);
});

test('a failed comparison keeps the current table and says why', async () => {
    const { doc, h, errors } = await historyPage({
        '/api/trends/compare': async () => ({ status: 404, body: { error: 'trend run not found: fixture-run-1' } }),
    });
    openTrends(doc);
    const before = doc.getElementById('trendRows').outerHTML;
    choose(doc, 'trendCompareSelect', 'fixture-run-1');
    await settle();
    assert.match(doc.getElementById('trendCompareStatus').textContent, /Comparison failed: trend run not found/);
    assert.equal(doc.getElementById('trendRows').outerHTML, before);
    assert.equal(doc.getElementById('trendCompareSelect').value, 'default');
    assert.ok(h);
    assert.deepEqual(errors, []);
});

test('the matrix shows one column per run with separate metrics and highlights the current run', async () => {
    const { w, doc, h, calls, errors } = await historyPage();
    openTrends(doc);
    const msel = doc.getElementById('trendMatrixRuns');
    await w.loadTrendMatrix();
    assert.match(doc.getElementById('trendMatrixStatus').textContent, /Choose 1 to 8/);
    [...msel.options].forEach(o => { o.selected = true; });
    doc.getElementById('trendMatrixBtn').click();
    await settle();
    assert.equal(calls.at(-1).url, '/api/trends/compare?runs=fixture-run-2,fixture-run-1');

    const m = h.matrix.matrix;
    const table = doc.getElementById('trendMatrixTable');
    const heads = [...table.querySelectorAll('thead th[data-run-id]')];
    assert.deepEqual(heads.map(th => th.dataset.runId), m.columns.map(c => c.run_id));
    assert.deepEqual(heads.filter(th => th.classList.contains('trend-current')).map(th => th.dataset.runId), [m.current_run_id]);
    assert.ok(heads[0].textContent.includes('(current)'));
    assert.ok(heads[2].textContent.includes('before index') && heads[2].textContent.includes('pinned'));

    const trs = [...table.querySelectorAll('tbody tr[data-shape-id]')];
    assert.equal(trs.length, m.rows.length);
    const row = m.rows.find(r => r.shape_summary === 'find on fields: sku');
    const tr = trs.find(t => t.dataset.shapeId === row.shape_id);
    row.cells.forEach(c => {
        const td = tr.querySelector(`td[data-run-id="${c.run_id}"]`);
        assert.equal(td.classList.contains('trend-current'), !!c.current);
        assert.ok(td.querySelector('.trend-m-lat').textContent.includes(`Avg ${c.avg_ms.toFixed(2)} ms`) && td.textContent.includes(`p95 ${c.sample_p95_ms.toFixed(2)} ms`));
        assert.ok(td.querySelector('.trend-m-eff').textContent.includes(`Efficiency ${c.score}`));
        assert.ok(td.querySelector('.trend-m-imp').textContent.includes(`${(c.impact_share * 100).toFixed(1)}%`));
        const badges = [...td.querySelectorAll('.insight-badge')].map(b => b.textContent);
        assert.equal(badges.length, c.current ? 0 : 3, 'efficiency, latency, and impact are labeled separately');
    });
    assert.equal(tr.querySelector('td[data-run-id="fixture-run-2"] .trend-m-eff .insight-badge').textContent, 'regressed');

    const st = w.currentTrendViewState();
    assert.deepEqual(st.matrix_run_ids, ['fixture-run-2', 'fixture-run-1']);
    assert.deepEqual(errors, []);
});

test('matrix cells that are not comparable show the gate reason', async () => {
    const h = H();
    const m = structuredClone(h.matrix);
    const cell = m.matrix.rows[0].cells[1];
    Object.assign(cell, { comparability: 'incompatible', reasons: ['shape_identity_version_changed: baseline si-2, current si-3'], efficiency: 'not_comparable' });
    const missing = m.matrix.rows[1].cells[2];
    Object.keys(missing).forEach(k => { if (!['run_id'].includes(k)) delete missing[k]; });
    Object.assign(missing, { recorded: false, comparability: 'missing', reasons: ['not_in_baseline_run: the chosen run did not record this shape'] });
    const { w, doc, errors } = await historyPage({ '/api/trends/compare?runs': async () => ({ body: m }) });
    openTrends(doc);
    [...doc.getElementById('trendMatrixRuns').options].forEach(o => { o.selected = true; });
    await w.loadTrendMatrix();
    const td = (r, id) => doc.querySelector(`#trendMatrixTable tr[data-shape-id="${m.matrix.rows[r].shape_id}"] td[data-run-id="${id}"]`);
    assert.match(td(0, cell.run_id).querySelector('.trend-m-gate').textContent, /incompatible: shape_identity_version_changed/);
    assert.match(td(1, missing.run_id).textContent, /not recorded.*not_in_baseline_run/);
    assert.deepEqual(errors, []);
});

test('Download Summary records the chosen comparison', async () => {
    const { w, doc } = await historyPage();
    w.eval('finalReportData = { config: {}, stats: {} };');
    openTrends(doc);
    choose(doc, 'trendCompareSelect', 'fixture-run-1');
    await settle();
    await w.downloadSummaryReport();
    const st = exportedSummary(w).trend_view_state;
    assert.equal(st.comparison, 'run');
    assert.equal(st.baseline_run_id, 'fixture-run-1');
});
