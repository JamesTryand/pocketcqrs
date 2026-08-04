# Vendored assets

The dashboard binary is self-contained: no CDN at runtime. Refresh by
re-installing from npm and re-copying the same paths.

| path | package | version | license |
| --- | --- | --- | --- |
| `webawesome/` | `@awesome.me/webawesome` (**`dist-cdn/`**: `webawesome.loader.js`, `chunks/`, `components/`, `styles/`) | 3.11.0 | MIT |
| `vendor/htmx.min.js` | `htmx.org` (`dist/htmx.min.js`) | 4.0.0-beta6 | BSD-0-Clause |
| `vendor/cytoscape.min.js` | `cytoscape` (`dist/cytoscape.min.js`) | 3.34.0 | MIT |
| `vendor/codemirror/` | `codemirror` (`lib/codemirror.{js,css}`, `mode/javascript/javascript.js`, `LICENSE`) | 5.65.21 | MIT |

Notes:

- **Use `dist-cdn/`, never `dist/`.** The npm `dist/` build is bundler-oriented
  and contains bare module specifiers (`@shoelace-style/animations`, `lit`, …)
  that browsers cannot resolve; `dist-cdn/` rewrites everything to relative
  imports and is what the Web Awesome CDN itself serves. A regression test
  (`TestVendoredWebAwesomeHasNoBareImports`) scans the embedded JS for bare
  imports.
- `webawesome.loader.js` auto-discovers `<wa-*>` tags and lazy-loads from
  `components/` and `chunks/` relative to its own URL — preserve the
  directory layout under `webawesome/`.
- `styles/` is vendored whole because the CSS files `@import` each other
  with relative paths.
- **CodeMirror 5, not 6, and unminified.** CM6 ships ESM with bare specifiers
  and would need a bundler — reintroducing exactly what the rule above
  guards against, in a tree the guard test would then have to trust. CM5's
  `lib/` files are UMD and load as plain `<script>`s. They are vendored
  **as published** (402 kB unminified): a minified artifact would have to be
  produced here, moving its provenance from "the npm package" to "whatever a
  local build emitted", which nothing can verify afterwards. Its editor
  colours come from `--wa-*` tokens in `app.css`, so it themes with the rest
  of the dashboard instead of shipping a CM theme.
- `TestVendoredAssetsHaveNoBareImports` scans **every** embedded JS tree, so
  the next thing vendored is checked too.
- Icons: the dashboard uses only the bundled `system` icon library
  (`<wa-icon library="system">` — inline SVGs, Font Awesome Free, icons
  under CC BY 4.0). The `default` icon library resolves from a CDN and is
  intentionally not used.
