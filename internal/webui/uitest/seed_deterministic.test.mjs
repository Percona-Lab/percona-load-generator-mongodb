// DOM tests for the Seed & Deterministic section: naming and order, the
// settings shown only with Deterministic traffic, the server choice that
// moves next to it (one set of controls, one state), and what turning it on
// and off changes, keeps, and submits.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page(cfg = {}) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true, ...cfg } }),
        '/api/recordings': async () => ({ body: { directories: [], recordings: [] } }),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return p;
}

const formData = doc => new doc.defaultView.FormData(doc.getElementById('configForm'));
const fire = (doc, el, type = 'change') => el.dispatchEvent(new doc.defaultView.Event(type, { bubbles: true }));
const setDet = (doc, on) => { const el = doc.getElementById('workloadDeterministic'); el.checked = on; fire(doc, el); };
const navLabels = doc => [...doc.querySelectorAll('#settingsNavList .settings-nav-label')].map(e => e.textContent);
const selectedGroup = doc => doc.querySelector('#settingsNavList [aria-selected="true"]')?.dataset.group;
const seedCard = doc => doc.getElementById('rrSeedCard');
const connCard = doc => doc.getElementById('uriGroup').closest('.settings-card');

// A generic run with Advanced open on Seed & Deterministic.
async function genericOnSeedPage(cfg) {
    const p = await page(cfg);
    p.w.selectMode('generic');
    p.w.toggleTier('advanced');
    p.w.selectSettingsGroup('reproducibility');
    return p;
}

test('the category reads Seed & Deterministic and Deterministic traffic comes before Random Seed', async () => {
    const { w, doc, errors } = await genericOnSeedPage();
    assert.ok(navLabels(doc).includes('Seed & Deterministic'));
    assert.ok(!navLabels(doc).some(l => /Record & replay/i.test(l)));
    assert.equal(selectedGroup(doc), 'reproducibility');
    assert.equal(seedCard(doc).querySelector('h4').textContent.trim(), 'Seed & Deterministic');
    const det = doc.getElementById('workloadDeterministic');
    const seed = doc.getElementById('randomSeedInput');
    assert.ok(det.compareDocumentPosition(seed) & doc.defaultView.Node.DOCUMENT_POSITION_FOLLOWING, 'Deterministic traffic is first');
    assert.equal(seed.closest('[hidden]'), null, 'Random Seed is usable without deterministic traffic');
    // A replay uses neither: the category holds outcome and import settings.
    w.selectMode('replay');
    await settle();
    if (!doc.body.classList.contains('tier-advanced-open')) w.toggleTier('advanced');
    assert.ok(navLabels(doc).includes('Outcomes & import'));
    assert.ok(!navLabels(doc).includes('Seed & Deterministic'));
    assert.deepEqual(errors, []);
});

test('Operations per Worker appears only with Deterministic traffic and says when it is missing', async () => {
    const { doc, errors } = await genericOnSeedPage();
    const row = doc.getElementById('operationsPerWorkerRow');
    const hint = doc.getElementById('operationsPerWorkerHint');
    assert.equal(row.hidden, true);
    setDet(doc, true);
    assert.equal(row.hidden, false);
    assert.equal(hint.hidden, false, 'the missing budget is flagged before launch');
    const ops = doc.getElementById('operationsPerWorker');
    ops.value = '500';
    fire(doc, ops, 'input');
    assert.equal(hint.hidden, true);
    setDet(doc, false);
    assert.equal(row.hidden, true);
    assert.equal(formData(doc).get('operations_per_worker'), '500', 'a hidden value is kept, not reset');
    assert.deepEqual(errors, []);
});

test('turning Deterministic traffic on sets a seed only when none is set, and off keeps a later edit', async () => {
    const { doc, errors } = await genericOnSeedPage();
    const seed = doc.getElementById('randomSeedInput');
    const note = doc.getElementById('workloadDeterministicAuto');
    setDet(doc, true);
    const picked = seed.value;
    assert.ok(Number(picked) > 0, 'a non-zero seed is filled in');
    assert.match(note.textContent, new RegExp(`Random Seed is ${picked}`));
    seed.value = '99';
    fire(doc, seed);
    setDet(doc, false);
    assert.equal(seed.value, '99', 'the seed the user typed is not undone');
    assert.match(note.textContent, /Kept your later change to Random Seed/);
    assert.equal(doc.getElementById('dropColl').checked, false, 'untouched values are restored');

    // A seed the user already chose is never replaced.
    setDet(doc, true);
    assert.equal(seed.value, '99');
    assert.doesNotMatch(note.textContent, /Random Seed/);
    assert.equal(formData(doc).get('random_seed'), '99');
    assert.deepEqual(errors, []);
});

test('several servers are chosen in Target Connection and turn on Deterministic traffic with a default budget', async () => {
    const { w, doc, errors } = await genericOnSeedPage();
    assert.ok(connCard(doc).contains(doc.getElementById('runOnRow')), 'the choice is in Target Connection');
    assert.equal(doc.getElementById('detServersSlot').hidden, true);
    assert.equal(doc.getElementById('serversDetNote').hidden, true);
    const det = doc.getElementById('workloadDeterministic');
    assert.equal(det.checked, false);
    assert.equal(seedCard(doc).dataset.tier, 'advanced');

    doc.getElementById('runOnSeveral').click();
    assert.equal(det.checked, true, 'several servers need the same operations on each');
    assert.equal(doc.getElementById('operationsPerWorker').value, '500', 'a default budget makes the run ready to launch');
    assert.ok(Number(doc.getElementById('randomSeedInput').value) > 0, 'a seed is filled in');
    const note = doc.getElementById('serversDetNote');
    assert.equal(note.hidden, false);
    assert.match(note.textContent, /Deterministic traffic was turned on/);
    assert.match(note.textContent, /Operations per Worker is set to 500/);
    assert.match(note.textContent, /marked incomplete/);
    assert.equal(seedCard(doc).dataset.tier, 'basic', 'its settings are in Basic while it applies');
    assert.equal(seedCard(doc).dataset.group, 'basic');
    assert.equal(doc.getElementById('uriGroup').hidden, true, 'MongoDB URI is not used');
    assert.match(doc.getElementById('runOnSeveralNote').textContent, /each server in the list below with the URI you enter for it; the single MongoDB URI field is hidden and ignored.*drops, seeding, and writes/);
    w.setReplayTargetRows([{ label: 'a1', uri: 'mongodb://a:1' }, { label: 'b2', uri: 'mongodb://b:2' }]);
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'true', 'Record Outcomes follows the targets');
    assert.equal(w.validateWorkloadRecordReplay(), '');

    // Back to one server: what was turned on for the servers is undone.
    doc.getElementById('runOnOne').click();
    assert.equal(det.checked, false);
    assert.equal(doc.getElementById('operationsPerWorker').value, '0');
    assert.equal(doc.getElementById('serversDetNote').hidden, true);
    assert.equal(seedCard(doc).dataset.tier, 'advanced');
    assert.equal(formData(doc).get('workload_replay_targets'), '');

    // A budget the user typed is kept, and so is Deterministic traffic they changed themselves.
    doc.getElementById('operationsPerWorker').value = '250';
    doc.getElementById('runOnSeveral').click();
    assert.equal(doc.getElementById('operationsPerWorker').value, '250');
    assert.doesNotMatch(doc.getElementById('serversDetNote').textContent, /is set to 500/);
    setDet(doc, false);
    assert.match(doc.getElementById('serversDetNote').textContent, /needs .*Deterministic traffic/);
    doc.getElementById('runOnOne').click();
    assert.equal(doc.getElementById('operationsPerWorker').value, '250');
    assert.deepEqual(errors, []);
});

test('a target problem found at launch is shown where the server list is', async () => {
    const { w, doc, errors } = await genericOnSeedPage();
    doc.getElementById('randomSeedInput').value = '7';
    doc.getElementById('operationsPerWorker').value = '100';
    setDet(doc, true);
    doc.getElementById('runOnSeveral').click();
    assert.equal(w.validateConfigForm(), false);
    assert.match(doc.getElementById('configInlineError').textContent, /Replay Targets needs 2 to 8 targets/);
    assert.equal(selectedGroup(doc), 'basic', 'Target Connection is open');
    assert.ok(connCard(doc).contains(doc.activeElement), 'focus is on the server list');
    assert.deepEqual(errors, []);
});

test('turning Deterministic traffic off keeps the servers and says the run uses MongoDB URI', async () => {
    const { w, doc, errors } = await genericOnSeedPage();
    doc.getElementById('randomSeedInput').value = '7';
    doc.getElementById('operationsPerWorker').value = '100';
    setDet(doc, true);
    doc.getElementById('runOnSeveral').click();
    w.setReplayTargetRows([{ label: 'a1', uri: 'mongodb://a:1' }, { label: 'b2', uri: 'mongodb://b:2' }]);
    assert.equal(JSON.parse(formData(doc).get('workload_replay_targets')).length, 2);

    setDet(doc, false);
    const note = doc.getElementById('workloadDeterministicAuto');
    assert.match(note.textContent, /this run connects only to the server in the MongoDB URI field\. The 2 listed servers are kept for later\./);
    assert.equal(formData(doc).get('workload_replay_targets'), '', 'targets are not sent without a traffic source');
    assert.equal(doc.getElementById('uriGroup').hidden, false);
    assert.equal(doc.getElementById('detServersSlot').hidden, true);
    assert.ok(connCard(doc).contains(doc.getElementById('runOnRow')));
    assert.equal(doc.getElementById('workloadRecordOutcomes').value, 'false', 'Record Outcomes goes back to its earlier value');
    assert.equal(w.validateWorkloadRecordReplay(), '');

    setDet(doc, true);
    assert.equal(doc.getElementById('runOnSeveral').checked, true, 'the choice comes back with it');
    assert.equal(JSON.parse(formData(doc).get('workload_replay_targets')).length, 2);
    assert.deepEqual(errors, []);
});

test('a saved deterministic multi-server config opens with the servers in Target Connection', async () => {
    const { w, doc, errors } = await page({
        WorkloadDeterministic: true, RandomSeed: 11, OperationsPerWorker: 50, DropCollections: true, SkipSeed: false, LoadProfile: { Kind: 'fixed' },
        WorkloadRecordOutcomes: true,
        WorkloadReplayTargets: [{ Label: 'a1', URI: 'mongodb://a:1' }, { Label: 'b2', URI: 'mongodb://b:2' }],
    });
    w.selectMode('generic');
    w.toggleTier('advanced');
    w.selectSettingsGroup('basic');
    assert.equal(doc.getElementById('workloadDeterministicAuto').style.display, 'none', 'a valid saved config is not changed');
    assert.equal(doc.getElementById('randomSeedInput').value, '11');
    assert.equal(doc.getElementById('operationsPerWorkerRow').hidden, false);
    assert.ok(connCard(doc).contains(doc.getElementById('workloadReplayTargetsList')));
    assert.equal(doc.getElementById('runOnSeveral').checked, true);
    const fd = formData(doc);
    assert.equal(fd.get('workload_deterministic'), 'true');
    assert.equal(fd.get('random_seed'), '11');
    assert.equal(fd.get('operations_per_worker'), '50');
    assert.equal(fd.get('workload_record_outcomes'), 'true');
    assert.deepEqual(JSON.parse(fd.get('workload_replay_targets')).map(t => t.label), ['a1', 'b2']);
    assert.equal(w.validateWorkloadRecordReplay(), '');

    // A record run cannot use servers: the choice is not offered there.
    w.selectMode('record');
    assert.equal(doc.getElementById('detServersSlot').hidden, true);
    assert.equal(doc.getElementById('runOnRow').hidden, true);
    assert.equal(formData(doc).get('workload_replay_targets'), '');
    assert.deepEqual(errors, []);
});

test('One server and Several servers explain themselves in tooltips, not inline text', async () => {
    const { doc, errors } = await genericOnSeedPage();
    for (const [id, note, text] of [['runOnOne', 'runOnOneNote', /one server in the MongoDB URI field/], ['runOnSeveral', 'runOnSeveralNote', /with the URI you enter for it/]]) {
        const btn = doc.querySelector(`label[for="${id}"]`).nextElementSibling;
        assert.ok(btn && btn.matches('.help-tip-btn'), `${id} has its own help button`);
        assert.match(btn.getAttribute('aria-label'), /^Help: (One|Several) server/);
        assert.equal(doc.getElementById(note).classList.contains('help-tip'), true, 'the text is not shown by default');
        assert.match(doc.getElementById(note).textContent, text);
    }
    assert.equal(doc.getElementById('runOnHelp'), null);
    assert.deepEqual(errors, []);
});

test('every settings card heading keeps its description behind the help button', async () => {
    const { doc, errors } = await page();
    for (const card of doc.querySelectorAll('#settingsPanels .settings-card:not(.rr-intro)')) {
        for (const help of card.querySelectorAll(':scope > .settings-help, :scope > header .settings-help')) {
            assert.equal(help.classList.contains('help-tip'), true, `${card.querySelector('h4')?.textContent.trim()}: description is a tooltip`);
        }
    }
    assert.deepEqual(errors, []);
});

test('Seed documents next to Seeding & Transactions turns seeding on and off', async () => {
    const { w, doc, errors } = await page({ SkipSeed: false, DocumentsCount: 5000 });
    w.selectMode('generic');
    const toggle = doc.getElementById('seedData');
    const count = doc.getElementById('seedDocumentCount');
    assert.ok(toggle.closest('.seed-subhead').querySelector('h4').textContent.includes('Seeding & Transactions'), 'the switch sits by the heading');
    assert.equal(toggle.closest('[data-tier]:not([data-tier="basic"])'), null, 'it is a Basic setting');
    assert.equal(doc.querySelectorAll('label[for="skipSeed"]').length, 0, 'no second control for the same setting');
    assert.equal(toggle.checked, true);
    assert.equal(count.disabled, false);

    toggle.checked = false;
    fire(doc, toggle);
    assert.equal(count.disabled, true, 'no count to enter when nothing is seeded');
    assert.equal(count.value, '5000', 'the count is kept');
    assert.equal(formData(doc).get('skip_seed'), 'true');
    assert.match(doc.getElementById('effectiveSummary').textContent, /no seeding/);

    toggle.checked = true;
    fire(doc, toggle);
    assert.equal(count.disabled, false);
    assert.equal(formData(doc).get('skip_seed'), null);
    assert.equal(formData(doc).get('documents_count'), '5000');
    assert.match(doc.getElementById('effectiveSummary').textContent, /5000 seed documents/);

    // Deterministic traffic needs seeding: it turns it back on and keeps it on.
    toggle.checked = false;
    fire(doc, toggle);
    setDet(doc, true);
    assert.equal(toggle.checked, true);
    assert.equal(count.disabled, false);
    assert.match(doc.getElementById('workloadDeterministicAuto').textContent, /Seed documents is on/);
    toggle.checked = false;
    fire(doc, toggle);
    assert.equal(toggle.checked, true, 'stays on while deterministic traffic is on');
    setDet(doc, false);
    assert.equal(toggle.checked, false, 'turning it off restores no seeding');
    assert.equal(count.disabled, true);
    assert.deepEqual(errors, []);
});

test('seeding is off by default, as skip_seed defaults to true', async () => {
    const { doc, errors } = await page();
    const toggle = doc.getElementById('seedData');
    assert.equal(toggle.checked, false, 'unchecked: no seeding');
    assert.equal(doc.getElementById('seedDocumentCount').disabled, true);
    assert.equal(new doc.defaultView.FormData(doc.getElementById('configForm')).get('skip_seed'), 'true');
    assert.match(doc.getElementById('seedDataNote').textContent, /Off \(default\): no documents are inserted before the run\. Inserts in the workload still run either way\./);
    assert.deepEqual(errors, []);
});

test('a saved config with skip_seed opens with seeding off', async () => {
    const { doc, errors } = await page({ SkipSeed: true });
    assert.equal(doc.getElementById('seedData').checked, false);
    assert.equal(doc.getElementById('seedDocumentCount').disabled, true);
    assert.deepEqual(errors, []);
});
