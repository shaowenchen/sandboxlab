package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// dataPlanePrefix is the mount the data plane is registered under. It is
// declared here rather than inside the handler because ServeHTTP's identity pass
// tests the same prefix to decide whether a key may arrive in the query string,
// and two copies of it would be two things to keep in step.
const dataPlanePrefix = "/sandbox/"

// dataPlane serves /sandbox/<id>/<port>/... by forwarding to the sandbox.
//
// The shape is <id>/<port>/<rest>, and the port is a name from the template
// rather than a number: an environment's addresses are stable across a template
// changing its ports, and `/sandbox/foo/api/` says what it reaches where
// `/sandbox/foo/8000/` only says where it goes. A numeric port is accepted too,
// for the case where the catalog has moved on and a bookmark has not.
func (s *Server) dataPlane(dp DataPlane) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Identity was resolved in ServeHTTP, which is also where the decision
		// about this family accepting the key in the query string was made. Here
		// it is only acted on: a browser navigation cannot set a header, so
		// refusing this family's key for arriving in the URL would refuse the
		// one case the route exists for.
		a := authOf(r)
		if a.err != nil && !errors.Is(a.err, auth.ErrUnauthenticated) {
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the sandbox keys could not be checked: " + a.err.Error()})
			return
		}
		if !a.identity.Authenticated() {
			s.unauthorized(w)
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, dataPlanePrefix)
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

		// A sandbox key reaches its own sandbox's ports and no others. Answered
		// as a missing sandbox for the same reason the JSON routes are: the
		// address is a name the caller chose, and one sandbox learning which
		// other names exist is not something this route should offer.
		if !a.identity.Admin() && a.identity.Sandbox != id {
			http.Error(w, "no sandbox named "+id, http.StatusNotFound)
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
