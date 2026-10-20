//go:build deploy

package main

import (
	"io/fs"
	"webfart/web"
)

// staticFS returns the embedded web assets baked into the binary at build time.
func staticFS() fs.FS { return web.EmbeddedFS() }
