// DOM tests for the Sweep load profile: the form, the dashboard between runs,
// and the sweep results in Compare Runs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 30));

const sweepRun = (id, workers, level, rate, avg, p95, extra = {}) => ({
    id, label: `mongo-8.0 - ${workers} workers`, status: 'completed', started_at: `2026-10-28T10:0${level}:00Z`, duration_sec: 60, active_sec: 60,
    total_ops: rate * 60, avg_latency_ms: avg, p95_latency_ms: p95, servers: ['db1:27017'],
    sweep: { id: 'sweep-1', label: 'mongo-8.0', level, levels: 3, workers }, ...extra,
});
const RUNS = [sweepRun('r3', 16, 3, 3000, 5.5, 12), sweepRun('r2', 8, 2, 2900, 2.1, 4), sweepRun('r1', 4, 1, 1500, 1.2, 2)];

async function page(runs = RUNS) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://live-host:27017' } }),
        '/api/run-history/': async () => ({ body: {}, status: 404 }),
        '/api/run-history': async () => ({ body: { runs } }),
    });
    await settle();
    return p;
}
const setField = (doc, name, v) => { const el = doc.querySelector(`[name="${name}"]`); el.value = v; return el; };

test('Sweep is a load profile with its own fields, hint, and tooltips', async () => {
    const { w, doc, errors } = await page();
    const kind = doc.getElementById('loadProfileKind');
    assert.ok([...kind.options].some(o => o.value === 'sweep'), 'Sweep is in the Load Profile list');
    kind.value = 'sweep';
    w.onLoadProfileKindChange();
    const groups = [...doc.querySelectorAll('.lp-group')].filter(g => g.style.display === 'block').map(g => g.dataset.lp);
    assert.deepEqual(groups, ['sweep']);
    assert.match(doc.getElementById('loadProfileHint').textContent, /once per worker count/);
    assert.match(doc.querySelector('label[for="sweepWorkers"]').title, /separate run/);
    assert.match(doc.querySelector('[data-lp="sweep"]').textContent.replace(/\s+/g, ' '), /Duration is the length of each run/);
    const help = doc.querySelector('[data-lp="sweep"]').textContent.replace(/\s+/g, ' ');
    assert.match(help, /cannot be combined with Record, Replay, Raw Injector, or Deterministic traffic/);
    assert.match(help, /saved once, when the first run starts/);
    assert.match(doc.getElementById('loadProfileHint').textContent, /Deterministic traffic/);
    assert.equal(doc.getElementById('sweepWorkers').name, 'load_profile_sweep_workers');
    assert.deepEqual(errors, []);
});

test('the summary shows the runs, the total time, and warns when Runs to keep is too low', async () => {
    const { w, doc } = await page();
    doc.getElementById('loadProfileKind').value = 'sweep';
    w.onLoadProfileKindChange();
    setField(doc, 'duration', '2m');
    setField(doc, 'iterations', '1');
    setField(doc, 'interval_delay', '0s');
    setField(doc, 'trend_history_runs', '10');
    doc.getElementById('sweepWorkers').value = '4, 8, 16, 32';
    w.updateSweepSummary();
    const text = doc.getElementById('sweepSummary').textContent;
    assert.match(text, /4 runs \(4, 8, 16, 32 workers\), about 8 min/);
    assert.doesNotMatch(text, /Runs to keep/);

    setField(doc, 'trend_history_runs', '3');
    w.updateSweepSummary();
    assert.match(doc.getElementById('sweepSummary').textContent, /Runs to keep is 3.*at least 4/);
});

test('a bad worker list or a mode that cannot sweep blocks the start with a message', async () => {
    const { w, doc } = await page();
    const kind = doc.getElementById('loadProfileKind');
    kind.value = 'fixed';
    assert.equal(w.validateSweep(), '', 'other profiles are not checked');
    kind.value = 'sweep';
    const sweep = doc.getElementById('sweepWorkers');
    for (const [list, want] of [['4', /at least 2/], ['4, x', /not a worker count/], ['4, 0', /not a worker count/], ['4, 8, 4', /listed twice/],
        ['1,2,3,4,5,6,7,8,9', /at most 8/]]) {
        sweep.value = list;
        assert.match(w.validateSweep(), want, list);
    }
    sweep.value = '4, 8, 16';
    assert.equal(w.validateSweep(), '');
    const mode = doc.getElementById('operationMode');
    mode.value = 'record';
    assert.match(w.validateSweep(), /generic or custom workload/);
    mode.value = '';
});

test('the dashboard follows a sweep: waits between runs and reopens for the next one', async () => {
    const { w, doc, errors } = await page();
    w.openLiveDashboard(60);
    w.stopPolling();
    const sw = (state, level, extra = {}) => ({ id: 'sweep-1', levels: 3, level, workers: [4, 8, 16], state, active: state === 'running' || state === 'waiting', message: `Level ${level} of 3`, pause_sec: 0, ...extra });

    assert.equal(w.handleSweepStats({ isRunning: true, dashboardId: 'd1', sweep: sw('running', 1) }), false, 'the first run is shown as any run');
    assert.equal(doc.getElementById('sweepBanner').hidden, false);
    assert.match(doc.getElementById('sweepBannerText').textContent, /Sweep of 3 runs \(4, 8, 16 workers\): Level 1 of 3/);

    // Between runs: no finished state, Stop stays, polling continues.
    assert.equal(w.handleSweepStats({ isRunning: false, dashboardId: 'd1', sweep: sw('waiting', 2) }), true);
    assert.equal(doc.getElementById('activeActions').style.display, 'block');
    assert.equal(doc.getElementById('finishActions').style.display, 'none');

    // A new run: the dashboard reopens once.
    assert.equal(w.handleSweepStats({ isRunning: true, dashboardId: 'd2', duration: '1m', totalIterations: 1, sweep: sw('running', 2) }), true);
    assert.equal(w.handleSweepStats({ isRunning: true, dashboardId: 'd2', sweep: sw('running', 2) }), false, 'the same run is not reopened again');

    // A finished sweep leaves its summary and the compare button, and no longer holds the dashboard.
    assert.equal(w.handleSweepStats({ isRunning: false, dashboardId: 'd3', sweep: sw('done', 3, { message: 'Sweep complete: 3 runs' }) }), false);
    assert.equal(doc.getElementById('sweepCompareBtn').hidden, false);
    assert.equal(doc.getElementById('sweepCompareBtn').dataset.sweep, 'sweep-1');
    assert.equal(w.handleSweepStats({ isRunning: false, dashboardId: 'd9' }), false);
    assert.equal(doc.getElementById('sweepBanner').hidden, true, 'a later run is not shown as part of the sweep');
    assert.deepEqual(errors, []);
});

test('Compare Runs lists the sweep results and Compare sweep selects every run of it', async () => {
    const { w, doc, errors } = await page();
    w.openLiveDashboard(60);
    w.stopPolling();
    w.showDashTab('compare');
    await settle();
    const rows = [...doc.querySelectorAll('#cmpSweeps .sweep-table tbody tr')].map(tr => [...tr.cells].map(c => c.textContent));
    assert.deepEqual(rows.map(r => r[0]), ['4', '8', '16'], 'ordered by worker count');
    assert.equal(rows[0][1], '1,500');
    assert.equal(rows[0][2], '×1.00');
    assert.equal(rows[1][2], '×1.93');
    assert.equal(rows[2][3], '5.5');
    assert.equal(rows[2][4], '12');
    assert.match(doc.querySelector('#cmpSweeps .sweep-head').textContent, /Sweep: mongo-8\.0.*3 of 3 runs stored/);
    assert.match(doc.querySelector('#cmpSweeps th[title]').title, /workers/);

    w.compareSweep('sweep-1');
    await settle();
    const picked = [...doc.querySelectorAll('#cmpPicker input[data-key]')].filter(b => b.checked).map(b => b.dataset.key.split('|')[0]).sort();
    assert.deepEqual(picked, ['r1', 'r2', 'r3']);
    assert.deepEqual(errors, []);
});

test('Completed Runs tags sweep runs and offers Compare sweep', async () => {
    const { w } = await page();
    const html = w.runHistoryRow(RUNS[1]);
    assert.match(html, /Sweep run 2 of 3 · 8 workers/);
    assert.match(html, /Compare sweep/);
    assert.match(html, /compareFromHistory\('r2', 'sweep-1'\)/);
    const plain = w.runHistoryRow({ id: 'x', label: 'plain', status: 'completed' });
    assert.doesNotMatch(plain, /Sweep/);
    assert.match(plain, />Compare</);
});

test('saved settings keep a sweep and bring it back', async () => {
    const { w, doc } = await page();
    const kind = doc.getElementById('loadProfileKind');
    kind.value = 'sweep';
    doc.getElementById('sweepWorkers').value = '2, 6, 12';
    const snap = w.settingsSnapshot();
    assert.equal(snap.fields.load_profile_kind, 'sweep');
    assert.equal(snap.fields.load_profile_sweep_workers, '2, 6, 12');

    kind.value = 'fixed';
    w.onLoadProfileKindChange();
    doc.getElementById('sweepWorkers').value = '1, 2';
    await w.applySavedSettings({ fields: snap.fields });
    assert.equal(kind.value, 'sweep');
    assert.equal(doc.getElementById('sweepWorkers').value, '2, 6, 12');
    assert.equal(doc.querySelector('[data-lp="sweep"]').style.display, 'block', 'the sweep fields are shown again');
});
