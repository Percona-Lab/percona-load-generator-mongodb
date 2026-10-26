// DOM tests for the multi-target replay workflow: the server recording
// selector (loading, empty, error, refresh, saved paths), the Replay Targets
// dependencies (Record Outcomes turned on, recording and no-source guidance),
// generated outcome files, and the comparison report links.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

const REC_A = { path: 'plgm_workload_record.ndjson', name: 'plgm_workload_record.ndjson', source: 'directory', size_bytes: 3 << 20, modified_at: '2026-10-25T14:02:00Z', created_at: '2026-10-25T14:00:00Z', workers: 4, ops: 149420, status: 'complete' };
const REC_B = { path: '/tmp/plgm-traffic-import-1.ndjson', name: 'app.log', source: 'import', size_bytes: 2048, modified_at: '2026-10-24T09:00:00Z', imported: true };
const BAD = { path: 'future.ndjson', name: 'future.ndjson', source: 'directory', size_bytes: 10, modified_at: '2026-10-20T09:00:00Z', problem: 'unsupported workload recording format "plgm-workload-record/9"' };

async function page(cfg = {}, recordings = () => ({ body: { directories: ['/srv/plgm'], recordings: [REC_A, REC_B, BAD] } })) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true, ...cfg } }),
        '/api/recordings': async () => recordings(),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return p;
}

const formData = doc => new doc.defaultView.FormData(doc.getElementById('configForm'));
const change = (doc, id, value) => {
    const el = doc.getElementById(id);
    if (el.type === 'checkbox') el.checked = value; else el.value = value;
    el.dispatchEvent(new doc.defaultView.Event('change', { bubbles: true }));
};

test('choosing Replay lists the recordings on the PLGM server and a choice fills the replay path', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('replay');
    await settle();
    assert.ok(w.fetchCalls.some(c => c.url === '/api/recordings'));
    const sel = doc.getElementById('workloadReplayRecording');
    const opts = Array.from(sel.options);
    assert.equal(opts.length, 4, 'placeholder plus three files');
    assert.match(opts[1].textContent, /plgm_workload_record\.ndjson · 149,420 ops · 3\.0 MiB · 2026-10-25 14:00/);
    assert.match(opts[2].textContent, /app\.log · 2\.0 KiB .*imported traffic/);
    assert.equal(opts[3].disabled, true, 'a file that cannot be replayed cannot be chosen');
    assert.match(opts[3].textContent, /cannot be replayed: unsupported/);
    assert.match(doc.getElementById('workloadReplayRecordingStatus').textContent, /3 recordings on the PLGM server in \/srv\/plgm/);
    assert.equal(doc.getElementById('workloadReplayManual').open, false, 'manual entry stays an alternative');

    sel.value = REC_A.path;
    sel.dispatchEvent(new w.Event('change', { bubbles: true }));
    assert.equal(formData(doc).get('workload_replay_path'), REC_A.path);
    assert.equal(w.validateWorkloadRecordReplay(), '');
    assert.deepEqual(errors, []);
});

test('refresh keeps a recording that still exists and clears one that is gone', async () => {
    let list = [REC_A, REC_B];
    const { w, doc, errors } = await page({}, () => ({ body: { directories: ['/srv/plgm'], recordings: list } }));
    w.selectMode('replay');
    await settle();
    const sel = doc.getElementById('workloadReplayRecording');
    sel.value = REC_B.path;
    sel.dispatchEvent(new w.Event('change', { bubbles: true }));

    list = [REC_B, REC_A];
    await w.loadServerRecordings();
    assert.equal(sel.value, REC_B.path, 'the selection survives a refresh');
    assert.equal(doc.getElementById('workloadReplayPath').value, REC_B.path);

    list = [REC_A];
    await w.loadServerRecordings();
    assert.equal(sel.value, '');
    assert.equal(doc.getElementById('workloadReplayPath').value, '', 'a vanished recording is not replayed by mistake');
    assert.match(doc.getElementById('workloadReplayRecordingStatus').textContent, /is no longer available: choose another one/);
    assert.match(w.validateWorkloadRecordReplay(), /choose a recording on the PLGM server/);
    assert.deepEqual(errors, []);
});

test('empty and failed listings explain what to do and keep manual entry available', async () => {
    let reply = { body: { directories: ['/srv/plgm'], recordings: [] } };
    const { w, doc, errors } = await page({}, () => reply);
    w.selectMode('replay');
    await settle();
    const status = doc.getElementById('workloadReplayRecordingStatus');
    assert.match(status.textContent, /No recordings found on the PLGM server in \/srv\/plgm\. Record a workload first/);
    assert.equal(doc.getElementById('workloadReplayRecording').options[0].textContent, 'No recordings found');

    reply = { status: 500, body: { error: 'disk unavailable' } };
    await w.loadServerRecordings();
    assert.match(status.textContent, /Could not list the recordings on the PLGM server \(disk unavailable\)/);
    assert.equal(status.dataset.state, 'error');
    assert.equal(doc.getElementById('workloadReplayManual').open, true);
    assert.equal(doc.getElementById('workloadReplayRecordingRefresh').disabled, false, 'refresh can be retried');
    assert.deepEqual(errors, []);
});

test('a saved server path stays in use and a typed path that is listed selects it', async () => {
    const { w, doc, errors } = await page({ WorkloadReplayEnabled: true, WorkloadReplayPath: '/data/old.ndjson' });
    await settle();
    assert.equal(doc.getElementById('operationMode').value, 'replay');
    assert.equal(doc.getElementById('workloadReplayPath').value, '/data/old.ndjson', 'saved configurations keep their path');
    assert.equal(doc.getElementById('workloadReplayManual').open, true);
    assert.match(doc.getElementById('workloadReplayRecordingStatus').textContent, /\/data\/old\.ndjson\) is used; it is not in this list/);

    const path = doc.getElementById('workloadReplayPath');
    path.value = REC_A.path;
    path.dispatchEvent(new w.Event('input', { bubbles: true }));
    assert.equal(doc.getElementById('workloadReplayRecording').value, REC_A.path);
    assert.deepEqual(errors, []);
});

test('listing Replay Targets turns Record Outcomes on with a note and restores it when they go', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('replay');
    await settle();
    const outcomes = doc.getElementById('workloadRecordOutcomes');
    assert.equal(outcomes.value, 'false');
    w.setReplayTargetRows([{ label: 'm9', uri: 'mongodb://a:1' }, { label: 'rs02', uri: 'mongodb://b:2' }, { label: 'rs03', uri: 'mongodb://c:3' }]);
    assert.equal(outcomes.value, 'true');
    const note = doc.getElementById('workloadRecordOutcomesAuto');
    assert.equal(note.hidden, false);
    assert.match(note.textContent, /On because Replay Targets are listed/);
    assert.match(doc.getElementById('replayTargetsStatus').textContent, /compared with m9.*new outcome file is generated for each target/);

    change(doc, 'workloadRecordOutcomes', 'false');
    assert.equal(outcomes.value, 'true', 'stays on while the targets need it');

    doc.getElementById('workloadReplayOutcomesPath').value = 'blog.ndjson';
    doc.getElementById('workloadReplayOutcomesPath').dispatchEvent(new w.Event('input', { bubbles: true }));
    assert.match(doc.getElementById('replayTargetsStatus').textContent, /its own copy of the outcome file you entered/);
    assert.equal(formData(doc).get('workload_replay_outcomes_path'), 'blog.ndjson', 'an explicit file name is kept');

    w.setReplayTargetRows([]);
    assert.equal(outcomes.value, 'false');
    assert.equal(note.hidden, true);
    assert.deepEqual(errors, []);
});

const shown = (doc, id) => !doc.getElementById(id).hidden;

test('Run on in Target Connection switches between MongoDB URI and the server list', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('replay');
    await settle();
    const conn = doc.querySelector('[name="uri"]').closest('.settings-card');
    assert.ok(conn.contains(doc.getElementById('runOnRow')), 'Run on sits with the connection');
    assert.ok(conn.contains(doc.getElementById('workloadReplayTargetsList')), 'the server list sits with the connection');
    assert.equal(conn.dataset.group, 'basic', 'and is a Basic setting');
    assert.equal(shown(doc, 'runOnRow'), true);
    assert.equal(doc.getElementById('runOnOne').checked, true, 'one server by default');
    assert.equal(shown(doc, 'uriGroup'), true);
    assert.equal(shown(doc, 'multiServerBlock'), false);

    doc.getElementById('runOnSeveral').click();
    assert.equal(shown(doc, 'uriGroup'), false, 'the list replaces MongoDB URI');
    assert.equal(shown(doc, 'multiServerBlock'), true);
    assert.equal(doc.querySelectorAll('#workloadReplayTargetsList .replay-target-row').length, 2, 'two empty rows to start');
    doc.querySelector('[name="uri"]').value = '';
    assert.equal(w.validateConfigForm(), false);
    assert.match(doc.getElementById('configInlineError').textContent, /got 0\): add the servers, or choose Run on: One server/);
    assert.doesNotMatch(doc.getElementById('configInlineError').textContent, /MongoDB URI is required/, 'a hidden URI is not required');
    assert.equal(doc.activeElement.closest('.tab-content')?.id, 'tab-conn');

    w.setReplayTargetRows([{ label: 'rs01', uri: 'mongodb://a:1' }, { label: 'rs02', uri: 'mongodb://b:2' }]);
    assert.deepEqual(JSON.parse(formData(doc).get('workload_replay_targets')).map(t => t.label), ['rs01', 'rs02']);
    assert.match(doc.getElementById('effectiveSummary').textContent, /2 replay targets/);

    doc.getElementById('runOnOne').click();
    assert.equal(shown(doc, 'uriGroup'), true);
    assert.equal(shown(doc, 'multiServerBlock'), false);
    assert.equal(formData(doc).get('workload_replay_targets'), '', 'one server: the list is kept but not sent');
    assert.equal(doc.querySelectorAll('.replay-target-label').length, 2);
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'false', 'outcomes go back once the targets are not used');
    assert.equal(w.validateConfigForm(), false);
    assert.match(doc.getElementById('configInlineError').textContent, /MongoDB URI is required/);
    assert.deepEqual(errors, []);
});

test('Starting-Data Fingerprint sits in the Record card when recording and in the Replay card otherwise', async () => {
    const { w, doc, errors } = await page();
    const fp = doc.getElementById('workloadDataFingerprint');
    w.selectMode('record');
    assert.equal(fp.closest('.settings-card').id, 'rrRecordCard');
    fp.value = 'counts';
    w.selectMode('replay');
    assert.equal(fp.closest('.settings-card').id, 'rrReplayCard');
    assert.equal(fp.value, 'counts', 'moving it keeps the value');
    assert.equal(doc.querySelectorAll('#workloadDataFingerprint').length, 1);
    assert.deepEqual(errors, []);
});

test('recording hides the server choice and sends no targets, and Replay brings them back', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('replay');
    w.setReplayTargetRows([{ label: 'm9', uri: 'mongodb://a:1' }, { label: 'rs02', uri: 'mongodb://b:2' }]);
    w.selectMode('record');
    assert.equal(shown(doc, 'runOnRow'), false);
    assert.equal(shown(doc, 'multiServerBlock'), false);
    assert.equal(shown(doc, 'uriGroup'), true, 'a recording runs on MongoDB URI');
    assert.equal(formData(doc).get('workload_replay_targets'), '');
    assert.equal(doc.querySelectorAll('.replay-target-uri')[1].value, 'mongodb://b:2', 'values are kept');
    assert.doesNotMatch(w.validateWorkloadRecordReplay(), /Replay Target/);

    w.selectMode('replay');
    assert.equal(doc.getElementById('runOnSeveral').checked, true);
    assert.equal(shown(doc, 'multiServerBlock'), true);
    assert.equal(shown(doc, 'uriGroup'), false);
    assert.deepEqual(JSON.parse(formData(doc).get('workload_replay_targets')).map(t => t.label), ['m9', 'rs02']);
    assert.deepEqual(errors, []);
});

test('generic and custom runs offer several servers only with deterministic traffic', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('generic');
    assert.equal(shown(doc, 'runOnRow'), false, 'no traffic source, no choice');
    w.setReplayTargetRows([{ label: 'm9', uri: 'mongodb://a:1' }, { label: 'rs02', uri: 'mongodb://b:2' }]);
    assert.equal(formData(doc).get('workload_replay_targets'), '', 'a generic run does not go to the targets');
    assert.equal(shown(doc, 'uriGroup'), true);
    assert.equal(w.validateWorkloadRecordReplay(), '', 'launch is not blocked');
    assert.match(doc.getElementById('workloadDeterministicNote').textContent, /does not make latency, throughput, or results identical/);

    doc.getElementById('randomSeedInput').value = '7';
    doc.getElementById('operationsPerWorker').value = '100';
    change(doc, 'workloadDeterministic', true);
    assert.equal(shown(doc, 'runOnRow'), true);
    assert.equal(shown(doc, 'multiServerBlock'), true);
    assert.equal(shown(doc, 'uriGroup'), false);
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'true');
    assert.match(doc.getElementById('replayTargetsStatus').textContent, /compared with m9/);
    assert.deepEqual(JSON.parse(formData(doc).get('workload_replay_targets')).map(t => t.label), ['m9', 'rs02']);
    assert.match(doc.getElementById('effectiveSummary').textContent, /2 replay targets/);
    assert.equal(w.validateWorkloadRecordReplay(), '');
    assert.deepEqual(errors, []);
});

test('comparison reports open in a new tab or download, and outcome files are shown', async () => {
    const { w, doc, errors } = await page();
    w.renderMultiTarget({
        kind: 'replay', mode: 'sequential', reference: 'm9',
        targets: [
            { index: 1, label: 'm9', status: 'complete', ops_done: 2, elapsed_sec: 1, outcome_file: '/srv/plgm/plgm_replay_outcomes-x.m9.ndjson' },
            { index: 2, label: 'rs02', status: 'complete', ops_done: 2, elapsed_sec: 1, outcome_file: '/srv/plgm/plgm_replay_outcomes-x.rs02.ndjson' },
        ],
        comparisons: [{ reference: 'm9', target: 'rs02', report_id: 'abc123', differing_ops: 982, latency_verdict: 'no_material_change' }],
    });
    const open = doc.querySelector('#multiTargetComparisons a.report-open');
    assert.equal(open.getAttribute('href'), '/api/replay-compare/abc123/report.html');
    assert.equal(open.getAttribute('target'), '_blank');
    assert.equal(open.getAttribute('rel'), 'noopener noreferrer');
    assert.match(open.textContent, /Open report for m9 vs rs02 \(opens in a new tab\)/);
    const dl = doc.querySelector('#multiTargetComparisons a.report-download');
    assert.equal(dl.getAttribute('href'), '/api/replay-compare/abc123/report.html?download=1');
    assert.equal(dl.getAttribute('download'), 'plgm-replay-compare-abc123.html');
    const rows = doc.querySelectorAll('#multiTargetRows tr');
    assert.equal(rows[1].lastElementChild.textContent, 'plgm_replay_outcomes-x.rs02.ndjson');
    assert.equal(rows[1].lastElementChild.title, '/srv/plgm/plgm_replay_outcomes-x.rs02.ndjson');
    assert.match(doc.getElementById('multiTargetOutcomeDir').textContent, /Outcome files are on the PLGM server in \/srv\/plgm\//);

    w.renderRunOutcomeFile('/srv/plgm/plgm_replay_outcomes-y.ndjson');
    const single = doc.getElementById('runOutcomeFile');
    assert.equal(single.hidden, false);
    assert.match(single.textContent, /Outcome file on the PLGM server: \/srv\/plgm\/plgm_replay_outcomes-y\.ndjson/);
    w.renderRunOutcomeFile(undefined);
    assert.equal(single.hidden, true);
    assert.deepEqual(errors, []);
});
