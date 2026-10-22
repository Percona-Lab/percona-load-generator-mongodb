// DOM tests for the Reproducible Workload settings on the Record & Replay
// tab: defaults, loading from /api/config, form submission, and client-side
// validation.
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

const FIELDS = ['random_seed', 'workload_record_enabled', 'workload_record_path', 'workload_record_max_bytes',
    'workload_record_backpressure', 'workload_record_compression', 'workload_replay_enabled', 'workload_replay_path', 'workload_replay_file', 'workload_replay_mode', 'workload_replay_allow_mismatch',
    'workload_record_outcomes', 'workload_record_result_checksums', 'workload_replay_outcomes_path', 'workload_deterministic_outcomes_path'];

test('record and replay controls live on the Record & Replay tab, default off, and are submitted', async () => {
    const { doc, errors } = await page();
    const w = doc.defaultView;
    w.selectMode('record');
    w.toggleTier('advanced');
    doc.getElementById('snav-reproducibility').click();
    assert.equal(doc.getElementById('snav-reproducibility').getAttribute('aria-selected'), 'true');
    assert.equal(doc.getElementById('rrSeedCard').dataset.groupHidden, '0', 'Record & replay shows its advanced cards');
    w.selectMode('');
    for (const name of FIELDS) {
        const el = doc.querySelector(`[name="${name}"]`);
        assert.ok(el, name);
        assert.equal(el.closest('.tab-content').id, 'tab-replay', name);
        assert.ok(el.closest('#configForm'), `${name} is submitted with the run`);
    }
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.equal(fd.get('random_seed'), '0');
    assert.equal(fd.get('workload_record_enabled'), 'false');
    assert.equal(fd.get('workload_replay_enabled'), 'false');
    assert.equal(fd.get('workload_replay_mode'), 'per_worker');
    assert.equal(fd.get('workload_replay_allow_mismatch'), 'false');
    assert.equal(fd.get('workload_record_max_bytes'), '536870912');
    assert.equal(fd.get('workload_record_backpressure'), 'fail');
    assert.equal(fd.get('workload_record_compression'), 'none');
    assert.equal(fd.get('workload_record_outcomes'), 'false');
    assert.equal(fd.get('workload_record_result_checksums'), 'false');
    assert.equal(fd.get('workload_replay_outcomes_path'), '');
    assert.equal(fd.get('workload_deterministic_outcomes_path'), '');
    assert.match(doc.getElementById('workloadReplayFile').getAttribute('accept'), /\.zst.*\.gz/);
    const card = doc.getElementById('workloadRecordReplayCard').textContent;
    assert.ok(card.includes('Off by default') && card.includes('real query values') && card.includes('cannot both be on'));
    assert.deepEqual(errors, []);
});

test('section tabs show one panel at a time, and validation opens the panel of the named setting', async () => {
    const { w, doc, errors } = await page();
    const panels = () => [...doc.querySelectorAll('#tab-replay .rr-panel.active')].map(p => p.id);
    const selected = () => [...doc.querySelectorAll('#tab-replay .rr-tab[aria-selected="true"]')].map(b => b.getAttribute('aria-controls'));
    const tabs = [...doc.querySelectorAll('#tab-replay .rr-tab')];
    assert.deepEqual(tabs.map(b => b.getAttribute('aria-controls')),
        ['rrSeedCard', 'rrRecordCard', 'rrReplayCard', 'rrTargetsCard', 'rrOutcomeCard', 'trafficImportCard']);
    assert.deepEqual(panels(), ['rrSeedCard']);
    for (const btn of tabs) {
        btn.click();
        const id = btn.getAttribute('aria-controls');
        assert.deepEqual(panels(), [id]);
        assert.deepEqual(selected(), [id]);
    }

    doc.getElementById('workloadRecordResultChecksums').value = 'true';
    assert.equal(w.validateConfigForm(), false);
    assert.deepEqual(panels(), ['rrOutcomeCard'], 'Result Checksums lives in Outcome Capture');
    doc.getElementById('workloadRecordResultChecksums').value = 'false';

    tabs[0].click();
    doc.getElementById('workloadReplayEnabled').value = 'true';
    assert.equal(w.validateConfigForm(), false);
    assert.deepEqual(panels(), ['rrReplayCard'], 'a replay without a recording opens Replay');
    assert.deepEqual(errors, []);
});

test('loaded config values fill the record and replay controls', async () => {
    const { doc, errors } = await page({
        RandomSeed: 42, WorkloadRecordEnabled: true, WorkloadRecordPath: 'rec.ndjson', WorkloadRecordMaxBytes: 4096,
        WorkloadReplayPath: 'in.ndjson', WorkloadReplayMode: 'timed', WorkloadReplayAllowMismatch: true,
    });
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.deepEqual(
        ['random_seed', 'workload_record_enabled', 'workload_record_path', 'workload_record_max_bytes', 'workload_replay_enabled', 'workload_replay_path', 'workload_replay_mode', 'workload_replay_allow_mismatch'].map(n => fd.get(n)),
        ['42', 'true', 'rec.ndjson', '4096', 'false', 'in.ndjson', 'timed', 'true']);
    assert.deepEqual(errors, []);
});

test('random seeds beyond 2^53 keep every digit in the form and on the dashboard', async () => {
    const big = '9007199254740993';
    const rep = fixture('trends');
    rep.metadata.generated_traffic = { fingerprint: 'ab'.repeat(32), status: 'complete', random_seed: 1, workers: 1, worker_hashes: {} };
    const { doc, errors } = await loadPage(rep, {
        '/api/config': async () => ({ rawBody: `{"URI":"mongodb://fixture","RandomSeed":${big}}` }),
        '/api/insights': async () => ({ rawBody: JSON.stringify(rep).replace('"random_seed":1', `"random_seed":${big}`) }),
    });
    await settle();
    assert.equal(new doc.defaultView.FormData(doc.getElementById('configForm')).get('random_seed'), big);
    assert.ok(doc.getElementById('generatedTrafficCard').textContent.includes(`Random seed ${big} `));
    assert.deepEqual(errors, []);
});

test('an unlimited size, block policy, and compression load from the config', async () => {
    const { doc, errors } = await page({
        WorkloadRecordEnabled: true, WorkloadRecordMaxBytes: 0, WorkloadRecordBackpressure: 'block', WorkloadRecordCompression: 'zstd',
    });
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.deepEqual(
        ['workload_record_max_bytes', 'workload_record_backpressure', 'workload_record_compression'].map(n => fd.get(n)),
        ['0', 'block', 'zstd']);
    assert.deepEqual(errors, []);
});

test('conflicting or incomplete record/replay settings block the run on the Record & Replay tab', async () => {
    const { w, doc, errors } = await page();
    const set = (id, v) => { doc.getElementById(id).value = v; };
    const check = msg => {
        assert.equal(w.validateConfigForm(), false);
        assert.equal(doc.activeElement.closest('.tab-content')?.id, 'tab-replay', 'focus moves to the setting to fix');
        assert.match(doc.getElementById('configInlineError').textContent, msg);
    };
    set('workloadRecordEnabled', 'true');
    set('workloadReplayEnabled', 'true');
    set('workloadReplayPath', 'in.ndjson');
    check(/cannot both be on/);

    set('workloadRecordEnabled', 'false');
    set('workloadReplayPath', '');
    check(/enter the recording path or upload a recording/);

    set('workloadReplayPath', 'in.ndjson');
    assert.equal(w.validateWorkloadRecordReplay(), '');
    set('workloadReplayEnabled', 'false');
    set('randomSeedInput', '1.5');
    assert.match(w.validateWorkloadRecordReplay(), /whole number/);
    set('randomSeedInput', '-3');
    assert.equal(w.validateWorkloadRecordReplay(), '');

    set('workloadRecordEnabled', 'true');
    doc.querySelector('[name="iterations"]').value = '3';
    assert.equal(w.validateWorkloadRecordReplay(), '', 'recording several iterations is allowed');
    doc.querySelector('[name="iterations"]').value = '1';
    assert.equal(w.validateWorkloadRecordReplay(), '');
    doc.getElementById('rawEnable').checked = true;
    check(/raw injector/);
    doc.getElementById('rawEnable').checked = false;
    set('workloadRecordEnabled', 'false');
    assert.deepEqual(errors, []);
});

test('outcome capture settings load from the config', async () => {
    const { doc, errors } = await page({
        WorkloadRecordOutcomes: true, WorkloadRecordResultChecksums: true,
        WorkloadReplayOutcomesPath: 'replay_out.ndjson', WorkloadDeterministicOutcomesPath: 'det_out.ndjson',
    });
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.deepEqual(
        ['workload_record_outcomes', 'workload_record_result_checksums', 'workload_replay_outcomes_path', 'workload_deterministic_outcomes_path'].map(n => fd.get(n)),
        ['true', 'true', 'replay_out.ndjson', 'det_out.ndjson']);
    assert.deepEqual(errors, []);
});

test('incomplete outcome capture settings block the run on the Record & Replay tab', async () => {
    const { w, doc, errors } = await page();
    const set = (id, v) => { doc.getElementById(id).value = v; };
    const check = msg => {
        assert.equal(w.validateConfigForm(), false);
        assert.equal(doc.activeElement.closest('.tab-content')?.id, 'tab-replay', 'focus moves to the setting to fix');
        assert.match(doc.getElementById('configInlineError').textContent, msg);
    };
    set('workloadRecordResultChecksums', 'true');
    check(/Result Checksums needs Record Outcomes on/);
    set('workloadRecordResultChecksums', 'false');
    set('workloadReplayOutcomesPath', 'out.ndjson');
    check(/Replay Outcome File is set: turn Record Outcomes on/);
    set('workloadReplayOutcomesPath', '');
    set('workloadDeterministicOutcomesPath', 'det.ndjson');
    check(/Deterministic Outcome File is set: turn Record Outcomes on/);
    set('workloadDeterministicOutcomesPath', '');

    set('workloadRecordOutcomes', 'true');
    check(/Record Outcomes needs Record Workload on/);
    set('workloadRecordEnabled', 'true');
    set('workloadRecordResultChecksums', 'true');
    assert.equal(w.validateWorkloadRecordReplay(), '', 'outcomes and checksums inline in a recording');
    set('workloadReplayOutcomesPath', 'out.ndjson');
    check(/Replay Outcome File is set but Replay Workload is off/);

    set('workloadRecordEnabled', 'false');
    set('workloadReplayEnabled', 'true');
    set('workloadReplayPath', 'in.ndjson');
    assert.equal(w.validateWorkloadRecordReplay(), '');
    set('workloadReplayOutcomesPath', '');
    check(/during a replay needs a Replay Outcome File/);
    set('workloadReplayEnabled', 'false');
    set('workloadReplayPath', '');

    set('workloadDeterministicOutcomesPath', 'det.ndjson');
    check(/Deterministic Outcome File is set but Deterministic traffic is off/);
    doc.getElementById('workloadDeterministic').checked = true;
    set('randomSeedInput', '7');
    set('operationsPerWorker', '10');
    doc.getElementById('dropColl').checked = true;
    doc.getElementById('skipSeed').checked = false;
    assert.equal(w.validateWorkloadRecordReplay(), '');
    set('workloadDeterministicOutcomesPath', '');
    check(/deterministic run needs a Deterministic Outcome File or Record Workload on/);
    assert.deepEqual(errors, []);
});
