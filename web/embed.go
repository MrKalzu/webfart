package web

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var staticEmbedFS embed.FS

// EmbeddedFS returns an fs.FS sub-tree rooted at "static".
func EmbeddedFS() fs.FS {
	sub, err := fs.Sub(staticEmbedFS, "static")
	if err != nil {
		return staticEmbedFS
	}
	return sub
}
