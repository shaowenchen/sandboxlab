// Package console serves the web interface.
//
// The whole thing is one HTML file, compiled into the binary. That is not a
// placeholder for something bigger — a control plane that manages a handful of
// short-lived sandboxes has a handful of views, and a build step, a bundler and
// a node_modules for them would be more moving parts than the interface is
// worth. It talks to the same /api/v1 the CLI does, so nothing here can drift
// from the API's behaviour.
package console

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// New returns the console handler.
//
// It serves the embedded files and falls back to index.html for anything it
// does not have, so a route the page handles client-side resolves on a reload
// rather than 404ing.
func New() (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, err := fs.Stat(sub, trimLeadingSlash(r.URL.Path)); err != nil {
			serveIndex(w, r, sub)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "the console is not available in this build", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func trimLeadingSlash(s string) string {
	if s == "" || s == "/" {
		return "."
	}
	if s[0] == '/' {
		return s[1:]
	}
	return s
}
