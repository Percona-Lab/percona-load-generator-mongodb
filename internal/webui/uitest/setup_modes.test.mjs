// Mode-first setup, help tooltips, and completed-run dashboards.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 30));

function card(doc, title) {
    return [...doc.querySelectorAll('.settings-card')].find(el => {
        const h = el.querySelector('h4');
        return h && h.textContent.replace(/\s+/g, ' ').trim().startsWith(title);
    });
}

async function page(config = { URI: 'mongodb://fixture' }, routes = {}) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: config }),
        ...routes,
    });
    await settle();
    return p;
}

test('each mode shows its settings and hides the others', async () => {
    const { w, doc, errors } = await page();
    assert.equal(doc.body.classList.contains('mode-active'), false);
    assert.equal(doc.getElementById('completedRunsBtn').textContent, 'Completed Runs');

    w.selectMode('generic');
    assert.equal(card(doc, 'Target Connection').dataset.modeHidden, '0');
    assert.equal(card(doc, 'Target Connection').dataset.tier, 'basic');
    assert.equal(card(doc, 'Workload Control').dataset.tier, 'basic');
    assert.equal(card(doc, 'Operation Mix Allocation').dataset.modeHidden, '0');
    assert.equal(card(doc, 'Record').dataset.modeHidden, '1');
    assert.equal(card(doc, 'Replay').dataset.modeHidden, '1');
    assert.equal(card(doc, 'Expert / Experimental Execution').dataset.modeHidden, '1');
    assert.equal(card(doc, 'TLS Client Settings').dataset.tier, 'expert');
    assert.equal(card(doc, 'Client Pacing & Access Pattern').dataset.tier, 'advanced');
    assert.equal(doc.body.classList.contains('tier-advanced-open'), false);
    assert.equal(doc.body.classList.contains('tier-expert-open'), false);
    assert.equal(doc.getElementById('definitionLibraryBlock').hidden, true);

    w.selectMode('custom');
    assert.equal(doc.getElementById('definitionLibraryBlock').hidden, false);
    assert.equal(card(doc, 'Record').dataset.modeHidden, '1');

    w.selectMode('record');
    assert.equal(card(doc, 'Record').dataset.modeHidden, '0');
    assert.equal(card(doc, 'Replay').dataset.modeHidden, '1');
    assert.equal(doc.getElementById('recordSourceRow').hidden, false);
    assert.equal(doc.getElementById('definitionLibraryBlock').hidden, true);
    doc.getElementById('recordWorkloadSource').value = 'custom';
    w.applyModeVisibility();
    assert.equal(doc.getElementById('definitionLibraryBlock').hidden, false);

    w.selectMode('replay');
    assert.equal(card(doc, 'Replay').dataset.modeHidden, '0');
    assert.equal(card(doc, 'Record').dataset.modeHidden, '1');
    assert.equal(card(doc, 'Operation Mix Allocation').dataset.modeHidden, '1');
    assert.equal(doc.getElementById('definitionLibraryBlock').hidden, false);

    w.selectMode('raw');
    assert.equal(card(doc, 'Expert / Experimental Execution').dataset.modeHidden, '0');
    assert.equal(card(doc, 'Expert / Experimental Execution').dataset.tier, 'basic');
    assert.equal(card(doc, 'Operation Mix Allocation').dataset.modeHidden, '1');
    assert.equal(card(doc, 'Replay').dataset.modeHidden, '1');
    assert.deepEqual(errors, []);
});

test('switching modes keeps compatible values and drops incompatible ones from execution', async () => {
    const { w, doc } = await page();
    const form = doc.getElementById('configForm');
    doc.querySelector('[name="uri"]').value = 'mongodb://db:27017';
    doc.querySelector('[name="concurrency"]').value = '8';
    doc.getElementById('workloadRecordPath').value = '/tmp/keep.ndjson';
    doc.getElementById('workloadReplayOps').value = 'reads_only';
    doc.getElementById('workloadDataFingerprint').value = 'counts';
    doc.getElementById('rawEnable').checked = true;
    doc.querySelector('[name="iterations"]').value = '4';

    w.selectMode('record');
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://db:27017');
    assert.equal(doc.getElementById('workloadRecordPath').value, '/tmp/keep.ndjson');
    assert.equal(doc.getElementById('workloadRecordEnabled').value, 'true');

    w.selectMode('generic');
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://db:27017');
    assert.equal(doc.querySelector('[name="concurrency"]').value, '8');
    assert.equal(doc.getElementById('workloadRecordPath').value, '/tmp/keep.ndjson');
    assert.equal(doc.querySelector('[name="iterations"]').value, '4');

    const fd = w.executionFormData(form);
    assert.equal(fd.get('operation_mode'), 'generic');
    assert.equal(fd.get('uri'), 'mongodb://db:27017');
    assert.equal(fd.get('concurrency'), '8');
    assert.equal(fd.get('duration'), '60s');
    assert.equal(fd.get('iterations'), '4');
    assert.equal(fd.get('workload_record_enabled'), 'false');
    assert.equal(fd.get('workload_replay_enabled'), 'false');
    assert.equal(fd.get('raw_injector_enabled'), 'false');
    assert.equal(fd.get('default_workload'), 'true');
    assert.equal(fd.get('workload_replay_ops'), 'all');
    assert.equal(fd.get('workload_data_fingerprint'), 'off');
    assert.equal(fd.get('collection_definition_id'), null);
    assert.equal(doc.getElementById('workloadRecordPath').value, '/tmp/keep.ndjson');
    assert.equal(doc.getElementById('workloadDataFingerprint').value, 'counts');

    w.selectMode('replay');
    doc.getElementById('selectedCollectionDefinitionId').value = 'col-1';
    doc.getElementById('selectedQueryDefinitionId').value = 'qry-1';
    const replay = w.executionFormData(form);
    assert.equal(replay.get('workload_replay_enabled'), 'true');
    assert.equal(replay.get('workload_record_enabled'), 'false');
    assert.equal(replay.get('default_workload'), 'false');
    assert.equal(replay.get('collection_definition_id'), 'col-1');
    assert.equal(replay.get('query_definition_id'), 'qry-1');
    assert.equal(replay.get('workload_deterministic'), 'false');

    w.selectMode('');
    assert.equal(doc.body.classList.contains('mode-active'), false);
    assert.equal(w.executionFormData(form).get('operation_mode'), '');
});

test('the summary describes the run and no changed-count indicator is shown', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    doc.querySelector('[name="uri"]').value = 'mongodb://db:27017';
    doc.querySelector('[name="iterations"]').value = '3';
    doc.querySelector('[name="max_pool_size"]').value = '50';
    w.updateEffectiveSummary();
    const summary = doc.getElementById('effectiveSummary').textContent;
    assert.match(summary, /Generic workload/);
    assert.match(summary, /mongodb:\/\/db:27017/);
    assert.match(summary, /16 workers/);
    assert.match(summary, /60s/);
    assert.doesNotMatch(summary, /changed/);
    assert.equal(doc.getElementById('advancedChanged'), null);
    assert.equal(doc.getElementById('expertChanged'), null);
    assert.equal(doc.querySelectorAll('.tier-badge').length, 0);
    assert.doesNotMatch(doc.getElementById('modeSettings').textContent, /\d+\s+changed/);
    // The changed values themselves are kept and sent.
    const fd = w.executionFormData(doc.getElementById('configForm'));
    assert.equal(fd.get('iterations'), '3');
    assert.equal(fd.get('max_pool_size'), '50');
});

test('settings levels are Basic, Advanced, and Expert only', async () => {
    const { w, doc } = await page();
    w.selectMode('generic');
    const levels = [...doc.querySelectorAll('#tierBar .tier-option')].map(el => el.textContent.trim());
    assert.deepEqual(levels, ['Basic', 'Advanced', 'Expert']);
    const tiers = new Set([...doc.querySelectorAll('[data-tier]')].map(el => el.dataset.tier));
    assert.deepEqual([...tiers].sort(), ['advanced', 'basic', 'expert']);
    const adv = doc.getElementById('tierAdvancedBtn');
    assert.equal(adv.getAttribute('aria-expanded'), 'false');
    adv.click();
    assert.equal(adv.getAttribute('aria-expanded'), 'true');
    assert.equal(doc.body.classList.contains('tier-advanced-open'), true);
    assert.match(doc.getElementById('tierStatus').textContent, /basic and advanced/);
    adv.click();
    assert.equal(doc.body.classList.contains('tier-advanced-open'), false);
});

test('help opens from the keyboard and from click, and stays out of the way of the control', async () => {
    const { w, doc } = await page();
    const note = doc.getElementById('workloadRecordEnabledNote');
    const control = doc.getElementById('workloadRecordEnabled');
    const label = doc.querySelector('label[for="workloadRecordEnabled"]');
    const btn = label.parentElement.querySelector('.help-tip-btn');
    assert.ok(btn, 'the help button sits with the label');
    assert.equal(label.parentElement.classList.contains('label-help'), true);
    assert.equal(btn.previousElementSibling, label, 'the button directly follows the label text');
    assert.equal(label.contains(btn), false, 'the button is not part of the label, so the control name stays clean');
    assert.equal(btn.getAttribute('aria-label'), 'Help: Record Workload');
    assert.equal(btn.getAttribute('aria-expanded'), 'false');
    assert.ok(control.closest('.setting-row').contains(note));
    assert.equal(note.classList.contains('help-tip'), true);

    btn.focus();
    assert.equal(btn.getAttribute('aria-expanded'), 'true');
    assert.equal(note.parentElement.classList.contains('help-tip-float'), true);
    assert.equal(note.parentElement.id, btn.getAttribute('aria-controls'));
    assert.ok(note.textContent.includes('On:'));
    btn.blur();
    assert.equal(btn.getAttribute('aria-expanded'), 'false');
    assert.ok(control.closest('.setting-row').contains(note));

    btn.click();
    assert.equal(btn.getAttribute('aria-expanded'), 'true');
    assert.equal(label.parentElement.classList.contains('help-pinned'), true);
    doc.body.dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
    assert.equal(btn.getAttribute('aria-expanded'), 'false');
    assert.equal(label.parentElement.classList.contains('help-pinned'), false);
    assert.ok(control.closest('.setting-row').contains(note));

    btn.click();
    doc.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    assert.equal(btn.getAttribute('aria-expanded'), 'false');
});

test('every help button sits directly after the label or heading it explains', async () => {
    const { doc, errors } = await page();
    const buttons = [...doc.querySelectorAll('.help-tip-btn')];
    assert.ok(buttons.length > 40, `found ${buttons.length} help buttons`);
    for (const btn of buttons) {
        const target = btn.previousElementSibling;
        assert.ok(target && target.matches('label, h3, h4, h5'), `${btn.getAttribute('aria-label')} follows a label or heading`);
        assert.equal(btn.parentElement.classList.contains('label-help'), true);
        const name = btn.getAttribute('aria-label');
        assert.match(name, /^Help: \S/);
        const text = target.textContent.replace(/\s+/g, ' ').trim();
        assert.ok(text.startsWith(name.slice('Help: '.length).slice(0, 20)), `${name} names "${text}"`);
    }
    // Runs to keep (a short label) and a checkbox label both get one.
    assert.ok(doc.querySelector('label[for="trendHistoryRuns"]').parentElement.querySelector('.help-tip-btn'));
    assert.ok(doc.querySelector('label[for="workloadDeterministic"]').parentElement.querySelector('.help-tip-btn'));
    // Alerts, status lines, and short notes stay visible inline.
    assert.equal(doc.querySelectorAll('.help-tip[role="alert"], .help-tip[role="status"]').length, 0);
    assert.equal(doc.getElementById('insightsExplainOffWarning').classList.contains('help-tip'), false);
    assert.equal(doc.getElementById('configInlineError').classList.contains('help-tip'), false);
    assert.deepEqual(errors, []);
});

test('inputs use content-based width categories', async () => {
    const { doc } = await page();
    const cls = sel => doc.querySelector(sel).className;
    assert.match(cls('#trendHistoryRuns'), /\bf-xs\b/);
    assert.match(cls('[name="find_percent"]'), /\bf-xs\b/);
    assert.match(cls('[name="max_pool_size"]'), /\bf-xs\b/);
    assert.match(cls('[name="duration"]'), /\bf-sm\b/);
    assert.match(cls('[name="insights_slow_threshold_ms"]'), /\bf-sm\b/);
    assert.match(cls('#workloadRecordMaxBytes'), /\bf-sm\b/);
    assert.match(cls('[name="username"]'), /\bf-md\b/);
    assert.match(cls('[name="raw_injector_db"]'), /\bf-md\b/);
    assert.match(cls('[name="uri"]'), /\bf-xl\b/);
    assert.match(cls('#workloadRecordPath'), /\bf-xl\b/);
    assert.match(cls('#workloadReplayPath'), /\bf-xl\b/);
    assert.match(cls('[name="csv_export_path"]'), /\bf-xl\b/);
    for (const el of doc.querySelectorAll('#configForm textarea')) assert.doesNotMatch(el.className, /\bf-(xs|sm|md)\b/);
    // Labels written without for= are tied to their control.
    assert.equal(doc.querySelector('[name="max_pool_size"]').labels.length, 1);
    assert.equal(doc.querySelector('[name="uri"]').labels[0].textContent.trim().startsWith('MongoDB URI'), true);
    // Width never follows the typed value.
    const runs = doc.getElementById('trendHistoryRuns');
    const before = runs.className + runs.getAttribute('style');
    runs.value = '50';
    runs.dispatchEvent(new doc.defaultView.Event('input', { bubbles: true }));
    assert.equal(runs.className + runs.getAttribute('style'), before);
});

test('a saved record configuration selects record mode and keeps its path', async () => {
    const { doc, errors } = await page({
        URI: 'mongodb://fixture',
        WorkloadRecordEnabled: true,
        WorkloadRecordPath: '/data/keep.ndjson',
        Concurrency: 4,
        Duration: '30s',
    });
    assert.equal(doc.getElementById('operationMode').value, 'record');
    assert.equal(doc.getElementById('workloadRecordPath').value, '/data/keep.ndjson');
    assert.equal(doc.querySelector('[name="concurrency"]').value, '4');
    assert.equal(doc.querySelector('[name="duration"]').value, '30s');
    assert.equal(doc.body.classList.contains('mode-active'), true);
    assert.deepEqual(errors, []);
});

test('completed runs open a stored dashboard and a damaged file does not hide the list', async () => {
    const snap = {
        id: 'run-1',
        label: 'Generic 2026-10-22',
        status: 'completed',
        mode: 'generic',
        started_at: '2026-10-22T12:00:00Z',
        duration_sec: 60,
        total_ops: 10,
        stats: {
            historical: true,
            opsPresent: true,
            isRunning: false,
            lifecyclePhase: 'completed',
            findOps: 10,
            insertOps: 0,
            updateOps: 0,
            upsertOps: 0,
            deleteOps: 0,
            aggOps: 0,
            chartHistory: [],
            duration: '60s',
            executionElapsedSec: 60,
            totalIterations: 1,
        },
        unavailable: ['Chart history kept the newest 3600 samples.'],
    };
    const { w, doc } = await page({ URI: 'mongodb://fixture' }, {
        '/api/stats': async () => ({ body: { isRunning: false } }),
        '/api/run-history': async (url) => {
            if (url.includes('/broken')) return { status: 422, body: { error: 'This run file is damaged and cannot be opened.' } };
            if (url.includes('/run-1')) return { body: snap };
            return {
                body: {
                    dir: '/tmp/plgm/run_dashboards',
                    keep_runs: 5,
                    max_age_days: 30,
                    max_pinned: 10,
                    chart_sample_cap: 3600,
                    runs: [
                        { id: 'run-1', label: 'Generic 2026-10-22', status: 'completed', mode: 'generic', started_at: '2026-10-22T12:00:00Z', duration_sec: 60, total_ops: 10, avg_latency_ms: 1.5 },
                        { id: 'broken', label: 'broken.json', status: 'damaged', error: 'not valid JSON' },
                    ],
                },
            };
        },
    });
    w.showRunHistory();
    await settle();
    const list = doc.getElementById('runHistoryBody').textContent;
    assert.match(list, /Generic 2026-10-22/);
    assert.match(list, /damaged/);
    assert.match(doc.getElementById('runHistoryMeta').textContent, /\/tmp\/plgm\/run_dashboards/);

    await w.openHistoricalRun('broken');
    assert.match(doc.getElementById('runHistoryError').textContent, /damaged/);
    assert.equal(doc.getElementById('historyBanner').hidden, true);
    assert.match(doc.getElementById('runHistoryBody').textContent, /Generic 2026-10-22/);

    await w.openHistoricalRun('run-1');
    assert.equal(doc.getElementById('historyBanner').hidden, false);
    assert.equal(w.fetchCalls.some(c => c.url === '/api/start'), false);
    assert.match(doc.getElementById('historyBanner').textContent, /does not start a workload/);
    assert.match(doc.getElementById('historyGapNotice').textContent, /3600/);
    assert.equal(doc.querySelector('.ms-tile[data-op="all"] .ms-v').textContent, '10');
    assert.equal(doc.getElementById('dashboard').style.display === 'none', false);
});

test('setup has one launch control and the mode cards are a single radiogroup', async () => {
    const { doc } = await page();
    assert.equal(doc.querySelectorAll('#startBtn').length, 1);
    assert.equal(doc.querySelectorAll('#changeModeBtn').length, 0);
    const group = doc.querySelector('.mode-grid');
    assert.equal(group.getAttribute('role'), 'radiogroup');
    const cards = [...group.querySelectorAll('.mode-card')];
    assert.equal(cards.length, 5);
    cards.forEach(btn => assert.equal(btn.getAttribute('role'), 'radio'));
    assert.equal(doc.getElementById('effectiveSummary').closest('#formFooter').id, 'formFooter');
});

test('mode cards are the only mode control and keep their selection from the keyboard', async () => {
    const { w, doc } = await page();
    assert.equal(doc.getElementById('changeModeBtn'), null);
    const completed = [...doc.querySelectorAll('button')].filter(b => b.textContent.trim() === 'Completed Runs');
    assert.equal(completed.length, 1);
    assert.equal(completed[0].id, 'completedRunsBtn');

    doc.querySelector('[name="uri"]').value = 'mongodb://keep';
    w.selectMode('generic');
    const generic = doc.querySelector('[data-mode="generic"]');
    const custom = doc.querySelector('[data-mode="custom"]');
    assert.equal(generic.getAttribute('aria-checked'), 'true');
    assert.equal(generic.classList.contains('selected'), true);
    assert.equal(generic.tabIndex, 0);
    assert.equal(custom.tabIndex, -1);
    generic.focus();
    generic.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    assert.equal(doc.getElementById('operationMode').value, 'custom');
    assert.equal(custom.getAttribute('aria-checked'), 'true');
    assert.equal(custom.tabIndex, 0);
    assert.equal(generic.getAttribute('aria-checked'), 'false');
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://keep');

    w.toggleTier('advanced');
    w.selectMode('custom');
    assert.equal(doc.body.classList.contains('tier-advanced-open'), true);
    w.selectMode('generic');
    assert.equal(doc.body.classList.contains('tier-advanced-open'), false);
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://keep');
});

test('an active run locks mode changes and keeps the values already entered', async () => {
    const { w, doc } = await page();
    doc.querySelector('[name="uri"]').value = 'mongodb://keep';
    w.selectMode('generic');
    w.setRunModeLock(true);
    assert.equal(doc.getElementById('modeLockNotice').hidden, false);
    assert.equal(doc.querySelector('[data-mode="replay"]').disabled, true);
    w.selectMode('replay');
    assert.equal(doc.getElementById('operationMode').value, 'generic');
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://keep');
    w.setRunModeLock(false);
    assert.equal(doc.getElementById('modeLockNotice').hidden, true);
    w.selectMode('replay');
    assert.equal(doc.getElementById('operationMode').value, 'replay');
    assert.equal(doc.querySelector('[name="uri"]').value, 'mongodb://keep');
});

test('the opening page explains each task and offers completed runs as a secondary route', async () => {
    const { w, doc } = await page({ URI: 'mongodb://fixture' }, {
        '/api/run-history': async () => ({ body: { runs: [] } }),
    });
    assert.equal(doc.body.classList.contains('mode-active'), false);
    assert.equal(doc.getElementById('modePickerTitle').textContent, 'What do you want to do?');
    assert.ok(doc.querySelector('.start-head .mode-lead').textContent.length > 40);
    const group = doc.querySelector('.mode-grid');
    assert.equal(group.getAttribute('aria-labelledby'), 'modePickerTitle');
    const cards = [...group.querySelectorAll('.mode-card')];
    assert.deepEqual(cards.map(c => c.dataset.mode), ['generic', 'custom', 'raw', 'record', 'replay']);
    for (const c of cards) {
        assert.equal(c.tagName, 'BUTTON', 'the whole card is the control');
        assert.ok(c.querySelector('.mode-card-icon svg'), `${c.dataset.mode} icon`);
        assert.equal(c.querySelector('.mode-card-icon').getAttribute('aria-hidden'), 'true');
        assert.ok(c.querySelector('.mode-card-title').textContent.trim().length > 3);
        const desc = c.querySelector('.mode-card-desc').textContent.trim();
        assert.match(desc, /^[A-Z][^.]+\.$/, `${c.dataset.mode} has one short sentence`);
        assert.ok(desc.length <= 90, `${c.dataset.mode} description is short`);
    }
    assert.equal(cards.filter(c => c.tabIndex === 0).length, 1, 'one tab stop in the radiogroup');

    const route = doc.getElementById('openCompletedRuns');
    assert.equal(group.contains(route), false, 'the completed-runs route is not a mode');
    assert.notEqual(route.getAttribute('role'), 'radio');
    route.click();
    await settle();
    assert.equal(doc.getElementById('runHistoryPanel').hidden, false);
    assert.ok(w.fetchCalls.some(c => c.url === '/api/run-history'));
    assert.match(doc.getElementById('runHistoryBody').textContent, /No completed runs yet/);
    assert.equal(w.fetchCalls.some(c => c.url === '/api/start'), false);
    w.hideRunHistory();

    cards[0].click();
    assert.equal(doc.getElementById('operationMode').value, 'generic');
    assert.equal(doc.body.classList.contains('mode-active'), true);
});
