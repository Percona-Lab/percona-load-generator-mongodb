// DOM tests for "Use defaults" on the task cards, "Reset to defaults" in the task
// header, and for picking the newest recording when Replay opens.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

// The last run left a ramp, deterministic traffic, three targets, and a
// custom mix; the startup configuration has none of that.
const LAST_RUN = {
    URI: 'mongodb://last-run', Concurrency: 64, Duration: '5m', DocumentsCount: 5000, FindPercent: 10, UpdatePercent: 90,
    LoadProfile: { Kind: 'ramp', StartWorkers: 2, EndWorkers: 40, RampOver: '30s' }, RandomSeed: 7, WorkloadDeterministic: true, OperationsPerWorker: 100,
    DropCollections: true, WorkloadRecordOutcomes: true, InsightsSlowThresholdMs: 5,
    WorkloadReplayTargets: [{ label: 'a', uri: 'mongodb://a:1' }, { label: 'b', uri: 'mongodb://b:2' }],
};
const DEFAULTS = { URI: 'mongodb://startup', Concurrency: 16, Duration: '60s', DocumentsCount: 100000, FindPercent: 60, UpdatePercent: 20, DeletePercent: 10, InsertPercent: 5, BulkInsertPercent: 5, InsightsEnabled: true };

async function page(recordings = []) {
    const p = await loadPage(fixture('trends'), {
        '/api/config?defaults=1': async () => ({ body: DEFAULTS }),
        '/api/config': async () => ({ body: LAST_RUN }),
        '/api/recordings': async () => ({ body: { directories: ['/srv/plgm'], recordings } }),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return p;
}
const val = (doc, name) => doc.querySelector(`[name="${name}"]`).value;

test('every task card has its own Use defaults button', async () => {
    const { doc, errors } = await page();
    const cards = doc.querySelectorAll('.mode-card');
    assert.equal(cards.length, 5);
    cards.forEach(card => {
        const btn = card.parentElement.querySelector('.mode-card-defaults');
        assert.ok(btn, card.dataset.mode);
        assert.equal(btn.textContent.trim(), 'Use defaults');
        assert.equal(btn.title, 'Settings PLGM started with (config.yaml). Your connection is kept.');
        assert.match(btn.getAttribute('aria-label'), /, use defaults$/);
        assert.equal(card.contains(btn), false, 'the defaults button is not nested inside the task button');
    });
    assert.deepEqual(errors, []);
});

test('Use defaults on a card selects the task with the startup settings and keeps the connection', async () => {
    const { w, doc, errors } = await page();
    assert.equal(val(doc, 'concurrency'), '64', 'the page starts from the last run');
    doc.querySelector('[name="uri"]').value = 'mongodb://typed-by-user';
    const btn = doc.querySelector('.mode-card[data-mode="generic"]').parentElement.querySelector('.mode-card-defaults');
    btn.click();
    await settle();
    assert.equal(doc.getElementById('operationMode').value, 'generic');
    assert.equal(val(doc, 'uri'), 'mongodb://typed-by-user', 'the connection is kept');
    assert.equal(val(doc, 'concurrency'), '16');
    assert.equal(val(doc, 'duration'), '60s');
    assert.equal(val(doc, 'documents_count'), '100000');
    assert.equal(val(doc, 'find_percent'), '60');
    assert.equal(val(doc, 'load_profile_kind'), 'fixed');
    assert.equal(val(doc, 'insights_slow_threshold_ms'), '200');
    assert.equal(doc.getElementById('workloadDeterministic').checked, false);
    assert.equal(doc.querySelectorAll('#workloadReplayTargetsList .replay-target-row').length, 0);
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'false');
    const status = doc.getElementById('defaultsStatus');
    assert.equal(status.hidden, false);
    assert.match(status.textContent, /Default settings loaded\. Your connection was kept\./);
    assert.equal(w.validateConfigForm(), true);
    assert.deepEqual(errors, []);
});

test('the task header offers Reset to defaults for the selected task', async () => {
    const { w, doc, errors } = await page();
    const header = doc.getElementById('modeResetDefaults');
    assert.equal(header.hidden, true, 'no task, no header button');
    assert.equal(header.textContent.trim(), 'Reset to defaults');
    w.selectMode('record');
    assert.equal(header.hidden, false);
    doc.querySelector('[name="concurrency"]').value = '99';
    header.click();
    await settle();
    assert.equal(doc.getElementById('operationMode').value, 'record');
    assert.equal(doc.getElementById('workloadRecordEnabled').value, 'true', 'the task still records');
    assert.equal(val(doc, 'concurrency'), '16');
    assert.equal(val(doc, 'workload_record_path'), '', 'a blank name gets a new file per run');
    w.selectMode('generic');
    assert.equal(doc.getElementById('defaultsStatus').hidden, true, 'the note goes away when the task changes');
    assert.deepEqual(errors, []);
});

test('Use defaults and Reset to defaults are unavailable while a workload runs', async () => {
    const { w, doc, errors } = await page();
    w.setRunModeLock(true);
    doc.querySelectorAll('.mode-card-defaults').forEach(b => assert.equal(b.disabled, true));
    assert.equal(doc.getElementById('modeResetDefaults').disabled, true);
    await w.startFromDefaults('generic');
    assert.equal(val(doc, 'concurrency'), '64', 'nothing changes while locked');
    w.setRunModeLock(false);
    assert.equal(doc.querySelector('.mode-card-defaults').disabled, false);
    assert.deepEqual(errors, []);
});

test('opening Replay selects the newest recording that can be replayed', async () => {
    const recordings = [
        { path: 'broken.ndjson', name: 'broken.ndjson', size_bytes: 10, modified_at: '2026-10-26T12:00:00Z', problem: 'unsupported workload recording format' },
        { path: 'plgm_workload_record-20261026T110000Z-ab12cd34.ndjson', name: 'plgm_workload_record-20261026T110000Z-ab12cd34.ndjson', size_bytes: 2048, modified_at: '2026-10-26T11:00:00Z', ops: 10 },
        { path: 'older.ndjson', name: 'older.ndjson', size_bytes: 1024, modified_at: '2026-10-25T11:00:00Z' },
    ];
    const { w, doc, errors } = await page(recordings);
    await w.startFromDefaults('replay');
    await settle();
    assert.equal(doc.getElementById('workloadReplayRecording').value, recordings[1].path);
    assert.equal(val(doc, 'workload_replay_path'), recordings[1].path);
    assert.match(doc.getElementById('workloadReplayRecordingStatus').textContent, /The newest one, plgm_workload_record-20261026T110000Z-ab12cd34\.ndjson, is selected\./);
    assert.equal(w.validateWorkloadRecordReplay(), '');

    // A recording the user picked is kept on refresh, not replaced by the newest.
    const sel = doc.getElementById('workloadReplayRecording');
    sel.value = 'older.ndjson';
    sel.dispatchEvent(new w.Event('change', { bubbles: true }));
    await w.loadServerRecordings();
    assert.equal(sel.value, 'older.ndjson');
    assert.deepEqual(errors, []);
});
