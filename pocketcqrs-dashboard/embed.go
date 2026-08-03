package main

import "embed"

// The dashboard is a self-contained binary: htmx, Web Awesome and
// cytoscape.js are vendored under assets/ (see assets/VENDORED.md) and the
// html/templates are embedded — no CDN, no runtime file dependencies.

//go:embed assets
var assetsFS embed.FS

//go:embed templates
var templateFS embed.FS
