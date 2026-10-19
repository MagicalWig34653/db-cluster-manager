// SPDX-License-Identifier: AGPL-3.0-only

// Package webui serves the built SvelteKit frontend embedded in the manager binary.
//
// The assets directory is populated by "make web"; only a placeholder is tracked.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:assets
var embedded embed.FS

// Handler serves the embedded frontend with single-page fallback to index.html.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(err)
	}
	return newHandler(sub)
}

func newHandler(root fs.FS) http.Handler {
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(root, "index.html"); err != nil {
			http.Error(w, "UI not built: run \"make web\" before building the manager", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			files.ServeHTTP(w, r)
			return
		}
		if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}
