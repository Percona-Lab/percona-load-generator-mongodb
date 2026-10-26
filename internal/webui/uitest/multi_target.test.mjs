// DOM tests for multi-target replay: the target list editor, mode, replay
// operations, and starting-data fingerprint controls (defaults, loading,
// submission, validation), the write confirmation, and the dashboard card.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page(cfg = {}) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true, ...cfg } }),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return p;
}

const formData = doc => new doc.defaultView.FormData(doc.getElementById('configForm'));

test('multi-target settings default off and submit no targets', async () => {
    const { doc, errors } = await page();
    const fd = formData(doc);
    assert.equal(fd.get('workload_replay_targets'), '');
    assert.equal(fd.get('workload_replay_targets_mode'), 'sequential');
    assert.equal(fd.get('workload_replay_ops'), 'all');
    assert.equal(fd.get('workload_data_fingerprint'), 'off');
    assert.equal(doc.querySelectorAll('#workloadReplayTargetsList .replay-target-row').length, 0);
    assert.equal(doc.getElementById('multiTargetCard').style.display, 'none');
    assert.deepEqual(errors, []);
});

test('loaded targets fill the editor and edits are submitted as JSON', async () => {
    const { w, doc, errors } = await page({
        WorkloadReplayEnabled: true, WorkloadReplayPath: 'rec.ndjson',
        WorkloadReplayTargets: [{ label: 'm7', uri: 'mongodb://a:1' }, { label: 'm8', uri: 'mongodb://b:2' }],
        WorkloadReplayTargetsMode: 'concurrent', WorkloadReplayOps: 'reads_only', WorkloadDataFingerprint: 'dbhash',
    });
    const rows = doc.querySelectorAll('#workloadReplayTargetsList .replay-target-row');
    assert.equal(rows.length, 2);
    assert.equal(rows[1].querySelector('.replay-target-uri').value, 'mongodb://b:2');
    let fd = formData(doc);
    assert.deepEqual(JSON.parse(fd.get('workload_replay_targets')), [{ label: 'm7', uri: 'mongodb://a:1' }, { label: 'm8', uri: 'mongodb://b:2' }]);
    assert.equal(fd.get('workload_replay_targets_mode'), 'concurrent');
    assert.equal(fd.get('workload_replay_ops'), 'reads_only');
    assert.equal(fd.get('workload_data_fingerprint'), 'dbhash');

    w.addReplayTargetRow('m9', 'mongodb://c:3');
    const label = doc.querySelectorAll('.replay-target-label')[0];
    label.value = 'm7x';
    label.dispatchEvent(new w.Event('input', { bubbles: true }));
    rows[1].querySelector('.replay-target-remove').click();
    fd = formData(doc);
    assert.deepEqual(JSON.parse(fd.get('workload_replay_targets')).map(t => t.label), ['m7x', 'm9']);
    assert.deepEqual(errors, []);
});

test('invalid multi-target settings block the run on the Record & Replay tab', async () => {
    const { w, doc, errors } = await page();
    const set = (id, v) => { doc.getElementById(id).value = v; };
    // Server list problems are fixed in Target Connection (Basic settings).
    const check = (msg, tab = 'tab-replay') => {
        assert.equal(w.validateConfigForm(), false);
        assert.equal(doc.activeElement.closest('.tab-content')?.id, tab, 'focus moves to the setting to fix');
        assert.match(doc.getElementById('configInlineError').textContent, msg);
    };
    set('workloadReplayOps', 'reads_only');
    check(/Reads only" needs Replay Workload on/);
    set('workloadReplayOps', 'all');
    set('workloadDataFingerprint', 'counts');
    check(/Starting-Data Fingerprint needs Record Workload/);
    set('workloadDataFingerprint', 'off');

    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'm9', uri: 'mongodb://b:2' }]);
    // Without a replay or deterministic traffic the targets are kept but not sent.
    assert.equal(w.validateWorkloadRecordReplay(), '');
    assert.equal(formData(doc).get('workload_replay_targets'), '');
    assert.match(doc.getElementById('replayTargetsStatus').textContent, /connects only to the server in the MongoDB URI field/);
    set('workloadReplayEnabled', 'true');
    set('workloadReplayPath', 'rec.ndjson');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }]);
    check(/2 to 8 targets/, 'tab-conn');
    w.addReplayTargetRow('m9', 'mongodb://b:2');
    assert.equal(w.validateWorkloadRecordReplay(), '', 'Record Outcomes is turned on and a blank outcome file is generated');
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'true');
    set('workloadReplayOutcomesPath', 'out.ndjson');
    assert.equal(w.validateWorkloadRecordReplay(), '');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'M8', uri: 'mongodb://b:2' }]);
    check(/used twice/, 'tab-conn');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'bad label', uri: 'mongodb://b:2' }]);
    check(/label must be/, 'tab-conn');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'm9', uri: 'http://b:2' }]);
    check(/must start with mongodb/, 'tab-conn');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'm9', uri: 'mongodb://a:1' }]);
    check(/same URI is listed twice/, 'tab-conn');
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'm9', uri: 'mongodb://b:2' }]);
    set('workloadReplayEnabled', 'false');
    set('workloadReplayOutcomesPath', '');
    set('workloadRecordEnabled', 'true');
    doc.getElementById('workloadDeterministic').checked = true;
    set('randomSeedInput', '5');
    set('operationsPerWorker', '10');
    doc.getElementById('dropColl').checked = true;
    doc.getElementById('skipSeed').checked = false;
    // Recording cannot use targets: they are kept, disabled, and not submitted.
    assert.equal(w.validateWorkloadRecordReplay(), '');
    assert.equal(formData(doc).get('workload_replay_targets'), '');
    assert.equal(doc.querySelectorAll('.replay-target-label').length, 2);
    assert.ok(doc.querySelector('.replay-target-label').disabled);
    assert.match(doc.getElementById('replayTargetsStatus').textContent, /not used while recording.*Replay a recording/);
    doc.getElementById('workloadDeterministic').checked = false;
    set('workloadRecordEnabled', 'false');
    set('workloadReplayEnabled', 'true');
    set('workloadReplayOutcomesPath', 'out.ndjson');
    doc.querySelector('[name="uri"]').value = '';
    assert.equal(w.validateWorkloadRecordReplay(), '', 'with targets listed, MongoDB URI is not required');
    assert.deepEqual(errors, []);
});

test('a run that writes to the targets asks for confirmation first', async () => {
    const { w, doc, errors } = await page();
    const asked = [];
    let answer = false;
    w.confirm = msg => { asked.push(msg); return answer; };
    assert.equal(w.confirmMultiTargetWrites(), true, 'no targets, no question');
    assert.equal(asked.length, 0);
    w.setReplayTargetRows([{ label: 'm8', uri: 'mongodb://a:1' }, { label: 'm9', uri: 'mongodb://b:2' }]);
    doc.getElementById('workloadReplayEnabled').value = 'true';
    doc.getElementById('dropColl').checked = false;
    doc.getElementById('skipSeed').checked = true;
    assert.equal(w.confirmMultiTargetWrites(), false);
    assert.match(asked[0], /replay the recorded writes on 2 targets: m8, m9/);
    assert.doesNotMatch(asked[0], /mongodb:\/\//);
    answer = true;
    assert.equal(w.confirmMultiTargetWrites(), true);
    doc.getElementById('workloadReplayOps').value = 'reads_only';
    asked.length = 0;
    assert.equal(w.confirmMultiTargetWrites(), true);
    assert.equal(asked.length, 0, 'a reads-only replay without drop or seed writes nothing');
    doc.getElementById('dropColl').checked = true;
    w.confirmMultiTargetWrites();
    assert.match(asked[0], /drop the collections/);
    assert.deepEqual(errors, []);
});

test('the dashboard card shows per-target progress and the comparisons', async () => {
    const { w, doc, errors } = await page();
    w.renderMultiTarget({
        kind: 'replay', mode: 'concurrent', reference: 'm8',
        targets: [
            { index: 1, label: 'm8', status: 'complete', ops_done: 10, ops_total: 12, ops_skipped: 2, elapsed_sec: 1.5, lag_max_ms: 3, lag_avg_ms: 1, data_fingerprint_match: true },
            { index: 2, label: 'm9', status: 'failed', error: 'connection refused', ops_done: 0, elapsed_sec: 0 },
        ],
        comparisons: [{ reference: 'm8', target: 'm9', error: 'target m9 has no outcome file (failed)' }],
        notes: ['The targets ran concurrently from one PLGM host: latencies may be affected.'],
    });
    const card = doc.getElementById('multiTargetCard');
    assert.notEqual(card.style.display, 'none');
    const rows = doc.querySelectorAll('#multiTargetRows tr');
    assert.equal(rows.length, 2);
    assert.match(rows[0].textContent, /10 \/ 12 \(2 writes skipped\)/);
    assert.match(rows[0].textContent, /max 3\.0 ms, avg 1\.0 ms/);
    assert.match(rows[0].textContent, /matches recording/);
    assert.match(rows[1].textContent, /failed: connection refused/);
    assert.match(doc.getElementById('multiTargetSummary').textContent, /Concurrent replay on 2 targets; each is compared with m8/);
    assert.match(doc.getElementById('multiTargetComparisons').textContent, /m8 vs m9: not compared/);
    assert.match(doc.getElementById('multiTargetNotes').textContent, /latencies may be affected/);

    w.renderMultiTarget({ kind: 'deterministic', mode: 'sequential', reference: 'a', fingerprints_match: true, targets: [{ index: 1, label: 'a', status: 'complete' }, { index: 2, label: 'b', status: 'complete' }],
        comparisons: [{ reference: 'a', target: 'b', differing_ops: 0, latency_verdict: 'no_change', report_id: 'abc123' }] });
    assert.match(doc.getElementById('multiTargetComparisons').textContent, /fingerprints match the reference: yes/);
    assert.equal(doc.querySelector('#multiTargetComparisons a').getAttribute('href'), '/api/replay-compare/abc123/report.html');
    w.renderMultiTarget(undefined);
    assert.equal(card.style.display, 'none');
    assert.deepEqual(errors, []);
});
