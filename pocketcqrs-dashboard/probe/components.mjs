// Headless-Edge probe: load a dashboard page, log any page errors, and
// report whether each Web Awesome custom element actually upgraded (has a
// shadowRoot). DASH.2's login-page bug looked fine over HTTP and was only
// visible here.
import puppeteer from 'puppeteer-core';

// PROBE_BROWSER can point at any Chromium binary; the default is the system
// Edge that Windows already has, so no browser download is needed.
const EDGE = process.env.PROBE_BROWSER || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const API = process.env.PROBE_BACKEND || 'http://127.0.0.1:8390';
const BASE = process.env.PROBE_DASHBOARD || 'http://127.0.0.1:8391';

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox'] });
const page = await browser.newPage();

const errors = [];
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
page.on('requestfailed', (r) => errors.push('requestfailed: ' + r.url() + ' ' + (r.failure()?.errorText ?? '')));
page.on('console', (m) => {
  if (m.type() === 'error') errors.push('console: ' + m.text());
});

// sign in first — every browsing page is behind the cookie. Take the token
// straight from the backend and plant the cookie, rather than driving the
// login form: this probe is about whether components boot, not about auth
// (which the HTTP smoke already covers).
const authResp = await fetch(`${API}/api/collections/_superusers/auth-with-password`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ identity: process.env.PROBE_USER || 'smoketest@example.com', password: process.env.PROBE_PASS || 'smoke-pass-1234' }),
});
const { token } = await authResp.json();
if (!token) throw new Error('no token from backend');
await page.setCookie({ name: 'pcqrs_auth', value: token, domain: '127.0.0.1', path: '/', httpOnly: true });

const pages = {
  '/': ['wa-page', 'wa-callout', 'wa-card', 'wa-icon'],
  '/aggregates': ['wa-scroller', 'wa-tag', 'wa-card'],
  '/aggregates/task/t1': ['wa-details', 'wa-breadcrumb', 'wa-breadcrumb-item'],
  '/events': ['wa-select', 'wa-option', 'wa-number-input', 'wa-button', 'wa-scroller'],
  '/consumers': ['wa-tag', 'wa-scroller', 'wa-details'],
};

let bad = 0;
for (const [path, tags] of Object.entries(pages)) {
  await page.goto(BASE + path, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 700)); // let the loader define lazily
  const report = await page.evaluate((tags) => {
    return tags.map((t) => {
      const el = document.querySelector(t);
      return {
        tag: t,
        present: !!el,
        defined: !!customElements.get(t),
        shadow: !!(el && el.shadowRoot),
      };
    });
  }, tags);
  for (const r of report) {
    const ok = r.present && r.defined && r.shadow;
    if (!ok) bad++;
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${path} ${r.tag} present=${r.present} defined=${r.defined} shadow=${r.shadow}`);
  }
  // the post-it timeline's readability depends on a part override landing
  if (path === '/aggregates/task/t1') {
    const c = await page.evaluate(() => {
      const note = document.querySelector('.postit-event');
      if (!note) return { noteBg: 'MISSING', noteColor: 'MISSING', detailsColor: 'MISSING' };
      const det = note.querySelector('wa-details');
      const inner = det.shadowRoot && det.shadowRoot.querySelector('details');
      return {
        noteBg: getComputedStyle(note).backgroundColor,
        noteColor: getComputedStyle(note).color,
        detailsColor: inner ? getComputedStyle(inner).color : 'n/a',
      };
    });
    console.log(`     post-it fill=${c.noteBg} text=${c.noteColor} details-part-text=${c.detailsColor}`);
    if (c.detailsColor !== c.noteColor) {
      bad++;
      console.log('FAIL wa-details part colour did not follow the pinned post-it text colour');
    }
  }
  if (path === '/') {
    const nodes = await page.evaluate(() => {
      const cy = document.getElementById('explorer');
      return cy ? cy.querySelectorAll('canvas').length : 0;
    });
    console.log(`     explorer canvases=${nodes}`);
    if (nodes === 0) {
      bad++;
      console.log('FAIL cytoscape did not render');
    }
  }
}

// The notation colours are palette tints on purpose: an event post-it must
// stay orange with dark text in dark mode, where semantic tokens would flip.
// That is the one claim the stylesheet comment makes, so check it.
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
await page.goto(BASE + '/aggregates/task/t1', { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 700));
const dark = await page.evaluate(() => {
  const note = document.querySelector('.postit-event');
  const det = note.querySelector('wa-details');
  const inner = det.shadowRoot && det.shadowRoot.querySelector('details');
  return {
    scheme: document.documentElement.className,
    bg: getComputedStyle(note).backgroundColor,
    text: getComputedStyle(note).color,
    detailsText: inner ? getComputedStyle(inner).color : 'n/a',
    payloadBg: getComputedStyle(note.querySelector('.payload')).backgroundColor,
    payloadText: getComputedStyle(note.querySelector('.payload')).color,
    pageBg: getComputedStyle(document.body).backgroundColor,
  };
});
console.log(`\ndark mode: html="${dark.scheme}" page=${dark.pageBg}`);
console.log(`     post-it fill=${dark.bg} text=${dark.text} details-part-text=${dark.detailsText}`);
console.log(`     payload-in-postit fill=${dark.payloadBg} text=${dark.payloadText}`);
if (dark.payloadText !== 'rgb(16, 18, 25)' || dark.payloadBg !== 'rgb(255, 240, 230)') {
  bad++;
  console.log('FAIL the payload inside a post-it inverted in dark mode');
}
if (dark.bg !== 'rgb(255, 223, 202)' || dark.text !== 'rgb(16, 18, 25)' || dark.detailsText !== dark.text) {
  bad++;
  console.log('FAIL the post-it notation colours did not hold in dark mode');
}

console.log('\npage errors: ' + (errors.length ? '\n  ' + errors.join('\n  ') : 'none'));
await browser.close();
process.exit(bad || errors.length ? 1 : 0);
