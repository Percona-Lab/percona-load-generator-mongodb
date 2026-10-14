// DOM tests for the legacy sort-order review flow in the definition library.
// API responses come from TestSortReviewUIFixtureMatchesAPI.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { fixture, loadPage, choose } from './harness.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const api = JSON.parse(readFileSync(join(here, '..', 'testdata', 'ui', 'definitions_sort_review.json'), 'utf8'));
const settle = () => new Promise(r => setTimeout(r, 10));
const SORT_PATH = 'queries[0].pipeline[0].$sort';
const CORRECTED = '{"b":-1,"a":1}';
// Mirrors the backend: a confirmation counts only for the sort spec actually submitted.
const submittedSpec = body => body.content.includes(CORRECTED) ? CORRECTED : '{"a":1,"b":-1}';
const confirmed = body => (body.reviewedSorts || []).some(rs => rs.path === SORT_PATH && rs.sortSpec === submittedSpec(body));
const anyConfirmation = body => (body.reviewedSorts || []).length > 0;

async function page() {
    const state = { legacy: api.legacy, puts: [], posts: [], starts: [] };
    const routes = {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', AggregatePercent: 100, ConnectionParams: { Username: 'u', Password: 'p' } } }),
        '/api/collection-definitions': async url => ({ body: url.endsWith('/cols') ? api.collection_list.definitions[1] : api.collection_list }),
        '/api/query-definitions': async (url, init) => {
            const id = decodeURIComponent(url.split('/api/query-definitions/')[1] || '');
            if ((init.method || 'GET') === 'PUT') {
                const body = JSON.parse(init.body);
                state.puts.push(body);
                if (!confirmed(body)) return { status: 409, body: anyConfirmation(body) ? api.put_stale : api.put_unreviewed };
                state.legacy = api.put_reviewed;
                return { body: api.put_reviewed };
            }
            if (init.method === 'POST') {
                const body = JSON.parse(init.body);
                state.posts.push(body);
                if (!confirmed(body)) return { status: 409, body: anyConfirmation(body) ? api.post_copy_stale : api.put_unreviewed };
                return { status: 201, body: api.post_copy_reviewed };
            }
            if (!id) return { body: api.query_list };
            if (id === 'legacy') return { body: state.legacy };
            if (id === 'current') return { body: api.current };
            return { body: { ...api.query_list.definitions[0], content: '{"queries":[]}' } };
        },
        '/api/start': async (url, init) => {
            state.starts.push(init.body.get('query_definition_id'));
            return state.legacy.sortOrderReview ? { status: 409, body: api.start_blocked } : { status: 400, body: { status: 'error', message: 'stub: not started' } };
        },
    };
    const loaded = await loadPage(fixture('empty'), routes);
    await settle();
    return { ...loaded, state };
}

const panel = doc => doc.getElementById('querySortReview');
const submit = (w, doc) => doc.getElementById('configForm').dispatchEvent(new w.Event('submit', { cancelable: true }));

test('selecting a legacy definition shows the ambiguous sort path and the alphabetization warning', async () => {
    const { w, doc, errors } = await page();
    const option = [...doc.querySelectorAll('#queryDefinitionSelect option')].find(o => o.value === 'legacy');
    assert.match(option.textContent, /sort order review needed/);
    assert.equal(panel(doc).style.display, 'none');

    choose(doc, 'queryDefinitionSelect', 'legacy');
    await settle();
    const el = panel(doc);
    assert.equal(el.style.display, 'block');
    assert.equal(el.getAttribute('role'), 'alert');
    assert.match(el.textContent, /may have alphabetized the fields of multi-key sorts/);
    assert.match(el.textContent, /queries\[0\]\.pipeline\[0\]\.\$sort/);
    assert.match(el.textContent, /has fields in this order: a, b/);
    const boxes = [...el.querySelectorAll('.sort-review-confirm')];
    assert.deepEqual(boxes.map(b => [b.dataset.path, b.checked]), [['queries[0].pipeline[0].$sort', false]]);
    assert.match(doc.getElementById('queryDefinitionStatus').textContent, /must be reviewed/);

    choose(doc, 'queryDefinitionSelect', 'current');
    await settle();
    assert.equal(panel(doc).style.display, 'none', 'unaffected definitions show no warning');
    assert.deepEqual(errors, []);
    w.close();
});

test('an unresolved definition is blocked before start and by the backend', async () => {
    const { w, doc, state } = await page();
    choose(doc, 'queryDefinitionSelect', 'legacy');
    await settle();
    submit(w, doc);
    await settle();
    assert.deepEqual(state.starts, [], 'preflight must not call /api/start');
    const inline = doc.getElementById('configInlineError');
    assert.equal(inline.style.display, 'block');
    assert.match(inline.textContent, /cannot run until its sort order is reviewed/);

    // A stale client that skips preflight still gets the backend review.
    w.eval('definitionState.query.selected.sortOrderReview = null; renderSortOrderReview(definitionState.query.selected);');
    assert.equal(panel(doc).style.display, 'none');
    w.launchWorkload(new w.FormData(doc.getElementById('configForm')));
    await settle();
    assert.deepEqual(state.starts, ['legacy']);
    assert.equal(panel(doc).style.display, 'block', 'backend 409 restores the warning');
    assert.match(doc.getElementById('errorMessage').textContent, /sort precedence is unknown/);
    w.close();
});

test('saving without confirmation is rejected; a confirmed save resolves the warning and the run proceeds', async () => {
    const { w, doc, state } = await page();
    choose(doc, 'queryDefinitionSelect', 'legacy');
    await settle();

    doc.getElementById('saveReviewedSortBtn').click();
    await settle();
    assert.equal(state.puts.length, 1);
    assert.deepEqual(state.puts[0].reviewedSorts, [], 'nothing is confirmed automatically');
    assert.match(doc.getElementById('sortReviewStatus').textContent, /saving does not confirm the order by itself/);
    assert.equal(panel(doc).style.display, 'block');

    // Confirm the loaded order, then correct the precedence in the editor: the confirmation is stale.
    panel(doc).querySelector('.sort-review-confirm').checked = true;
    const editor = doc.getElementById('queryDefinitionEditor');
    editor.value = editor.value.replace('{"a":1,"b":-1}', CORRECTED);
    doc.getElementById('saveReviewedSortBtn').click();
    await settle();
    assert.equal(state.puts.length, 2);
    assert.deepEqual(state.puts[1].reviewedSorts, [{ path: SORT_PATH, sortSpec: '{"a":1,"b":-1}' }]);
    assert.match(doc.getElementById('sortReviewStatus').textContent, /confirmations are stale/);
    let box = panel(doc).querySelector('.sort-review-confirm');
    assert.equal(box.checked, false, 'a stale confirmation is not carried over');
    assert.equal(box.dataset.spec, CORRECTED);
    assert.match(panel(doc).textContent, /has fields in this order: b, a/);

    // Confirm the submitted order and save.
    box.checked = true;
    doc.getElementById('saveReviewedSortBtn').click();
    await settle();
    assert.equal(state.puts.length, 3);
    assert.deepEqual(state.puts[2].reviewedSorts, [{ path: SORT_PATH, sortSpec: CORRECTED }]);
    assert.ok(state.puts[2].content.includes(CORRECTED), 'the corrected order is what is saved');
    assert.equal(panel(doc).style.display, 'none');
    assert.match(doc.getElementById('queryDefinitionStatus').textContent, /reviewed sort order; it can run now/);
    assert.ok(doc.getElementById('queryDefinitionEditor').value.includes('"b": -1'), 'editor reloads the saved order');

    submit(w, doc);
    await settle();
    assert.deepEqual(state.starts, ['legacy'], 'the reviewed definition reaches /api/start: ' + doc.getElementById('configInlineError').textContent);
    w.close();
});

test('Save As New from an unresolved definition requires every sort to be confirmed', async () => {
    const { w, doc, state, dom } = await page();
    choose(doc, 'queryDefinitionSelect', 'legacy');
    await settle();
    const posts = () => w.fetchCalls.filter(c => c.method === 'POST' && c.url === '/api/query-definitions');
    doc.getElementById('queryEditorName').value = 'legacy copy';
    w.saveQueryDefinitionAsNew();
    await settle();
    assert.equal(posts().length, 0);
    assert.match(doc.getElementById('sortReviewStatus').textContent, /Confirm the field order of every sort/);
    assert.equal(state.puts.length, 0);

    // A confirmation of the loaded order does not cover an edited copy; the backend returns the new checklist.
    panel(doc).querySelector('.sort-review-confirm').checked = true;
    const editor = doc.getElementById('queryDefinitionEditor');
    editor.value = editor.value.replace('{"a":1,"b":-1}', CORRECTED).replace('by_a_b', 'copy_b_a');
    w.saveQueryDefinitionAsNew();
    await settle();
    assert.equal(posts().length, 1);
    const legacy = api.legacy;
    assert.deepEqual([state.posts[0].sourceDefinitionId, state.posts[0].sourceUpdatedAt, state.posts[0].sourceContentVersion], [legacy.id, legacy.updatedAt, 0],
        'the copy carries its source identity and version');
    assert.deepEqual(state.posts[0].reviewedSorts, [{ path: SORT_PATH, sortSpec: '{"a":1,"b":-1}' }]);
    const box = panel(doc).querySelector('.sort-review-confirm');
    assert.equal(box.checked, false);
    assert.equal(box.dataset.spec, CORRECTED);

    w.saveQueryDefinitionAsNew();
    await settle();
    assert.equal(posts().length, 1, 'an unconfirmed copy is not sent again');
    box.checked = true;
    w.saveQueryDefinitionAsNew();
    await settle();
    assert.equal(posts().length, 2);
    assert.deepEqual(state.posts[1].reviewedSorts, [{ path: SORT_PATH, sortSpec: CORRECTED }]);
    assert.match(doc.getElementById('queryEditorStatus').textContent, /Saved legacy copy/);
    dom.window.close();
});
