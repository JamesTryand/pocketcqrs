import puppeteer from 'puppeteer-core';
const EDGE = process.env.PROBE_BROWSER || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const BASE = process.env.PROBE_DASHBOARD || 'http://127.0.0.1:8391';
const API = process.env.PROBE_BACKEND || 'http://127.0.0.1:8390';
const { token } = await (await fetch(`${API}/api/collections/_superusers/auth-with-password`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ identity: 'smoketest@example.com', password: 'smoke-pass-1234' }) })).json();
const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push('pageerror: ' + e.message));
page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') errs.push(`console.${m.type()}: ` + m.text()); });
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });
let bad = 0;
await page.goto(BASE + '/scaffold', { waitUntil: 'networkidle2' });
await new Promise(r => setTimeout(r, 900));
const rep = await page.evaluate(() => ['wa-card','wa-callout','wa-input','wa-checkbox','wa-button','wa-scroller','wa-icon']
  .map(t => { const el = document.querySelector(t); return { t, present: !!el, defined: !!customElements.get(t), shadow: !!(el && el.shadowRoot) }; }));
for (const r of rep) { const ok = r.present && r.defined && r.shadow; if (!ok) bad++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} /scaffold ${r.t} present=${r.present} defined=${r.defined} shadow=${r.shadow}`); }
// Fill the wizard and generate, the way an operator would. The click and
// the navigation wait must be raced together: a plain form submit can
// complete before a wait registered after the click ever attaches.
await page.evaluate(() => {
  const set = (sel, v, i = 0) => { document.querySelectorAll(sel)[i].value = v; };
  set('wa-input[name="aggregate"]', 'ticket');
  set('wa-input[name="commandName"]', 'OpenTicket', 0);
  set('wa-input[name="commandEvent"]', 'TicketOpened', 0);
  set('wa-input[name="commandFields"]', 'subject:text', 0);
  set('wa-input[name="collection"]', 'tickets');
});
await Promise.all([
  page.waitForNavigation({ waitUntil: 'networkidle2' }).catch(() => {}),
  page.evaluate(() => document.querySelector('form[action="/scaffold"] wa-button[type="submit"]').click()),
]);
await new Promise(r => setTimeout(r, 1200));
const out = await page.evaluate(() => ({
  text: document.body.textContent.replace(/\s+/g, ' '),
  cards: document.querySelectorAll('pre.payload').length,
}));
const okGen = out.cards >= 2 && /trigger decider ticket/.test(out.text) && /Nothing has been written/.test(out.text);
if (!okGen) bad++;
console.log(`${okGen ? 'ok  ' : 'FAIL'} the wizard generated a slice in the browser (source blocks=${out.cards})`);
console.log(`     ${out.text.slice(out.text.indexOf('Generated') >= 0 ? out.text.indexOf('Generated') : 0, 140)}`);
console.log('\npage errors/warnings: ' + (errs.length ? '\n  ' + errs.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errs.length ? 1 : 0);
