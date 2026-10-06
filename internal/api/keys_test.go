package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shaowenchen/sandboxlab/internal/config"
)

// The scoped tier, end to end through the router.
//
// These are the tests that make "a sandbox key reaches its own sandbox and
// nothing else" a property rather than a claim: each one names a route a
// sandbox key might plausibly be tried against and asserts the answer it must
// get. The distinction that matters throughout is 403 against 404 — a route
// class a sandbox key may not use at all is a refusal of the *key*, while
// another sandbox's id is answered as though it were not there, so the API does
// not become a directory of other people's sandboxes.

// twoSandboxes is a server with "alpha" and "beta" created, and both keys.
func twoSandboxes(t *testing.T) (*Server, *stubService, string, string) {
	t.Helper()
	cfg := config.Config{Namespace: "ops-system", SandboxNamespacePrefix: "sbx-", DataPlane: true}
	s, svc := newTestServerWithStub(t, cfg, &recordingDataPlane{})

	if w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"alpha"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating alpha = %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"beta"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating beta = %d: %s", w.Code, w.Body.String())
	}
	return s, svc, svc.stubKey("alpha"), svc.stubKey("beta")
}

// A sandbox key is refused on the routes that are not about one sandbox.
//
// All of these are 403 rather than 404: they are not addresses of another
// sandbox that does not exist, they are things this key is not for. Listing is
// the one that matters most — it would expose every other sandbox in the
// deployment — and the catalog is the one that would let a key change what can
// be created.
func TestASandboxKeyCannotReachTheDeploymentWideRoutes(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"listing every sandbox", http.MethodGet, "/api/v1/sandboxes", ""},
		{"creating a sandbox", http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-sandbox","name":"gamma"}`},
		{"the overview", http.MethodGet, "/api/v1/overview", ""},
		{"the catalog", http.MethodGet, "/api/v1/catalog", ""},
		{"one template", http.MethodGet, "/api/v1/catalog/agent-sandbox", ""},
		{"adding a template", http.MethodPost, "/api/v1/catalog", `{"document":"id: temp\nimage: x:1\n"}`},
		{"removing a template", http.MethodDelete, "/api/v1/catalog/agent-sandbox", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, tc.method, tc.path, alphaKey, tc.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s with a sandbox key = %d, want 403: %s", tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// A sandbox key is answered as though another sandbox did not exist.
//
// 404 rather than 403, so the answer is the same whether the id belongs to
// somebody else or to nobody — one sandbox key cannot use the API to find out
// which names exist.
func TestASandboxKeyCannotReachAnotherSandbox(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"reading it", http.MethodGet, "/api/v1/sandboxes/beta", ""},
		{"its logs", http.MethodGet, "/api/v1/sandboxes/beta/logs", ""},
		{"its usage", http.MethodGet, "/api/v1/sandboxes/beta/usage", ""},
		{"its events", http.MethodGet, "/api/v1/sandboxes/beta/events", ""},
		{"its key", http.MethodGet, "/api/v1/sandboxes/beta/key", ""},
		{"renewing it", http.MethodPost, "/api/v1/sandboxes/beta/renew", `{"ttl":"2h"}`},
		{"running a command in it", http.MethodPost, "/api/v1/sandboxes/beta/exec", `{"command":["ls"]}`},
		{"reading a file in it", http.MethodGet, "/api/v1/sandboxes/beta/files?path=/etc/passwd", ""},
		{"writing a file in it", http.MethodPut, "/api/v1/sandboxes/beta/files?path=/tmp/x", `{"content":"x"}`},
		{"deleting it", http.MethodDelete, "/api/v1/sandboxes/beta", ""},
		{"an id that does not exist", http.MethodGet, "/api/v1/sandboxes/nobody", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, tc.method, tc.path, alphaKey, tc.body)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s with alpha's key = %d, want 404: %s", tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// A sandbox key reaches its own sandbox, for everything that sandbox is.
func TestASandboxKeyReachesItsOwnSandbox(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"reading it", http.MethodGet, "/api/v1/sandboxes/alpha", ""},
		{"its logs", http.MethodGet, "/api/v1/sandboxes/alpha/logs", ""},
		{"its usage", http.MethodGet, "/api/v1/sandboxes/alpha/usage", ""},
		{"its events", http.MethodGet, "/api/v1/sandboxes/alpha/events", ""},
		{"its key", http.MethodGet, "/api/v1/sandboxes/alpha/key", ""},
		{"renewing it", http.MethodPost, "/api/v1/sandboxes/alpha/renew", `{"ttl":"2h"}`},
		{"running a command in it", http.MethodPost, "/api/v1/sandboxes/alpha/exec", `{"command":["ls"]}`},
		{"reading a file in it", http.MethodGet, "/api/v1/sandboxes/alpha/files?path=/etc/hosts", ""},
		{"writing a file in it", http.MethodPut, "/api/v1/sandboxes/alpha/files?path=/tmp/x", `{"content":"x"}`},
		{"deleting it", http.MethodDelete, "/api/v1/sandboxes/alpha", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, tc.method, tc.path, alphaKey, tc.body)
			if w.Code < 200 || w.Code > 299 {
				t.Errorf("%s %s with alpha's own key = %d, want 2xx: %s", tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// A sandbox key can reach its own sandbox's ports and no others'.
func TestASandboxKeyIsConfinedOnTheDataPlane(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	if w := do(t, s, http.MethodGet, "/sandbox/alpha/api/", alphaKey, ""); w.Code != http.StatusOK {
		t.Errorf("reaching alpha's own port = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, http.MethodGet, "/sandbox/beta/api/", alphaKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("reaching beta's port with alpha's key = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// The key in the query string works on the data plane and nowhere else.
//
// It has to work there — a browser navigation cannot set a header — and it must
// not work on the JSON API, whose callers can always set one and whose URLs end
// up in access logs, shell history and Referer headers.
func TestASandboxKeyInTheQueryStringIsOnlyForTheDataPlane(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	if w := do(t, s, http.MethodGet, "/sandbox/alpha/api/?key="+alphaKey, "", ""); w.Code != http.StatusOK {
		t.Errorf("the data plane refused the key in the query string = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha?key="+alphaKey, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the JSON API accepted the key in the query string = %d, want 401: %s", w.Code, w.Body.String())
	}
}

// A sandbox key cannot rotate a key, its own included.
//
// Rotating its own would let a sandbox lock out whoever is holding its key,
// which is a denial of service with no upside — so the route is admin-only, and
// the answer is 403 rather than 404 because it is the key being refused rather
// than an id that is not there.
func TestASandboxKeyCannotRotate(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	for _, path := range []string{"/api/v1/sandboxes/alpha/key/rotate", "/api/v1/sandboxes/beta/key/rotate"} {
		if w := do(t, s, http.MethodPost, path, alphaKey, ""); w.Code != http.StatusForbidden {
			t.Errorf("POST %s with a sandbox key = %d, want 403: %s", path, w.Code, w.Body.String())
		}
	}
}

// A sandbox key never sees another sandbox's key, and its own it may read.
func TestASandboxKeySeesOnlyItsOwnKey(t *testing.T) {
	s, _, alphaKey, betaKey := twoSandboxes(t)

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha/key", alphaKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reading alpha's own key = %d: %s", w.Code, w.Body.String())
	}
	var own sandboxKeyResponse
	decode(t, w, &own)
	if own.Key != alphaKey || own.Sandbox != "alpha" {
		t.Errorf("alpha's key response = %+v, want alpha's key", own)
	}
	if own.Key == betaKey {
		t.Error("alpha was handed beta's key")
	}

	// And beta's id answers beta as a missing sandbox, so alpha cannot fetch it
	// by asking for it directly — the same 404 as for an id nobody owns.
	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes/beta/key", alphaKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("alpha reading beta's key = %d, want 404", w.Code)
	}
}

// A sandbox key in a listing is blanked rather than omitted per-entry, so a
// caller cannot tell from the shape which sandbox it is looking at.
//
// The listing is admin-only, so the only way to see one as a non-admin is to be
// the admin; this asserts the redaction directly through the helper, which is
// what every read path goes through.
func TestTheKeyIsRedactedForAnIdentityThatMayNotSeeIt(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	sandboxes := []struct {
		name string
		path string
		key  string
	}{
		{"alpha reading alpha", "/api/v1/sandboxes/alpha", alphaKey},
		{"the admin reading alpha", "/api/v1/sandboxes/alpha", testKey},
	}
	for _, tc := range sandboxes {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, http.MethodGet, tc.path, tc.key, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", tc.path, w.Code, w.Body.String())
			}
			var sb struct {
				Key string `json:"key"`
			}
			decode(t, w, &sb)
			if tc.key == testKey && sb.Key == "" {
				t.Error("the admin was not shown the sandbox's key")
			}
			if tc.key == alphaKey && sb.Key == "" {
				t.Error("a sandbox was not shown its own key")
			}
		})
	}
}

// The create response carries the key, so a caller has it without a second call.
func TestCreateReturnsTheSandboxKey(t *testing.T) {
	cfg := config.Config{Namespace: "ops-system", SandboxNamespacePrefix: "sbx-"}
	s, _ := newTestServerWithStub(t, cfg, nil)

	w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"alpha"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a sandbox = %d: %s", w.Code, w.Body.String())
	}
	var sb struct {
		Key string `json:"key"`
	}
	decode(t, w, &sb)
	if sb.Key == "" {
		t.Error("the create response carried no key")
	}

	// And it is the real one: it authenticates as that sandbox.
	if got := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha", sb.Key, ""); got.Code != http.StatusOK {
		t.Errorf("the key from the create response was refused = %d: %s", got.Code, got.Body.String())
	}
}

// A key the resolver could not check is a 503, not a 401.
//
// The distinction is the point: a cluster outage must not tell an operator their
// credential is wrong, and a 401 would send them to rotate a key that is fine.
func TestAResolverFailureIsUnavailableRatherThanUnauthorized(t *testing.T) {
	cfg := config.Config{Namespace: "ops-system", SandboxNamespacePrefix: "sbx-"}
	s, svc := newTestServerWithStub(t, cfg, nil)
	// A sandbox to ask about, created while the resolver still works.
	if w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"alpha"}`); w.Code != http.StatusCreated {
		t.Fatalf("creating alpha = %d: %s", w.Code, w.Body.String())
	}

	svc.resolveErr = errors.New("the cluster is unreachable")

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha", "some-sandbox-key", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("an unresolvable key = %d, want 503: %s", w.Code, w.Body.String())
	}

	// The admin key does not go through the resolver at all — it is a value in
	// memory — so it is unaffected by the cluster being down. That is what lets
	// an operator still reach the API to find out what is wrong, rather than
	// being locked out by the same outage they are trying to diagnose.
	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes", testKey, ""); w.Code != http.StatusOK {
		t.Errorf("the admin key was refused while the resolver was down = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// A key in the response is JSON, and the field is the one the CLI and the
// console will read.
func TestTheKeyResponseShape(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha/key", testKey, "")
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("the key response is not JSON: %v", err)
	}
	for _, field := range []string{"sandbox", "key"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("the key response has no %q field: %s", field, w.Body.String())
		}
	}

	// And rotation returns the same shape with a different key, which is what
	// makes the two interchangeable for a client.
	rot := do(t, s, http.MethodPost, "/api/v1/sandboxes/alpha/key/rotate", testKey, "")
	if rot.Code != http.StatusOK {
		t.Fatalf("rotating = %d: %s", rot.Code, rot.Body.String())
	}
	var after sandboxKeyResponse
	decode(t, rot, &after)
	if after.Key == alphaKey {
		t.Error("a rotation returned the same key")
	}
	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha", alphaKey, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("the rotated-away key still works = %d", w.Code)
	}
}

// A listing reports every sandbox's key to the admin, and blanks them for
// anyone else.
//
// The admin case is what the console depends on: it draws an Open link and a
// copyable brief per row, and both should carry the *sandbox's* key rather than
// the session's, so a link handed on does not hand on the deployment's key with
// it. The non-admin case is the other half — there is no route a sandbox key
// could reach that returns a listing today, but the redaction is what makes that
// a property of the code rather than of which routes happen to exist.
func TestAListingReportsOnlyTheKeysTheCallerMaySee(t *testing.T) {
	s, _, alphaKey, betaKey := twoSandboxes(t)
	want := map[string]string{"alpha": alphaKey, "beta": betaKey}

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("listing = %d: %s", w.Code, w.Body.String())
	}
	var listed struct {
		Sandboxes []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"sandboxes"`
	}
	decode(t, w, &listed)
	if len(listed.Sandboxes) != 2 {
		t.Fatalf("the listing has %d sandboxes, want 2", len(listed.Sandboxes))
	}
	for _, sb := range listed.Sandboxes {
		if sb.Key == "" {
			t.Errorf("the listing reported no key for %s; the console's Open link and brief fall back to the deployment's key without one", sb.ID)
			continue
		}
		if sb.Key != want[sb.ID] {
			t.Errorf("the listing reported key %q for %s, want %q", sb.Key, sb.ID, want[sb.ID])
		}
	}
}

// A read of one sandbox carries its key, which is what `sandbox get` prints.
func TestReadingASandboxCarriesItsKey(t *testing.T) {
	s, _, alphaKey, _ := twoSandboxes(t)

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/alpha", testKey, "")
	var sb struct {
		Key string `json:"key"`
	}
	decode(t, w, &sb)
	if sb.Key != alphaKey {
		t.Errorf("reading alpha reported key %q, want %q", sb.Key, alphaKey)
	}
}

// A handler reached without going through ServeHTTP is nobody, not the admin.
//
// The zero Identity is the admin tier, so getting this wrong would hand an
// unauthenticated caller everything — which is exactly the bug the gate exists
// to make impossible.
func TestARequestWithoutAnIdentityIsNobody(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/alpha", nil)
	a := authOf(r)
	if a.identity.Admin() {
		t.Error("a request with no resolved identity reads as the admin tier")
	}
	if a.identity.Authenticated() {
		t.Error("a request with no resolved identity reads as authenticated")
	}
}
