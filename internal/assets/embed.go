// Package assets embeds the compiled UI into the binary.
// Build the UI first: cd ui && npm run build
package assets

import (
	"embed"
	"io/fs"
)

//go:embed dist
var embedded embed.FS

// FS is the embedded UI filesystem rooted at the dist/ directory.
// It is nil when dist/index.html is a placeholder (UI not built).
var FS, _ = fs.Sub(embedded, "dist")
