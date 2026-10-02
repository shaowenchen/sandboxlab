package auth

import (
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
