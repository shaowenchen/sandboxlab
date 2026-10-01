package api_test

// These tests wire the whole control plane together — the HTTP layer, the
// service, and the cluster client over a fake cluster — instead of stubbing the
// service.
//
// The point is the seams. The api package's own tests use a stub, which proves
// the routing; the proxy test uses the real service, but only for its own
// routes. Nothing until here would notice the two halves disagreeing — a create
// body whose field names the service does not read, a state the API reports
// differently from the one the client sets — because each side would be
// internally consistent and wrong.

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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shaowenchen/sandboxlab/internal/api"
	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
	"github.com/shaowenchen/sandboxlab/internal/userservice"
)

const (
	integrationKey = "integration-key"
	// The base path is set here on purpose: every address the API reports, and
	// every route it serves, has to carry it, and a test at the root would not
	// notice if one of them did not.
	integrationBase = "/sandbox"
)

// newIntegrationServer builds the real stack over a fake cluster that settles
// deployments the way the controller would.
func newIntegrationServer(t *testing.T) (*api.Server, *k8s.Client) {
	t.Helper()
	cfg := config.Config{
		Namespace:              "ops-system",
		SandboxNamespacePrefix: "sbx-",
		PublicURL:              "https://sandbox.example.com",
		BasePath:               integrationBase,
		APIKey:                 integrationKey,
		DefaultTTL:             time.Hour,
		MaxTTL:                 4 * time.Hour,
		DataPlane:              true,
	}

	cs := fake.NewClientset()
	cs.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		dep := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
		dep.Status = appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
		return false, nil, nil
	})
	client := k8s.NewWithClientset(cs, cfg)

	templates, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}

	// The whole stack, including the real user store over the same fake
	// cluster — so this exercises the ownership and quota rules rather than a
	// stand-in for them.
	users := userservice.New(client.Users(), client)
	return api.New(api.Deps{
		Config:  cfg,
		Service: sandbox.New(cfg, templates, client, users),
		Users:   users,
		Auth:    auth.New(cfg.APIKey, users),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), client
}

// call issues a request and returns the recorder. A path is given root-relative
// and is prefixed with the deployment's base path here, so a test reads as the
// route it is exercising rather than as its full address.
func call(t *testing.T, s *api.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, integrationBase+path, nil)
	} else {
		r = httptest.NewRequest(method, integrationBase+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Sandbox-Key", integrationKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func unmarshal[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
	return out
}

func TestIntegrationLifecycle(t *testing.T) {
	s, client := newIntegrationServer(t)
	ctx := context.Background()

	// ── create ──────────────────────────────────────────────────────────────
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"all-in-one","name":"myshop","ttl":"30m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	created := unmarshal[model.Sandbox](t, w)

	if created.ID != "myshop" || created.Template != "all-in-one" {
		t.Errorf("created = %+v, want id myshop from all-in-one", created)
	}
	if created.Image != "linuxserver/webtop:ubuntu-xfce" {
		t.Errorf("image = %q, want %s", created.Image, "linuxserver/webtop:ubuntu-xfce")
	}
	// The state comes back from the cluster, not from the create call.
	if created.State != model.StateRunning {
		t.Errorf("state = %q, want Running", created.State)
	}
	// The address carries the deployment's public URL and base path, and names
	// the port in the path. all-in-one serves exactly one port — the desktop —
	// so this is also what checks the port list survives the round trip.
	wantURL := "https://sandbox.example.com/sandbox/sandbox/myshop/desktop/"
	if len(created.Endpoints) == 0 || created.Endpoints[0].URL != wantURL {
		t.Errorf("endpoints = %+v, want the first at %s", created.Endpoints, wantURL)
	}

	// ── the cluster really has it ───────────────────────────────────────────
	if _, err := client.Get(ctx, "myshop"); err != nil {
		t.Fatalf("the sandbox is not in the cluster: %v", err)
	}

	// ── list ────────────────────────────────────────────────────────────────
	w = call(t, s, http.MethodGet, "/api/v1/sandboxes", "")
	listed := unmarshal[struct {
		Sandboxes []model.Sandbox `json:"sandboxes"`
		Count     int             `json:"count"`
	}](t, w)
	if listed.Count != 1 || len(listed.Sandboxes) != 1 {
		t.Fatalf("list returned %d sandboxes, want 1", len(listed.Sandboxes))
	}
	if listed.Sandboxes[0].ID != "myshop" {
		t.Errorf("listed %q, want myshop", listed.Sandboxes[0].ID)
	}

	// ── get ─────────────────────────────────────────────────────────────────
	w = call(t, s, http.MethodGet, "/api/v1/sandboxes/myshop", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200", w.Code)
	}
	got := unmarshal[model.Sandbox](t, w)
	if got.Env == nil && len(got.Endpoints) == 0 {
		t.Error("get returned a sandbox with neither env nor endpoints")
	}

	// ── overview ────────────────────────────────────────────────────────────
	w = call(t, s, http.MethodGet, "/api/v1/overview", "")
	ov := unmarshal[api.OverviewResponse](t, w)
	if ov.Total != 1 || ov.ByTemplate["all-in-one"] != 1 {
		t.Errorf("overview = %+v, want one all-in-one", ov)
	}

	// ── renew ───────────────────────────────────────────────────────────────
	w = call(t, s, http.MethodPost, "/api/v1/sandboxes/myshop/renew", `{"ttl":"3h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("renew = %d, want 200: %s", w.Code, w.Body.String())
	}
	renewed := unmarshal[model.Sandbox](t, w)
	if got := renewed.ExpiresAt.Sub(renewed.CreatedAt); got < 3*time.Hour-time.Second {
		t.Errorf("ttl after renew = %v, want about 3h", got)
	}

	// ── delete ──────────────────────────────────────────────────────────────
	if w := call(t, s, http.MethodDelete, "/api/v1/sandboxes/myshop", ""); w.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200: %s", w.Code, w.Body.String())
	}
	if _, err := client.Get(ctx, "myshop"); err == nil {
		t.Error("the sandbox is still in the cluster after a delete")
	}
	if w := call(t, s, http.MethodGet, "/api/v1/sandboxes/myshop", ""); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func TestIntegrationEnvironmentAndTemplateDefaults(t *testing.T) {
	s, _ := newIntegrationServer(t)

	// The caller's environment has to survive the JSON layer and land on the
	// container, merged over the template's.
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{
		"template": "python",
		"name": "wither",
		"env": {"GREETING": "hello", "DEBUG": "1"}
	}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	sb := unmarshal[model.Sandbox](t, w)
	if sb.Env["GREETING"] != "hello" || sb.Env["DEBUG"] != "1" {
		t.Errorf("env = %v, want the caller's two variables", sb.Env)
	}
	// The template's default TTL applies when the caller names none.
	if got := sb.ExpiresAt.Sub(sb.CreatedAt); got < 29*time.Minute || got > 31*time.Minute {
		t.Errorf("ttl = %v, want the template's 30m default", got)
	}
}

func TestIntegrationLimitsAndConflicts(t *testing.T) {
	s, _ := newIntegrationServer(t)
	ctx := context.Background()

	if w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"python","name":"taken"}`); w.Code != http.StatusCreated {
		t.Fatalf("the first create = %d, want 201: %s", w.Code, w.Body.String())
	}
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"python","name":"taken"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("a create with a taken name = %d, want 409", w.Code)
	}

	// And a second, differently spelled, create of the same name is the same
	// conflict — normalization is visible from outside, which is the point.
	w = call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"python","name":"Taken"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("a create with a differently-spelled taken name = %d, want 409", w.Code)
	}
	_ = ctx
}

func TestIntegrationCatalogMatchesWhatCanBeCreated(t *testing.T) {
	// Every template the catalog lists must be creatable through the API. This
	// is the test that would have caught a template with a field the service
	// rejects, or a port the cluster layer cannot build.
	s, _ := newIntegrationServer(t)

	w := call(t, s, http.MethodGet, "/api/v1/catalog", "")
	templates := unmarshal[struct {
		Templates []model.Template `json:"templates"`
	}](t, w)
	if len(templates.Templates) == 0 {
		t.Fatal("the catalog is empty")
	}

	for _, tmpl := range templates.Templates {
		t.Run(tmpl.ID, func(t *testing.T) {
			w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"`+tmpl.ID+`"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("creating %s = %d, want 201: %s", tmpl.ID, w.Code, w.Body.String())
			}
			sb := unmarshal[model.Sandbox](t, w)
			if sb.Template != tmpl.ID {
				t.Errorf("created %q, want %q", sb.Template, tmpl.ID)
			}
			if sb.Image != tmpl.Image {
				t.Errorf("image = %q, want the template's %q", sb.Image, tmpl.Image)
			}
			if len(sb.Endpoints) != len(tmpl.Ports) {
				t.Errorf("got %d endpoints, want one per port (%d)", len(sb.Endpoints), len(tmpl.Ports))
			}
		})
	}
}

func TestIntegrationNotFoundIsAGoodMessage(t *testing.T) {
	s, _ := newIntegrationServer(t)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/sandboxes/ghost", ""},
		{http.MethodDelete, "/api/v1/sandboxes/ghost", ""},
		{http.MethodPost, "/api/v1/sandboxes/ghost/renew", `{"ttl":"1h"}`},
		{http.MethodGet, "/api/v1/sandboxes/ghost/logs", ""},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := call(t, s, tc.method, tc.path, tc.body)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
			}
			body := unmarshal[struct {
				Error string `json:"error"`
			}](t, w)
			// The message has to name what was not found; a bare 404 leaves a
			// caller with nothing to check.
			if !strings.Contains(body.Error, "ghost") {
				t.Errorf("error %q does not name the sandbox", body.Error)
			}
		})
	}
}
