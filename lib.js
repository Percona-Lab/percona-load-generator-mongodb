const { chromium } = require('playwright-core');
async function getPage() {
  const browser = await chromium.connectOverCDP('http://127.0.0.1:9333');
  const ctx = browser.contexts()[0];
  let page = ctx.pages().find(p => p.url().includes('9911')) || ctx.pages()[0];
  return { browser, page };
}
async function run(fn) {
  const { browser, page } = await getPage();
  try { await fn(page); } catch (e) { console.error('ERR', e.message); process.exitCode = 1; }
  // do not close the browser: keep state between scripts
  browser._connection && browser._connection.close && 0;
  process.exit(process.exitCode || 0);
}
module.exports = { run };
