# Vendored assets

The dashboard binary is self-contained: no CDN at runtime. Refresh by
re-installing from npm and re-copying the same paths.

| path | package | version | license |
| --- | --- | --- | --- |
| `webawesome/` | `@awesome.me/webawesome` (`dist/webawesome.loader.js`, `dist/chunks/`, `dist/components/`, `dist/styles/`) | 3.11.0 | MIT |
| `vendor/htmx.min.js` | `htmx.org` (`dist/htmx.min.js`) | 4.0.0-beta6 | BSD-0-Clause |
| `vendor/cytoscape.min.js` | `cytoscape` (`dist/cytoscape.min.js`) | 3.34.0 | MIT |

Notes:

- `webawesome.loader.js` auto-discovers `<wa-*>` tags and lazy-loads from
  `components/` and `chunks/` relative to its own URL — preserve the
  directory layout under `webawesome/`.
- `styles/` is vendored whole because the CSS files `@import` each other
  with relative paths.
- Icons: the dashboard uses only the bundled `system` icon library
  (`<wa-icon library="system">` — inline SVGs, Font Awesome Free, icons
  under CC BY 4.0). The `default` icon library resolves from a CDN and is
  intentionally not used.
