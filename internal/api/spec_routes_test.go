package api_test

// The spec and the routes are the same list.
//
// api/openapi.yaml is the source of truth: four SDKs are generated from it, and
// the console and the CLI are written against it. Nothing enforced that it
// described this server. A path in the spec that the server does not register
// generates a client that calls an endpoint that is not there; a route the
// server registers that the spec omits is one no client can discover, and one
// the next person to write an SDK will not know about.
//
// Neither is visible from the spec alone or from the routes alone, which is why
// this reads both. It is what makes "the spec is the source of truth" a
// property rather than a claim.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shaowenchen/sandboxlab/internal/api"
	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// specPath is the document, relative to this package's directory.
const specPath = "../../api/openapi.yaml"

// planeStub is a data plane that does nothing. This test is about which routes
// exist, not what they do with a request.
type planeStub struct{}

func (planeStub) Serve(http.ResponseWriter, *http.Request, string, string, string) {}

// aFullDeployment is the server the spec is checked against: every route a
// deployment registers, which means a user store and a data plane together.
//
// Built here rather than from either test helper, because neither helper is a
// full deployment — one has users and no plane, the other a plane and no users
// — and a check built on either would compare the spec against part of the
// interface and pass. That reads as coverage while being less of it.
func aFullDeployment(t *testing.T) *api.Server {
	t.Helper()

	cfg := config.Config{
		Namespace: "ops-system", SandboxNamespacePrefix: "sbx-",
		APIKey: "spec-check-key", DataPlane: true,
		DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour,
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
	return api.New(api.Deps{
		Config:    cfg,
		Service:   sandbox.New(cfg, templates, client),
		Auth:      auth.New(cfg.APIKey),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataPlane: planeStub{},
	})
}

// normPath compares two ways of spelling the same route.
//
// A trailing slash is not a route difference — Go's mux treats "/x/" as a
// subtree and "/x" as the exact path, so which is registered is a decision
// about matching rather than about the interface — and the spec picks whichever
// reads better to a human.
//
// The evaluation prefixes are a real difference, so they are replaced rather
// than dropped: OpenAPI names path parameters and this server's mux names them,
// and a route with a different *number* of them is a different route.
func normPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(p, "{}")
}

// specRoutes reads the paths and methods the spec declares.
func specRoutes(t *testing.T) map[string][]string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", specPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s declares no paths; the parser or the document is wrong", specPath)
	}

	// Only the methods a client can call. A path item also carries keys like
	// `parameters` and `summary`, which are not operations.
	verbs := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

	out := map[string][]string{}
	for path, item := range doc.Paths {
		var methods []string
		for key := range item {
			if verbs[strings.ToLower(key)] {
				methods = append(methods, strings.ToUpper(key))
			}
		}
		sort.Strings(methods)
		out[normPath(path)] = methods
	}
	return out
}

// serverRoutes is every route a server registered.
func serverRoutes(s *api.Server) map[string][]string {
	out := map[string][]string{}
	for _, r := range s.Routes() {
		out[normPath(r.Pattern)] = append([]string(nil), r.Methods...)
	}
	return out
}

func TestTheSpecMatchesTheRoutes(t *testing.T) {
	spec := specRoutes(t)
	routes := serverRoutes(aFullDeployment(t))

	// ── the spec describes nothing this server does not serve ───────────────
	var undocumented []string
	for path, methods := range spec {
		got, ok := routes[path]
		if !ok {
			undocumented = append(undocumented, path)
			continue
		}
		// "*" is the data plane, which forwards the backend's own methods, so
		// any method the spec names is one it will pass through.
		if len(got) == 1 && got[0] == "*" {
			continue
		}
		for _, m := range methods {
			if !contains(got, m) {
				undocumented = append(undocumented, m+" "+path)
			}
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("the spec documents routes this server does not answer:\n  %s\n"+
			"a client generated from the spec would call endpoints that are not there",
			strings.Join(undocumented, "\n  "))
	}

	// ── and serves nothing the spec does not describe ───────────────────────
	var undescribed []string
	for pattern, methods := range routes {
		want, ok := spec[pattern]
		if !ok {
			undescribed = append(undescribed, pattern)
			continue
		}
		// The data plane's methods belong to whatever the sandbox runs; the
		// spec can only say the path exists.
		if len(methods) == 1 && methods[0] == "*" {
			continue
		}
		for _, m := range methods {
			if !contains(want, m) {
				undescribed = append(undescribed, m+" "+pattern)
			}
		}
	}
	sort.Strings(undescribed)
	if len(undescribed) > 0 {
		t.Errorf("this server answers routes the spec does not describe:\n  %s\n"+
			"a client generated from the spec cannot reach them",
			strings.Join(undescribed, "\n  "))
	}
}

// The comparison includes the one route that comes and goes.
//
// The data-plane route is registered only when the deployment has a plane, so a
// server built without one would leave it out of the comparison above and still
// pass. This asserts aFullDeployment really is a full deployment.
func TestTheComparisonIncludesTheConditionalRoutes(t *testing.T) {
	routes := serverRoutes(aFullDeployment(t))
	if _, ok := routes[normPath("/sandbox/{id}/{port}/")]; !ok {
		t.Error("/sandbox/{id}/{port}/ is absent, so the spec check is not covering it")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestDescribeListsTheRoutesThatExist is the third copy, checked.
//
// describe() is a hand-written list of endpoints served without a key — it is
// what a client reads to learn the API's shape. Nothing compared it against
// anything, and it drifted: it advertised /api/v1/whoami for a while after that
// route was deleted, which is a contract document promising an endpoint that is
// not there.
//
// The spec check above cannot catch it, because describe() is not the spec. So
// this reads the document the server actually serves and compares it with the
// routes the server actually registered.
func TestDescribeListsTheRoutesThatExist(t *testing.T) {
	s := aFullDeployment(t)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/describe", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/describe = %d, want 200", w.Code)
	}
	var body struct {
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("describe is not JSON: %v", err)
	}
	if len(body.Endpoints) == 0 {
		t.Fatal("describe lists no endpoints")
	}

	// The routes that answer without a key. Everything else is behind one, and
	// describe is not a place to advertise what a caller cannot yet reach.
	routes := serverRoutes(s)

	for _, ep := range body.Endpoints {
		if _, ok := routes[normPath(ep.Path)]; !ok {
			t.Errorf("describe advertises %s %s, which this server does not answer", ep.Method, ep.Path)
		}
	}

	// And the other way, for the routes a client most needs to find: every
	// sandbox operation should be described, or a client reading only describe
	// would not know it exists.
	described := map[string]bool{}
	for _, ep := range body.Endpoints {
		described[normPath(ep.Path)] = true
	}
	for pattern := range routes {
		if strings.HasPrefix(pattern, "/api/v1/sandboxes") && !described[pattern] {
			t.Errorf("the route %s is not in describe, so a client reading describe cannot find it", pattern)
		}
	}
}
