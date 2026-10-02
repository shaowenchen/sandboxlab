// Package auth decides who a request is from.
//
// There are two kinds of caller. An administrator holds the deployment's own
// key and may do anything. A user holds a key issued to them and may create
// sandboxes and see their own. Both arrive the same way, and the difference is
// resolved here — once, into an Identity — so every handler downstream asks
// "who is this" rather than re-reading headers.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Role is what a caller may do.
type Role string

const (
	// RoleAdmin may see and do everything, including managing users.
	RoleAdmin Role = "admin"
	// RoleUser may create sandboxes and see their own.
	RoleUser Role = "user"
)

// Identity is who made a request.
type Identity struct {
	Role Role
	// Name is the user's name, and is empty for an administrator — the
	// administrator is the deployment, not a person with a record.
	Name string
}

// IsAdmin reports whether the caller may manage users and see everything.
func (i Identity) IsAdmin() bool { return i.Role == RoleAdmin }

// Owns reports whether a sandbox with this owner belongs to the caller.
//
// An administrator owns nothing in particular and may reach everything, so this
// is true for them always; a user owns what carries their name.
func (i Identity) Owns(owner string) bool {
	return i.IsAdmin() || (i.Name != "" && i.Name == owner)
}

// UserLookup resolves a presented key to the user it belongs to. The store
// implements it; the interface is here so this package does not depend on the
// cluster.
type UserLookup interface {
	// MatchKey returns the name of the user whose key this is. It is where the
	// search over users lives, because that is where the users are — and it is
	// the only place a key is compared, so there is one comparison to get right
	// rather than one per caller.
	MatchKey(ctx context.Context, key string) (string, bool)
}

// Authenticator decides which identity a request carries.
type Authenticator struct {
	adminKey []byte
	users    UserLookup
}

// New builds an authenticator over the deployment's admin key and the user
// store.
func New(adminKey string, users UserLookup) *Authenticator {
	return &Authenticator{adminKey: []byte(adminKey), users: users}
}

// Key returns the administrator's key, so the summary can report it.
func (a *Authenticator) Key() string { return string(a.adminKey) }

// Header names the key may arrive in.
const (
	AuthorizationHeader = "Authorization"
	APIKeyHeader        = "X-Sandbox-Key"
	// QueryParam is accepted where neither header can be set — a browser
	// navigation to a sandbox's URL, most of all. The data-plane routes read
	// it; the JSON API does not, so a key cannot end up in a URL that gets
	// logged by accident.
	QueryParam = "key"
)

// Identity resolves a request to who made it.
//
// The administrator's key is checked first, and both comparisons are constant
// time. A user's is a search over the user list, which is linear — and
// deliberate: the alternative is a key hash in a header or an index to keep
// consistent, and both are a second thing to get wrong for a deployment whose
// user list is a handful of people on a runner that lives for four hours.
func (a *Authenticator) Identity(ctx context.Context, r *http.Request) (Identity, bool) {
	return a.resolve(ctx, presentedKey(r))
}

// IdentityURL is Identity, also accepting the key in the query string. It is for
// the routes a browser opens directly — the data plane, where a link to a
// sandbox's own port is followed by navigating to it.
//
// The console is not one of those any more: its document is served without a
// key, and the page then presents the key itself on every call it makes. The
// query string is still what its ?key= link arrives in, and the page reads that
// and uses it as a key rather than the server accepting it here.
func (a *Authenticator) IdentityURL(ctx context.Context, r *http.Request) (Identity, bool) {
	if id, ok := a.Identity(ctx, r); ok {
		return id, true
	}
	return a.resolve(ctx, r.URL.Query().Get(QueryParam))
}

func (a *Authenticator) resolve(ctx context.Context, key string) (Identity, bool) {
	if key == "" {
		return Identity{}, false
	}
	if a.compareAdmin(key) {
		return Identity{Role: RoleAdmin}, true
	}
	if a.users == nil {
		return Identity{}, false
	}
	name, ok := a.users.MatchKey(ctx, key)
	if !ok {
		return Identity{}, false
	}
	return Identity{Role: RoleUser, Name: name}, true
}

func (a *Authenticator) compareAdmin(candidate string) bool {
	if len(a.adminKey) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), a.adminKey) == 1
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

// ── the request context ─────────────────────────────────────────────────────

type identityKey struct{}

// WithIdentity attaches an identity to a context.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// FromContext returns the identity a request was authorised as.
//
// The zero value is an unauthenticated caller, which every caller treats as
// "may do nothing" — so a route that forgets to check gets no access rather
// than all of it.
func FromContext(ctx context.Context) Identity {
	if id, ok := ctx.Value(identityKey{}).(Identity); ok {
		return id
	}
	return Identity{}
}
