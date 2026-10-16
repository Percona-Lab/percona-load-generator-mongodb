// DOM tests for Query Analysis > Replay Comparison: kept reports, sides,
// overall facts and heuristics, filters, the shared op detail modal, the
// compare form, downloads, and the link from same-recording trend rows.
// replay_compare.json is real API output written by
// TestUIFixturesMatchAPIReplayCompare.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFixture, loadPage, choose } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

function routes(data, posts = [], postReply = null) {
    return {
        '/api/replay-compare/': async url => {
            const id = decodeURIComponent(url.slice('/api/replay-compare/'.length));
            return id === data.report.id ? { body: { report: data.report } } : { status: 404, body: { error: 'replay comparison not found' } };
        },
        '/api/replay-compare': async (url, init) => {
            if ((init.method || 'GET') === 'POST') {
                posts.push(Object.fromEntries(init.body.entries()));
                return postReply || { body: { report: data.report } };
            }
            return { body: data.list };
        },
    };
}

async function replayPage({ list, posts, postReply } = {}) {
    const data = readFixture('replay_compare.json');
    if (list) data.list = list;
    const p = await loadPage(data.insights, routes(data, posts, postReply));
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    p.doc.querySelector('.dash-tab-btn[data-dashtab="queries"]').click();
    return { ...p, data };
}

async function openReport(doc, id = 'fixture-compare-1') {
    doc.getElementById('qaTabReplayBtn').click();
    await settle();
    choose(doc, 'replayCompareSelect', id);
    await settle();
}

const shapeRows = doc => [...doc.querySelectorAll('#replayShapeRows tbody tr')];
const opRows = doc => [...doc.querySelectorAll('#replayOpRows tbody tr[data-op-key]')];

test('a kept comparison shows both sides, overall facts, heuristic notes, and per-shape plans', async () => {
    const { doc, data, errors } = await replayPage();
    doc.getElementById('qaTabReplayBtn').click();
    assert.equal(doc.getElementById('queryAnalysisTabReplay').style.display, 'block');
    assert.equal(doc.getElementById('qaTabReplayBtn').getAttribute('aria-pressed'), 'true');
    await settle();
    assert.ok(doc.getElementById('replayEmpty'));
    const opts = [...doc.getElementById('replayCompareSelect').options].map(o => o.value);
    assert.deepEqual(opts, ['', 'fixture-compare-1']);
    choose(doc, 'replayCompareSelect', 'fixture-compare-1');
    await settle();

    const sides = doc.getElementById('replaySides');
    const a = sides.querySelector('tr[data-side="a"]').textContent, b = sides.querySelector('tr[data-side="b"]').textContent;
    assert.ok(a.includes('MongoDB 8.0') && a.includes('8.0.4') && a.includes('replica_set') && a.includes('replay-8.0.ndjson') && a.includes('fixture-run-1'));
    assert.ok(b.includes('MongoDB 9.0') && b.includes('9.0.0') && b.includes('sharded') && b.includes('fixture-run-2'));
    assert.ok(doc.getElementById('replayLiteralBanner').textContent.includes('Contains literal query values'));
    assert.ok(doc.getElementById('replayIdentity').textContent.includes('is the same on both sides'));
    const overall = doc.getElementById('replayOverall').textContent;
    assert.ok(overall.includes(`differing ${data.report.overall.differing_ops}`), overall);
    assert.ok(overall.includes('only on B: 50 MaxTimeMSExpired: 1') && overall.includes('missing on B: 1'), overall);
    const notes = doc.getElementById('replayNotes').textContent;
    assert.ok(notes.includes('heuristics') && notes.includes('facts'), notes);

    assert.equal(shapeRows(doc).length, data.report.shapes.length);
    const sku = shapeRows(doc).find(tr => tr.textContent.includes('find on fields: sku'));
    assert.equal(sku.querySelector('.replay-shape-latency .insight-badge').textContent, 'slower on B');
    assert.ok(sku.querySelector('.replay-shape-plan').textContent.includes('changed: COLLSCAN → FETCH <- IXSCAN'));
    assert.ok(sku.querySelector('.replay-shape-plan').textContent.includes('annotation only'));
    assert.equal(opRows(doc).length, data.report.differing_ops.length);
    assert.deepEqual(errors, []);
});

test('the shape and operation filters narrow the tables', async () => {
    const { doc, errors } = await replayPage();
    await openReport(doc);
    choose(doc, 'replayShapeFilter', 'differing');
    assert.deepEqual(shapeRows(doc).map(tr => tr.dataset.shapeId.length > 0 && tr.textContent.includes('sku')), [true]);
    choose(doc, 'replayShapeFilter', 'no_material_change');
    assert.equal(shapeRows(doc).length, 1);
    assert.ok(shapeRows(doc)[0].textContent.includes('find on fields: status'));
    choose(doc, 'replayShapeFilter', 'faster');
    assert.equal(shapeRows(doc).length, 0);
    assert.ok(doc.getElementById('replayShapesEmpty'));

    const kinds = [...doc.getElementById('replayOpFilter').options].map(o => o.value);
    assert.deepEqual(kinds, ['all', 'result_count_mismatch', 'checksum_mismatch', 'error_only_b', 'missing_in_b']);
    choose(doc, 'replayOpFilter', 'error_only_b');
    assert.equal(opRows(doc).length, 1);
    assert.ok(opRows(doc)[0].textContent.includes('seq 7') && opRows(doc)[0].textContent.includes('error 50 MaxTimeMSExpired'));
    choose(doc, 'replayOpFilter', 'all');
    assert.equal(opRows(doc).length, 4);
    assert.deepEqual(errors, []);
});

test('an operation opens in the shared detail modal with the literal command and both outcomes', async () => {
    const { doc, w, errors } = await replayPage();
    await openReport(doc);
    const modal = doc.getElementById('shapeDetailModal');
    opRows(doc)[0].click();
    assert.equal(modal.style.display, 'flex');
    assert.equal(doc.activeElement, doc.getElementById('shapeDetailX'));
    assert.ok(doc.getElementById('shapeDetailTitle').textContent.includes('shop.orders'));
    const body = doc.getElementById('shapeDetailBody').textContent;
    assert.ok(body.includes('seq 3 (worker 2 #2)') && body.includes('Command (literal values) and outcomes'));
    assert.equal(doc.getElementById('replayDetailKinds').textContent, 'result count differs');
    const sides = [...doc.querySelectorAll('#replayDetailSides .trend-values-side')];
    assert.deepEqual(sides.map(s => s.dataset.side), ['a', 'b']);
    sides.forEach(s => {
        assert.ok(s.querySelector('.trend-value-sample pre').textContent.includes('"secret-sku-3"'));
        assert.ok(s.querySelector('.trend-value-match-mark').textContent.includes('same command on both sides'));
    });
    assert.ok(sides[0].textContent.includes('MongoDB 8.0') && sides[0].textContent.includes('returned 1'));
    assert.ok(sides[1].textContent.includes('9.0.0') && sides[1].textContent.includes('returned 2'));
    assert.ok(opRows(doc)[0].classList.contains('selected'));

    doc.getElementById('shapeDetailCloseBtn').click();
    assert.equal(modal.style.display, 'none');
    assert.equal(doc.activeElement, opRows(doc)[0]);

    opRows(doc)[3].querySelector('button').click();
    assert.ok(doc.getElementById('shapeDetailBody').textContent.includes('missing on B'));
    assert.ok(doc.querySelector('#replayDetailSides [data-side="b"]').textContent.includes('Outcome: not run'));
    w.closeShapeDetail();
    assert.equal(doc.activeElement, opRows(doc)[3].querySelector('button'));
    assert.deepEqual(errors, []);
});

test('the compare form posts paths, labels, plan runs, and reports a refused mismatch', async () => {
    const posts = [];
    const { doc, errors } = await replayPage({ posts });
    doc.getElementById('qaTabReplayBtn').click();
    await settle();
    doc.getElementById('replayCompareForm').dispatchEvent(new doc.defaultView.Event('submit', { cancelable: true }));
    assert.ok(doc.getElementById('replayCompareStatus').textContent.includes('side A'));
    assert.equal(posts.length, 0);

    const runs = [...doc.getElementById('replayARun').options].map(o => o.value);
    assert.deepEqual(runs, ['', 'fixture-run-2', 'fixture-run-1']);
    doc.getElementById('replayAPath').value = '/tmp/a.ndjson';
    doc.getElementById('replayBPath').value = '/tmp/b.ndjson';
    doc.getElementById('replayALabel').value = 'MongoDB 8.0';
    choose(doc, 'replayARun', 'fixture-run-1');
    doc.getElementById('replayAllowMismatch').checked = true;
    doc.getElementById('replayCompareForm').dispatchEvent(new doc.defaultView.Event('submit', { cancelable: true }));
    await settle();
    assert.deepEqual(posts[0], { a_path: '/tmp/a.ndjson', b_path: '/tmp/b.ndjson', label_a: 'MongoDB 8.0', top_n: '20', run_a: 'fixture-run-1', allow_mismatch: 'true' });
    assert.ok(doc.getElementById('replayCompareStatus').textContent.includes('4 differing operations'));
    assert.equal(doc.getElementById('replayCompareSelect').value, 'fixture-compare-1');
    assert.ok(doc.getElementById('replaySides'));

    const refused = await replayPage({ postReply: { status: 409, body: { error: 'the recording header hash differs' } } });
    refused.doc.getElementById('qaTabReplayBtn').click();
    refused.doc.getElementById('replayAPath').value = '/tmp/a.ndjson';
    refused.doc.getElementById('replayBPath').value = '/tmp/c.ndjson';
    refused.doc.getElementById('replayCompareForm').dispatchEvent(new refused.doc.defaultView.Event('submit', { cancelable: true }));
    await settle();
    assert.ok(refused.doc.getElementById('replayCompareStatus').textContent.includes('refused: the recording header hash differs'));
    assert.ok(refused.doc.getElementById('replayEmpty'));
    assert.deepEqual(errors, []);
});

test('the report downloads as HTML from the server and as JSON', async () => {
    const { doc, w, data, errors } = await replayPage();
    await openReport(doc);
    const html = doc.getElementById('replayDownloadHtml');
    assert.equal(html.getAttribute('href'), '/api/replay-compare/fixture-compare-1/report.html');
    doc.getElementById('replayDownloadJson').click();
    const { href, name } = w.downloads[w.downloads.length - 1];
    assert.equal(name, 'plgm-replay-compare-fixture-compare-1.json');
    const json = JSON.parse(decodeURIComponent(href.replace(/^data:text\/json;charset=utf-8,/, '')));
    assert.equal(json.identity, data.report.identity);
    assert.deepEqual(errors, []);
});

test('same-recording trend rows say same values without literals and link to the kept comparison', async () => {
    const { doc, w, data, errors } = await replayPage();
    doc.getElementById('qaTabTrendsBtn').click();
    await settle();
    const rows = [...doc.querySelectorAll('#trendRows tbody tr[data-shape-id]')];
    assert.equal(rows.length, data.insights.trends.rows.length);
    rows.forEach(tr => {
        const cell = tr.querySelector('.trend-values');
        assert.equal(cell.querySelector('.insight-badge').textContent, 'same values');
        assert.ok(cell.textContent.includes('both runs replayed the same recording'));
        assert.equal(cell.querySelector('.trend-replay-link').dataset.compareId, 'fixture-compare-1');
    });
    rows[0].click();
    assert.ok(doc.getElementById('trendDetailReplayNote').textContent.includes('no literal values are stored'));
    assert.equal(doc.querySelectorAll('#trendDetailValues .trend-values-side').length, 0);
    // The harness stubs anchor click() to record downloads; dispatch instead.
    doc.querySelector('#trendDetailValues .trend-replay-link').dispatchEvent(new w.MouseEvent('click', { bubbles: true, cancelable: true }));
    await settle();
    assert.equal(doc.getElementById('shapeDetailModal').style.display, 'none');
    assert.equal(doc.getElementById('queryAnalysisTabReplay').style.display, 'block');
    assert.equal(doc.getElementById('queryAnalysisTabTrends').style.display, 'none');
    assert.ok(doc.getElementById('replaySides').textContent.includes('MongoDB 9.0'));

    const none = await replayPage({ list: { reports: [], max_reports: 10 } });
    none.doc.getElementById('qaTabTrendsBtn').click();
    await settle();
    assert.equal(none.doc.querySelectorAll('.trend-replay-link').length, 0);
    assert.ok(none.doc.querySelector('#trendRows .trend-values').textContent.includes('same values'));
    assert.deepEqual(errors, []);
});
