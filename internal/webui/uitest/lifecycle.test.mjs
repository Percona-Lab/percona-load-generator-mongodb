// DOM tests for the live header clock during seeding and for reattaching to a
// run that is still going after a reload or a second start.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 30));

const seeding = {
    isRunning: true,
    lifecyclePhase: 'initializing',
    lifecycleStep: 'Seeding initial data',
    lifecycleStepIndex: 5,
    lifecycleStepTotal: 5,
    lifecycleStepProgressPct: 0,
    lifecycleMessage: 'Seeding up to 400000 documents per collection',
    initializationElapsedSec: 12.4,
    executionElapsedSec: 0,
    initializationDurationSec: 0,
    duration: '20s',
    totalIterations: 1,
    intervalDelay: '0s',
};

test('the header clock counts preparation time while seeding', async () => {
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: { isRunning: false, lifecyclePhase: 'idle' } }),
    });
    await settle();
    w.applyLiveStats(seeding);
    assert.equal(doc.getElementById('statusIndicator').textContent, 'INITIALIZING');
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:12 init');
    assert.equal(doc.getElementById('timerNote').style.display, 'none');
    assert.equal(doc.getElementById('progressBar').style.width, '0%');
    assert.match(doc.getElementById('iterationBadge').textContent, /Seeding initial data/);

    w.applyLiveStats({ ...seeding, initializationElapsedSec: 13.1, lifecycleStepProgressPct: 40 });
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:13 init');
    assert.equal(doc.getElementById('progressBar').style.width, '40%');

    w.applyLiveStats({
        ...seeding,
        lifecyclePhase: 'running',
        executionElapsedSec: 4.2,
        initializationDurationSec: 13.1,
        initializationElapsedSec: 13.1,
    });
    assert.equal(doc.getElementById('statusIndicator').textContent, 'RUNNING');
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:04 / 00:20');
    assert.ok(Math.abs(parseFloat(doc.getElementById('progressBar').style.width) - 21) < 0.01);
    assert.deepEqual(errors, []);
});

test('the clock total includes the pause between iterations', async () => {
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: { isRunning: false, lifecyclePhase: 'idle' } }),
    });
    await settle();
    const paced = {
        isRunning: true,
        executionElapsedSec: 0,
        duration: '30s',
        totalIterations: 5,
        intervalDelay: '10s',
        initializationElapsedSec: 12,
        initializationDurationSec: 0,
        lifecycleStep: 'Seeding initial data',
        lifecycleStepIndex: 5,
        lifecycleStepTotal: 5,
    };
    w.applyLiveStats({ ...paced, lifecyclePhase: 'initializing' });
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:12 init');
    assert.equal(doc.getElementById('timerNote').style.display, 'none');

    w.applyLiveStats({
        ...paced,
        lifecyclePhase: 'running',
        executionElapsedSec: 101,
        initializationDurationSec: 12,
        initializationElapsedSec: 12,
    });
    assert.equal(doc.getElementById('timerDisplay').textContent, '01:41 / 03:10');
    assert.equal(doc.getElementById('timerNote').textContent, '5 runs of 30s, plus 10s between runs (4 pauses).');
    assert.equal(doc.getElementById('timerNote').style.display, 'block');
    w.stopPolling();
    assert.deepEqual(errors, []);
});

test('a deterministic run keeps the duration clock and continues past one pass', async () => {
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: { isRunning: false, lifecyclePhase: 'idle' } }),
    });
    await settle();
    w.applyLiveStats({
        isRunning: true,
        lifecyclePhase: 'running',
        workloadDeterministic: true,
        operationsPerWorker: 100,
        executionElapsedSec: 4.2,
        duration: '30s',
        totalIterations: 1,
        intervalDelay: '0s',
        initializationDurationSec: 12,
    });
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:04 / 00:30');
    assert.match(doc.getElementById('lifecycleHint').textContent, /continues that same workload until 00:30/);

    w.applyLiveStats({
        isRunning: false,
        lifecyclePhase: 'completed',
        workloadDeterministic: true,
        operationsPerWorker: 100,
        executionElapsedSec: 30,
        duration: '30s',
        totalIterations: 1,
        intervalDelay: '0s',
        generatedTraffic: {
            status: 'complete', operations_per_worker: 100, workers: 4,
            ops_sent: 2400, ops_planned: 400, safety_timeout: '30s',
        },
    });
    assert.equal(doc.getElementById('statusIndicator').textContent, 'FINISHED');
    assert.equal(doc.getElementById('iterationBadge').textContent, '1 / 1 COMPLETED');
    assert.match(doc.getElementById('lifecycleHint').textContent, /continued until 30s/);
    assert.equal(doc.getElementById('timerDisplay').textContent, '00:30 / 00:30');
    w.stopPolling();
    assert.deepEqual(errors, []);
});

test('a reload during a run returns to that run', async () => {
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: seeding }),
    });
    await settle();
    try {
        assert.equal(doc.getElementById('configForm').style.display, 'none');
        assert.equal(doc.getElementById('dashboard').style.display, 'block');
        assert.equal(doc.getElementById('statusIndicator').textContent, 'INITIALIZING');
        assert.equal(doc.getElementById('timerDisplay').textContent, '00:12 init');
        assert.notEqual(doc.getElementById('errorModal').style.display, 'flex');
    } finally {
        w.stopPolling();
    }
    assert.deepEqual(errors, []);
});

test('a reload restores chart history from the start of the run', async () => {
    const point = (seq, elapsed, findRate, findTotal, marker) => ({
        seq,
        elapsedSec: elapsed,
        iteration: 1,
        marker: marker || '',
        rates: { total: findRate, find: findRate, insert: 0, update: 0, delete: 0, aggregate: 0 },
        totals: { total: findTotal, find: findTotal, insert: 0, update: 0, delete: 0, aggregate: 0 },
        avg: { find: 1.5 },
        p95: { find: 2 },
        p99: { find: 3 },
    });
    const running = {
        isRunning: true,
        lifecyclePhase: 'running',
        executionElapsedSec: 10,
        initializationDurationSec: 3,
        initializationElapsedSec: 3,
        duration: '60s',
        totalIterations: 1,
        intervalDelay: '0s',
        currentIteration: 1,
        findOps: 5000,
        insertOps: 0,
        updateOps: 0,
        upsertOps: 0,
        deleteOps: 0,
        aggOps: 0,
        chartHistory: [point(1, 1, 10, 10, 'Iter 1'), point(2, 5, 20, 100), point(3, 10, 12, 5000)],
    };
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/stats': async () => ({ body: running }),
    });
    await settle();
    try {
        assert.equal(doc.getElementById('timeScrubber').max, '2');
        assert.equal(doc.getElementById('rate-finds').textContent, '12 ops/sec');
        assert.equal(doc.getElementById('timerDisplay').textContent, '00:10 / 01:00');
        w.applyLiveStats({ ...running, chartHistory: [point(4, 11, 15, 5100)] });
        assert.equal(doc.getElementById('timeScrubber').max, '3');
        assert.equal(doc.getElementById('rate-finds').textContent, '15 ops/sec');
        w.applyLiveStats({ ...running, chartHistory: [point(1, 1, 10, 10, 'Iter 1'), point(2, 5, 20, 100)] });
        assert.equal(doc.getElementById('timeScrubber').max, '3');
    } finally {
        w.stopPolling();
    }
    assert.deepEqual(errors, []);
});

test('starting while a workload is already running opens that run', async () => {
    let started = false;
    const { w, doc, errors } = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture' } }),
        '/api/start': async () => {
            started = true;
            return { status: 409, body: { status: 'error', message: 'workload is already running' } };
        },
        '/api/stats': async () => ({
            body: started ? seeding : { isRunning: false, lifecyclePhase: 'idle' },
        }),
    });
    await settle();
    assert.notEqual(doc.getElementById('dashboard').style.display, 'block');
    const fd = new w.FormData();
    fd.set('duration', '20s');
    fd.set('iterations', '1');
    fd.set('interval_delay', '0s');
    w.launchWorkload(fd);
    await settle();
    try {
        assert.equal(doc.getElementById('configForm').style.display, 'none');
        assert.equal(doc.getElementById('dashboard').style.display, 'block');
        assert.equal(doc.getElementById('statusIndicator').textContent, 'INITIALIZING');
        assert.equal(doc.getElementById('timerDisplay').textContent, '00:12 init');
        assert.notEqual(doc.getElementById('errorModal').style.display, 'flex');
    } finally {
        w.stopPolling();
    }
    assert.deepEqual(errors, []);
});
