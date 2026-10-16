// DOM tests for Import Captured Traffic on the Record & Replay tab:
// explanations and defaults, the controls stay out of the run form, the
// convert request, the summary, download, Use for replay, and failures.
// traffic_import.json is real API output written by
// TestUIFixturesMatchAPITrafficImport.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fixture, readFixture, loadPage } from './harness.mjs';

const settle = () => new Promise(r => setTimeout(r, 10));

async function page(reply) {
    const data = readFixture('traffic_import.json');
    const posts = [];
    const p = await loadPage(fixture('trends'), {
        '/api/config': async () => ({ body: { URI: 'mongodb://fixture', InsightsEnabled: true } }),
        '/api/import-traffic': async (url, init) => {
            posts.push([...init.body.entries()]);
            return reply ? reply(data) : { body: { import: data.import } };
        },
    });
    await settle();
    p.w.HTMLElement.prototype.scrollIntoView = () => {};
    return { ...p, data, posts };
}

function chooseFiles(w, doc, names) {
    const files = names.map(n => new w.File(['{}\n'], n));
    Object.defineProperty(doc.getElementById('trafficImportFiles'), 'files', { value: files, configurable: true });
}

test('the import panel explains itself, defaults off, and is never submitted with the run', async () => {
    const { doc, errors } = await page();
    const intro = doc.getElementById('trafficImportIntro');
    assert.equal(intro.closest('.tab-content').id, 'tab-replay');
    assert.equal(intro.closest('.settings-card').id, 'trafficImportCard');
    for (const text of ['pt-upgrade', 'system.profile', 'slowms', 'profiling level 2', 'literal values', 'plgm import-traffic', 'getMore']) {
        assert.ok(intro.textContent.includes(text), text);
    }
    for (const id of ['trafficImportWorkersNote', 'trafficImportDbNote', 'trafficImportCompressionNote', 'trafficImportSkipGeneratedNote']) {
        const t = doc.getElementById(id).textContent;
        assert.ok(/(On|Set|zstd \/ gzip):/.test(t) && /(Off|0|Empty|None) \(default\):/.test(t), `${id}: ${t}`);
    }
    assert.equal(doc.getElementById('trafficImportWorkers').value, '0');
    assert.equal(doc.getElementById('trafficImportDb').value, '');
    assert.equal(doc.getElementById('trafficImportCompression').value, 'none');
    assert.equal(doc.getElementById('trafficImportSkipGenerated').checked, false);
    for (const id of ['trafficImportFiles', 'trafficImportWorkers', 'trafficImportDb', 'trafficImportCompression', 'trafficImportSkipGenerated']) {
        assert.equal(doc.getElementById(id).getAttribute('name'), null, `${id} must not be a run setting`);
    }
    assert.equal(doc.getElementById('trafficImportBtn').getAttribute('type'), 'button');
    assert.deepEqual(errors, []);
});

test('Convert needs files, sends the options, and shows the summary and actions', async () => {
    const { w, doc, posts, data, errors } = await page();
    await w.runTrafficImport();
    assert.equal(posts.length, 0);
    assert.ok(doc.getElementById('trafficImportStatus').textContent.includes('Choose one or more'));

    chooseFiles(w, doc, ['a.log', 'b.json.gz']);
    doc.getElementById('trafficImportWorkers').value = '4';
    doc.getElementById('trafficImportDb').value = 'appdb';
    doc.getElementById('trafficImportCompression').value = 'zstd';
    doc.getElementById('trafficImportSkipGenerated').checked = true;
    doc.getElementById('trafficImportBtn').click();
    await settle();
    assert.equal(posts.length, 1);
    const sent = posts[0];
    assert.deepEqual(sent.filter(([k]) => k === 'traffic_file').map(([, f]) => f.name), ['a.log', 'b.json.gz']);
    const fields = Object.fromEntries(sent.filter(([k]) => k !== 'traffic_file'));
    assert.deepEqual(fields, { workers: '4', default_db: 'appdb', compression: 'zstd', skip_server_generated: 'true' });

    const r = data.import.report;
    assert.ok(doc.getElementById('trafficImportStatus').textContent.includes(`Converted ${r.converted} operations`));
    const row = name => doc.querySelector(`#trafficImportSummary tr[data-row="${name}"] td`).textContent;
    assert.ok(row('Converted').includes(`${r.converted} of ${r.entries} entries`) && row('Converted').includes('mongod_log'));
    assert.ok(row('Workers').includes(`${r.connections} connections → ${r.workers} workers`));
    assert.ok(row('Skipped').includes('getmore 1') && row('Skipped').includes('truncated 1') && row('Skipped').includes('duplicate_write_statement 1'));
    assert.ok(row('Unsupported commands').includes('count 1'));
    assert.ok(row('Flagged').includes('server_generated_values 2') && row('Flagged').includes('not exactly as captured'));
    assert.ok(row('Options not replayed').includes('arrayFilters 1'));
    assert.ok(doc.getElementById('trafficImportWarnings').textContent.includes('line 15'));
    assert.ok(doc.getElementById('trafficImportNote').textContent.includes('slowms'));
    const dl = doc.getElementById('trafficImportDownload');
    assert.equal(dl.getAttribute('href'), '/api/import-traffic/fixture-import-1/recording');
    assert.equal(dl.getAttribute('download'), data.import.file_name);
    assert.ok(doc.getElementById('trafficImportResult').textContent.includes('Contains literal values'));

    doc.getElementById('trafficImportUseBtn').click();
    assert.equal(doc.getElementById('workloadReplayEnabled').value, 'true');
    assert.equal(doc.getElementById('workloadReplayPath').value, data.import.server_path);
    assert.ok(doc.getElementById('trafficImportStatus').textContent.includes('Skip Seed on'));
    assert.deepEqual(errors, []);
});

test('a failed import shows the error and its counts without download or replay actions', async () => {
    const { w, doc, errors } = await page(data => ({ status: 400, body: data.failed }));
    chooseFiles(w, doc, ['empty.log']);
    await w.runTrafficImport();
    assert.ok(doc.getElementById('trafficImportStatus').textContent.includes('Import failed: no operations could be converted'));
    assert.ok(doc.querySelector('#trafficImportSummary tr[data-row="Not operations"] td').textContent.includes('1'));
    assert.equal(doc.getElementById('trafficImportDownload'), null);
    assert.equal(doc.getElementById('trafficImportUseBtn'), null);
    assert.equal(doc.getElementById('workloadReplayEnabled').value, 'false');
    assert.deepEqual(errors, []);
});
