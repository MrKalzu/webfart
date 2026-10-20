//go:build !deploy

package main

import "io/fs"

// staticFS returns nil in dev mode — assets are read from -static disk dir.
func staticFS() fs.FS { return nil }
