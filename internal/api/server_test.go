package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/console"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

const testKey = "test-key"

// newTestServer builds a server over the built-in catalog and no data plane, so
// every route can be exercised without a cluster.
func newTestServer(t *testing.T, cfg config.Config, dp DataPlane) *Server {
	t.Helper()
	if cfg.APIKey == "" {
		cfg.APIKey = testKey
	}
	// Supplying a proxy means the test wants the data plane on; the routes are
	// registered from the configuration, so the two have to agree.
	if dp != nil {
		cfg.DataPlane = true
	}
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	svc := newStubService(cfg, c)
	return New(Deps{
		Config:    cfg,
		Service:   svc,
		Auth:      auth.New(cfg.APIKey),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataPlane: dp,
	})
}

// newTestServerWithStub is newTestServer for the tests that need to reach into
// the service — to set up state or to check what it was asked. A nil proxy
// leaves the data plane off.
func newTestServerWithStub(t *testing.T, cfg config.Config, dp DataPlane) (*Server, *stubService) {
	t.Helper()
	if cfg.APIKey == "" {
		cfg.APIKey = testKey
	}
	if dp != nil {
		cfg.DataPlane = true
	}
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	svc := newStubService(cfg, c)
	return New(Deps{
		Config:    cfg,
		Service:   svc,
		Auth:      auth.New(cfg.APIKey),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataPlane: dp,
	}), svc
}

// do issues a request, with the key unless it is the empty string.
func do(t *testing.T, s *Server, method, path, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("X-Sandbox-Key", key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// decode unmarshals a response body, failing the test if it is not JSON.
func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
}

// ── unauthenticated routes ──────────────────────────────────────────────────

func TestHealthAndReadyNeedNoKey(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)

	if w := do(t, s, http.MethodGet, "/healthz", "", ""); w.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", w.Code)
	}
	// The stub cluster is reachable, so ready is 200 — the point here is that
	// a kubelet probing these has no key to give.
	if w := do(t, s, http.MethodGet, "/readyz", "", ""); w.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", w.Code)
	}
}

func TestReadyReportsAnUnreachableCluster(t *testing.T) {
	cfg := config.Config{}
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	svc := newStubService(cfg, c)
	svc.clusterUp = false
	s := New(Deps{
		Config:  cfg,
		Service: svc,
		Auth:    auth.New(testKey),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	w := do(t, s, http.MethodGet, "/readyz", "", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with no cluster = %d, want 503", w.Code)
	}
}

func TestDescribeNeedsNoKey(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	w := do(t, s, http.MethodGet, "/api/v1/describe", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/describe = %d, want 200 without a key", w.Code)
	}
	var body struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"endpoints"`
	}
	decode(t, w, &body)
	if body.Name == "" || body.Version == "" {
		t.Error("describe returned an empty name or version")
	}
	if len(body.Endpoints) == 0 {
		t.Error("describe lists no endpoints")
	}
}

// ── authentication ──────────────────────────────────────────────────────────

func TestAuthIsRequired(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	paths := []string{
		"/api/v1/catalog",
		"/api/v1/overview",
		"/api/v1/sandboxes",
		"/api/v1/sandboxes/one",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			for _, key := range []string{"", "wrong"} {
				w := do(t, s, http.MethodGet, p, key, "")
				if w.Code != http.StatusUnauthorized {
					t.Errorf("GET %s with key %q = %d, want 401", p, key, w.Code)
				}
			}
		})
	}
}

func TestConfigNeedsNoKey(t *testing.T) {
	// The console reads this before anyone has typed a key, to say what the
	// deployment is serving. It reports no sandbox and no credential, so the
	// read is safe to leave open — and it is the same courtesy describe extends.
	s := newTestServer(t, config.Config{}, nil)
	if w := do(t, s, http.MethodGet, "/api/v1/config", "", ""); w.Code != http.StatusOK {
		t.Errorf("GET /api/v1/config with no key = %d, want 200", w.Code)
	}
}

func TestAuthAcceptsBothHeaders(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Authorization: Bearer = %d, want 200", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/api/v1/catalog", nil)
	r.Header.Set("Authorization", "bearer "+testKey) // the scheme is case-insensitive
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("lowercase bearer = %d, want 200", w.Code)
	}
}

// ── catalog ─────────────────────────────────────────────────────────────────

func TestListCatalog(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	w := do(t, s, http.MethodGet, "/api/v1/catalog", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/catalog = %d, want 200", w.Code)
	}
	var body struct {
		Templates []model.Template `json:"templates"`
	}
	decode(t, w, &body)
	if len(body.Templates) == 0 {
		t.Fatal("the catalog is empty")
	}
	// Sorted by id, so the console does not reshuffle between refreshes.
	for i := 1; i < len(body.Templates); i++ {
		if body.Templates[i-1].ID > body.Templates[i].ID {
			t.Errorf("catalog is not sorted: %s before %s", body.Templates[i-1].ID, body.Templates[i].ID)
		}
	}
}

func TestGetCatalogEntry(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)

	w := do(t, s, http.MethodGet, "/api/v1/catalog/agent-sandbox", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/catalog/agent-sandbox = %d, want 200", w.Code)
	}
	var tmpl model.Template
	decode(t, w, &tmpl)
	if tmpl.ID != "agent-sandbox" {
		t.Errorf("id = %q, want agent-sandbox", tmpl.ID)
	}

	if w := do(t, s, http.MethodGet, "/api/v1/catalog/nope", testKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("GET an unknown template = %d, want 404", w.Code)
	}
}

// The catalog is writable at runtime, through the same POST/PUT/DELETE the
// console and the CLI use. These are the handlers, not the model: they decode a
// document, reject one that does not fit, and answer with the right status.
func TestAddCatalogEntry(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)

	// A new id is a 201.
	body := `{"document":"id: tool\nimage: registry.example.com/tool:1\nports:\n  - name: web\n    port: 8080\n"}`
	w := do(t, s, http.MethodPost, "/api/v1/catalog", testKey, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST a new template = %d, want 201: %s", w.Code, w.Body.String())
	}
	var added model.Template
	decode(t, w, &added)
	if added.ID != "tool" || added.Image != "registry.example.com/tool:1" {
		t.Errorf("added = %+v, want id tool", added)
	}
	// And it is now in the catalog, which is what makes it creatable.
	if got, ok := s.svc.Catalog().Get("tool"); !ok || got.Image != "registry.example.com/tool:1" {
		t.Errorf("the added template is not in the catalog: %+v", got)
	}

	// The same id again is a conflict, so a mistyped create cannot clobber a
	// template someone else added.
	conflict := `{"document":"id: tool\nimage: registry.example.com/tool:2\n"}`
	if w := do(t, s, http.MethodPost, "/api/v1/catalog", testKey, conflict); w.Code != http.StatusConflict {
		t.Fatalf("POST an existing template = %d, want 409: %s", w.Code, w.Body.String())
	}
	if got, _ := s.svc.Catalog().Get("tool"); got.Image != "registry.example.com/tool:1" {
		t.Errorf("a conflicted POST changed the template: %q", got.Image)
	}

	// Unless the caller says overwrite, in which case it replaces and is a 200.
	overwrite := `{"overwrite":true,"document":"id: tool\nimage: registry.example.com/tool:3\n"}`
	if w := do(t, s, http.MethodPost, "/api/v1/catalog", testKey, overwrite); w.Code != http.StatusOK {
		t.Fatalf("POST with overwrite = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got, _ := s.svc.Catalog().Get("tool"); got.Image != "registry.example.com/tool:3" {
		t.Errorf("the overwritten template image = %q, want ...tool:3", got.Image)
	}

	// A document that is not a usable template is a 400, and leaves the catalog
	// as it was.
	for _, tc := range []struct{ name, body string }{
		{"no image", `{"document":"id: broken\n"}`},
		{"a misspelled field", `{"document":"id: broken\nimage: x\nttlDefualt: 30m\n"}`},
		{"an id that is not usable", `{"document":"id: Not An Id\nimage: x\n"}`},
		{"not a document at all", `{"document":"::: not yaml :::"}`},
		{"no document", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, s, http.MethodPost, "/api/v1/catalog", testKey, tc.body); w.Code != http.StatusBadRequest {
				t.Errorf("POST %s = %d, want 400: %s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

func TestDeleteCatalogEntry(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)

	if w := do(t, s, http.MethodDelete, "/api/v1/catalog/agent-infra", testKey, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE a template = %d, want 200: %s", w.Code, w.Body.String())
	}
	if _, ok := s.svc.Catalog().Get("agent-infra"); ok {
		t.Error("the template is still in the catalog after a delete")
	}
	if w := do(t, s, http.MethodDelete, "/api/v1/catalog/agent-infra", testKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE a template twice = %d, want 404", w.Code)
	}
}

// ── sandboxes ───────────────────────────────────────────────────────────────

func TestCreateSandbox(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"my-box"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/sandboxes = %d, want 201: %s", w.Code, w.Body.String())
	}
	var sb model.Sandbox
	decode(t, w, &sb)
	if sb.ID != "my-box" {
		t.Errorf("id = %q, want my-box", sb.ID)
	}
	if sb.Template != "agent-sandbox" {
		t.Errorf("template = %q, want agent-sandbox", sb.Template)
	}
}

func TestCreateSandboxRejects(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		// existing is a sandbox to put in place before the request, for the
		// cases that are about something already being there.
		existing string
	}{
		{name: "no such template", body: `{"template":"nope"}`, status: http.StatusBadRequest},
		{name: "no template at all", body: `{}`, status: http.StatusBadRequest},
		{name: "a name with nothing usable in it", body: `{"template":"agent-sandbox","name":"!!!"}`, status: http.StatusBadRequest},
		{name: "a bad ttl", body: `{"template":"agent-sandbox","ttl":"soon"}`, status: http.StatusBadRequest},
		{name: "a negative ttl", body: `{"template":"agent-sandbox","ttl":"-1h"}`, status: http.StatusBadRequest},
		{name: "an unknown field", body: `{"template":"agent-sandbox","colour":"blue"}`, status: http.StatusBadRequest},
		{name: "a name that is taken", body: `{"template":"agent-sandbox","name":"taken"}`, status: http.StatusConflict, existing: "taken"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, svc := newTestServerWithStub(t, config.Config{}, nil)
			if tc.existing != "" {
				svc.boxes[tc.existing] = model.Sandbox{ID: tc.existing, Template: "agent-sandbox"}
			}
			w := do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, tc.body)
			if w.Code != tc.status {
				t.Errorf("POST returned %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			// Every error carries a message: a status alone would leave the
			// caller with nothing to act on.
			var body errorBody
			decode(t, w, &body)
			if body.Error == "" {
				t.Error("the error response has no message")
			}
		})
	}
}

func TestListSandboxes(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one"}`)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-infra","name":"two"}`)

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sandboxes = %d, want 200", w.Code)
	}
	var body struct {
		Sandboxes []model.Sandbox `json:"sandboxes"`
		Count     int             `json:"count"`
	}
	decode(t, w, &body)
	if body.Count != 2 || len(body.Sandboxes) != 2 {
		t.Errorf("got %d sandboxes (count %d), want 2", len(body.Sandboxes), body.Count)
	}
}

func TestGetAndDeleteSandbox(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one"}`)

	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes/one", testKey, ""); w.Code != http.StatusOK {
		t.Errorf("GET /api/v1/sandboxes/one = %d, want 200", w.Code)
	}
	if w := do(t, s, http.MethodDelete, "/api/v1/sandboxes/one", testKey, ""); w.Code != http.StatusOK {
		t.Errorf("DELETE /api/v1/sandboxes/one = %d, want 200", w.Code)
	}
	if w := do(t, s, http.MethodGet, "/api/v1/sandboxes/one", testKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("GET a deleted sandbox = %d, want 404", w.Code)
	}
	if w := do(t, s, http.MethodDelete, "/api/v1/sandboxes/one", testKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE a deleted sandbox = %d, want 404", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	w := do(t, s, http.MethodPut, "/api/v1/sandboxes", testKey, `{}`)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/v1/sandboxes = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow == "" {
		t.Error("a 405 did not carry an Allow header")
	}
}

func TestRenewSandbox(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one","ttl":"30m"}`)

	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/one/renew", testKey, `{"ttl":"2h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("renew = %d, want 200: %s", w.Code, w.Body.String())
	}
	var sb model.Sandbox
	decode(t, w, &sb)
	if sb.ExpiresAt.IsZero() {
		t.Error("renewing did not give the sandbox an expiry")
	}

	if w := do(t, s, http.MethodPost, "/api/v1/sandboxes/nope/renew", testKey, `{"ttl":"1h"}`); w.Code != http.StatusNotFound {
		t.Errorf("renewing a sandbox that does not exist = %d, want 404", w.Code)
	}
}

func TestRenewWithoutATTLRemovesTheExpiry(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one","ttl":"30m"}`)

	// An empty ttl is a real request: a sandbox someone is working in should be
	// able to be made to stop disappearing.
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/one/renew", testKey, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("renew with no ttl = %d, want 200: %s", w.Code, w.Body.String())
	}
	var sb model.Sandbox
	decode(t, w, &sb)
	if !sb.ExpiresAt.IsZero() {
		t.Errorf("expiresAt = %v after renewing with no ttl, want no expiry", sb.ExpiresAt)
	}
}

func TestGetSandboxLogs(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one"}`)

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/one/logs?tail=50", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("logs = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Logs string `json:"logs"`
	}
	decode(t, w, &body)
}

func TestOverview(t *testing.T) {
	s := newTestServer(t, config.Config{}, nil)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"one"}`)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-sandbox","name":"two"}`)
	do(t, s, http.MethodPost, "/api/v1/sandboxes", testKey, `{"template":"agent-infra","name":"three"}`)

	w := do(t, s, http.MethodGet, "/api/v1/overview", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/overview = %d, want 200", w.Code)
	}
	var body struct {
		Total      int            `json:"total"`
		ByTemplate map[string]int `json:"byTemplate"`
		Cluster    bool           `json:"cluster"`
	}
	decode(t, w, &body)
	if body.Total != 3 {
		t.Errorf("total = %d, want 3", body.Total)
	}
	if body.ByTemplate["agent-sandbox"] != 2 || body.ByTemplate["agent-infra"] != 1 {
		t.Errorf("byTemplate = %v, want agent-sandbox 2 and agent-infra 1", body.ByTemplate)
	}
	if !body.Cluster {
		t.Error("cluster reported as down when it is reachable")
	}
}

// ── config ──────────────────────────────────────────────────────────────────

func TestGetConfig(t *testing.T) {
	s := newTestServer(t, config.Config{
		PublicURL:  "https://sandbox.example.com",
		BasePath:   "/sandbox",
		DefaultTTL: time.Hour,
		MaxTTL:     4 * time.Hour,
	}, nil)

	w := do(t, s, http.MethodGet, "/sandbox/api/v1/config", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("config under a base path = %d, want 200", w.Code)
	}
	var body map[string]any
	decode(t, w, &body)
	if body["basePath"] != "/sandbox" {
		t.Errorf("basePath = %v, want /sandbox", body["basePath"])
	}
	// The base path is part of every address this deployment hands out, so a
	// client cannot assume it is at the root.
	if body["apiVersion"] != APIVersion {
		t.Errorf("apiVersion = %v, want %v", body["apiVersion"], APIVersion)
	}
}

// ── the base path ───────────────────────────────────────────────────────────

func TestBasePath(t *testing.T) {
	s := newTestServer(t, config.Config{BasePath: "/sandbox"}, nil)

	t.Run("served under the base path", func(t *testing.T) {
		if w := do(t, s, http.MethodGet, "/sandbox/api/v1/catalog", testKey, ""); w.Code != http.StatusOK {
			t.Errorf("GET under the base path = %d, want 200", w.Code)
		}
	})

	t.Run("not served outside it", func(t *testing.T) {
		// A request to the root of the host is not this deployment's, and
		// answering it would mean two addresses for one service.
		if w := do(t, s, http.MethodGet, "/api/v1/catalog", testKey, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET outside the base path = %d, want 404", w.Code)
		}
	})

	t.Run("health is under it too", func(t *testing.T) {
		// The probe is configured by the chart, which knows the base path, so
		// one rule for every route is simpler than an exception for two.
		if w := do(t, s, http.MethodGet, "/sandbox/healthz", "", ""); w.Code != http.StatusOK {
			t.Errorf("GET /sandbox/healthz = %d, want 200", w.Code)
		}
	})

	t.Run("the bare base path reaches the console", func(t *testing.T) {
		if w := do(t, s, http.MethodGet, "/sandbox", testKey, ""); w.Code != http.StatusOK {
			t.Errorf("GET /sandbox = %d, want 200", w.Code)
		}
	})
}

// The console's document is served without a key, and it is the only thing that
// is.
//
// It has to be: the sign-in form lives in that document, so serving it only to
// an already-authenticated caller means a browser — which cannot put a key on a
// navigation — is refused before it can render the form that would ask for one.
// The page carries nothing; every call it makes is authenticated as before,
// which is what the rest of this test checks.
func TestTheConsoleDocumentNeedsNoKey(t *testing.T) {
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	cfg := config.Config{Namespace: "ops-system", BasePath: "/sandbox", APIKey: testKey}
	con, err := console.New()
	if err != nil {
		t.Fatalf("building the console: %v", err)
	}
	s := New(Deps{
		Config:  cfg,
		Service: newStubService(cfg, c),
		Auth:    auth.New(cfg.APIKey),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Console: con,
	})

	// Without a key, and with a wrong one: the document is still served. A
	// browser has no way to send a key on a navigation, so "no key" is the
	// normal case here rather than an error.
	for _, key := range []string{"", "not-the-key"} {
		w := do(t, s, http.MethodGet, "/sandbox/", key, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET /sandbox/ with key %q = %d, want 200", key, w.Code)
		}
		if !strings.Contains(w.Body.String(), "<form") {
			t.Errorf("GET /sandbox/ with key %q did not serve the page", key)
		}
	}

	// What the document gives away is what it must: the API behind it refuses
	// every one of its calls without a key. This is the assertion that makes the
	// document being open uninteresting — it lists nothing on its own.
	for _, path := range []string{"/sandbox/api/v1/sandboxes", "/sandbox/api/v1/overview", "/sandbox/api/v1/catalog"} {
		if w := do(t, s, http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no key = %d, want 401", path, w.Code)
		}
	}
	// A wrong key is refused on the calls the page makes, and the right one is
	// not — which is the whole of what signing in decides now that there is no
	// role to report.
	if w := do(t, s, http.MethodGet, "/sandbox/api/v1/sandboxes", "not-the-key", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("sandboxes with a wrong key = %d, want 401", w.Code)
	}
	if w := do(t, s, http.MethodGet, "/sandbox/api/v1/sandboxes", testKey, ""); w.Code != http.StatusOK {
		t.Errorf("sandboxes with the real key = %d, want 200", w.Code)
	}
}

// An /api/ path this server does not serve is a 404, not the console's page.
//
// The console is the fallback for every path in the deployment, so without a
// guard a removed route — /api/v1/users, most of all, since removing it is the
// point — answers 200 with HTML. A client generated from the old spec reads
// that as success and gets a page where it expected a user.
func TestAnUnknownAPIPathIsNotFound(t *testing.T) {
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	con, err := console.New()
	if err != nil {
		t.Fatalf("building the console: %v", err)
	}
	cfg := config.Config{Namespace: "ops-system", BasePath: "/sandbox", APIKey: testKey}
	s := New(Deps{
		Config:  cfg,
		Service: newStubService(cfg, c),
		Auth:    auth.New(cfg.APIKey),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Console: con,
	})

	// The paths the removal took out, and one that never existed.
	for _, path := range []string{
		"/sandbox/api/v1/users",
		"/sandbox/api/v1/users/alice",
		"/sandbox/api/v1/whoami",
		"/sandbox/api/v1/nope",
	} {
		t.Run(path, func(t *testing.T) {
			w := do(t, s, http.MethodGet, path, testKey, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, w.Code)
			}
			if strings.Contains(w.Body.String(), "<form") {
				t.Errorf("GET %s served the console instead of a 404", path)
			}
		})
	}

	// And the console's own client-side routes still resolve, which is what the
	// fallback is for.
	if w := do(t, s, http.MethodGet, "/sandbox/", "", ""); w.Code != http.StatusOK {
		t.Errorf("GET /sandbox/ = %d, want 200", w.Code)
	}
}

func TestTrimBasePath(t *testing.T) {
	tests := []struct {
		path string
		base string
		want string
		ok   bool
	}{
		{"/sandbox", "/sandbox", "/", true},
		{"/sandbox/", "/sandbox", "/", true},
		{"/sandbox/api/v1", "/sandbox", "/api/v1", true},
		{"/sandboxing/api", "/sandbox", "", false},
		{"/api/v1", "/sandbox", "", false},
		{"/", "/sandbox", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := trimBasePath(tc.path, tc.base)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("trimBasePath(%q, %q) = %q, %v; want %q, %v", tc.path, tc.base, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// ── the data plane ──────────────────────────────────────────────────────────

// recordingDataPlane records what it was asked to proxy.
type recordingDataPlane struct {
	calls []string
}

func (r *recordingDataPlane) Serve(w http.ResponseWriter, req *http.Request, id, port, rest string) {
	r.calls = append(r.calls, id+"/"+port+"/"+rest)
	w.WriteHeader(http.StatusOK)
}

func TestDataPlaneRouting(t *testing.T) {
	dp := &recordingDataPlane{}
	s, svc := newTestServerWithStub(t, config.Config{}, dp)
	// The data plane checks ownership against the sandbox before it forwards,
	// so the sandbox has to be there — a route that forwarded first would be
	// one a user could reach another user's port through.
	if _, err := svc.Create(context.Background(), sandbox.CreateInput{
		Template: "agent-infra",
		Name:     "demo",
	}); err != nil {
		t.Fatalf("creating the sandbox under test: %v", err)
	}

	t.Run("a request reaches the proxy", func(t *testing.T) {
		w := do(t, s, http.MethodGet, "/sandbox/demo/api/v1/health", testKey, "")
		if w.Code != http.StatusOK {
			t.Fatalf("proxied request = %d, want 200", w.Code)
		}
		if len(dp.calls) != 1 || dp.calls[0] != "demo/api/v1/health" {
			t.Errorf("the proxy was called with %v, want demo/api/v1/health", dp.calls)
		}
	})

	t.Run("the key may arrive in the query string", func(t *testing.T) {
		// A browser navigation cannot set a header, which is the whole reason
		// this route accepts one.
		dp.calls = nil
		if w := do(t, s, http.MethodGet, "/sandbox/demo/api/?key="+testKey, "", ""); w.Code != http.StatusOK {
			t.Errorf("a request with the key in the query string = %d, want 200", w.Code)
		}
	})

	t.Run("the key is required", func(t *testing.T) {
		dp.calls = nil
		if w := do(t, s, http.MethodGet, "/sandbox/demo/api/", "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("a request with no key = %d, want 401", w.Code)
		}
		if len(dp.calls) != 0 {
			t.Error("an unauthenticated request reached the proxy")
		}
	})

	t.Run("a malformed address is refused", func(t *testing.T) {
		dp.calls = nil
		for _, path := range []string{"/sandbox/", "/sandbox/demo", "/sandbox/demo/"} {
			if w := do(t, s, http.MethodGet, path, testKey, ""); w.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", path, w.Code)
			}
		}
		if len(dp.calls) != 0 {
			t.Error("a malformed address reached the proxy")
		}
	})

	t.Run("the key does not reach the sandbox", func(t *testing.T) {
		// The sandbox's own application has no business seeing the control
		// plane's credential.
		dp.calls = nil
		var sawQuery string
		inner := &queryCapturing{on: func(q string) { sawQuery = q }}
		innerServer, innerSvc := newTestServerWithStub(t, config.Config{}, inner)
		if _, err := innerSvc.Create(context.Background(), sandbox.CreateInput{
			Template: "agent-infra",
			Name:     "demo",
		}); err != nil {
			t.Fatalf("creating the sandbox under test: %v", err)
		}
		do(t, innerServer, http.MethodGet, "/sandbox/demo/api/x?key="+testKey+"&other=1", "", "")
		if strings.Contains(sawQuery, testKey) {
			t.Errorf("the API key was forwarded to the sandbox: %q", sawQuery)
		}
		if !strings.Contains(sawQuery, "other=1") {
			t.Errorf("a query parameter was dropped: %q", sawQuery)
		}
	})
}

type queryCapturing struct {
	on func(string)
}

func (q *queryCapturing) Serve(w http.ResponseWriter, r *http.Request, id, port, rest string) {
	q.on(r.URL.RawQuery)
	w.WriteHeader(http.StatusOK)
}

func TestDataPlaneIsOffWhenDisabled(t *testing.T) {
	cfg := config.Config{APIKey: testKey, DataPlane: false}
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	dp := &recordingDataPlane{}
	// The handler is passed but the configuration turns it off, so the route is
	// not registered at all rather than registered and refusing.
	svc := newStubService(cfg, c)
	s := New(Deps{
		Config:    cfg,
		Service:   svc,
		Auth:      auth.New(testKey),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataPlane: dp,
	})
	w := do(t, s, http.MethodGet, "/sandbox/demo/api/", testKey, "")
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusNotFound {
		t.Errorf("GET a data-plane route with the plane off = %d, want 404 or 401", w.Code)
	}
	if len(dp.calls) != 0 {
		t.Error("the proxy was reached with the data plane turned off")
	}
}
