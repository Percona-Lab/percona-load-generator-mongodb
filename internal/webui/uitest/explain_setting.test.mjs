// DOM tests for the post-run explain setting note and off warning.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page(explainEnabled) {
    const p = await loadPage(fixture('ready'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true, InsightsExplainEnabled: explainEnabled } }),
    });
    await settle();
    return p;
}

test('explain setting explains what depends on it and its impact', async () => {
    const { doc, errors } = await page(true);
    const help = doc.getElementById('insightsExplainHelp').textContent;
    for (const want of ['Recommended: keep on', 'scan efficiency', 'execution overhead', 'index recommendations', 'only latency and workload impact']) {
        assert.ok(help.includes(want), `help missing ${want}: ${help}`);
    }
    const impact = help + doc.getElementById('insightsExplainHelp').nextElementSibling.textContent;
    assert.ok(impact.includes('never during the measured window'), impact);
    assert.equal(doc.getElementById('insightsExplainOffWarning').style.display, 'none');
    assert.deepEqual(errors, []);
});

test('results from a run without explain say so on both analysis tabs', async () => {
    const off = await page(true);
    // insights_disabled.json is a run with insights_explain_enabled false.
    const disabled = fixture('disabled');
    await off.w.renderFinalInsights(disabled);
    const notice = disabled.efficiency_view.notice;
    assert.ok(notice.includes('Post-run explain was turned off') && notice.includes('Query Analysis (Explain)'), notice);
    for (const id of ['insightsExplainNotice', 'queryAnalysisExplainNotice']) {
        const el = off.doc.getElementById(id);
        assert.equal(el.hidden, false, id);
        assert.equal(el.textContent, notice, id);
    }
    assert.ok(off.doc.getElementById('queryAnalysisExplainRequirement').textContent.includes('Requires Post-Run Explain Sampling'));
    const ready = fixture('ready');
    assert.equal(ready.efficiency_view.notice, undefined);
    await off.w.renderFinalInsights(ready);
    for (const id of ['insightsExplainNotice', 'queryAnalysisExplainNotice']) {
        assert.equal(off.doc.getElementById(id).hidden, true, id);
    }
    assert.deepEqual(off.errors, []);
});

test('explain is on when the config does not set it', async () => {
    const { doc, errors } = await page(undefined);
    assert.equal(doc.getElementById('insightsExplainEnabled').checked, true);
    assert.equal(doc.getElementById('insightsExplainOffWarning').style.display, 'none');
    assert.deepEqual(errors, []);
});

test('explain settings have their own tab; other settings moved to related tabs', async () => {
    const { doc, errors } = await page(true);
    const tabOf = sel => doc.querySelector(sel).closest('.tab-content').id;
    for (const name of ['insights_explain_enabled', 'insights_explain_top_n', 'insights_explain_verbosity',
        'insights_explain_severity_mode', 'insights_explain_efficiency_mode', 'insights_explain_max_time_ms',
        'insights_explain_workers', 'insights_explain_retries', 'insights_explain_backoff_ms',
        'insights_latency_target_avg_ms', 'insights_latency_target_p95_ms', 'insights_latency_target_p99_ms']) {
        assert.equal(tabOf(`[name="${name}"]`), 'tab-query-analysis', name);
    }
    for (const name of ['insights_enabled', 'insights_sampling_rate', 'insights_max_events', 'csv_export_path']) {
        assert.equal(tabOf(`[name="${name}"]`), 'tab-adv', name);
    }
    assert.equal(tabOf('[name="sharding_mode"]'), 'tab-workload');
    assert.equal(tabOf('[name="find_batch_size"]'), 'tab-conn');
    assert.equal(tabOf('[name="retry_attempts"]'), 'tab-raw');
    const help = doc.getElementById('insightsExplainHelp').textContent;
    assert.ok(help.includes('What it does') && help.includes('listIndexes'), help);
    assert.deepEqual(errors, []);
});

test('invalid explain settings open the Query Analysis tab', async () => {
    const { w, doc, errors } = await page(true);
    w.HTMLElement.prototype.scrollIntoView = () => {};
    doc.querySelector('[name="insights_explain_top_n"]').value = '0';
    assert.equal(w.validateConfigForm(), false);
    assert.ok(doc.getElementById('tab-query-analysis').classList.contains('active'));
    const active = doc.querySelector('.tab-btn.active');
    assert.equal(active.textContent, 'Query Analysis (Explain)');
    assert.deepEqual(errors, []);
});

test('turning explain off shows what will be missing; turning it on hides it', async () => {
    const { w, doc, errors } = await page(false);
    const box = doc.getElementById('insightsExplainEnabled');
    const warn = doc.getElementById('insightsExplainOffWarning');
    assert.equal(box.checked, false);
    assert.equal(warn.style.display, 'block');
    assert.ok(warn.textContent.includes('Scan efficiency') && warn.textContent.includes('will not be available'), warn.textContent);
    box.checked = true;
    box.dispatchEvent(new w.Event('change', { bubbles: true }));
    assert.equal(warn.style.display, 'none');
    box.checked = false;
    box.dispatchEvent(new w.Event('change', { bubbles: true }));
    assert.equal(warn.style.display, 'block');
    assert.deepEqual(errors, []);
});
