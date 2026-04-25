package main

import (
	_ "embed"
	"strings"
)

// adminUI is the unified single-page web dashboard served at /ui and the
// per-tab routes (/ui/clipsync, /ui/filesync, /ui/gateway, /ui/perf,
// /ui/logs, /ui/metrics, /ui/api, /ui/debug). Tab is selected from the
// URL path. Polls API endpoints every second. No external dependencies —
// vanilla JS + CSS only.
//
// The HTML, CSS, and JS sources live as plain files under cmd/mesh/assets/
// so editor tooling (syntax highlighting, language servers, formatters)
// works on them. Embedded into the binary at compile time via //go:embed
// and composed once at package init via a single replacement pass that
// substitutes the inline CSS and JS bodies and the runtime version string.
// The handler still writes one precomputed string per request.

//go:embed assets/admin.html
var adminHTMLTemplate string

//go:embed assets/admin.css
var adminCSS string

//go:embed assets/admin.js
var adminJS string

var adminUI = strings.NewReplacer(
	"__MESH_CSS__", adminCSS,
	"__MESH_JS__", adminJS,
	"__MESH_VERSION__", version,
).Replace(adminHTMLTemplate)
