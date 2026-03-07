// Package defaults embeds the built-in static assets (index.html, 404.html,
// style.css) that ship with the binary. These are used as a fallback when the
// configured files.root directory does not contain those files, ensuring that
// the server always has sensible default pages regardless of deployment layout.
//
// Embedded asset paths use the "public/" prefix, e.g.:
//
//	data, err := fs.ReadFile(defaults.FS, "public/index.html")
package defaults

import "embed"

// FS holds the embedded public/ directory tree.
// Consumers should read files via fs.ReadFile(FS, "public/<name>") where
// <name> is one of: index.html, 404.html, style.css.
//
//go:embed public/index.html public/404.html public/style.css
var FS embed.FS
