// DOM tests for the Compare Runs tab: stored runs and servers on the same
// charts, the running workload growing against finished runs, one line per
// server of a run, and the picker.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 20));

const pts = (n, rate, p99) => Array.from({ length: n }, (_, i) => ({
    seq: i + 1, elapsedSec: (i + 1) * 0.5, rates: { total: rate }, totals: {}, avg: { total: 1 }, p95: { total: 2 }, p99: { total: p99 },
}));

const summary = (id, label, started, servers) => ({ id, label, status: 'completed', started_at: started, duration_sec: 30, servers });
const stored = (id, label, servers, rate, config = {}) => ({
    id, label, status: 'completed', config: { concurrency: 4, duration: '30s', ...config },
    stats: servers.length >= 2
        ? { historical: true, targets: servers.map((sv, i) => ({ index: i + 1, label: sv, chartHistory: pts(6, rate * (i + 1), 5) })), chartHistory: [] }
        : { historical: true, chartHistory: pts(6, rate, 5) },
});

async function page(runs, snaps) {
    const routes = {
        '/api/config': async () => ({ body: { URI: 'mongodb://u:p@live-host:27017/admin' } }),
        '/api/run-history/': async url => ({ body: snaps[decodeURIComponent(url.split('/api/run-history/')[1])] || {}, status: snaps[decodeURIComponent(url.split('/api/run-history/')[1])] ? 200 : 404 }),
        '/api/run-history': async () => ({ body: { runs } }),
    };
    const p = await loadPage(fixture('trends'), routes);
    await settle();
    p.w.openLiveDashboard(60);
    p.w.stopPolling();
    return p;
}
const lines = w => Array.from(w.eval('cmp').charts.throughput.data.datasets, d => d.label);
const cmpState = w => w.eval('cmp');
const boxes = doc => [...doc.querySelectorAll('#cmpPicker input[data-key]')];

const RUNS = [
    summary('b', 'Wed run', '2026-10-28T10:00:00Z', ['rs01', 'rs02']),
    summary('a', 'Mon run', '2026-10-26T10:00:00Z', ['db1:27017']),
    summary('c', 'Old run', '2026-10-20T10:00:00Z', ['db1:27017']),
];
const SNAPS = {
    a: stored('a', 'Mon run', ['db1:27017'], 1000, { concurrency: 4 }),
    b: stored('b', 'Wed run', ['rs01', 'rs02'], 800, { concurrency: 8 }),
    c: stored('c', 'Old run', ['db1:27017'], 500),
};

test('the Compare Runs tab shows the latest two stored runs, one line per server', async () => {
    const { w, doc, errors } = await page(RUNS, SNAPS);
    w.showDashTab('compare');
    await settle();
    assert.equal(doc.getElementById('cmpPanel').style.display, '');
    assert.equal(doc.getElementById('mspanel-live').style.display, 'none', 'the live charts give way to the comparison');
    assert.equal(doc.getElementById('msLegend').style.display, 'none', 'the live charts legend is not shown over the comparison');
    w.showDashTab('overview');
    assert.equal(doc.getElementById('msLegend').style.display, '');
    w.showDashTab('compare');
    assert.deepEqual(lines(w), ['Wed run · rs01', 'Wed run · rs02', 'Mon run · db1:27017']);
    assert.equal(boxes(doc).filter(b => b.checked).length, 3);
    assert.equal(boxes(doc).length, 4, 'every server of every stored run can be picked');

    const sets = Array.from(cmpState(w).charts.throughput.data.datasets);
    assert.notDeepEqual(Array.from(sets[0].borderDash), Array.from(sets[2].borderDash), 'each run has its own line style');
    assert.deepEqual(Array.from(sets[0].borderDash), Array.from(sets[1].borderDash), 'the servers of one run share it');
    assert.notEqual(sets[0].borderColor, sets[1].borderColor, 'each server has its own color');
    assert.match(doc.getElementById('cmpDiff').textContent, /Settings that differ: workers: 8 vs 4/);
    assert.match(doc.getElementById('cmpLegend').textContent, /Mon run · db1:27017/);
    assert.deepEqual(errors, []);
});

test('the same server in two runs has one color and two line styles; picking and the limit', async () => {
    const { w, doc } = await page(RUNS, SNAPS);
    w.showDashTab('compare');
    await settle();
    const pick = (key, on) => { const b = boxes(doc).find(x => x.dataset.key === key); b.checked = on; b.dispatchEvent(new w.Event('change', { bubbles: true })); };
    pick('c|db1:27017', true);
    await settle();
    const d = Array.from(cmpState(w).charts.throughput.data.datasets);
    const mon = d.find(x => x.label === 'Mon run · db1:27017'), old = d.find(x => x.label === 'Old run · db1:27017');
    assert.equal(mon.borderColor, old.borderColor, 'the same server is the same color across days');
    assert.notDeepEqual(Array.from(mon.borderDash), Array.from(old.borderDash));
    pick('b|rs02', false);
    assert.equal(lines(w).includes('Wed run · rs02'), false);

    // At most 8 lines: the ninth tick is refused with a reason.
    const many = Array.from({ length: 10 }, (_, i) => summary('m' + i, 'M' + i, `2026-10-${10 + i}T10:00:00Z`, ['s']));
    const snaps = Object.fromEntries(many.map(r => [r.id, stored(r.id, r.label, ['s'], 100)]));
    const q = await page(many, snaps);
    q.w.showDashTab('compare');
    await settle();
    boxes(q.doc).forEach(b => { b.checked = true; b.dispatchEvent(new q.w.Event('change', { bubbles: true })); });
    assert.equal(cmpState(q.w).selected.size, 8);
    assert.match(q.doc.getElementById('cmpHint').textContent, /Up to 8 lines/);
});

test('Latest two runs and Clear', async () => {
    const { w, doc } = await page(RUNS, SNAPS);
    w.showDashTab('compare');
    await settle();
    doc.getElementById('cmpClear').click();
    assert.deepEqual(lines(w), []);
    assert.match(doc.getElementById('cmpHint').textContent, /Tick at least one server/);
    doc.getElementById('cmpLatestTwo').click();
    await settle();
    assert.equal(lines(w).length, 3);
});

test('the running workload is a live line that keeps growing against a finished run', async () => {
    const { w, doc, errors } = await page(RUNS, SNAPS);
    const live = n => ({ isRunning: true, lifecyclePhase: 'running', executionElapsedSec: n / 2, duration: '60s', totalIterations: 1, dashboardId: 'live1', chartHistory: pts(n, 1200, 3), findOps: 1 });
    w.applyLiveStats(live(4));
    w.showDashTab('compare');
    await settle();
    assert.match(doc.querySelector('#cmpPicker .cmp-live legend').textContent, /Running now/);
    const first = cmpState(w).charts.throughput.data.datasets;
    assert.equal(first[0].label, 'Running now · live-host:27017', 'a single-server run is named by its host');
    assert.equal(first[0].borderWidth, 3, 'the live line is heavier');
    assert.equal(first[0].data.length, 4);
    assert.deepEqual(lines(w).slice(1), ['Wed run · rs01', 'Wed run · rs02'], 'the newest finished run is drawn with it');

    w.applyLiveStats(live(8));
    assert.equal(cmpState(w).charts.throughput.data.datasets[0].data.length, 8, 'new samples extend the live line');
    assert.equal(doc.querySelectorAll('#cmpPicker .cmp-live').length, 1);

    // Once stored, the finished run is that same entry, not a second one.
    w.eval('cmp').runs = [summary('live1', 'Stored copy', '2026-10-30T10:00:00Z', ['live-host:27017']), ...RUNS];
    w.applyLiveStats({ ...live(8), isRunning: false, lifecyclePhase: 'completed' });
    assert.equal(doc.querySelectorAll('#cmpPicker fieldset').length, RUNS.length + 1);
    assert.equal(lines(w).filter(l => l.includes('live-host')).length, 1);
    assert.deepEqual(errors, []);
});

test('waits between iterations are gaps, not a line along zero', async () => {
    const wait = pts(6, 1000, 5).map((p, i) => (i >= 2 && i < 4 ? { ...p, phase: 'wait', rates: { total: 0 }, p99: { total: null } } : p));
    const snaps = { a: { ...stored('a', 'Mon run', ['db1:27017'], 1000), stats: { historical: true, chartHistory: wait } } };
    const { w } = await page([RUNS[1]], snaps);
    w.showDashTab('compare');
    await settle();
    const d = Array.from(cmpState(w).charts.throughput.data.datasets[0].data, p => p.y);
    assert.deepEqual(d, [1000, 1000, null, null, 1000, 1000]);

    // Workers not running (an iteration being prepared) is a gap too.
    const idle = pts(4, 1000, 5).map((p, i) => (i === 2 ? { ...p, phase: 'idle', rates: { total: 0 } } : p));
    const q = await page([RUNS[1]], { a: { ...stored('a', 'Mon run', ['db1:27017'], 1000), stats: { historical: true, chartHistory: idle } } });
    q.w.showDashTab('compare');
    await settle();
    assert.deepEqual(Array.from(q.w.eval('cmp').charts.throughput.data.datasets[0].data, p => p.y), [1000, 1000, null, 1000]);
});

test('a stored run opens on the Compare Runs tab from Completed Runs', async () => {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://x' } }),
        '/api/stats': async () => ({ body: { isRunning: false } }),
        '/api/run-history/a': async () => ({ body: { ...SNAPS.a, stats: { ...SNAPS.a.stats, isRunning: false, lifecyclePhase: 'completed' } } }),
        '/api/run-history/b': async () => ({ body: SNAPS.b }),
        '/api/run-history': async () => ({ body: { runs: RUNS } }),
    });
    await settle();
    const row = p.w.eval('runHistoryRow')(RUNS[1]);
    assert.match(row, /compareFromHistory\('a'\)/);
    await p.w.compareFromHistory('a');
    await settle();
    assert.equal(p.doc.getElementById('cmpPanel').style.display, '');
    assert.ok(lines(p.w).some(l => l === 'Mon run · db1:27017'), 'the opened run is drawn');
    assert.equal(lines(p.w).length, 3, 'with the newest other run');
    assert.deepEqual(p.errors, []);
});
