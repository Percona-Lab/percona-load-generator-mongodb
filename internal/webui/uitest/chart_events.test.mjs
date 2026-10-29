// DOM tests for what the charts say about a run: waits between iterations
// as shaded bands with their length, other events as labeled lines, average
// rates that leave the waits out, gaps without samples, long runs drawn
// without losing dips, and the dashboard timer that keeps going in a
// background tab.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page() {
    const p = await loadPage(fixture('trends'), { '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }) });
    await settle();
    p.w.openLiveDashboard(60);
    p.w.stopPolling();
    return p;
}

// Two iterations of 10 s at 1,000 ops/s with a 5 s wait between them.
function twoIterations() {
    const pts = [];
    let seq = 0, total = 0, active = 0;
    const add = (t, rate, extra = {}) => {
        const dt = 1;
        if (!extra.phase) { total += rate * dt; active += dt; }
        pts.push({ seq: ++seq, elapsedSec: t, activeSec: active, iteration: extra.iteration || 1, workers: 4, rates: { total: rate, find: rate }, totals: { total, find: total }, avg: { total: extra.phase ? null : 2 }, p95: {}, p99: { total: extra.phase ? null : 5 }, ...extra });
    };
    add(1, 1000, { marker: 'Iter 1' });
    for (let t = 2; t <= 10; t++) add(t, 1000);
    add(11, 0, { phase: 'wait', waiting: true, marker: 'Wait' });
    for (let t = 12; t <= 15; t++) add(t, 0, { phase: 'wait', waiting: true });
    add(16, 1000, { iteration: 2, marker: 'Iter 2; Workers 4→8' });
    for (let t = 17; t <= 25; t++) add(t, 1000, { iteration: 2 });
    return { pts, total, active };
}

function single(points, extra = {}) {
    const last = points[points.length - 1];
    return { isRunning: true, lifecyclePhase: 'running', executionElapsedSec: last.elapsedSec, activeSec: last.activeSec, duration: '10s', totalIterations: 2, intervalDelay: '5s', sampleIntervalMs: 1000, findOps: last.totals.total, chartHistory: points, ...extra };
}

test('a wait between iterations is a labeled band and the average rate leaves it out', async () => {
    const { w, doc, errors } = await page();
    const { pts, total, active } = twoIterations();
    w.applyLiveStats(single(pts));
    const c = w.eval('msCharts');
    const bands = c.throughput.$msBands;
    assert.equal(bands.length, 1);
    assert.equal(bands[0].phase, 'wait');
    assert.equal(bands[0].text, 'Wait 5s', 'the band says how long the wait was');
    assert.deepEqual([bands[0].from, bands[0].to], [10, 15]);
    assert.equal(c.latency.$msBands.length, 1, 'every chart shows the band');
    // The wait is drawn as zero throughput and a latency gap, like a server's opcounters.
    const thr = c.throughput.data.datasets[0].data.find(d => d.x === 12);
    const lat = c.latency.data.datasets[0].data.find(d => d.x === 12);
    assert.equal(thr.y, 0);
    assert.equal(lat.y, null);

    const markers = Array.from(c.throughput.$msMarkers, m => m.text);
    assert.deepEqual(markers, ['Iter 1', 'Iter 2', 'Workers 4→8'], '"Wait" is the band label, other events are lines');

    // 20,000 operations in 20 s of work: 1,000/s, not 20,000 / 25 s.
    const tile = doc.querySelector('#mspanel-tiles .ms-tile[data-op="all"]').textContent;
    assert.ok(tile.includes((total).toLocaleString()), tile);
    assert.match(tile, /1,000\/s avg/);
    assert.equal(active, 20);

    // A zoom across the wait divides by the active seconds in range too.
    w.eval('msView').zoom = [5, 20];
    w.renderMultiServer();
    assert.match(doc.querySelector('#mspanel-tiles .ms-tile[data-op="all"]').textContent, /1,000\/s avg/);

    const legend = doc.getElementById('msLegend').textContent;
    assert.match(legend, /Wait between iterations/);
    assert.match(legend, /A sample every 1000 ms, recorded by PLGM even when this tab is hidden/);
    assert.match(legend, /counts only the time workers ran/);
    assert.match(doc.getElementById('lifecycleHint').textContent, /includes the 5s waits between iterations; average rates do not/);
    assert.deepEqual(errors, []);
});

test('a server waiting between iterations says so, with the iteration', async () => {
    const { w, doc } = await page();
    const { pts } = twoIterations();
    const wait = pts.slice(0, 13);
    const tg = (i, label, status, points, phase = '') => {
        const last = points[points.length - 1] || { elapsedSec: 0, activeSec: 0 };
        return { index: i, label, status, phase, elapsedSec: last.elapsedSec, activeSec: last.activeSec, ops: { total: 1 }, dist: {}, chartHistory: points, latencyHeatmap: [] };
    };
    w.applyLiveStats({ isRunning: true, lifecyclePhase: 'running', totalIterations: 2, executionElapsedSec: 13, duration: '10s',
        multi_target: { kind: 'deterministic', mode: 'sequential', reference: 'rs01', targets: [] },
        targets: [tg(1, 'rs01', 'running', wait, 'wait'), tg(2, 'rs02', 'pending', [])] });
    const chips = [...doc.querySelectorAll('#msServers .ms-server')].map(b => b.textContent.replace(/\s+/g, ' ').trim());
    assert.deepEqual(chips, ['rs01 reference · waiting between iterations · iteration 1/2', 'rs02 waiting']);
    // On several servers each label names its server.
    const c = w.eval('msCharts');
    assert.equal(c.throughput.$msBands[0].text, 'rs01: Wait 3s');
});

test('the same event on several servers is one label naming them', async () => {
    const { w } = await page();
    const pts = label => [
        { seq: 1, elapsedSec: 0.5, activeSec: 0.5, rates: { total: 10 }, totals: { total: 5 }, avg: {}, p95: {}, p99: {}, marker: 'Iter 1' },
        { seq: 2, elapsedSec: 30, activeSec: 30, rates: { total: 10 }, totals: { total: 300 }, avg: {}, p95: {}, p99: {}, marker: label === 'rs02' ? 'Stopped by user' : '' },
    ];
    const tg = (i, label) => ({ index: i, label, status: 'complete', elapsedSec: 30, activeSec: 30, ops: { total: 300 }, dist: {}, chartHistory: pts(label), latencyHeatmap: [] });
    w.applyLiveStats({ isRunning: false, lifecyclePhase: 'completed', totalIterations: 1, executionElapsedSec: 30, duration: '30s',
        multi_target: { kind: 'deterministic', mode: 'concurrent', reference: 'rs01', targets: [] }, targets: [tg(1, 'rs01'), tg(2, 'rs02')] });
    const texts = Array.from(w.eval('msCharts').throughput.$msMarkers, m => m.text);
    assert.deepEqual(texts.sort(), ['rs01, rs02: Iter 1', 'rs01: Finished', 'rs02: Stopped by user']);
});

test('a gap without samples breaks the line, and stops and the end of a deterministic pass are labeled', async () => {
    const { w } = await page();
    const pts = [
        { seq: 1, elapsedSec: 1, activeSec: 1, rates: { total: 100 }, totals: { total: 100 }, avg: {}, p95: {}, p99: {}, marker: 'Iter 1' },
        { seq: 2, elapsedSec: 2, activeSec: 2, rates: { total: 100 }, totals: { total: 200 }, avg: {}, p95: {}, p99: {}, marker: 'All 80 planned ops sent, repeating' },
        { seq: 3, elapsedSec: 40, activeSec: 40, rates: { total: 100 }, totals: { total: 4000 }, avg: {}, p95: {}, p99: {}, marker: 'No samples for 38s' },
        { seq: 4, elapsedSec: 41, activeSec: 41, rates: { total: 100 }, totals: { total: 4100 }, avg: {}, p95: {}, p99: {}, marker: 'Stopped by user' },
    ];
    w.applyLiveStats(single(pts, { isRunning: false, lifecyclePhase: 'failed' }));
    const c = w.eval('msCharts');
    const data = c.throughput.data.datasets[0].data;
    assert.deepEqual(Array.from(data, d => d.y), [100, 100, null, 100, 100], 'a null point breaks the line across the gap');
    const markers = c.throughput.$msMarkers;
    assert.deepEqual(Array.from(markers, m => m.text), ['Iter 1', 'Planned ops sent, repeating', 'No samples for 38s', 'Stopped by user'], 'a stop is not also called Failed');
    // The tooltip has the whole event, with the count.
    assert.match(w.msTooltipFooter([{ raw: data[1] }]), /All 80 planned ops sent, repeating/);
    assert.equal(markers[3].color, w.eval('colors').red);
});

test('a long run keeps every sample and draws dips that thinning would drop', async () => {
    const { w } = await page();
    const pts = Array.from({ length: 20000 }, (_, i) => ({ seq: i + 1, elapsedSec: (i + 1) / 2, activeSec: (i + 1) / 2, rates: { total: i === 12345 ? 0 : 5000 }, totals: { total: 5000 * (i + 1) }, avg: {}, p95: {}, p99: {} }));
    w.applyLiveStats(single(pts, { totalIterations: 1 }));
    const s = w.eval('msServers')[0];
    assert.equal(s.points.length, 20000, 'every sample is kept');
    const drawn = w.eval('msCharts').throughput.data.datasets[0].data;
    assert.ok(drawn.length <= 2100, `drawn ${drawn.length}`);
    assert.ok(drawn.some(d => d.y === 0 && d.x === 6173), 'the one-sample dip is still drawn');
    assert.equal(drawn[drawn.length - 1].x, 10000, 'the last sample is drawn');
});

test('the dashboard keeps polling from a worker timer and tells the server the refresh rate', async () => {
    const { w } = await page();
    const made = [];
    w.Worker = class { constructor() { made.push(this); } postMessage(ms) { this.ms = ms; } terminate() { this.stopped = true; } };
    w.URL.createObjectURL = () => 'blob:x';
    w.eval('historicalView = ""');
    w.document.getElementById('refreshRate').value = '250';
    w.updateRefreshRate();
    w.startPolling();
    assert.equal(made.length, 1);
    assert.equal(made[0].ms, 250);
    assert.match(w.liveStatsURL(), /&refreshMs=250$/);
    w.stopPolling();
    assert.equal(made[0].stopped, true);
});
