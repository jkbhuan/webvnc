package main

import (
	"embed"
	"io/fs"
)

// distFS holds the compiled Vue frontend.
//
// The `all:` prefix matters: without it, go:embed silently skips files whose
// names begin with `_` or `.`, and Vite's asset names are not guaranteed to
// avoid those prefixes.
//
//go:embed all:frontend/dist
var distFS embed.FS

// frontendFS returns the built SPA rooted at the dist directory, so that
// "/index.html" resolves inside the embedded tree.
func frontendFS() (fs.FS, error) {
	return fs.Sub(distFS, "frontend/dist")
}
