// DOM tests for deterministic generated traffic: the checkbox and budget on
// the Record & Replay tab (default off, loading, submission, validation) and the
// traffic fingerprint card on the dashboard.
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

test('the deterministic checkbox and budget default off and are not submitted as on', async () => {
    const { doc, errors } = await page();
    const box = doc.getElementById('workloadDeterministic');
    assert.equal(box.type, 'checkbox');
    assert.equal(box.checked, false);
    for (const name of ['workload_deterministic', 'operations_per_worker']) {
        const el = doc.querySelector(`[name="${name}"]`);
        assert.equal(el.closest('#workloadRecordReplayCard') !== null, true, name);
        assert.ok(el.closest('#configForm'), `${name} is submitted with the run`);
    }
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.equal(fd.get('workload_deterministic'), null);
    assert.equal(fd.get('operations_per_worker'), '0');
    assert.equal(doc.getElementById('generatedTrafficCard').style.display, 'none');
    assert.deepEqual(errors, []);
});

test('loaded config values fill the deterministic controls', async () => {
    const { doc, errors } = await page({ WorkloadDeterministic: true, OperationsPerWorker: 500, RandomSeed: 9 });
    const fd = new doc.defaultView.FormData(doc.getElementById('configForm'));
    assert.equal(fd.get('workload_deterministic'), 'true');
    assert.equal(fd.get('operations_per_worker'), '500');
    assert.deepEqual(errors, []);
});

test('incomplete deterministic settings block the run on the Record & Replay tab', async () => {
    const { w, doc, errors } = await page();
    const set = (id, v) => { doc.getElementById(id).value = v; };
    const check = msg => {
        assert.equal(w.validateConfigForm(), false);
        assert.ok(doc.getElementById('tab-replay').classList.contains('active'));
        assert.match(doc.getElementById('configInlineError').textContent, msg);
    };
    set('operationsPerWorker', '-2');
    check(/Operations per Worker must be/);
    set('operationsPerWorker', '0');
    assert.equal(w.validateWorkloadRecordReplay(), '', 'budget alone with deterministic off is fine');

    doc.getElementById('workloadDeterministic').checked = true;
    set('randomSeedInput', '0');
    check(/non-zero Random Seed/);
    set('randomSeedInput', '5');
    check(/Operations per Worker greater than 0/);
    set('operationsPerWorker', '10');
    doc.getElementById('dropColl').checked = true;
    doc.getElementById('skipSeed').checked = false;
    set('loadProfileKind', 'ramp');
    check(/fixed worker count/);
    set('loadProfileKind', 'fixed');
    doc.getElementById('dropColl').checked = false;
    check(/Drop Collections on and Skip Seed off/);
    doc.getElementById('dropColl').checked = true;
    doc.getElementById('skipSeed').checked = true;
    check(/Drop Collections on and Skip Seed off/);
    doc.getElementById('skipSeed').checked = false;
    set('workloadReplayEnabled', 'true');
    set('workloadReplayPath', 'in.ndjson');
    check(/cannot both be on/);
    set('workloadReplayEnabled', 'false');
    doc.getElementById('rawEnable').checked = true;
    check(/raw injector/);
    doc.getElementById('rawEnable').checked = false;
    assert.equal(w.validateWorkloadRecordReplay(), '');
    assert.deepEqual(errors, []);
});

test('same values from equal fingerprints do not mark a summary as containing literal values', async () => {
    const { w, errors } = await page();
    const traffic = { rows: [{ query_values: { state: 'same_values', source: 'same_generated_traffic', label: 'Same values: both runs generated the same deterministic traffic' } }] };
    assert.equal(w.trendHasValues(traffic), true, 'the values column is still shown');
    assert.equal(w.trendHasLiteralValues(traffic), false);
    const literal = { rows: [{ query_values: { state: 'same_values', current: { samples: [] } } }] };
    assert.equal(w.trendHasLiteralValues(literal), true);
    assert.deepEqual(errors, []);
});

test('the dashboard shows the traffic fingerprint (hashes only) and hides it for other runs', async () => {
    const { w, doc, errors } = await page();
    const fp = 'ab'.repeat(32);
    w.renderFinalInsights({
        metadata: {
            status: 'disabled', generated_traffic: {
                fingerprint: fp, commands_hash: 'cd'.repeat(32), status: 'incomplete', reason: 'the safety timeout (duration=5s) was reached',
                plgm_version: 'dev', collections_hash: 'ef'.repeat(32), queries_hash: '01'.repeat(32), random_seed: 7, workers: 2,
                operations_per_worker: 10, iterations: 1, ops_planned: 20, ops_generated: 12, ops_sent: 12,
                worker_hashes: { '10': 'h10', '2': 'h2' }, safety_timeout: '5s',
            },
        }, summary: {},
    });
    const card = doc.getElementById('generatedTrafficCard');
    assert.equal(card.style.display, '');
    assert.equal(doc.getElementById('generatedTrafficFingerprint').textContent, fp);
    assert.equal(doc.getElementById('generatedTrafficStatus').textContent, 'incomplete');
    const text = card.textContent;
    assert.ok(text.includes('safety timeout (duration=5s)') && text.includes('planned 20, generated 12, sent 12') && text.includes('Cross-worker interleaving'));
    assert.ok(text.indexOf('worker 2:') < text.indexOf('worker 10:'), 'worker hashes in numeric order');

    w.renderFinalInsights({ metadata: { status: 'disabled' }, summary: {} });
    assert.equal(card.style.display, 'none');
    assert.equal(doc.getElementById('generatedTrafficBody').innerHTML, '');
    assert.deepEqual(errors, []);
});
