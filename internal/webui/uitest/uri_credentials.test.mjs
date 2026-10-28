// DOM tests for credentials in MongoDB URIs: a URI with a username and
// password needs no Username / Password fields (no prompt), each server of a
// several-servers run can use its own, and URI credentials that replace the
// fields are announced in a notice that does not stop the run.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page(cfg = {}) {
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', DefaultWorkload: true, FindPercent: 100, ...cfg } }),
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    p.w.selectMode('generic');
    p.launched = [];
    p.w.launchWorkload = fd => p.launched.push(fd);
    return p;
}

const submit = (w, doc) => doc.getElementById('configForm').dispatchEvent(new w.Event('submit', { cancelable: true }));
const setField = (doc, name, v) => { doc.querySelector(`[name="${name}"]`).value = v; };
const modalOpen = doc => doc.getElementById('passwordModal').style.display === 'flex';
const notice = doc => doc.getElementById('credentialsNotice');

function form(w, fields) {
    const fd = new w.FormData();
    Object.entries(fields).forEach(([k, v]) => fd.set(k, v));
    return fd;
}

test('uriCredentials reads the username and password of mongodb and mongodb+srv URIs', async () => {
    const { w, errors } = await page();
    const c = u => ({ ...w.uriCredentials(u) });
    assert.deepEqual(c('mongodb://u:p@h:1/'), { user: 'u', hasPass: true });
    assert.deepEqual(c('mongodb+srv://a%40b:p%2F@c.example/db?x=1'), { user: 'a@b', hasPass: true });
    assert.deepEqual(c('mongodb://u@h1:1,h2:2/'), { user: 'u', hasPass: false });
    assert.deepEqual(c('mongodb://h:1/?authSource=admin'), { user: '', hasPass: false });
    assert.deepEqual(c('mongodb://h:1/path@x'), { user: '', hasPass: false }, 'an @ after the host is not userinfo');
    assert.deepEqual(errors, []);
});

test('a URI with a username and password launches without the credentials prompt or a notice', async () => {
    const p = await page();
    const { w, doc, errors } = p;
    setField(doc, 'uri', 'mongodb://alice:secret@localhost:27017/');
    setField(doc, 'username', '');
    setField(doc, 'password', '');
    submit(w, doc);
    assert.equal(modalOpen(doc), false);
    assert.equal(p.launched.length, 1);
    assert.equal(notice(doc).hidden, true);
    assert.deepEqual(errors, []);
});

test('a URI without credentials still asks for the missing username and password', async () => {
    const p = await page();
    const { w, doc } = p;
    setField(doc, 'uri', 'mongodb://localhost:27017/');
    setField(doc, 'username', '');
    setField(doc, 'password', '');
    submit(w, doc);
    assert.equal(modalOpen(doc), true);
    assert.equal(p.launched.length, 0);
    assert.notEqual(doc.getElementById('modalUsernameGroup').style.display, 'none');
    assert.notEqual(doc.getElementById('modalPasswordGroup').style.display, 'none');
    assert.match(doc.getElementById('credModalMessage').textContent, /for the MongoDB connection/);

    // A URI with only a username asks for the password alone.
    w.closeModal();
    setField(doc, 'uri', 'mongodb://alice@localhost:27017/');
    submit(w, doc);
    assert.equal(doc.getElementById('modalUsernameGroup').style.display, 'none');
    assert.notEqual(doc.getElementById('modalPasswordGroup').style.display, 'none');
});

test('both the URI and the fields hold credentials: the run starts and a notice closes after 5 seconds', async () => {
    const p = await page();
    const { w, doc, errors } = p;
    const timers = [];
    const realSetTimeout = w.setTimeout;
    w.setTimeout = (fn, ms) => { if (ms === 5000) { timers.push(fn); return 1; } return realSetTimeout(fn, ms); };
    setField(doc, 'uri', 'mongodb://alice:secret@localhost:27017/');
    setField(doc, 'username', 'plgm');
    setField(doc, 'password', 'other');
    submit(w, doc);
    assert.equal(modalOpen(doc), false);
    assert.equal(p.launched.length, 1, 'the notice never stops the run');
    const box = notice(doc);
    assert.equal(box.hidden, false);
    assert.equal(box.getAttribute('role'), 'status');
    assert.match(doc.getElementById('credentialsNoticeText').textContent, /MongoDB URI includes a username and password.*fields are ignored/);
    assert.equal(timers.length, 1, 'closes itself after 5 seconds');
    timers[0]();
    assert.equal(box.hidden, true);

    submit(w, doc);
    assert.equal(box.hidden, false);
    box.querySelector('button[aria-label="Close"]').click();
    assert.equal(box.hidden, true, 'the x closes it sooner');
    w.setTimeout = realSetTimeout;
    assert.deepEqual(errors, []);
});

test('the same username without a password in the fields is not a conflict', async () => {
    const { w } = await page();
    assert.deepEqual(Array.from(w.credentialsOverriddenByURI(form(w, { uri: 'mongodb://plgm:x@h/', username: 'plgm', password: '' }))), []);
    assert.deepEqual(Array.from(w.credentialsOverriddenByURI(form(w, { uri: 'mongodb://plgm:x@h/', username: 'plgm', password: 'y' }))), ['MongoDB URI']);
    assert.deepEqual(Array.from(w.credentialsOverriddenByURI(form(w, { uri: 'mongodb://h/', username: 'plgm', password: 'y' }))), []);
});

test('several servers: each URI uses its own credentials and only servers without them need the fields', async () => {
    const { w } = await page();
    const targets = JSON.stringify([
        { label: 'm8', uri: 'mongodb://a:pa@a:1/' },
        { label: 'm9', uri: 'mongodb://b:2/' },
        { label: 'm10', uri: 'mongodb://c:pc@c:3/' },
    ]);
    const base = { uri: 'mongodb://ignored/', workload_replay_targets: targets };
    const need = w.separateCredentialsNeeded(form(w, base));
    assert.deepEqual(Array.from(need.user), ['m9']);
    assert.deepEqual(Array.from(need.pass), ['m9']);
    assert.equal(need.several, true);
    assert.deepEqual(Array.from(w.credentialsOverriddenByURI(form(w, { ...base, username: 'plgm', password: 'x' }))), ['m8', 'm10']);

    const all = JSON.stringify([{ label: 'm8', uri: 'mongodb://a:pa@a:1/' }, { label: 'm9', uri: 'mongodb://b:pb@b:2/' }]);
    const none = w.separateCredentialsNeeded(form(w, { uri: 'mongodb://ignored/', workload_replay_targets: all }));
    assert.deepEqual([Array.from(none.user), Array.from(none.pass)], [[], []], 'different users per server need no fields');

    w.showCredentialsNotice(['m8', 'm10'], 3);
    assert.match(w.document.getElementById('credentialsNoticeText').textContent, /^m8, m10 have a username and password in their URIs.*only to the other servers/);
    w.showCredentialsNotice(['m8', 'm9'], 2);
    assert.match(w.document.getElementById('credentialsNoticeText').textContent, /^Every server \(m8, m9\)/);
    w.closeCredentialsNotice();
});

test('the credentials prompt names the servers whose URI has no credentials', async () => {
    const { w, doc } = await page();
    w.openCredentialModal(true, true, ['m9']);
    assert.match(doc.getElementById('credModalMessage').textContent, /No username or password was provided for m9 \(no username or password in its URI\)/);
    w.closeModal();
});
