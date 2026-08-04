// DASH.5 probe: the function editor in a real browser.
//
// The claim that HTTP checks cannot make: CodeMirror attaches, and its
// content is written back to the <textarea> that htmx actually submits. If
// that write-through breaks, every save silently posts stale source — the
// page looks perfect and the edit is lost.
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

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();
const errors = [];
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
page.on('requestfailed', (r) => errors.push('requestfailed: ' + r.url() + ' ' + (r.failure()?.errorText ?? '')));
page.on('console', (m) => {
  if (m.type() === 'error' || m.type() === 'warning') errors.push(`console.${m.type()}: ` + m.text());
});
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });

await page.goto(BASE + '/functions/audit.js', { waitUntil: 'networkidle2' });
await sleep(900);

// 1. CodeMirror attached to the textarea
const attached = await page.evaluate(() => {
  const cm = document.querySelector('.CodeMirror');
  const area = document.querySelector('#function-source');
  return {
    cmPresent: !!cm,
    cmHasContent: !!cm && cm.textContent.includes('@trigger'),
    areaPresent: !!area,
    areaHidden: !!area && getComputedStyle(area).display === 'none',
    lineNumbers: !!document.querySelector('.CodeMirror-linenumber'),
    highlighted: !!document.querySelector('.cm-keyword, .cm-comment, .cm-string'),
  };
});
check(attached.cmPresent && attached.cmHasContent, 'CodeMirror attached and holds the file', JSON.stringify(attached));
check(attached.lineNumbers && attached.highlighted, 'line numbers and JS highlighting are on');
check(attached.areaPresent && attached.areaHidden, 'the real <textarea> is still in the form, hidden behind the editor');

// 2. THE critical one: type in CodeMirror, confirm the textarea follows.
// htmx collects form values from the textarea and knows nothing about the
// editor widget, so without write-through every save posts stale source.
const typed = await page.evaluate(() => {
  const cm = document.querySelector('.CodeMirror').CodeMirror;
  cm.setValue('//@trigger event TaskCreated\nconsole.log("typed in the browser");\n');
  return {
    editor: cm.getValue().includes('typed in the browser'),
    textarea: document.querySelector('#function-source').value.includes('typed in the browser'),
  };
});
check(typed.editor, 'the editor accepts input');
check(typed.textarea, 'the edit is written through to the <textarea> htmx submits', JSON.stringify(typed));

// 3. Save it through the UI and confirm the BACKEND got what was typed.
await page.evaluate(() => {
  document.querySelector('wa-button[value="save"]').click();
});
await sleep(1800);
const saved = await (await fetch(`${API}/api/cqrs/admin/functions/audit.js`, { headers: { Authorization: token } })).json();
check(
  (saved.source || '').includes('typed in the browser'),
  'the source typed in the browser is what reached the backend',
  (saved.source || '').slice(0, 60),
);
const flash = await page.evaluate(() => document.querySelector('#editor-panel wa-callout')?.textContent.trim().slice(0, 60));
check(/Saved, not live/.test(flash || ''), 'the panel reports saved-but-not-live', flash);

// 4. After the htmx swap the panel is a NEW element — the editor has to
// re-attach, or the operator is left with a plain textarea after one save.
const reattached = await page.evaluate(() => {
  const cm = document.querySelector('.CodeMirror');
  return { present: !!cm, wired: !!(cm && cm.CodeMirror) };
});
check(reattached.present && reattached.wired, 'CodeMirror re-attached after the htmx swap', JSON.stringify(reattached));

// 5. a dry run, driven from the page
await page.evaluate(() => {
  const cm = document.querySelector('.CodeMirror').CodeMirror;
  cm.setValue('//@trigger event TaskCreated\nconsole.log("candidate");\n');
  document.querySelector('wa-button[value="dryrun"]').click();
});
await sleep(1800);
const dry = await page.evaluate(() => {
  const card = document.querySelector('.dryrun-result');
  return { present: !!card, text: card?.textContent.replace(/\s+/g, ' ').trim().slice(0, 90) };
});
check(dry.present && /compile|Parses/i.test(dry.text || ''), 'the dry-run panel renders its result', dry.text);

// 6. the editor follows the colour scheme like everything else
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
await sleep(500);
const dark = await page.evaluate(() => {
  const cm = document.querySelector('.CodeMirror');
  const body = getComputedStyle(document.body);
  return {
    scheme: document.documentElement.className,
    cmBg: getComputedStyle(cm).backgroundColor,
    cmColor: getComputedStyle(cm).color,
    pageBg: body.backgroundColor,
  };
});
// CodeMirror's own stylesheet is light-only; the tokens must have won
const isWhite = dark.cmBg === 'rgb(255, 255, 255)';
check(dark.scheme.includes('wa-dark'), 'the page went dark');
check(!isWhite, 'the editor surface followed the theme instead of staying a white slab', JSON.stringify(dark));

console.log('\npage errors/warnings: ' + (errors.length ? '\n  ' + errors.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errors.length ? 1 : 0);


