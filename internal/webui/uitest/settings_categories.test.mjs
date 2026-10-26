// Settings categories: Basic settings plus functional groups for the settings
// that Advanced and Expert add, and validation that reaches any category.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 30));

async function page(config = { URI: 'mongodb://fixture' }) {
    const p = await loadPage(fixture('trends'), { '/api/config': async () => ({ body: config }) });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    p.doc.querySelector('[name="find_percent"]').value = '100';
    return p;
}

// The harness replaces HTMLAnchorElement.click for downloads, so links get a real event.
const follow = (w, a) => a.dispatchEvent(new w.MouseEvent('click', { bubbles: true, cancelable: true }));
const navGroups = doc => [...doc.querySelectorAll('#settingsNavList [role="tab"]')].map(t => t.dataset.group);
const selectedGroup = doc => doc.querySelector('#settingsNavList [aria-selected="true"]')?.dataset.group;
const shownCards = (w, doc) => [...doc.querySelectorAll('#settingsPanels .settings-card[data-group]')]
    .filter(c => w.settingsCardShown(c) && c.dataset.groupHidden !== '1')
    .map(c => c.querySelector('h4').childNodes[0].textContent.replace(/\s+/g, ' ').trim());

test('Basic shows only basic settings; Advanced and Expert add a category list', async () => {
    const { w, doc, errors } = await page();
    w.selectMode('generic');
    assert.equal(doc.getElementById('settingsNav').hidden, true, 'no category list at Basic');
    assert.deepEqual(shownCards(w, doc), ['Target Connection', 'Workload Control', 'Data & Query Definitions', 'Operation Mix Allocation']);

    w.toggleTier('advanced');
    const nav = doc.getElementById('settingsNav');
    assert.equal(nav.hidden, false);
    assert.equal(doc.getElementById('settingsBody').classList.contains('has-nav'), true);
    assert.deepEqual(navGroups(doc), ['basic', 'execution', 'reproducibility', 'insights', 'explain', 'history']);
    assert.equal(selectedGroup(doc), 'execution', 'opening a level shows the first category it adds');
    assert.deepEqual(shownCards(w, doc), ['Client Pacing & Access Pattern']);

    w.toggleTier('expert');
    assert.deepEqual(navGroups(doc), ['basic', 'connection', 'execution', 'reproducibility', 'insights', 'explain', 'history']);
    assert.equal(selectedGroup(doc), 'execution', 'the category stays when another level opens');
    assert.deepEqual(shownCards(w, doc), ['Execution / Runtime Tuning', 'Client Pacing & Access Pattern', 'Sharding Strategy', 'Debug / Experimental']);

    doc.getElementById('snav-basic').click();
    assert.deepEqual(shownCards(w, doc), ['Target Connection', 'Workload Control', 'Data & Query Definitions', 'Operation Mix Allocation']);
    // Advanced fields inside Basic cards stay with them.
    assert.equal(doc.querySelector('[name="auth_source"]').closest('.settings-card').dataset.group, 'basic');

    const panels = doc.getElementById('settingsPanels');
    assert.equal(panels.getAttribute('role'), 'tabpanel');
    assert.equal(panels.getAttribute('aria-labelledby'), 'snav-basic');
    assert.equal(doc.getElementById('settingsNavList').getAttribute('aria-orientation'), 'vertical');
    assert.deepEqual(errors, []);
});

test('every card belongs to one category and each control appears once', async () => {
    const { doc } = await page();
    const cards = [...doc.querySelectorAll('#settingsPanels .settings-card')].filter(c => !c.classList.contains('rr-intro'));
    for (const c of cards) assert.ok(c.dataset.group, `${c.querySelector('h4')?.textContent} has a category`);
    // A radio group (several inputs, one name) is one control.
    const names = [...doc.querySelectorAll('#configForm [name]')]
        .filter((el, i, all) => el.type !== 'radio' || all.findIndex(o => o.type === 'radio' && o.name === el.name) === i)
        .map(el => el.name);
    assert.deepEqual(names.filter((n, i) => names.indexOf(n) !== i), [], 'no setting has two editable controls');
});

test('each operation mode lists only the categories it uses', async () => {
    const { w, doc } = await page();
    const groupsFor = mode => {
        w.selectMode(mode);
        w.toggleTier('advanced');
        w.toggleTier('expert');
        return navGroups(doc);
    };
    assert.deepEqual(groupsFor('custom'), ['basic', 'connection', 'execution', 'reproducibility', 'data', 'insights', 'explain', 'history']);
    assert.deepEqual(groupsFor('record'), ['basic', 'connection', 'execution', 'reproducibility', 'data', 'insights', 'explain', 'history']);
    assert.deepEqual(groupsFor('replay'), ['basic', 'connection', 'execution', 'reproducibility', 'insights', 'explain', 'history']);
    assert.deepEqual(groupsFor('raw'), ['basic', 'connection', 'execution', 'insights', 'history']);
    w.selectSettingsGroup('execution');
    assert.deepEqual(shownCards(w, doc), ['Debug / Experimental'], 'raw mode keeps only the retry controls here');
    // Changing mode returns to Basic settings with no extra levels open.
    w.selectMode('generic');
    assert.equal(doc.getElementById('settingsNav').hidden, true);
});

test('a category that disappears falls back to Basic settings', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    w.toggleTier('expert');
    w.selectSettingsGroup('connection');
    assert.equal(selectedGroup(doc), 'connection');
    w.toggleTier('advanced');
    assert.equal(selectedGroup(doc), 'connection');
    w.toggleTier('expert');
    assert.equal(selectedGroup(doc), 'basic', 'Connection has only expert settings');
    w.toggleTier('advanced');
    assert.equal(doc.getElementById('settingsNav').hidden, true);
});

test('values persist across categories, levels, and modes, and are all sent', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    w.toggleTier('advanced');
    w.toggleTier('expert');
    w.selectSettingsGroup('connection');
    doc.querySelector('[name="max_pool_size"]').value = '300';
    w.selectSettingsGroup('execution');
    doc.querySelector('[name="think_time_ms"]').value = '15';
    w.selectSettingsGroup('history');
    doc.querySelector('[name="trend_history_runs"]').value = '9';
    w.selectSettingsGroup('basic');
    w.toggleTier('expert');
    w.toggleTier('advanced');
    w.selectMode('custom');
    w.selectMode('generic');
    assert.equal(doc.querySelector('[name="max_pool_size"]').value, '300');
    assert.equal(doc.querySelector('[name="think_time_ms"]').value, '15');
    assert.equal(doc.querySelector('[name="trend_history_runs"]').value, '9');
    const fd = w.executionFormData(doc.getElementById('configForm'));
    assert.equal(fd.get('max_pool_size'), '300', 'settings in closed levels still run');
    assert.equal(fd.get('think_time_ms'), '15');
    assert.equal(fd.get('trend_history_runs'), '9');
});

test('arrow keys, Home, and End move through the categories', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    w.toggleTier('advanced');
    const list = doc.getElementById('settingsNavList');
    const key = k => doc.activeElement.dispatchEvent(new w.KeyboardEvent('keydown', { key: k, bubbles: true }));
    doc.getElementById('snav-execution').focus();
    key('ArrowDown');
    assert.equal(selectedGroup(doc), 'reproducibility');
    assert.equal(doc.activeElement.id, 'snav-reproducibility');
    key('ArrowUp');
    key('ArrowUp');
    assert.equal(doc.activeElement.id, 'snav-basic');
    key('End');
    assert.equal(doc.activeElement.id, 'snav-history');
    key('Home');
    assert.equal(doc.activeElement.id, 'snav-basic');
    const stops = [...list.querySelectorAll('[role="tab"]')].filter(t => t.tabIndex === 0);
    assert.equal(stops.length, 1, 'one tab stop in the list');
});

test('an error in a closed level opens it, focuses the field, and marks the category', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    const maxPool = doc.querySelector('[name="max_pool_size"]');
    maxPool.value = '5';
    doc.querySelector('[name="min_pool_size"]').value = '10';
    doc.querySelector('[name="insights_sampling_rate"]').value = '0';
    assert.equal(w.validateConfigForm(), false);

    assert.equal(doc.body.classList.contains('tier-expert-open'), true);
    assert.equal(selectedGroup(doc), 'connection');
    assert.equal(doc.activeElement, maxPool);
    assert.equal(maxPool.getAttribute('aria-invalid'), 'true');
    const msg = doc.getElementById(maxPool.getAttribute('aria-describedby'));
    assert.match(msg.textContent, /Max Pool Size must be greater/);
    assert.equal(msg.closest('.form-group'), maxPool.closest('.form-group'), 'the message sits with the field');
    assert.match(doc.getElementById('configInlineError').textContent, /Max Pool Size.*Other problems are marked/);

    // Insights Capture is Advanced, so that level opens too and its category is marked.
    assert.equal(doc.body.classList.contains('tier-advanced-open'), true);
    assert.ok(doc.querySelector('#snav-connection .settings-nav-error'));
    assert.ok(doc.querySelector('#snav-insights .settings-nav-error'));
    assert.match(doc.getElementById('snav-insights').textContent, /has errors/);
    assert.equal(doc.querySelector('#snav-history .settings-nav-error'), null);
    assert.equal(doc.querySelectorAll('#settingsNav .settings-nav-error').length, 2);
    assert.doesNotMatch(doc.getElementById('settingsNav').textContent, /\d/, 'markers carry no counts');

    // Fixing the field removes its message and its category marker.
    maxPool.value = '50';
    maxPool.dispatchEvent(new w.Event('input', { bubbles: true }));
    assert.equal(maxPool.hasAttribute('aria-invalid'), false);
    assert.equal(doc.querySelector('#snav-connection .settings-nav-error'), null);
    assert.ok(doc.querySelector('#snav-insights .settings-nav-error'));

    // The next attempt opens the remaining problem.
    assert.equal(w.validateConfigForm(), false);
    assert.equal(selectedGroup(doc), 'insights');
    assert.equal(doc.activeElement, doc.querySelector('[name="insights_sampling_rate"]'));
});

test('browser constraints on hidden fields are reported instead of blocking silently', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    assert.equal(doc.getElementById('configForm').noValidate, true);
    const iterations = doc.querySelector('[name="iterations"]');
    iterations.value = '0';
    assert.equal(w.validateConfigForm(), false);
    assert.equal(doc.body.classList.contains('tier-advanced-open'), true, 'Iterations is an Advanced field');
    assert.equal(selectedGroup(doc), 'basic');
    assert.equal(doc.activeElement, iterations);
    assert.match(doc.getElementById('configInlineError').textContent, /^Iterations/);
    iterations.value = '1';
    // A setting the mode does not use is not checked.
    doc.querySelector('[name="access_pattern_hotspot_percent"]').value = '0';
    w.selectMode('replay');
    doc.getElementById('workloadReplayPath').value = 'in.ndjson';
    doc.getElementById('selectedCollectionDefinitionId').value = 'c';
    doc.getElementById('selectedQueryDefinitionId').value = 'q';
    assert.equal(w.validateConfigForm(), true);
});

test('dependency links open the prerequisite in its category', async () => {
    const { w, doc } = await page({ URI: 'mongodb://fixture', InsightsEnabled: false });
    w.selectMode('generic');
    w.toggleTier('advanced');
    w.selectSettingsGroup('explain');
    const needs = doc.getElementById('insightsExplainNeedsInsights');
    assert.equal(needs.hidden, false, 'explain says it needs insights');
    follow(w, needs.querySelector('a'));
    assert.equal(selectedGroup(doc), 'insights');
    assert.equal(doc.activeElement.id, 'insightsEnabled');
    doc.getElementById('insightsEnabled').checked = true;
    doc.getElementById('insightsEnabled').dispatchEvent(new w.Event('change', { bubbles: true }));
    assert.equal(needs.hidden, true);

    const link = [...doc.querySelectorAll('#settingsPanels a.goto-link')].find(a => a.textContent === 'Query analysis');
    follow(w, link);
    assert.equal(selectedGroup(doc), 'explain');
    assert.equal(doc.activeElement.id, 'insightsExplainEnabled');

    w.selectSettingsGroup('reproducibility');
    doc.getElementById('workloadDeterministic').checked = true;
    w.applyDeterministicRequirements();
    const auto = doc.getElementById('workloadDeterministicAuto');
    assert.equal(auto.classList.contains('help-tip'), false, 'the automatic changes stay visible');
    follow(w, auto.querySelector('a'));
    assert.equal(selectedGroup(doc), 'basic');
    assert.equal(doc.activeElement.id, 'dropColl');
});

test('a saved configuration fills controls in every category', async () => {
    const { w, doc } = await page({
        URI: 'mongodb://fixture',
        ConnectionParams: { MaxPoolSize: 321 },
        TrendHistoryRuns: 7,
    });
    if (!doc.body.classList.contains('mode-active')) w.selectMode('generic');
    assert.equal(doc.querySelector('[name="max_pool_size"]').value, '321');
    assert.equal(doc.querySelector('[name="trend_history_runs"]').value, '7');
    w.toggleTier('advanced');
    w.toggleTier('expert');
    w.selectSettingsGroup('connection');
    assert.ok(shownCards(w, doc).includes('Runtime Transport & Pooling'));
    assert.equal(doc.querySelector('[name="max_pool_size"]').value, '321');
});

test('a setting the mode does not use is shown, with a note, when its value blocks Launch', async () => {
    const { w, doc } = await page();
    w.selectMode('raw');
    const runs = doc.querySelector('[name="trend_history_runs"]');
    const card = runs.closest('.settings-card');
    assert.equal(card.dataset.modeHidden, '1', 'Trend History is not used by the stress test');
    runs.value = '0';
    assert.equal(w.validateConfigForm(), false);
    assert.equal(card.dataset.errorReveal, '1');
    assert.equal(w.settingsCardShown(card), true);
    assert.equal(selectedGroup(doc), 'history');
    assert.equal(doc.activeElement, runs);
    assert.match(card.querySelector('.mode-unused-note').textContent, /Not used by "Stress test".*checked at launch/);
    // Fixing it keeps the section in place while editing; the next Launch hides it again.
    runs.value = '5';
    runs.dispatchEvent(new w.Event('input', { bubbles: true }));
    assert.equal(w.settingsCardShown(card), true);
    assert.equal(w.validateConfigForm(), true);
    assert.equal(w.settingsCardShown(card), false);
    assert.equal(card.querySelector('.mode-unused-note'), null);
    // A mode change also clears it.
    runs.value = '0';
    w.validateConfigForm();
    w.selectMode('generic');
    assert.equal(card.dataset.errorReveal, undefined);
});

test('replay runs show Sharding Strategy, which their collection setup uses', async () => {
    const { w, doc } = await page();
    w.selectMode('replay');
    const card = doc.querySelector('[name="sharding_mode"]').closest('.settings-card');
    assert.equal(card.dataset.modeHidden, '0');
    w.toggleTier('expert');
    w.selectSettingsGroup('execution');
    assert.ok(shownCards(w, doc).includes('Sharding Strategy'));
    w.selectMode('raw');
    assert.equal(card.dataset.modeHidden, '1');
});
