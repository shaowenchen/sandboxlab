// Package auth decides whether a request may be served.
//
// There is one key, held by the deployment, and it may do everything. A request
// either presents it or it does not. That is the whole model, which is why this
// package is short: authorization here is a comparison, not a decision, and
// there is no caller to identify — nothing downstream asks who made a request,
// because the answer would not change what it does.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authenticator checks the deployment's key.
type Authenticator struct {
	key []byte
}

// New builds an authenticator over the deployment's key.
func New(key string) *Authenticator {
	return &Authenticator{key: []byte(key)}
}

// Header names the key may arrive in.
const (
	AuthorizationHeader = "Authorization"
	APIKeyHeader        = "X-Sandbox-Key"
	// QueryParam is accepted where neither header can be set — a browser
	// navigation to a sandbox's URL, most of all. The data-plane routes read it;
	// the JSON API does not, so a key cannot end up in a URL that gets logged by
	// accident.
	QueryParam = "key"
)

// OK reports whether the request presents the deployment's key.
func (a *Authenticator) OK(r *http.Request) bool {
	return a.accepts(presentedKey(r))
}

// OKURL is OK, also accepting the key in the query string. It is for the routes
// a browser opens by navigating to them — the data plane, where a link to a
// sandbox's own port is followed rather than fetched.
//
// The console is not one of those: its document is served without a key, and the
// page then presents the key itself on every call it makes. The query string is
// still what its ?key= link arrives in, and the page reads that and uses it as a
// key rather than the server accepting it here.
func (a *Authenticator) OKURL(r *http.Request) bool {
	if a.OK(r) {
		return true
	}
	return a.accepts(r.URL.Query().Get(QueryParam))
}

// accepts compares a presented key in constant time.
//
// Constant time because the alternative leaks the key a byte at a time to anyone
// who can measure a response. An empty key never matches: a deployment that was
// somehow started without one refuses everything rather than accepting an empty
// header, which is the failure that would open it to the world.
func (a *Authenticator) accepts(candidate string) bool {
	if candidate == "" || len(a.key) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), a.key) == 1
}

func presentedKey(r *http.Request) string {
	if k := bearer(r.Header.Get(AuthorizationHeader)); k != "" {
		return k
	}
	return r.Header.Get(APIKeyHeader)
}

func bearer(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
