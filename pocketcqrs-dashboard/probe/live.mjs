// DASH.4 headless-Edge probe: the things HTTP assertions cannot see.
//
//   1. htmx actually loads and its polls actually fire.
//   2. Web Awesome upgrades components that arrive in a SWAP, not just at
//      load (the autoloader's MutationObserver) — otherwise polled rows
//      render as undefined elements: unstyled text, no icons, no error.
//   3. The out-of-band rider updates a figure OUTSIDE the swapped table,
//      end to end: a dead letter created while the page sits open must
//      appear in the header count with no reload.
//   4. The colour-scheme flip works in BOTH directions (the DASH.3
//      carry-over) and the cytoscape graph re-reads its tokens.
import puppeteer from 'puppeteer-core';

const EDGE = process.env.PROBE_BROWSER || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const BASE = process.env.PROBE_DASHBOARD || 'http://127.0.0.1:8391';
const API = process.env.PROBE_BACKEND || 'http://127.0.0.1:8390';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let bad = 0;
const check = (ok, name, detail = '') => {
  if (!ok) bad++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${detail ? ' -- ' + detail : ''}`);
};

const { token } = await (
  await fetch(`${API}/api/collections/_superusers/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity: process.env.PROBE_USER || 'smoketest@example.com', password: process.env.PROBE_PASS || 'smoke-pass-1234' }),
  })
).json();
if (!token) throw new Error('no token from backend');

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();
const errors = [];
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
page.on('requestfailed', (r) => errors.push('requestfailed: ' + r.url() + ' ' + (r.failure()?.errorText ?? '')));
page.on('console', (m) => {
  if (m.type() === 'error' || m.type() === 'warning') errors.push(`console.${m.type()}: ` + m.text());
});

// plant the cookie rather than driving the login form (DASH.3 lesson:
// form-driving silently leaves the probe on the login page)
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });

// ---------------------------------------------------------------- consumers
const polls = [];
page.on('request', (r) => {
  if (r.url().includes('/fragments/')) polls.push(r.url());
});

await page.goto(BASE + '/consumers', { waitUntil: 'networkidle2' });
await sleep(800);

check(await page.evaluate(() => typeof window.htmx === 'object' || typeof window.htmx === 'function'), 'htmx is loaded');
check(
  await page.evaluate(() => !!document.querySelector('#checkpoints-body')?.getAttribute('hx-trigger')),
  'the checkpoints body carries its trigger',
);

const before = polls.length;
await sleep(5200);
const fired = polls.length - before;
check(fired >= 2, `polling fires (${fired} fragment requests in ~5s at every 2s)`);

// components that arrived in a SWAP must be upgraded by the WA autoloader
const swapped = await page.evaluate(() => {
  const body = document.querySelector('#checkpoints-body');
  const tag = body?.querySelector('wa-tag');
  return {
    hasBody: !!body,
    powered: body?.getAttribute('data-htmx-powered') === 'true',
    tag: tag?.textContent.trim().slice(0, 24) ?? null,
    defined: !!customElements.get('wa-tag'),
    shadow: !!tag?.shadowRoot,
  };
});
check(swapped.hasBody && swapped.powered, 'the swapped-in table body is htmx-processed (polling continues)');
check(swapped.defined && swapped.shadow, 'a <wa-tag> inside swapped rows upgraded', JSON.stringify(swapped));

// ------------------------------------------------- out-of-band rider, live
//
// Assert the count MOVES rather than that it starts at zero: the probe must
// not care what the instance already had in it. It installs its own poison
// function too, so it does not depend on the instance being hand-prepared.
const pendingCount = (text) => {
  const m = /(\d+) pending/.exec(text ?? '');
  return m ? Number(m[1]) : 0;
};
const admin = (path, init) =>
  fetch(`${API}${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', Authorization: token, ...(init?.headers ?? {}) },
  });

await admin('/api/cqrs/admin/functions/probe_poison.js', {
  method: 'PUT',
  body: JSON.stringify({
    source: '//@trigger event TaskCreated\nthrow new Error("probe: poisoned delivery");\n',
  }),
});
await admin('/api/cqrs/admin/reload', { method: 'POST' }); // effect tier: any mode
await sleep(500);

const pendingBefore = await page.evaluate(() => document.querySelector('#dl-pending')?.textContent.trim());
await admin(`/api/cqrs/task/probe-${Date.now()}/CreateTask`, {
  method: 'POST',
  body: JSON.stringify({ title: 'probe' }),
});
// that delivery fails, so it becomes a dead letter. The page is never
// reloaded — only the 2s poll and its out-of-band rider can show it.
await sleep(6000);
const pendingAfter = await page.evaluate(() => document.querySelector('#dl-pending')?.textContent.trim());
const headAfter = await page.evaluate(() => document.querySelector('#log-head')?.textContent.trim());
check(
  pendingCount(pendingAfter) > pendingCount(pendingBefore),
  'a new dead letter reaches the header count with no reload (out-of-band swap)',
  `before="${pendingBefore}" after="${pendingAfter}"`,
);
check(/position \d+/.test(headAfter ?? ''), 'the head-of-log rider updates too', `"${headAfter}"`);

// -------------------------------------------------------- colour scheme x2
await page.goto(BASE + '/', { waitUntil: 'networkidle2' });
await sleep(900);
const light = await page.evaluate(() => document.documentElement.className);
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
await sleep(400);
const dark = await page.evaluate(() => document.documentElement.className);
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }]);
await sleep(400);
const backToLight = await page.evaluate(() => document.documentElement.className);
check(light.includes('wa-light'), 'starts light', light);
check(dark.includes('wa-dark') && !dark.includes('wa-light'), 'flips to dark', dark);
// this is the DASH.3 carry-over: the old script only ever ran one way, so
// going back to light left the page stuck on wa-dark for the session
check(backToLight.includes('wa-light') && !backToLight.includes('wa-dark'), 'flips BACK to light', backToLight);

// and the graph follows: its node colours are canvas-baked, so it has to
// re-read the tokens on the event the layout script fires
const graph = await page.evaluate(() => {
  const canvases = document.querySelectorAll('#explorer canvas').length;
  return { canvases };
});
check(graph.canvases > 0, 'the catalog explorer still renders after two scheme flips', JSON.stringify(graph));

console.log('\npage errors/warnings: ' + (errors.length ? '\n  ' + errors.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errors.length ? 1 : 0);
