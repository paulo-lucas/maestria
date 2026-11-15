package ui

//go:generate go run ../../tools/syncdesign -design ../../design -out static

// DesignAssets lists the files copied from design/ into static/: the name under static/ mapped to
// its path under design/. design/ is the source of truth; static/ holds the embedded copies.
// After changing design/, run `go generate ./internal/ui` (or `go generate ./...`). A test fails
// if the copies drift.
//
// app.js is not in this list: it is the panel's own script, not part of the design files.
var DesignAssets = map[string]string{
	"tokens.css":     "tokens.css",
	"base.css":       "base.css",
	"components.css": "components.css",
	"shell.css":      "shell.css",
	"icons.svg":      "assets/icons.svg",
	"sidebar.js":     "assets/sidebar.js",

	// Favicon set and web manifest (linked from both shells; see the "head" template).
	"favicon.svg":          "assets/favicon.svg",
	"favicon-32.png":       "assets/favicon-32.png",
	"apple-touch-icon.png": "assets/apple-touch-icon.png",
	"icon-192.png":         "assets/icon-192.png",
	"icon-512.png":         "assets/icon-512.png",
	"manifest.webmanifest": "assets/manifest.webmanifest",
}
