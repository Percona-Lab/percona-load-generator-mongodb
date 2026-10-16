// DOM interaction tests for the Trends tab (baseline comparisons, filters,
// details, reset, and Download Summary) in internal/webui/static/index.html.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage, choose, exportedSummary } from './harness.mjs';

const rows = doc => [...doc.querySelectorAll('#trendRows tbody tr[data-shape-id]')];
const LABELED = ['improved', 'regressed', 'increased', 'decreased'];
const BADGE = {
    improved: 'improved', regressed: 'regressed', increased: 'larger share', decreased: 'smaller share',
    no_material_change: 'no material change', insufficient_evidence: 'insufficient evidence',
    not_comparable: 'not comparable', no_baseline: 'no baseline'
};

function openTrends(doc) {
    doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').click();
    doc.getElementById('qaTabTrendsBtn').click();
}

test('trends tab lists every API row in order with the API labels for each metric', async () => {
    const data = fixture('trends');
    const view = data.trends;
    const { doc, errors } = await loadPage(data);
    openTrends(doc);
    assert.equal(doc.getElementById('queryAnalysisTabTrends').style.display, 'block');
    assert.equal(doc.getElementById('queryAnalysisTabEfficiency').style.display, 'none');
    assert.equal(doc.getElementById('qaTabTrendsBtn').getAttribute('aria-pressed'), 'true');
    assert.ok(doc.getElementById('queryAnalysisPanel').contains(doc.getElementById('trendTable')));
    assert.ok(!doc.getElementById('insightsPanel').contains(doc.getElementById('trendTable')));

    const trs = rows(doc);
    assert.deepEqual(trs.map(r => r.dataset.shapeId), view.rows.map(r => r.shape_id));
    trs.forEach((tr, i) => {
        const r = view.rows[i];
        for (const [cls, metric] of [['.trend-eff', 'efficiency'], ['.trend-lat', 'latency'], ['.trend-imp', 'impact']]) {
            const cell = tr.querySelector(cls);
            assert.equal(cell.querySelector('.insight-badge').textContent, BADGE[r[metric].state]);
            assert.ok(cell.textContent.includes(r[metric].label), `${metric} label for ${r.shape_summary}`);
        }
        assert.ok(tr.querySelector('.trend-baseline').textContent.startsWith(r.comparability));
    });
    const sum = doc.getElementById('trendSummary').textContent;
    assert.ok(sum.includes(`${view.store.baselines} of max ${view.store.max_baselines} shape baselines`));
    assert.ok(sum.includes('Oldest / newest baseline'));
    assert.ok(doc.getElementById('trendCausationNote').textContent.includes('not evidence of cause'));
    assert.ok(doc.getElementById('trendCriteria').textContent.includes('not statistical significance tests'));
    assert.deepEqual(errors, []);
});

test('only rows meeting the evidence criteria carry improved or regressed badges', async () => {
    const data = fixture('trends');
    const { doc } = await loadPage(data);
    openTrends(doc);
    const byShape = Object.fromEntries(data.trends.rows.map(r => [r.shape_summary, r]));
    const row = summary => rows(doc).find(tr => tr.dataset.shapeId === byShape[summary].shape_id);

    assert.equal(row('find on fields: sku').querySelector('.trend-eff .insight-badge').textContent, 'improved');
    assert.equal(row('find on fields: status').querySelector('.trend-eff .insight-badge').textContent, 'regressed');
    for (const sparse of ['find on fields: region', 'find on fields: tag']) {
        assert.equal(row(sparse).querySelector('.trend-eff .insight-badge').textContent, 'insufficient evidence', sparse);
    }
    assert.equal(row('find on fields: email').querySelector('.trend-lat .insight-badge').textContent, 'insufficient evidence');

    for (const tr of rows(doc)) {
        const r = data.trends.rows.find(x => x.shape_id === tr.dataset.shapeId);
        for (const [cls, metric] of [['.trend-eff', 'efficiency'], ['.trend-lat', 'latency'], ['.trend-imp', 'impact']]) {
            const badge = tr.querySelector(`${cls} .insight-badge`).textContent;
            if (['improved', 'regressed', 'larger share', 'smaller share'].includes(badge)) {
                assert.ok(LABELED.includes(r[metric].state) && r.comparability === 'comparable', `${r.shape_summary} ${metric}`);
            }
        }
    }
});

test('details modal shows reasons, separate evidence counts, and intervention annotations', async () => {
    const data = fixture('trends');
    const { doc } = await loadPage(data);
    openTrends(doc);
    const modal = doc.getElementById('shapeDetailModal');
    const detail = doc.getElementById('shapeDetailBody');
    const sku = data.trends.rows.find(r => r.shape_summary === 'find on fields: sku');
    const rowFor = id => rows(doc).find(tr => tr.dataset.shapeId === id);
    const btn = () => rowFor(sku.shape_id).querySelector('button');
    btn().click();
    assert.equal(modal.style.display, 'flex');
    assert.equal(doc.querySelectorAll('#trendRows tbody tr').length, rows(doc).length, 'no inline detail rows');
    assert.equal(btn().getAttribute('aria-expanded'), 'true');
    assert.ok(rowFor(sku.shape_id).classList.contains('selected'));
    assert.equal(doc.getElementById('shapeDetailTitle').textContent, `${sku.namespace} • ${sku.operation} • ${sku.shape_summary}`);
    assert.equal(detail.querySelector('.trend-eff .insight-badge').textContent, BADGE[sku.efficiency.state]);
    assert.ok(doc.getElementById('trendDetailBaseline').textContent.startsWith(sku.comparability));
    const text = doc.getElementById('trendDetailReasons').textContent;
    assert.ok(text.includes('Intervention (plan_changed): winning plan COLLSCAN → FETCH <- IXSCAN'));
    assert.ok(text.includes('Intervention (index_inventory_changed)'));
    assert.ok(text.includes('Workload change (workload_config_changed): concurrency 4 → 8'));
    assert.ok(text.includes('Evidence, baseline: 200 operations (full run), 200 retained latency samples, 2 distinct explained commands'));
    assert.ok(text.includes('Latency: retained-sample p95'));

    doc.getElementById('shapeDetailX').click();
    assert.equal(modal.style.display, 'none');
    assert.equal(btn().getAttribute('aria-expanded'), 'false');
    assert.equal(doc.activeElement, btn(), 'focus returns to Details');

    const region = data.trends.rows.find(r => r.shape_summary === 'find on fields: region');
    rowFor(region.shape_id).querySelector('td').click();
    assert.equal(modal.style.display, 'flex', 'a row click opens the modal');
    const rtext = detail.textContent;
    assert.ok(rtext.includes('Scan efficiency (insufficient_evidence): baseline run has partial evidence'));
    assert.ok(rtext.includes('do not add explain evidence'));
    assert.ok(!rtext.includes('Intervention (plan_changed)'), 'only the clicked shape is shown');
    doc.getElementById('shapeDetailCloseBtn').click();
    assert.equal(modal.style.display, 'none');
    assert.equal(doc.activeElement, rowFor(region.shape_id), 'focus returns to the row');
});

test('trend hint is shown and the modal closes on Escape and overlay click; selected text does not open it', async () => {
    const data = fixture('trends');
    const { w, doc, errors } = await loadPage(data);
    openTrends(doc);
    assert.equal(doc.getElementById('trendRowHint').textContent.trim(), '👉 Click any row (or Details) to open the full details for that query shape.');
    const modal = doc.getElementById('shapeDetailModal');
    const tr = () => rows(doc)[0];

    tr().querySelector('button').click();
    doc.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    assert.equal(modal.style.display, 'none', 'Escape closes');
    tr().querySelector('button').click();
    modal.dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
    assert.equal(modal.style.display, 'none', 'overlay click closes');
    assert.equal(doc.activeElement, tr().querySelector('button'));

    const range = doc.createRange();
    range.selectNodeContents(tr().querySelector('td'));
    w.getSelection().removeAllRanges();
    w.getSelection().addRange(range);
    assert.ok(String(w.getSelection()).length > 0);
    tr().querySelector('td').click();
    assert.notEqual(modal.style.display, 'flex', 'selecting text must not open details');
    assert.deepEqual(errors, []);
});

test('filters select labeled, insufficient, and not comparable rows', async () => {
    const data = fixture('trends');
    const { doc } = await loadPage(data);
    openTrends(doc);
    const states = r => [r.efficiency.state, r.latency.state, r.impact.state];
    const shown = () => rows(doc).map(tr => tr.dataset.shapeId);

    choose(doc, 'trendStateFilter', 'labeled');
    assert.deepEqual(shown(), data.trends.rows.filter(r => states(r).some(s => LABELED.includes(s))).map(r => r.shape_id));
    assert.ok(shown().length >= 2);
    choose(doc, 'trendStateFilter', 'insufficient');
    assert.deepEqual(shown(), data.trends.rows.filter(r => states(r).includes('insufficient_evidence')).map(r => r.shape_id));
    choose(doc, 'trendStateFilter', 'not_comparable');
    assert.deepEqual(shown(), data.trends.rows.filter(r => states(r).some(s => s === 'not_comparable' || s === 'no_baseline')).map(r => r.shape_id));
    choose(doc, 'trendStateFilter', 'all');
    assert.equal(shown().length, data.trends.rows.length);
});

test('a first run without baselines shows no improvement or regression anywhere', async () => {
    const data = fixture('ready');
    assert.equal(data.trends.status, 'no_baseline');
    const { doc, errors } = await loadPage(data);
    openTrends(doc);
    assert.equal(rows(doc).length, data.trends.rows.length);
    const badges = [...doc.querySelectorAll('#trendRows .insight-badge')].map(b => b.textContent);
    assert.ok(badges.length > 0 && badges.every(b => b === 'no baseline' || b === 'not comparable'), badges.join(','));
    assert.ok(doc.getElementById('trendSummary').textContent.includes('no_stored_baselines'));
    assert.deepEqual(errors, []);
});

test('reset needs a confirming second click, then calls DELETE and reports the result', async () => {
    let deletes = 0;
    const { w, doc } = await loadPage(fixture('trends'), {
        '/api/trend-baselines': async (url, init) => {
            if ((init.method || 'GET') === 'DELETE') deletes++;
            return { body: { store: { status: 'empty', baselines: 0 } } };
        }
    });
    openTrends(doc);
    const btn = doc.getElementById('trendResetBtn');
    btn.click();
    assert.equal(deletes, 0, 'first click only arms the reset');
    assert.match(doc.getElementById('trendResetStatus').textContent, /Click again/);
    await w.resetTrendBaselines();
    assert.equal(deletes, 1);
    assert.equal(w.fetchCalls.filter(c => c.url === '/api/trend-baselines')[0].method, 'DELETE');
    assert.match(doc.getElementById('trendResetStatus').textContent, /Baselines cleared \(store: empty, 0 stored\).*made before the reset/);
    assert.equal(btn.textContent, 'Reset baselines');
});

test('a failed reset says the baselines were not changed', async () => {
    const { w, doc } = await loadPage(fixture('trends'), {
        '/api/trend-baselines': async () => ({ status: 500, body: { error: 'remove trend baselines: permission denied' } })
    });
    openTrends(doc);
    doc.getElementById('trendResetBtn').click();
    await w.resetTrendBaselines();
    assert.match(doc.getElementById('trendResetStatus').textContent, /Reset failed: remove trend baselines: permission denied\. Stored baselines were not changed\./);
});

test('Download Summary exports the same trends and the visible trend state', async () => {
    const data = fixture('trends');
    const { w, doc } = await loadPage(data);
    w.eval('finalReportData = { config: {}, stats: {} };');
    openTrends(doc);
    choose(doc, 'trendStateFilter', 'labeled');
    await w.downloadSummaryReport();
    const exported = exportedSummary(w);
    assert.deepEqual(exported.insights.trends, data.trends);
    const st = exported.trend_view_state;
    assert.equal(st.filter, 'labeled');
    assert.equal(st.version, data.trends.version);
    assert.deepEqual(st.visible_shape_ids, rows(doc).map(tr => tr.dataset.shapeId));
    assert.equal(st.total_rows, data.trends.rows.length);
});

test('missing trends render an explicit empty state without labels', async () => {
    const data = fixture('ready');
    delete data.trends;
    const { doc, errors } = await loadPage(data);
    openTrends(doc);
    assert.match(doc.getElementById('trendEmpty').textContent, /No trend comparison was returned/);
    assert.equal(rows(doc).length, 0);

    const { doc: doc2 } = await loadPage(new Error('network down'));
    assert.match(doc2.getElementById('trendEmpty').textContent, /No improvement or regression is claimed/);
    assert.deepEqual(errors, []);
});
