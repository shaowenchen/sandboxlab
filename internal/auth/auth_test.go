package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	adminKey = "s3cret-admin"
	aliceKey = "alice-key-1234"
	bobKey   = "bob-key-5678"
)

// fakeUsers is a user list in memory: a name to a key.
type fakeUsers map[string]string

func (f fakeUsers) MatchKey(_ context.Context, key string) (string, bool) {
	for name, stored := range f {
		if stored == key {
			return name, true
		}
	}
	return "", false
}

func newAuth() *Authenticator {
	return New(adminKey, fakeUsers{"alice": aliceKey, "bob": bobKey})
}

func request(header, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

func TestIdentityResolvesEachKindOfCaller(t *testing.T) {
	a := newAuth()
	ctx := context.Background()

	tests := []struct {
		name   string
		header string
		value  string
		want   Identity
		ok     bool
	}{
		{"the administrator's key", APIKeyHeader, adminKey, Identity{Role: RoleAdmin}, true},
		{"the administrator's key as a bearer", AuthorizationHeader, "Bearer " + adminKey, Identity{Role: RoleAdmin}, true},
		{"a user's key", APIKeyHeader, aliceKey, Identity{Role: RoleUser, Name: "alice"}, true},
		{"another user's key", APIKeyHeader, bobKey, Identity{Role: RoleUser, Name: "bob"}, true},
		{"a wrong key", APIKeyHeader, "nope", Identity{}, false},
		{"no key", "", "", Identity{}, false},
		{"a bare key in Authorization", AuthorizationHeader, adminKey, Identity{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.Identity(ctx, request(tc.header, tc.value))
			if ok != tc.ok {
				t.Fatalf("Identity() ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("Identity() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNoUserStoreStillAuthenticatesTheAdministrator(t *testing.T) {
	// A deployment whose user store could not be built is still administrable;
	// it just has no users. Refusing everything instead would lock an operator
	// out of the console they need in order to see what went wrong.
	a := New(adminKey, nil)
	if _, ok := a.Identity(context.Background(), request(APIKeyHeader, adminKey)); !ok {
		t.Error("the administrator's key was refused with no user store")
	}
	if _, ok := a.Identity(context.Background(), request(APIKeyHeader, aliceKey)); ok {
		t.Error("a user's key authenticated with no user store")
	}
}

func TestAnEmptyAdminKeyRefusesEverything(t *testing.T) {
	// An authenticator with no key must reject everything. The alternative —
	// treating an empty configured key as "no auth needed" — would turn a
	// misconfiguration into an open API.
	a := New("", fakeUsers{"alice": aliceKey})
	ctx := context.Background()

	if _, ok := a.Identity(ctx, request(APIKeyHeader, "")); ok {
		t.Error("an empty key authenticated a request with no credentials")
	}
	// And a user whose key is somehow empty does not authenticate either.
	a2 := New(adminKey, fakeUsers{"carol": ""})
	if _, ok := a2.Identity(ctx, request(APIKeyHeader, "")); ok {
		t.Error("an empty user key authenticated")
	}
}

func TestIdentityURLAcceptsTheQueryString(t *testing.T) {
	a := newAuth()
	ctx := context.Background()

	t.Run("the administrator", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key="+adminKey, nil)
		id, ok := a.IdentityURL(ctx, r)
		if !ok || !id.IsAdmin() {
			t.Errorf("IdentityURL() = %+v, %v; want the administrator", id, ok)
		}
	})

	t.Run("a user", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key="+aliceKey, nil)
		id, ok := a.IdentityURL(ctx, r)
		if !ok || id.Name != "alice" {
			t.Errorf("IdentityURL() = %+v, %v; want alice", id, ok)
		}
	})

	t.Run("a wrong key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key=nope", nil)
		if _, ok := a.IdentityURL(ctx, r); ok {
			t.Error("IdentityURL accepted a wrong key")
		}
	})
}

func TestIdentityDoesNotAcceptTheQueryString(t *testing.T) {
	// The JSON API takes a header only, so a key cannot end up in a URL that
	// gets logged by a proxy or a browser's history.
	a := newAuth()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes?key="+adminKey, nil)
	if _, ok := a.Identity(context.Background(), r); ok {
		t.Error("Identity accepted the key from the query string")
	}
}

func TestOwns(t *testing.T) {
	admin := Identity{Role: RoleAdmin}
	alice := Identity{Role: RoleUser, Name: "alice"}
	bob := Identity{Role: RoleUser, Name: "bob"}
	unauthenticated := Identity{}

	tests := []struct {
		name  string
		who   Identity
		owner string
		want  bool
	}{
		{"an administrator owns everything", admin, "alice", true},
		{"and the deployment's own", admin, "", true},
		{"a user owns their own", alice, "alice", true},
		{"and not another's", alice, "bob", false},
		{"and not the deployment's", alice, "", false},
		{"bob does not own alice's", bob, "alice", false},
		// An unauthenticated caller reaches nothing — the zero Identity is what
		// a route that forgot to authenticate sees, and it must not be a way in.
		{"nobody owns anything without an identity", unauthenticated, "alice", false},
		{"including the deployment's own", unauthenticated, "", false},
		// A user with an empty name is malformed; it must not match the
		// deployment's own sandboxes, whose owner label is also empty.
		{"a nameless user matches nothing", Identity{Role: RoleUser}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.who.Owns(tc.owner); got != tc.want {
				t.Errorf("Owns(%q) = %v, want %v", tc.owner, got, tc.want)
			}
		})
	}
}

func TestIdentityContext(t *testing.T) {
	t.Run("round trips", func(t *testing.T) {
		want := Identity{Role: RoleUser, Name: "alice"}
		ctx := WithIdentity(context.Background(), want)
		if got := FromContext(ctx); got != want {
			t.Errorf("FromContext() = %+v, want %+v", got, want)
		}
	})

	t.Run("the zero value is nobody", func(t *testing.T) {
		// A handler that reads the identity without one in the context must get
		// the identity that can do nothing, not the one that can do everything.
		got := FromContext(context.Background())
		if got.Role != "" || got.IsAdmin() {
			t.Errorf("FromContext() on an empty context = %+v, want nobody", got)
		}
	})
}

func TestKeyIsReported(t *testing.T) {
	if got := newAuth().Key(); got != adminKey {
		t.Errorf("Key() = %q, want the administrator's key", got)
	}
}
