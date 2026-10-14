// Shared jsdom harness for the dashboard DOM tests. Fixtures are the real
// /api/insights output written by TestUIFixturesMatchAPI in the Go tests.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { JSDOM, VirtualConsole } from 'jsdom';

const here = dirname(fileURLToPath(import.meta.url));
const pageHTML = readFileSync(join(here, '..', 'static', 'index.html'), 'utf8');
export const fixture = name => JSON.parse(readFileSync(join(here, '..', 'testdata', 'ui', `insights_${name}.json`), 'utf8'));

// routes maps a URL prefix to async (url, init) => { status, body }.
export async function loadPage(insights, routes = {}) {
    const errors = [];
    const vc = new VirtualConsole();
    vc.on('jsdomError', e => { if (!String(e.message).includes('Not implemented')) errors.push(e); });
    const dom = new JSDOM(pageHTML, {
        runScripts: 'dangerously',
        pretendToBeVisual: true,
        url: 'http://localhost/',
        virtualConsole: vc,
        beforeParse(window) {
            // Chart.js is not under test: any property read yields a callable stub.
            const stub = () => new Proxy(function () {}, {
                get: (t, k) => (k in t ? t[k] : (t[k] = stub())),
                apply: () => stub(),
                construct: () => stub(),
            });
            window.Chart = stub();
            window.HTMLCanvasElement.prototype.getContext = () => null;
            window.fetchCalls = [];
            window.fetch = async (url, init = {}) => {
                window.fetchCalls.push({ url: String(url), method: (init.method || 'GET').toUpperCase() });
                for (const [prefix, handler] of Object.entries(routes)) {
                    if (String(url).startsWith(prefix)) {
                        const { status = 200, body = {} } = await handler(String(url), init);
                        return { ok: status < 400, status, json: async () => structuredClone(body), text: async () => JSON.stringify(body) };
                    }
                }
                if (String(url).startsWith('/api/insights')) {
                    if (insights instanceof Error) throw insights;
                    return { ok: true, status: 200, json: async () => structuredClone(insights) };
                }
                return { ok: true, status: 200, json: async () => ({}), text: async () => '{}' };
            };
            window.downloads = [];
            window.HTMLAnchorElement.prototype.click = function () {
                window.downloads.push({ href: this.getAttribute('href'), name: this.getAttribute('download') });
            };
        },
    });
    await new Promise(r => dom.window.addEventListener('load', r));
    const w = dom.window;
    await w.fetchFinalInsights();
    return { dom, w, doc: w.document, errors };
}

export function choose(doc, id, value) {
    const el = doc.getElementById(id);
    el.value = value;
    el.dispatchEvent(new doc.defaultView.Event('change', { bubbles: true }));
}

export function exportedSummary(w) {
    const { href } = w.downloads[w.downloads.length - 1];
    return JSON.parse(decodeURIComponent(href.replace(/^data:text\/json;charset=utf-8,/, '')));
}
