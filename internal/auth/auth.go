// Package auth decides whether a request may be served, and as whom.
//
// There are two tiers of credential, and the difference between them is reach.
//
// An **admin key** is the deployment's own, configured at boot (SANDBOX_API_KEY)
// and may do anything this service can. It is the whole identity of the
// deployment: presenting it means "you may do everything".
//
// A **sandbox key** belongs to one sandbox and reaches only that sandbox. It is
// issued when the sandbox is created — see internal/k8s, which keeps it in the
// sandbox's own namespace — and resolved per request, so it is not a second copy
// of the configuration but a lookup. Its purpose is the one the single tier
// could not serve: letting someone work in their own sandbox without handing
// them a credential that can run code in every other one.
//
// Where the line between the tiers falls is decided by the API layer, which is
// the only place that knows what a request is addressing — this package answers
// "who is this", not "may they do this". So the model is two pieces: OK, which
// is a comparison and says whether the deployment's key was presented, and
// Identity, which is who the request is — including nobody, which is a callers
// reach the key-less routes as.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
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

// Identity is who a request is: which tier, and — for a sandbox key — which
// sandbox.
type Identity struct {
	// Sandbox is the sandbox a sandbox key belongs to and reaches. Empty means
	// the admin tier.
	Sandbox string

	// Anonymous says no credential was presented and none was recognised.
	//
	// It has to be a field of its own rather than an empty Sandbox, because the
	// zero Identity is the admin tier: every route that authenticates builds one
	// for a valid admin key, so "Sandbox is empty" cannot mean both "this is the
	// operator" and "this is nobody". Only the routes reachable without a key
	// ever see this true; a route behind the middleware has already refused an
	// unauthenticated request.
	Anonymous bool
}

// Admin reports whether this identity is the unrestricted tier.
//
// An anonymous request is not: it reaches the routes that need no key and
// nothing else, so anything narrowing what a caller may see has to check this
// before it checks the sandbox.
func (i Identity) Admin() bool { return i.Sandbox == "" && !i.Anonymous }

// Authenticated reports whether a credential was presented and recognised.
func (i Identity) Authenticated() bool { return !i.Anonymous }

// KeyResolver maps a presented sandbox key to the sandbox that owns it.
//
// It is an interface rather than a concrete dependency so this package does not
// import the Kubernetes client, and so a test can supply a resolver without a
// cluster. A nil resolver means this deployment cannot resolve sandbox keys at
// all, and then only the admin tier exists — which is what a test that wants to
// exercise the routes without a cluster gets.
type KeyResolver interface {
	// ResolveSandboxKey returns the sandbox owning the presented key. ok is
	// false when the key belongs to no sandbox, which is not an error: an
	// unrecognised key is simply not a credential.
	ResolveSandboxKey(ctx context.Context, presented string) (sandboxID string, ok bool, err error)
}

// ErrUnauthenticated means the presented credential is not one this deployment
// recognises, at either tier.
var ErrUnauthenticated = errors.New("unauthenticated")

// Resolve reports who a request is, or ErrUnauthenticated.
//
// The admin tier is checked first and short-circuits: it is a fixed value held
// in memory, so the common case costs no cluster call, and an admin key that
// happened to equal a sandbox key would still be admin.
//
// The presented key is taken as an argument rather than read from the request so
// that the caller decides whether this route may take it from the query string —
// only the data plane may, and that is a routing decision this package has no
// business making.
//
// A resolver failure is returned rather than swallowed. The caller turns it into
// a 503: a key that could not be checked is not a key that failed to check out,
// and reporting it as 401 would tell an operator their credential is wrong when
// the truth is that the cluster is unreachable.
func (a *Authenticator) Resolve(ctx context.Context, presented string, resolver KeyResolver) (Identity, error) {
	if a.accepts(presented) {
		return Identity{}, nil
	}

	// An unusable key is not worth a cluster call: an empty or whitespace value
	// cannot be a sandbox key, and asking is a free round trip for an
	// unauthenticated caller to spend.
	//
	// The failure returns Anonymous rather than the zero Identity, and that is
	// load-bearing: the zero Identity is the *admin* tier, so returning it here
	// would hand an unauthenticated caller everything.
	if resolver == nil || strings.TrimSpace(presented) == "" {
		return Identity{Anonymous: true}, ErrUnauthenticated
	}

	id, ok, err := resolver.ResolveSandboxKey(ctx, presented)
	if err != nil {
		return Identity{Anonymous: true}, fmt.Errorf("resolving the sandbox key: %w", err)
	}
	if !ok {
		return Identity{Anonymous: true}, ErrUnauthenticated
	}
	return Identity{Sandbox: id}, nil
}

// PresentedKey is the credential a request carries in its headers.
func PresentedKey(r *http.Request) string { return presentedKey(r) }

// PresentedKeyURL is PresentedKey, also accepting the key in the query string.
// It is for the routes a browser opens by navigating to them — the data plane,
// where a link to a sandbox's own port is followed rather than fetched — and not
// for the JSON API, whose callers can always set a header and whose URLs end up
// in logs.
func PresentedKeyURL(r *http.Request) string {
	if k := presentedKey(r); k != "" {
		return k
	}
	return r.URL.Query().Get(QueryParam)
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
