package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// dataPlane serves /sandbox/<id>/<port>/... by forwarding to the sandbox.
//
// The shape is <id>/<port>/<rest>, and the port is a name from the template
// rather than a number: an environment's addresses are stable across a template
// changing its ports, and `/sandbox/foo/api/` says what it reaches where
// `/sandbox/foo/8000/` only says where it goes. A numeric port is accepted too,
// for the case where the catalog has moved on and a bookmark has not.
func (s *Server) dataPlane(dp DataPlane) http.HandlerFunc {
	// Root-relative, because ServeHTTP has already removed the deployment's
	// base path. Applying it again here would leave every request under a base
	// path unrecognised — which is what a test through the router caught that a
	// test of this function alone would not have.
	const prefix = "/sandbox/"
	return func(w http.ResponseWriter, r *http.Request) {
		// A browser navigation cannot set a header, so this is the one family
		// of routes where the key may arrive in the query string.
		if !s.auth.OKURL(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandboxlab"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "a valid API key is required"})
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		id, remainder, ok := strings.Cut(rest, "/")
		if !ok || id == "" {
			writeJSON(w, http.StatusBadRequest, errorBody{
				Error: "a sandbox address is /sandbox/<id>/<port>/ (for example /sandbox/demo/api/)",
			})
			return
		}
		port, tail, _ := strings.Cut(remainder, "/")
		if port == "" {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "the sandbox address names no port"})
			return
		}

		// The sandbox is looked up before the proxy is handed anything, and not
		// by the proxy — the proxy forwards to whatever target it is given, so
		// this is where the address is checked against something that exists.
		// Proxying is the one route a sandbox's own application serves, and it
		// would otherwise be a way to reach any name at all on the network.
		if _, err := s.svc.Get(r.Context(), id); err != nil {
			if errors.Is(err, sandbox.ErrNotFound) {
				http.Error(w, "no sandbox named "+id, http.StatusNotFound)
				return
			}
			writeError(w, logger(r), err)
			return
		}

		// The key travels in the query string on these routes, and a sandbox's
		// own application has no business seeing it. It is removed before the
		// request is forwarded.
		q := r.URL.Query()
		q.Del(auth.QueryParam)
		r.URL.RawQuery = q.Encode()

		dp.Serve(w, r, id, port, tail)
	}
}
