// System page: every component boots, the reload indicator is hidden while
// idle, and the reload report swaps in place rather than navigating.
import puppeteer from 'puppeteer-core';

const EDGE = process.env.PROBE_BROWSER || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const BASE = process.env.PROBE_DASHBOARD || 'http://127.0.0.1:8391';
const API = process.env.PROBE_BACKEND || 'http://127.0.0.1:8390';
const { token } = await (await fetch(`${API}/api/collections/_superusers/auth-with-password`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ identity: process.env.PROBE_USER || 'smoketest@example.com', password: process.env.PROBE_PASS || 'smoke-pass-1234' }) })).json();
const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push('pageerror: ' + e.message));
page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') errs.push(`console.${m.type()}: ` + m.text()); });
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });
let bad = 0;
// the System page after a reload, so the report panel is populated too
await page.goto(BASE + '/system', { waitUntil: 'networkidle2' });
await new Promise(r => setTimeout(r, 900));
const rep = await page.evaluate(() => {
  const tags = ['wa-card', 'wa-callout', 'wa-button', 'wa-tag', 'wa-spinner', 'wa-icon'];
  return tags.map(t => { const el = document.querySelector(t);
    return { t, present: !!el, defined: !!customElements.get(t), shadow: !!(el && el.shadowRoot) }; });
});
for (const r of rep) { const ok = r.present && r.defined && r.shadow; if (!ok) bad++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} /system ${r.t} present=${r.present} defined=${r.defined} shadow=${r.shadow}`); }
// the spinner must be HIDDEN until a request is in flight
const spin = await page.evaluate(() => {
  const s = document.querySelector('wa-spinner.htmx-indicator');
  return s ? { vis: getComputedStyle(s).visibility, op: getComputedStyle(s).opacity } : null; });
console.log(`     reload spinner idle: ${JSON.stringify(spin)}`);
if (!spin || spin.vis !== 'hidden') { bad++; console.log('FAIL the htmx indicator is visible while idle'); }
// drive the reload button and confirm the report panel swaps in place
await page.evaluate(() => document.querySelector('form[hx-post="/system/reload"] wa-button').click());
await new Promise(r => setTimeout(r, 1500));
const after = await page.evaluate(() => {
  const p = document.querySelector('#reload-report');
  return { text: p ? p.textContent.replace(/\s+/g, ' ').trim().slice(0, 90) : null,
           url: location.pathname, calloutShadow: !!p?.querySelector('wa-callout')?.shadowRoot }; });
console.log(`     after reload click: url=${after.url} callout-upgraded=${after.calloutShadow}`);
console.log(`     report: ${after.text}`);
if (after.url !== '/system' || !after.calloutShadow || !/tier/i.test(after.text || '')) {
  bad++; console.log('FAIL the reload report did not swap in place'); }
const sc = await page.evaluate(() => { const s = document.querySelector('#reload-report wa-scroller'); return { present: !!s, shadow: !!(s && s.shadowRoot) }; });
console.log(`${sc.present && sc.shadow ? 'ok  ' : 'FAIL'} /system wa-scroller in the swapped report present=${sc.present} shadow=${sc.shadow}`);
if (!sc.present || !sc.shadow) bad++;
console.log('\npage errors/warnings: ' + (errs.length ? '\n  ' + errs.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errs.length ? 1 : 0);

