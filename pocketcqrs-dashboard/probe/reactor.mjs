// Probe: the reactor dry-run panel, and the scaffolder's warnings callout.
//
// Both render something the server can only be asked for over HTTP — the
// question here is whether the PAGE shows it. A `mode=reactor` response with a
// populated `dispatches` array and a page that renders an empty panel look
// identical to every backend test.
//
// Self-sufficient, like the others: it installs its own reactor through the
// admin API and asserts the panel names the command that reactor would send,
// rather than assuming an instance someone prepared by hand.
import puppeteer from 'puppeteer-core';
const EDGE = process.env.PROBE_BROWSER || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const BASE = process.env.PROBE_DASHBOARD || 'http://127.0.0.1:8391';
const API = process.env.PROBE_BACKEND || 'http://127.0.0.1:8390';

const { token } = await (await fetch(`${API}/api/collections/_superusers/auth-with-password`, {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ identity: 'smoketest@example.com', password: 'smoke-pass-1234' }) })).json();

const REACTOR = `//@trigger reactor TaskCompleted
//@dispatches task/CreateTask

function reactTo(event) {
  return [{
    aggregate: 'task',
    id: 'probe-followup-' + event.aggregateId,
    command: 'CreateTask',
    payload: { title: 'follow up on ' + event.aggregateId }
  }];
}
`;

// Install the fixture ourselves. A probe that only passes against an
// instance someone hand-prepared is a probe nobody else can run.
await fetch(`${API}/api/cqrs/admin/functions/probe_react.js`, {
  method: 'PUT', headers: { 'Content-Type': 'application/json', Authorization: token },
  body: JSON.stringify({ source: REACTOR }),
});
// ...and give the log a TaskCompleted to react to, so the panel has real
// dispatches to show rather than an honest empty table.
await fetch(`${API}/api/cqrs/task/probe-t1/CreateTask`, {
  method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: token },
  body: JSON.stringify({ title: 'probe task' }),
});
await fetch(`${API}/api/cqrs/task/probe-t1/CompleteTask`, {
  method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: token },
  body: JSON.stringify({}),
});

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push('pageerror: ' + e.message));
page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') errs.push(`console.${m.type()}: ` + m.text()); });
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });
let bad = 0;
const check = (ok, label, extra = '') => {
  if (!ok) bad++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${label}${extra ? ' — ' + extra : ''}`);
};

// ---- 1. the react dry-run panel ----
await page.goto(`${BASE}/functions/probe_react.js`, { waitUntil: 'networkidle2' });
await new Promise(r => setTimeout(r, 900));

// the editor page must have picked `react` as the mode for a reactor file:
// the mode comes from the declaration rather than a server-side guess, so a
// wrong one here means the two classifiers have drifted
const mode = await page.evaluate(() =>
  document.querySelector('input[name="mode"]')?.value || '');
check(mode === 'reactor', 'the editor chose mode=reactor for a reactor file', `mode=${mode || 'none'}`);

await Promise.all([
  page.waitForNavigation({ waitUntil: 'networkidle2' }).catch(() => {}),
  page.evaluate(() => {
    const btn = [...document.querySelectorAll('wa-button')]
      .find(b => /dry run/i.test(b.textContent));
    btn.click();
  }),
]);
await new Promise(r => setTimeout(r, 1200));

const panel = await page.evaluate(() => {
  const text = document.body.textContent.replace(/\s+/g, ' ');
  const rows = [...document.querySelectorAll('table tbody tr')].map(tr =>
    [...tr.querySelectorAll('td')].map(td => td.textContent.trim()).join(' | '));
  return { text, rows, hasPanel: /Dry run/i.test(text) };
});
check(panel.hasPanel, 'the dry-run panel rendered');
check(/would dispatch/i.test(panel.text), 'the panel says what the reactor would dispatch');
// The point of the whole panel: the DISPATCHES table, which exists nowhere
// else in the UI. An empty one renders perfectly and proves nothing.
const dispatchRow = panel.rows.find(r => /CreateTask/.test(r) && /probe-followup-/.test(r));
check(!!dispatchRow, 'the dispatches table lists the command and its derived target id',
  dispatchRow || `rows=${panel.rows.length}`);
check(/nothing was dispatched/i.test(panel.text) || /no decider ran/i.test(panel.text),
  'the panel says plainly that nothing was dispatched');

// ---- 2. the scaffolder's warnings callout ----
// A command with two possible events cannot have its outcome rule derived, and
// a generated file that RUNS looks finished — so the warning is the only thing
// that says the slice is unfinished. It renders on the page or it is invisible.
await page.goto(BASE + '/scaffold', { waitUntil: 'networkidle2' });
await new Promise(r => setTimeout(r, 700));
await page.evaluate(() => {
  const set = (sel, v, i = 0) => { document.querySelectorAll(sel)[i].value = v; };
  set('wa-input[name="aggregate"]', 'probePayment');
  set('wa-input[name="commandName"]', 'AttemptPayment', 0);
  set('wa-input[name="commandEvent"]', 'PaymentAccepted, PaymentRefused', 0);
  // no fields is fine here: the wizard marks "no payload" EXPLICITLY, which
  // is a decision rather than a gap, so it produces no warning of its own.
  // The outcome rule is the warning this exercises.
  set('wa-input[name="commandFields"]', '', 0);
});
await Promise.all([
  page.waitForNavigation({ waitUntil: 'networkidle2' }).catch(() => {}),
  page.evaluate(() => document.querySelector('form[action="/scaffold"] wa-button[type="submit"]').click()),
]);
await new Promise(r => setTimeout(r, 1200));

const warn = await page.evaluate(() => {
  const callouts = [...document.querySelectorAll('wa-callout')];
  const warning = callouts.find(c => c.getAttribute('variant') === 'warning');
  return {
    generated: document.querySelectorAll('pre.payload').length,
    found: !!warning,
    upgraded: !!(warning && warning.shadowRoot),
    text: warning ? warning.textContent.replace(/\s+/g, ' ').trim() : '',
  };
});
check(warn.generated >= 1, 'the wizard still generated a slice', `source blocks=${warn.generated}`);
check(warn.found, 'a warnings callout rendered for an unfinished slice');
check(warn.upgraded, 'the callout is a real upgraded component, not inert markup');
check(/still need deciding/i.test(warn.text) && /AttemptPayment/.test(warn.text) &&
  /2 different events/.test(warn.text),
  'the callout names the command whose outcome rule is missing', warn.text.slice(0, 140));

// leave the instance as we found it
await fetch(`${API}/api/cqrs/admin/functions/probe_react.js`, {
  method: 'DELETE', headers: { Authorization: token },
});

console.log('\npage errors/warnings: ' + (errs.length ? '\n  ' + errs.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errs.length ? 1 : 0);
