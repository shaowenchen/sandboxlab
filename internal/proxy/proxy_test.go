package proxy_test

// The proxy is tested through the API router rather than by calling Serve
// directly. The interesting part of this package is how it maps a public
// address onto a sandbox's own, and that mapping is a contract shared with the
// router — the router splits the path, the proxy forwards the remainder. A test
// that called Serve with a hand-split path would pass while the two disagreed
// about where the split is.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/sandboxlab/internal/api"
	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/proxy"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

const testKey = "test-key"

// resolver returns a fixed target, or an error.
type resolver struct {
	target *url.URL
	err    error
}

func (r *resolver) Target(context.Context, string, string) (*url.URL, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.target, nil
}

// newServer builds a real server over the real proxy, pointed at target.
func newServer(t *testing.T, cfg config.Config, res api.DataPlane) (*api.Server, *sandbox.Service) {
	t.Helper()
	if cfg.APIKey == "" {
		cfg.APIKey = testKey
	}
	cfg.DataPlane = true
	c, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	// A fake clientset: the proxy routes are the subject here, and none of them
	// touch the cluster — the resolver the proxy holds is what answers.
	client := k8s.NewWithClientset(fake.NewClientset(), cfg)
	svc := sandbox.New(cfg, c, client)
	return api.New(api.Deps{
		Config:    cfg,
		Service:   svc,
		Auth:      auth.New(cfg.APIKey),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DataPlane: res,
	}), svc
}

func newProxyServer(t *testing.T, cfg config.Config, res *resolver) *api.Server {
	t.Helper()
	s, _ := newServer(t, cfg, proxy.New(cfg, res, slog.New(slog.NewTextHandler(io.Discard, nil))))
	return s
}

// newProxyServerWithSandbox is newProxyServer with a sandbox named "demo" that
// already exists.
//
// The data plane checks ownership against the sandbox before it forwards, so a
// test that only wants to exercise the proxy still has to have one there — and
// going through a create is what makes the check itself part of what is
// exercised rather than mocked away.
func newProxyServerWithSandbox(t *testing.T, cfg config.Config, res *resolver) *api.Server {
	t.Helper()
	s, svc := newServer(t, cfg, proxy.New(cfg, res, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if _, err := svc.Create(context.Background(), sandbox.CreateInput{
		Template: "agent-infra",
		Name:     "demo",
	}); err != nil {
		t.Fatalf("creating the sandbox under test: %v", err)
	}
	return s
}

func get(t *testing.T, s *api.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Sandbox-Key", testKey)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestForwardsTheRemainderToTheSandbox(t *testing.T) {
	var gotPath, gotHost, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost, gotQuery = r.URL.Path, r.Host, r.URL.RawQuery
		w.Header().Set("X-Backend", "yes")
		_, _ = w.Write([]byte("hello from the sandbox"))
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{target: target})
	w := get(t, s, "/sandbox/demo/api/health?wait=1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello from the sandbox" {
		t.Errorf("body = %q, want the backend's response", w.Body.String())
	}
	if w.Header().Get("X-Backend") != "yes" {
		t.Error("a backend response header was dropped")
	}
	// The public prefix is not the sandbox's business: it serves /health, not
	// /sandbox/demo/api/health.
	if gotPath != "/health" {
		t.Errorf("the backend saw path %q, want /health", gotPath)
	}
	if gotQuery != "wait=1" {
		t.Errorf("the backend saw query %q, want wait=1", gotQuery)
	}
	// The Host is the target's. A sandbox that trusts its Host header — the
	// agent-infra image's VS Code and desktop, notably — would otherwise see
	// the public hostname and either refuse or build redirects pointing
	// outside.
	if gotHost != target.Host {
		t.Errorf("the backend saw Host %q, want its own %q", gotHost, target.Host)
	}
}

func TestTheRootOfASandboxPort(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{target: target})

	// With no trailing slash the sandbox's root is meant, not a directory
	// listing at "/api".
	if w := get(t, s, "/sandbox/demo/api"); w.Code != http.StatusOK {
		t.Errorf("GET /sandbox/demo/api = %d, want 200", w.Code)
	}
	if gotPath != "/" {
		t.Errorf("the backend saw path %q, want /", gotPath)
	}

	if w := get(t, s, "/sandbox/demo/api/"); w.Code != http.StatusOK {
		t.Errorf("GET /sandbox/demo/api/ = %d, want 200", w.Code)
	}
	if gotPath != "/" {
		t.Errorf("the backend saw path %q, want /", gotPath)
	}
}

func TestBasePathIsNotForwarded(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	cfg := config.Config{BasePath: "/prefix", PublicURL: "https://example.com"}
	s := newProxyServerWithSandbox(t, cfg, &resolver{target: target})

	if w := get(t, s, "/prefix/sandbox/demo/api/x"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotPath != "/x" {
		t.Errorf("the backend saw path %q, want /x", gotPath)
	}
}

func TestSandboxThatIsGone(t *testing.T) {
	s := newProxyServer(t, config.Config{}, &resolver{err: k8s.ErrNotFound})
	if w := get(t, s, "/sandbox/demo/api/"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a sandbox that does not exist", w.Code)
	}
}

func TestSandboxThatCannotBeReached(t *testing.T) {
	// The ordinary case: a sandbox that is not up yet, or one whose container
	// has died. It is a 502 with the reason in it, not a bare status.
	//
	// The sandbox itself has to exist, or the ownership check refuses it first
	// with a 404 and the resolver is never reached — which is what this test
	// would then be proving instead.
	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{err: errNoService{}})
	w := get(t, s, "/sandbox/demo/api/")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no service yet") {
		t.Errorf("body = %q, want the resolver's reason", w.Body.String())
	}
}

type errNoService struct{}

func (errNoService) Error() string { return "the sandbox has no service yet" }

func TestBackendRefusesTheConnection(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, _ := url.Parse(backend.URL)
	backend.Close() // now nothing is listening

	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{target: target})
	w := get(t, s, "/sandbox/demo/api/")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "could not reach sandbox") {
		t.Errorf("body = %q, want it to say the sandbox could not be reached", w.Body.String())
	}
}

func TestRedirectsAreRewritten(t *testing.T) {
	// A sandbox that redirects to "/login" means "login on me". The caller
	// asked for a path under /sandbox/<id>/<port>/, so a bare "/login" would
	// send them to the control plane's own root instead.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/relative":
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
		case "/dot-relative":
			w.Header().Set("Location", "login")
			w.WriteHeader(http.StatusFound)
		case "/elsewhere":
			w.Header().Set("Location", "https://other.example.com/thing")
			w.WriteHeader(http.StatusFound)
		case "/absolute-self":
			// A sandbox that builds an absolute URL from its Host. The proxy
			// set Host to the in-cluster address, so this is what comes back.
			w.Header().Set("Location", "http://"+r.Host+"/home")
			w.WriteHeader(http.StatusFound)
		}
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s := newProxyServerWithSandbox(t, config.Config{PublicURL: "https://sandbox.example.com"}, &resolver{target: target})

	tests := []struct {
		path string
		want string
	}{
		{"/relative", "https://sandbox.example.com/sandbox/demo/api/login"},
		{"/dot-relative", "https://sandbox.example.com/sandbox/demo/api/login"},
		{"/absolute-self", "https://sandbox.example.com/sandbox/demo/api/home"},
		// A redirect somewhere else entirely is not this deployment's business.
		{"/elsewhere", "https://other.example.com/thing"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			w := get(t, s, "/sandbox/demo/api"+tc.path)
			if got := w.Header().Get("Location"); got != tc.want {
				t.Errorf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBodiesAreForwarded(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{target: target})
	req := httptest.NewRequest(http.MethodPost, "/sandbox/demo/api/run", strings.NewReader(`{"command":"ls"}`))
	req.Header.Set("X-Sandbox-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// A POST with a body is half of what these sandboxes are for — the
	// agent-infra image's own API is driven entirely by them.
	if got != `{"command":"ls"}` {
		t.Errorf("the backend received %q, want the request body", got)
	}
}

func TestResponsesAreNotCached(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)

	s := newProxyServerWithSandbox(t, config.Config{}, &resolver{target: target})
	w := get(t, s, "/sandbox/demo/api/")
	// A cached page for a sandbox that has since been recreated is exactly the
	// confusion this avoids.
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
