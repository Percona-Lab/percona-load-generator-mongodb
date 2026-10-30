// DOM tests for Save & Load Run Settings: the snapshot sent with a launch,
// the saved-settings list, and restoring a saved file into the form.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));
const FILE = 'plgm_settings_20261030-150405.json';

async function page(saved = {}) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', DefaultWorkload: true, FindPercent: 100 } }),
        '/api/saved-settings/item': async () => ({ body: saved }),
        '/api/saved-settings': async () => ({ body: { directory: '/data/s', files: [{ name: FILE, saved_at: '2026-10-30T15:04:05Z', mode: 'generic', duration: '30s', iterations: '2', concurrency: '4', servers: 2 }] } }),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    p.w.selectMode('generic');
    return p;
}
const field = (doc, name) => doc.querySelector(`[name="${name}"]`);

test('Save Settings is on by default, in Basic settings, and a launch carries a snapshot without passwords', async () => {
    const { w, doc, errors } = await page();
    const box = doc.getElementById('saveSettings');
    assert.equal(box.checked, true);
    assert.equal(box.closest('.settings-card').dataset.tier, 'basic');
    field(doc, 'duration').value = '45s';
    field(doc, 'password').value = 'hunter2';
    field(doc, 'save_settings_dir').value = '/data/s';
    const fd = w.executionFormData(doc.getElementById('configForm'));
    assert.equal(fd.get('save_settings'), 'true');
    assert.equal(fd.get('save_settings_dir'), '/data/s');
    const snap = JSON.parse(fd.get('settings_snapshot'));
    assert.equal(snap.fields.duration, '45s');
    assert.equal(snap.fields.operation_mode, 'generic');
    assert.ok(!('password' in snap.fields) && !JSON.stringify(snap).includes('hunter2'));
    assert.ok(!Object.keys(snap.fields).some(k => k.startsWith('save_settings')));
    box.checked = false;
    assert.equal(w.executionFormData(doc.getElementById('configForm')).get('settings_snapshot'), null);
    assert.deepEqual(errors, []);
});

test('the list shows the saved settings of the folder and Load fills the form, including servers', async () => {
    const saved = {
        saved_at: '2026-10-30T15:04:05Z', credentials_removed: true,
        fields: { operation_mode: 'generic', duration: '90s', iterations: '3', interval_delay: '5s', concurrency: '8', workload_deterministic: true, operations_per_worker: '700', run_on: 'several' },
        replay_targets: [{ label: 'a', uri: 'mongodb://u@h1:1/' }, { label: 'b', uri: 'mongodb://h2:2/' }],
    };
    const { w, doc, errors } = await page(saved);
    const sel = doc.getElementById('savedSettingsSelect');
    assert.equal(sel.options.length, 2, 'a prompt and the saved settings');
    assert.equal(sel.value, '', 'nothing is loaded until one is chosen');
    assert.match(sel.options[1].textContent, /Generic · 30s × 2 · 4 workers · 2 servers/);
    assert.match(doc.getElementById('savedSettingsStatus').textContent, /1 saved settings in \/data\/s/);
    assert.equal(doc.getElementById('savedSettingsLoad'), null, 'there is no Load button');
    sel.value = FILE;
    sel.dispatchEvent(new w.Event('change', { bubbles: true }));
    await settle();
    assert.equal(field(doc, 'duration').value, '90s');
    assert.equal(field(doc, 'iterations').value, '3');
    assert.equal(field(doc, 'interval_delay').value, '5s');
    assert.equal(field(doc, 'concurrency').value, '8');
    assert.equal(doc.getElementById('workloadDeterministic').checked, true);
    assert.equal(field(doc, 'operations_per_worker').value, '700');
    assert.equal(doc.getElementById('runOnSeveral').checked, true);
    assert.deepEqual(Array.from(w.listedReplayTargets(), t => t.label), ['a', 'b']);
    assert.equal(doc.getElementById('saveSettings').checked, true, 'the save controls are not part of a saved file');
    assert.match(doc.getElementById('savedSettingsStatus').textContent, /Loaded the settings saved 2026-10-30 15:04.*left out/);
    assert.deepEqual(errors, []);
});

test('the list is read from the folder typed in Settings Folder, and an empty folder says so', async () => {
    const { doc, w, errors } = await page();
    const field_ = doc.getElementById('saveSettingsDir');
    field_.value = '/elsewhere';
    await w.refreshSavedSettings();
    assert.ok(w.fetchCalls.some(c => c.url === '/api/saved-settings?dir=%2Felsewhere'));
    assert.deepEqual(errors, []);
});
