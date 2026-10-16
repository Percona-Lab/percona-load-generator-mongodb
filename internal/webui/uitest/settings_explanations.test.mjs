// DOM tests for the explanations next to the opt-in settings: each states
// what happens when on/set and when off/empty (the default), sits next to
// its control in the right section, and the controls render their defaults.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

// control id -> [note id, tab, card id or null, ON phrase, OFF phrase, extra phrases]
const SETTINGS = {
    trendStoreQueryValues: ['trendStoreQueryValuesNote', 'tab-trends', null, 'On:', 'Off (default):',
        ['real query values', 'PLGM_INSIGHTS_TREND_STORE_QUERY_VALUES']],
    randomSeedInput: ['randomSeedNote', 'tab-replay', 'rrSeedCard', 'Set (non-zero):', '0 (default):',
        ['identical documents', 'random_seed', 'PLGM_RANDOM_SEED']],
    workloadRecordEnabled: ['workloadRecordEnabledNote', 'tab-replay', 'rrRecordCard', 'On:', 'Off (default):',
        ['real query values', 'per operation', 'PLGM_WORKLOAD_RECORD_ENABLED']],
    workloadRecordPath: ['workloadRecordPathNote', 'tab-replay', 'rrRecordCard', 'Set:', 'Empty (default):',
        ['0600', 'PLGM_WORKLOAD_RECORD_PATH']],
    workloadRecordMaxBytes: ['workloadRecordMaxBytesNote', 'tab-replay', 'rrRecordCard', 'Set:', 'Empty (default):',
        ['truncated', '0:', 'unlimited', '536870912', 'PLGM_WORKLOAD_RECORD_MAX_BYTES']],
    workloadRecordBackpressure: ['workloadRecordBackpressureNote', 'tab-replay', 'rrRecordCard', 'Block:', 'Fail (default):',
        ['every operation is recorded', 'throughput may drop', 'PLGM_WORKLOAD_RECORD_BACKPRESSURE']],
    workloadRecordCompression: ['workloadRecordCompressionNote', 'tab-replay', 'rrRecordCard', 'zstd / gzip:', 'None (default):',
        ['uncompressed bytes', 'PLGM_WORKLOAD_RECORD_COMPRESSION']],
    workloadReplayEnabled: ['workloadReplayEnabledNote', 'tab-replay', 'rrReplayCard', 'On:', 'Off (default):',
        ['Duration, load profile, access pattern, and think time are ignored', 'PLGM_WORKLOAD_REPLAY_ENABLED']],
    workloadReplayPath: ['workloadReplayPathNote', 'tab-replay', 'rrReplayCard', 'Set:', 'Empty (default):',
        ['uploaded file', 'PLGM_WORKLOAD_REPLAY_PATH']],
    workloadReplayMode: ['workloadReplayModeNote', 'tab-replay', 'rrReplayCard', 'Timed:', 'Per worker (default):',
        ['lag', 'PLGM_WORKLOAD_REPLAY_MODE']],
    workloadReplayAllowMismatch: ['workloadReplayAllowMismatchNote', 'tab-replay', 'rrReplayCard', 'On:', 'Off (default):',
        ['not be comparable', 'PLGM_WORKLOAD_REPLAY_ALLOW_MISMATCH']],
    workloadDeterministic: ['workloadDeterministicNote', 'tab-replay', 'rrSeedCard', 'On:', 'Off (default):',
        ['without a recording file', 'safety timeout', 'traffic fingerprint', 'Cross-worker interleaving', 'workload_deterministic', 'PLGM_WORKLOAD_DETERMINISTIC']],
    operationsPerWorker: ['operationsPerWorkerNote', 'tab-replay', 'rrSeedCard', 'Set:', '0 (default):',
        ['exactly this many operations', 'operations_per_worker', 'PLGM_OPERATIONS_PER_WORKER']],
    workloadRecordOutcomes: ['workloadRecordOutcomesNote', 'tab-replay', 'rrOutcomeCard', 'On:', 'Off (default):',
        ['client latency', 'never the message', 'matched/modified/upserted', 'commands sent, stats, insights, and trends do not change', 'workload_record_outcomes', 'PLGM_WORKLOAD_RECORD_OUTCOMES']],
    workloadRecordResultChecksums: ['workloadRecordResultChecksumsNote', 'tab-replay', 'rrOutcomeCard', 'On:', 'Off (default):',
        ['order-sensitive sha256', 'worker CPU', 'after the latency is measured', 'needs Record Outcomes on', 'PLGM_WORKLOAD_RECORD_RESULT_CHECKSUMS']],
    workloadReplayOutcomesPath: ['workloadReplayOutcomesPathNote', 'tab-replay', 'rrOutcomeCard', 'Set:', 'Empty (default):',
        ['0600', "recording's sequence numbers", 'PLGM_WORKLOAD_REPLAY_OUTCOMES_PATH']],
    workloadDeterministicOutcomesPath: ['workloadDeterministicOutcomesPathNote', 'tab-replay', 'rrOutcomeCard', 'Set:', 'Empty (default):',
        ['0600', 'iteration, worker, and operation number', 'traffic fingerprint', 'PLGM_WORKLOAD_DETERMINISTIC_OUTCOMES_PATH']],
    workloadReplayOps: ['workloadReplayOpsNote', 'tab-replay', 'rrReplayCard', 'Reads only:', 'All (default):',
        ['skips every write', 'counted and reported', 'workload_replay_ops', 'PLGM_WORKLOAD_REPLAY_OPS']],
    workloadReplayTargetsList: ['workloadReplayTargetsNote', 'tab-replay', 'rrTargetsCard', 'Listed (2&ndash;8 targets):'.replace('&ndash;', '\u2013'), 'Empty (default):',
        ['only on those', 'MongoDB URI is not used', 'own client, workers, stats, outcome file', 'compared with the first target', 'workload_replay_targets', 'PLGM_WORKLOAD_REPLAY_TARGETS']],
    workloadReplayTargetsMode: ['workloadReplayTargetsModeNote', 'tab-replay', 'rrTargetsCard', 'Concurrent:', 'Sequential (default):',
        ['latencies may be affected', 'client-side lag', 'workload_replay_targets_mode', 'PLGM_WORKLOAD_REPLAY_TARGETS_MODE']],
    workloadDataFingerprint: ['workloadDataFingerprintNote', 'tab-replay', 'rrTargetsCard', 'Counts / dbHash:', 'Off (default):',
        ['document count and index list', 'fails on a difference', 'byte-identical copies', 'workload_data_fingerprint', 'PLGM_WORKLOAD_DATA_FINGERPRINT']],
};

async function page() {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true } }),
    });
    await settle();
    return p;
}

test('every opt-in setting explains On and Off (default) next to its control', async () => {
    const { doc, errors } = await page();
    for (const [id, [noteId, tab, cardId, on, off, extra]] of Object.entries(SETTINGS)) {
        const control = doc.getElementById(id);
        const note = doc.getElementById(noteId);
        assert.ok(control && note, id);
        assert.equal(control.closest('.tab-content').id, tab, `${id} tab`);
        assert.equal(note.closest('.tab-content').id, tab, `${noteId} tab`);
        assert.equal(note.closest('.settings-card'), control.closest('.settings-card'), `${noteId} is in the same section as ${id}`);
        if (cardId) {
            assert.equal(control.closest('.settings-card').id, cardId, `${id} card`);
            assert.ok(control.closest('#workloadRecordReplayCard'), `${id} is in the Record & Replay area`);
            assert.ok(control.closest('.setting-row').contains(note), `${noteId} is on the same row as ${id}`);
        }
        const text = note.textContent;
        assert.ok(text.indexOf(on) >= 0, `${noteId}: ${on}`);
        assert.ok(text.indexOf(off) > text.indexOf(on), `${noteId}: ${off} follows ${on}`);
        for (const s of extra) assert.ok(text.includes(s), `${noteId}: ${s}`);
        const label = doc.querySelector(`label[for="${id}"]`);
        assert.ok(label, `${id} has a label`);
    }
    const card = doc.getElementById('workloadRecordReplayCard');
    assert.match(card.querySelector('h4').textContent, /Reproducible Workload \(Record & Replay\)/);
    const trendCard = doc.getElementById('trendStoreQueryValues').closest('.settings-card');
    assert.ok(trendCard.querySelector('[name="trend_history_runs"]'), 'trend values setting sits with the other trend settings');
    assert.deepEqual(errors, []);
});

test('opt-in settings render off/empty by default', async () => {
    const { doc, errors } = await page();
    assert.equal(doc.getElementById('trendStoreQueryValues').checked, false);
    assert.equal(doc.getElementById('randomSeedInput').value, '0');
    assert.equal(doc.getElementById('workloadRecordEnabled').value, 'false');
    assert.equal(doc.getElementById('workloadRecordPath').value, '');
    assert.equal(doc.getElementById('workloadRecordMaxBytes').value, '536870912');
    assert.equal(doc.getElementById('workloadRecordBackpressure').value, 'fail');
    assert.equal(doc.getElementById('workloadRecordCompression').value, 'none');
    assert.equal(doc.getElementById('workloadReplayEnabled').value, 'false');
    assert.equal(doc.getElementById('workloadReplayPath').value, '');
    assert.equal(doc.getElementById('workloadReplayFile').files.length, 0);
    assert.equal(doc.getElementById('workloadReplayMode').value, 'per_worker');
    assert.equal(doc.getElementById('workloadReplayAllowMismatch').value, 'false');
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'false');
    assert.equal(doc.getElementById('workloadRecordResultChecksums').value, 'false');
    assert.equal(doc.getElementById('workloadReplayOutcomesPath').value, '');
    assert.equal(doc.getElementById('workloadDeterministicOutcomesPath').value, '');
    assert.equal(doc.getElementById('workloadReplayOps').value, 'all');
    assert.equal(doc.getElementById('workloadReplayTargetsMode').value, 'sequential');
    assert.equal(doc.getElementById('workloadDataFingerprint').value, 'off');
    assert.equal(doc.getElementById('workloadReplayTargets').value, '');
    assert.deepEqual(errors, []);
});
