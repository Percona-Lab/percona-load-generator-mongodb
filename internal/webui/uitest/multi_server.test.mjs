// DOM tests for the dashboard charts: one line per server on Live Metrics and
// Latency Analysis (one line for a run on one server), one Servers /
// Operation choice for the total cards and every chart, per-server cursors,
// the Time Machine, drag-to-zoom, markers, and stored runs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

function points(n, seq0, rate, p99) {
    return Array.from({ length: n }, (_, i) => ({
        seq: seq0 + i + 1, elapsedSec: (i + 1) * 0.5,
        rates: { total: rate, find: rate * 0.6, insert: rate * 0.1, update: rate * 0.2, delete: rate * 0.1, aggregate: 0 },
        totals: {}, avg: { total: 1, find: 0.8, update: 1.4 }, p95: { total: 2, find: 1.6, update: 2.8 }, p99: { total: p99, find: p99 * 0.8, update: p99 * 1.3 },
    }));
}
const dist = (avg, p95, p99, count = 100) => ({ avg, min: avg / 10, max: p99 * 2, p95, p99, count, sum: avg * count });
function target(index, label, status, opts = {}) {
    return {
        index, label, status, elapsedSec: opts.elapsed ?? 60,
        ops: { total: 1000 * index, find: 600 * index, insert: 100 * index, update: 200 * index, delete: 100 * index, aggregate: 0 },
        dist: { total: dist(1, 2, opts.p99 ?? 4), find: dist(0.8, 1.6, 3), update: dist(1.4, 2.8, opts.updateP99 ?? 5), insert: dist(1, 1, 1, 0) },
        chartHistory: opts.points ?? points(4, 0, 5000, opts.p99 ?? 4),
        latencyHeatmap: opts.heat ?? [{ elapsedSec: 1, timestampUnixMs: 1000 + index, count: 50, p50: 1, p95: 2, p99: opts.p99 ?? 4, max: 9 }],
    };
}
const liveStats = (targets, extra = {}) => ({
    isRunning: true, lifecyclePhase: 'running', executionElapsedSec: 35, duration: '60s', totalIterations: 1,
    multi_target: { kind: 'replay', mode: 'sequential', reference: 'rs01', targets: targets.map(t => ({ index: t.index, label: t.label, status: t.status, ops_done: 1, elapsed_sec: t.elapsedSec, outcome_file: '' })) },
    targets, ...extra,
});

async function page() {
    const p = await loadPage(fixture('trends'), { '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }) });
    await settle();
    p.w.openLiveDashboard(60);
    p.w.stopPolling();
    return p;
}
const visible = (doc, id) => { const el = doc.getElementById(id); return !el.hidden && el.style.display !== 'none'; };
const charts = w => w.eval('msCharts');

test('a run on several servers draws one line per server on both tabs', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete'), target(2, 'rs02', 'complete'), target(3, 'rs03', 'running', { elapsed: 35 })]));
    for (const id of ['mspanel-bar', 'mspanel-tiles', 'mspanel-live', 'mspanel-compare']) assert.ok(visible(doc, id), `${id} on Live Metrics`);
    for (const id of ['statsGrid', 'dashpanel-throughput', 'donutChart', 'dashpanel-heatmap']) assert.equal(doc.getElementById(id), null, `${id} is gone`);
    assert.ok(visible(doc, 'timeMachineBar'));
    for (const id of ['mspanel-timeline', 'mspanel-distribution', 'mspanel-heatmap']) assert.equal(visible(doc, id), false, `${id} waits for its tab`);
    // Live Throughput and Live Latency come before Compare servers.
    const live = doc.getElementById('mspanel-live'), cmp = doc.getElementById('mspanel-compare');
    assert.ok(live.compareDocumentPosition(cmp) & w.Node.DOCUMENT_POSITION_FOLLOWING);
    assert.ok(doc.getElementById('mspanel-tiles').compareDocumentPosition(live) & w.Node.DOCUMENT_POSITION_FOLLOWING);
    const servers = [...doc.querySelectorAll('#msServers .ms-server')].map(b => b.textContent.replace(/\s+/g, ' ').trim());
    assert.deepEqual(servers, ['rs01 reference · complete', 'rs02 complete', 'rs03 running 35s']);
    const c = charts(w);
    for (const name of ['throughput', 'latency', 'compare']) assert.equal(c[name].data.datasets.length, 3, `${name}: one line per server`);
    assert.deepEqual(Array.from(c.throughput.data.datasets, d => d.label), ['rs01', 'rs02', 'rs03 (running)']);
    assert.equal(c.throughput.data.datasets[0].data[0].x, 0.5, 'x is seconds since the server started');
    assert.equal(c.throughput.data.datasets[0].data[0].y, 5000);

    w.showDashTab('latency');
    for (const id of ['mspanel-bar', 'mspanel-timeline', 'mspanel-distribution', 'mspanel-heatmap']) assert.ok(visible(doc, id), `${id} on Latency Analysis`);
    for (const id of ['mspanel-tiles', 'mspanel-live', 'mspanel-compare']) assert.equal(visible(doc, id), false, `${id} hidden`);
    w.showDashTab('queries');
    assert.equal(visible(doc, 'mspanel-bar'), false, 'the toolbar is only on the chart tabs');
    assert.deepEqual(errors, []);
});

test('one Operation picker drives the cards and every chart, on both tabs', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete', { p99: 4 }), target(2, 'rs02', 'complete', { p99: 8 })]));
    const ops = doc.getElementById('msOps');
    assert.equal(ops.getAttribute('role'), 'radiogroup');
    assert.deepEqual([...ops.querySelectorAll('[role="radio"]')].map(b => b.textContent), ['All operations', 'Find', 'Insert', 'Update', 'Delete', 'Aggregate', 'Transaction']);
    assert.equal(ops.querySelector('[aria-checked="true"]').dataset.op, 'all');

    ops.querySelector('[data-op="update"]').click();
    assert.equal(ops.querySelector('[aria-checked="true"]').dataset.op, 'update');
    assert.equal(ops.querySelector('[data-op="update"]').tabIndex, 0, 'roving tab stop');
    assert.equal(doc.querySelector('.ms-tile[aria-pressed="true"]').dataset.op, 'update');
    for (const id of ['msSubThroughput', 'msSubLatency', 'msSubCompare', 'msSubTimeline', 'msSubDistribution']) {
        assert.match(doc.getElementById(id).textContent, /^Update/, `${id} names the operation`);
    }
    assert.equal(charts(w).throughput.data.datasets[0].data[0].y, 1000, 'update rate');
    assert.equal(charts(w).compare.data.datasets[1].data[0].y, 8 * 1.3, 'update p99');
    assert.match(doc.getElementById('msHint').textContent, /Showing update for rs01, rs02/);

    // A total card is a shortcut for the picker.
    doc.querySelector('.ms-tile[data-op="find"]').click();
    assert.equal(ops.querySelector('[aria-checked="true"]').dataset.op, 'find');
    // Arrow keys move the choice.
    ops.querySelector('[data-op="find"]').focus();
    ops.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    assert.equal(ops.querySelector('[aria-checked="true"]').dataset.op, 'insert');
    assert.equal(doc.activeElement.dataset.op, 'insert');
    // The same choice applies on Latency Analysis.
    w.showDashTab('latency');
    assert.match(doc.getElementById('msSubTimeline').textContent, /^Insert/);
    assert.deepEqual(errors, []);
});

test('Servers toggles hide a server everywhere but always keep one', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete'), target(2, 'rs02', 'complete')]));
    const btn = label => [...doc.querySelectorAll('#msServers .ms-server')].find(b => b.dataset.label === label);
    btn('rs02').click();
    assert.equal(btn('rs02').getAttribute('aria-pressed'), 'false');
    assert.deepEqual(Array.from(charts(w).throughput.data.datasets, d => d.label), ['rs01']);
    assert.equal(doc.querySelectorAll('.ms-tile[data-op="all"] .ms-tile-row').length, 1);
    assert.equal(doc.querySelectorAll('#msDistRows tr').length, 1);
    assert.equal(doc.querySelectorAll('#msHeatmapRows .ms-hm-row').length, 1);
    btn('rs01').click();
    assert.equal(btn('rs01').getAttribute('aria-pressed'), 'true', 'the last shown server stays');
    btn('rs02').click();
    assert.equal(charts(w).throughput.data.datasets.length, 2);
    assert.deepEqual(errors, []);
});

test('cards, distribution, and heatmap show each server', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete', { p99: 4, updateP99: 5 }), target(2, 'rs02', 'complete', { p99: 8, updateP99: 5.1 }), target(3, 'rs03', 'running', { elapsed: 30, p99: 3 })]));
    const total = doc.querySelector('.ms-tile[data-op="all"]');
    assert.match(total.textContent, /Total Ops/);
    assert.deepEqual(Array.from(total.querySelectorAll('.ms-tile-row'), r => Array.from(r.children, c => c.textContent)), [['rs01', '1,000', '17/s avg'], ['rs02', '2,000', '33/s avg'], ['rs03', '3,000', '100/s avg']]);

    const rows = [...doc.querySelectorAll('#msDistRows tr')];
    assert.equal(rows.length, 3);
    assert.ok(rows[0].classList.contains('ms-ref'));
    assert.match(rows[1].textContent, /8\.00 ms\+100\.0% vs rs01/);
    assert.ok(rows[1].querySelector('.ms-worse'), 'slower than the reference is marked');
    assert.ok(rows[2].querySelector('.ms-better'));
    assert.match(doc.getElementById('msDistNote').textContent, /rs03 covers its first 30 s so far/);
    doc.querySelector('#msOps [data-op="update"]').click();
    assert.ok(doc.querySelectorAll('#msDistRows tr')[1].querySelector('.ms-same'), 'within 5% reads as the same');
    doc.querySelector('#msOps [data-op="insert"]').click();
    assert.equal(doc.querySelectorAll('#msDistRows tr')[0].querySelectorAll('.empty-dash').length, 5, 'no operations, no numbers');

    const strips = [...doc.querySelectorAll('#msHeatmapRows .ms-hm-row')];
    assert.equal(strips.length, 3);
    assert.match(strips[2].querySelector('.ms-hm-label').textContent, /rs03running 30s/);
    const bars = strips.map(s => s.querySelectorAll('.heatmap-col').length);
    assert.ok(bars.every(n => n === bars[0]), 'the strips share one time axis');
    const tall = strips[1].querySelector('.heatmap-col[style*="background"]');
    assert.equal(tall.style.height, '100%', 'height is relative to the worst window of all servers');
    assert.match(doc.getElementById('msSubHeatmap').textContent, /all operations/);
    assert.deepEqual(errors, []);
});

test('polls ask each server only for new samples, and append them', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete'), target(2, 'rs02', 'running', { points: points(2, 0, 4000, 4), heat: [{ elapsedSec: 1, timestampUnixMs: 5000, count: 1, p99: 4 }] })]));
    assert.match(w.liveStatsURL(), /&targetSince=1:4:1001,2:2:5000$/);
    const more = target(2, 'rs02', 'running', { points: points(4, 0, 4000, 4).slice(1), heat: [] });
    w.applyLiveStats(liveStats([{ ...target(1, 'rs01', 'complete'), chartHistory: [], latencyHeatmap: [] }, more]));
    assert.equal(charts(w).throughput.data.datasets[1].data.length, 4, 'seq 2 is not added twice');
    assert.match(w.liveStatsURL(), /2:4:5000$/);
    assert.deepEqual(errors, []);
});

const single = (extra = {}) => ({
    isRunning: true, lifecyclePhase: 'running', executionElapsedSec: 4, duration: '60s', totalIterations: 2,
    findOps: 600, insertOps: 100, updateOps: 150, upsertOps: 50, deleteOps: 100, aggOps: 0,
    distTotal: dist(1, 2, 4), distFind: dist(0.8, 1.6, 3), distUpdate: dist(1.4, 2.8, 5), distTrans: dist(6, 9, 12, 10),
    chartHistory: points(8, 0, 5000, 4).map((p, i) => ({ ...p, totals: { total: (i + 1) * 100, find: (i + 1) * 60 }, marker: i === 0 ? 'Iter 1' : (i === 5 ? 'Iter 2' : '') })),
    latencyHeatmap: [{ elapsedSec: 1, timestampUnixMs: 1, count: 10, p50: 1, p95: 2, p99: 3, max: 4 }, { elapsedSec: 2, timestampUnixMs: 2, count: 10, p50: 1, p95: 2, p99: 6, max: 8 }],
    ...extra,
});

test('a run on one server uses the same view with one line', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(single());
    for (const id of ['mspanel-bar', 'mspanel-tiles', 'mspanel-live', 'mspanel-compare']) assert.ok(visible(doc, id), id);
    assert.equal(doc.getElementById('msServersGroup').hidden, true, 'nothing to toggle with one server');
    assert.ok(doc.getElementById('mspanel-tiles').classList.contains('ms-tiles-single'));
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-v').textContent, '1,000');
    assert.equal(doc.querySelector('.ms-tile[data-op="update"] .ms-v').textContent, '200', 'updates include upserts');
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-r').textContent, '250/s avg');
    const c = charts(w);
    for (const name of ['throughput', 'latency', 'compare', 'timeline']) assert.equal(c[name].data.datasets.length, 1, name);
    assert.equal(c.throughput.data.datasets[0].label, 'This run');
    assert.match(doc.getElementById('msHint').textContent, /^Showing all operations in the cards/);
    assert.match(doc.getElementById('msSubThroughput').textContent, /this run/);
    // Iteration markers come back on every chart.
    assert.deepEqual(Array.from(c.throughput.$msMarkers, m => [m.x, m.text]), [[0.5, 'Iter 1'], [3, 'Iter 2']]);
    w.showDashTab('latency');
    assert.equal(doc.getElementById('msDistHead').textContent, 'Run');
    assert.match(doc.querySelector('#msDistRows tr').textContent, /This run1\.00 ms/);
    assert.equal(doc.querySelectorAll('#msHeatmapRows .ms-hm-row').length, 1);
    // Transactions have latency but no throughput.
    doc.querySelector('#msOps [data-op="transaction"]').click();
    assert.match(doc.querySelector('#msDistRows tr').textContent, /6\.00 ms/);
    assert.match(doc.getElementById('msSubThroughput').textContent, /not tracked per transaction/);
    assert.equal(doc.querySelector('.ms-tile[aria-pressed="true"]'), null, 'no card for transactions');
    assert.match(w.liveStatsURL(), /^\/api\/stats\?chartSince=8$/);
    assert.deepEqual(errors, []);
});

test('the Time Machine goes back to a second and Back to Live returns', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(single());
    const scrubber = doc.getElementById('timeScrubber');
    assert.equal(scrubber.max, '4');
    assert.equal(scrubber.value, '4');
    scrubber.value = '2';
    scrubber.dispatchEvent(new w.Event('input', { bubbles: true }));
    const xs = () => Array.from(charts(w).throughput.data.datasets[0].data, p => p.x);
    assert.deepEqual(xs(), [0.5, 1, 1.5, 2], 'the lines stop at that second');
    assert.equal(charts(w).throughput.options.scales.x.max, 2);
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-v').textContent, '400', 'totals at that second');
    assert.equal(doc.getElementById('zoomRangeLabel').textContent, 'At 00:02');
    assert.ok(doc.getElementById('liveBtn').classList.contains('active'));
    w.showDashTab('latency');
    assert.match(doc.getElementById('msDistNote').textContent, /whole run/);
    w.applyLiveStats(single({ executionElapsedSec: 5, chartHistory: points(10, 0, 5000, 4).slice(8) }));
    assert.deepEqual(xs(), [0.5, 1, 1.5, 2], 'new samples do not move a paused view');
    w.goToLive();
    assert.equal(xs().length, 10);
    assert.equal(doc.getElementById('liveBtn').classList.contains('active'), false);
    assert.equal(doc.getElementById('zoomRangeLabel').textContent, 'Drag a graph to zoom');
    assert.deepEqual(errors, []);
});

test('zoom limits every chart and the cards to a range until it is reset', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(single());
    w.eval('msView.zoom = [1, 3]; renderMultiServer()');
    for (const name of ['throughput', 'latency', 'compare', 'timeline']) {
        const c = charts(w)[name];
        assert.deepEqual([c.options.scales.x.min, c.options.scales.x.max], [1, 3], name);
        assert.deepEqual(Array.from(c.data.datasets[0].data, p => p.x), [1, 1.5, 2, 2.5, 3], name);
    }
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-v').textContent, '400', 'operations between 1 s and 3 s');
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-r').textContent, '200/s avg');
    assert.equal(doc.getElementById('zoomRangeLabel').textContent, 'Zoom: 00:01 - 00:03');
    assert.equal(doc.getElementById('resetZoomBtn').style.display, 'inline-flex');
    w.resetChartZoom();
    assert.equal(charts(w).throughput.data.datasets[0].data.length, 8);
    assert.equal(doc.getElementById('resetZoomBtn').style.display, 'none');
    assert.deepEqual(errors, []);
});

test('a new run starts clean after a run on several servers', async () => {
    const { w, doc, errors } = await page();
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete'), target(2, 'rs02', 'complete')]));
    assert.match(w.liveStatsURL(), /targetSince/);
    w.openLiveDashboard(60);
    w.stopPolling();
    w.applyLiveStats(single());
    assert.equal(doc.getElementById('msServersGroup').hidden, true);
    assert.equal(charts(w).throughput.data.datasets.length, 1);
    assert.doesNotMatch(w.liveStatsURL(), /targetSince/);
    assert.deepEqual(errors, []);
});

test('a stored run on several servers reopens with every server', async () => {
    const snap = {
        id: 'r1', label: 'Replay', status: 'completed', duration_sec: 60,
        stats: { ...liveStats([target(1, 'rs01', 'complete'), target(2, 'rs02', 'complete')]), isRunning: false, historical: true, lifecyclePhase: 'completed', opsPresent: true, chartHistory: [] },
    };
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: { isRunning: false } }),
        '/api/run-history/r1': async () => ({ body: snap }),
    });
    await settle();
    await p.w.openHistoricalRun('r1');
    assert.ok(visible(p.doc, 'mspanel-live'));
    assert.equal(charts(p.w).throughput.data.datasets.length, 2);
    assert.deepEqual(p.errors, []);
});

test('heatmap windows fill the time they cover, so uneven polls leave no gaps', async () => {
    const { w, doc, errors } = await page();
    const heat = [1.4, 2.9, 4.0].map((e, i) => ({ elapsedSec: e, timestampUnixMs: 100 + i, count: 10, p50: 1, p95: 2, p99: 2 + i, max: 5 }));
    w.applyLiveStats(liveStats([target(1, 'rs01', 'complete', { elapsed: 4, heat }), target(2, 'rs02', 'complete', { elapsed: 4 })]));
    const cols = [...doc.querySelectorAll('#msHeatmapRows .ms-hm-row')[0].querySelectorAll('.heatmap-col')];
    assert.equal(cols.length, 4, 'one column per second');
    assert.ok(cols.every(c => c.style.background), 'every second has a window');
    assert.match(cols[2].title || cols[2]._hm.title, /window ending 2\.9s/, 'the 1.4-2.9 s window fills second 3');
    assert.deepEqual(errors, []);
});

test('a sample recorded out of order never draws a line back in time', async () => {
    const { w, errors } = await page();
    const pts = points(3, 0, 1000, 4);
    pts[2].elapsedSec = 0.2;
    w.applyLiveStats(liveStats([target(1, 'rs01', 'running', { points: pts }), target(2, 'rs02', 'pending', { points: [] })]));
    assert.deepEqual(Array.from(charts(w).throughput.data.datasets[0].data, d => d.x), [0.5, 1]);
    assert.match(w.liveStatsURL(), /1:3:/, 'its seq still moves the cursor');
    assert.deepEqual(errors, []);
});

test('a heatmap window after a pause does not cover the idle time', async () => {
    const { w, doc, errors } = await page();
    const heat = [1, 2, 8].map((e, i) => ({ elapsedSec: e, timestampUnixMs: 10 + i, count: 10, p50: 1, p95: 2, p99: 3, max: 4 }));
    // Samples: running to 2 s, waiting until 7 s, running again.
    const pts = [1, 2, 3, 4, 5, 6, 7, 8].map((x, i) => ({ seq: i + 1, elapsedSec: x, waiting: x > 2 && x <= 7, rates: {}, totals: {}, avg: {}, p95: {}, p99: {} }));
    w.applyLiveStats(single({ executionElapsedSec: 8, latencyHeatmap: heat, chartHistory: pts }));
    const cols = [...doc.querySelectorAll('#msHeatmapRows .heatmap-col')];
    assert.equal(cols.length, 8);
    assert.deepEqual(cols.map(c => !!c.style.background), [true, true, false, false, false, false, false, true], 'the wait from 2 s to 7 s stays empty');
    assert.equal(doc.getElementById('msCompareTitle').textContent, 'Tail latency');
    assert.equal(charts(w).throughput.options.scales.x.title.text, 'Seconds since the run started');
    assert.deepEqual(errors, []);
});
