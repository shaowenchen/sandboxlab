package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const key = "s3cret-deployment-key"

func newAuth() *Authenticator {
	return New(key)
}

func request(header, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		r.Header.Set(header, value)
	}
	return r
}

func TestOKAcceptsTheKeyFromEitherHeader(t *testing.T) {
	a := newAuth()

	tests := []struct {
		name   string
		header string
		value  string
		want   bool
	}{
		{"the key header", APIKeyHeader, key, true},
		{"the key as a bearer", AuthorizationHeader, "Bearer " + key, true},
		{"bearer is case-insensitive", AuthorizationHeader, "bearer " + key, true},
		{"a wrong key", APIKeyHeader, "nope", false},
		{"a wrong key as a bearer", AuthorizationHeader, "Bearer nope", false},
		{"no key", "", "", false},
		// A bare key in Authorization is not a credential this accepts: the
		// header has a scheme, and honouring both spellings would mean two things
		// to get right where one is enough.
		{"a bare key in Authorization", AuthorizationHeader, key, false},
		// The key with trailing junk is not the key.
		{"the key with a suffix", APIKeyHeader, key + "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.OK(request(tc.header, tc.value)); got != tc.want {
				t.Errorf("OK() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnEmptyKeyRefusesEverything(t *testing.T) {
	// An authenticator with no key must reject everything. The alternative —
	// treating an empty configured key as "no auth needed" — would turn a
	// misconfiguration into an open API, which is the one failure here that
	// matters.
	a := New("")

	if a.OK(request(APIKeyHeader, "")) {
		t.Error("an empty configured key accepted a request with no credentials")
	}
	if a.OK(request(APIKeyHeader, "anything")) {
		t.Error("an empty configured key accepted a request")
	}
	if a.OK(request(AuthorizationHeader, "Bearer ")) {
		t.Error("an empty configured key accepted an empty bearer")
	}
}

func TestOKURLAcceptsTheQueryString(t *testing.T) {
	a := newAuth()

	t.Run("from the query string", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key="+key, nil)
		if !a.OKURL(r) {
			t.Error("OKURL refused the key in the query string")
		}
	})

	t.Run("still from a header", func(t *testing.T) {
		r := request(APIKeyHeader, key)
		if !a.OKURL(r) {
			t.Error("OKURL refused the key in a header")
		}
	})

	t.Run("a wrong key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key=nope", nil)
		if a.OKURL(r) {
			t.Error("OKURL accepted a wrong key")
		}
	})
}

func TestOKDoesNotAcceptTheQueryString(t *testing.T) {
	// The JSON API takes a header only, so a key cannot end up in a URL that gets
	// logged by a proxy or kept in a browser's history.
	a := newAuth()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes?key="+key, nil)
	if a.OK(r) {
		t.Error("OK accepted the key from the query string")
	}
	// And it is the query string alone that OKURL adds, not a second way in.
	if !a.OKURL(r) {
		t.Error("OKURL should accept the query string this test is about")
	}
}

// stubResolver is a KeyResolver over a fixed map.
type stubResolver struct {
	keys map[string]string
	err  error
	// asked records what was presented, so a test can assert that an unusable
	// key never reached the resolver.
	asked []string
}

func (s *stubResolver) ResolveSandboxKey(_ context.Context, presented string) (string, bool, error) {
	s.asked = append(s.asked, presented)
	if s.err != nil {
		return "", false, s.err
	}
	id, ok := s.keys[presented]
	return id, ok, nil
}

func TestResolveReportsWhoACallerIs(t *testing.T) {
	a := newAuth()
	resolver := &stubResolver{keys: map[string]string{"sandbox-key-for-alpha": "alpha"}}

	tests := []struct {
		name      string
		presented string
		want      Identity
		wantErr   error
	}{
		{
			name:      "the deployment's key is the admin tier",
			presented: key,
			want:      Identity{},
		},
		{
			name:      "a sandbox key is that sandbox",
			presented: "sandbox-key-for-alpha",
			want:      Identity{Sandbox: "alpha"},
		},
		{
			// Not the zero Identity: that is the admin tier, and returning it
			// here would hand an unauthenticated caller everything.
			name:      "nothing presented is nobody",
			presented: "",
			want:      Identity{Anonymous: true},
			wantErr:   ErrUnauthenticated,
		},
		{
			name:      "a key nobody knows is nobody",
			presented: "not-a-key",
			want:      Identity{Anonymous: true},
			wantErr:   ErrUnauthenticated,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.Resolve(context.Background(), tc.presented, resolver)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Resolve(%q) error = %v, want %v", tc.presented, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %+v, want %+v", tc.presented, got, tc.want)
			}
			if got.Admin() != (tc.want == Identity{}) {
				t.Errorf("Resolve(%q).Admin() = %v, want %v", tc.presented, got.Admin(), tc.want == Identity{})
			}
		})
	}
}

// An unusable key never reaches the resolver.
//
// Asking would be a cluster read for an unauthenticated caller to spend, and a
// whitespace-only key cannot be one this deployment issued.
func TestResolveDoesNotAskAboutAnEmptyKey(t *testing.T) {
	a := newAuth()
	resolver := &stubResolver{keys: map[string]string{}}

	for _, presented := range []string{"", "   ", "\n"} {
		if _, err := a.Resolve(context.Background(), presented, resolver); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Resolve(%-8q) error = %v, want ErrUnauthenticated", presented, err)
		}
	}
	if len(resolver.asked) != 0 {
		t.Errorf("the resolver was asked about %q, which is not a key", resolver.asked)
	}
}

// A resolver that could not answer is an error, not an unauthenticated caller.
//
// The API turns this into a 503: a key that could not be checked is not a key
// that failed to check out, and reporting it as 401 would tell an operator their
// credential is wrong when the cluster is simply unreachable.
func TestResolveReportsAResolverFailureAsAnError(t *testing.T) {
	a := newAuth()
	resolver := &stubResolver{err: errors.New("the cluster is unreachable")}

	got, err := a.Resolve(context.Background(), "some-sandbox-key", resolver)
	if err == nil {
		t.Fatal("a resolver failure was reported as success")
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a resolver failure was reported as an unauthenticated caller")
	}
	if got.Authenticated() {
		t.Error("a resolver failure produced an authenticated identity")
	}
}

// With no resolver there is only the admin tier, and everything else is nobody.
func TestResolveWithoutAResolver(t *testing.T) {
	a := newAuth()

	if got, err := a.Resolve(context.Background(), key, nil); err != nil || !got.Admin() {
		t.Errorf("the deployment's key without a resolver = (%+v, %v), want the admin", got, err)
	}
	if got, err := a.Resolve(context.Background(), "anything", nil); !errors.Is(err, ErrUnauthenticated) || got.Admin() {
		t.Errorf("an unknown key without a resolver = (%+v, %v), want nobody", got, err)
	}
}

// PresentedKey and PresentedKeyURL are the two ways a request may carry a key,
// and the difference between them is the query string.
//
// They are exported because the API layer decides which a route family may use:
// the data plane takes the URL form, because a browser navigation cannot set a
// header, and the JSON API takes the header form only.
func TestPresentedKeyIsAHeaderAndPresentedKeyURLAddsTheQueryString(t *testing.T) {
	t.Run("a header", func(t *testing.T) {
		r := request(APIKeyHeader, "from-a-header")
		if got := PresentedKey(r); got != "from-a-header" {
			t.Errorf("PresentedKey = %q, want the header's value", got)
		}
		if got := PresentedKeyURL(r); got != "from-a-header" {
			t.Errorf("PresentedKeyURL = %q, want the header's value", got)
		}
	})

	t.Run("a bearer", func(t *testing.T) {
		r := request(AuthorizationHeader, "Bearer from-a-bearer")
		if got := PresentedKey(r); got != "from-a-bearer" {
			t.Errorf("PresentedKey = %q, want the bearer's value", got)
		}
	})

	t.Run("the query string, which only the URL form sees", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key=from-a-url", nil)
		if got := PresentedKey(r); got != "" {
			t.Errorf("PresentedKey = %q; the query string is not a header", got)
		}
		if got := PresentedKeyURL(r); got != "from-a-url" {
			t.Errorf("PresentedKeyURL = %q, want the query string's value", got)
		}
	})

	t.Run("a header wins over the query string", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandbox/demo/api/?key=from-a-url", nil)
		r.Header.Set(APIKeyHeader, "from-a-header")
		if got := PresentedKeyURL(r); got != "from-a-header" {
			t.Errorf("PresentedKeyURL = %q, want the header to take precedence", got)
		}
	})
}
