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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/exec"

	"github.com/shaowenchen/sandboxlab/internal/api"
	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
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

	return api.New(api.Deps{
		Config:  cfg,
		Service: sandbox.New(cfg, templates, client),
		Auth:    auth.New(cfg.APIKey),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), client
}

// recorderRunner is the executor for the integration tests.
//
// The fake clientset cannot exec, so without one the full stack stops at the
// transport and everything above it — the fields the API passes down, the ones
// the service threads through — goes untested. It answers with what the scripts
// would have printed for the file protocol, and records what it was asked to
// run so a test can assert the path arrived as an argument.
type recorderRunner struct {
	// reply is written to stdout.
	reply string
	// stderr is written to stderr.
	stderr string
	// exitCode, when non-zero, is returned as the command's status.
	exitCode int
	// calls is every command, in order.
	calls [][]string
	// stdin is what the last call was given.
	stdin []byte
}

func (r *recorderRunner) Stream(_ context.Context, _, _ string, opts k8s.StreamOptions) error {
	r.calls = append(r.calls, opts.Command)
	r.stdin = nil
	if opts.Stdin != nil {
		r.stdin, _ = io.ReadAll(opts.Stdin)
	}
	if opts.Stdout != nil && r.reply != "" {
		_, _ = io.WriteString(opts.Stdout, r.reply)
	}
	if opts.Stderr != nil && r.stderr != "" {
		_, _ = io.WriteString(opts.Stderr, r.stderr)
	}
	if r.exitCode != 0 {
		return exec.CodeExitError{Err: errors.New("exit status"), Code: r.exitCode}
	}
	return nil
}

// newIntegrationServerWithRunner is newIntegrationServer with an executor, so
// the whole stack — route, service, client, transport — is exercised.
func newIntegrationServerWithRunner(t *testing.T, runner k8s.Runner) (*api.Server, *fake.Clientset) {
	t.Helper()
	cfg := config.Config{
		Namespace:              "ops-system",
		SandboxNamespacePrefix: "sbx-",
		PublicURL:              "https://sandbox.example.com",
		BasePath:               integrationBase,
		APIKey:                 integrationKey,
		DefaultTTL:             time.Hour,
		MaxTTL:                 4 * time.Hour,
		ExecTimeout:            time.Minute,
		MaxExecTimeout:         10 * time.Minute,
		MaxFileBytes:           2 << 20,
	}

	cs := fake.NewClientset()
	cs.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		dep := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
		dep.Status = appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
		return false, nil, nil
	})
	client := k8s.NewWithClientset(cs, cfg).WithRunner(runner)

	templates, err := catalog.Loader{}.Load()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}
	return api.New(api.Deps{
		Config:  cfg,
		Service: sandbox.New(cfg, templates, client),
		Auth:    auth.New(cfg.APIKey),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}), cs
}

// addSandboxPod puts a pod in a sandbox's namespace, the way the Deployment's
// controller would.
//
// The fake cluster runs no controller, so a created sandbox has a namespace and
// a Deployment and nothing running — which is the genuine "still starting"
// state, and the reason exec against a fresh sandbox is a 409 rather than a
// 500. A test that wants the running case has to say so.
func addSandboxPod(t *testing.T, cs *fake.Clientset, id string) {
	t.Helper()
	if _, err := cs.CoreV1().Pods("sbx-"+id).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "sandbox-7d9f8c4b5-x2jql",
			Labels: map[string]string{"sandbox.sandboxlab/app": "sandbox"},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}
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
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-infra","name":"myshop","ttl":"30m"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	created := unmarshal[model.Sandbox](t, w)

	if created.ID != "myshop" || created.Template != "agent-infra" {
		t.Errorf("created = %+v, want id myshop from agent-infra", created)
	}
	if created.Image != "ghcr.io/agent-infra/sandbox:1.11.0" {
		t.Errorf("image = %q, want %s", created.Image, "ghcr.io/agent-infra/sandbox:1.11.0")
	}
	// The state comes back from the cluster, not from the create call.
	if created.State != model.StateRunning {
		t.Errorf("state = %q, want Running", created.State)
	}
	// The address carries the deployment's public URL and base path, and names
	// the port in the path. agent-infra serves everything from one port — the
	// whole sandbox — so this is also what checks the port list survives the
	// round trip.
	wantURL := "https://sandbox.example.com/sandbox/sandbox/myshop/aio/"
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
	if ov.Total != 1 || ov.ByTemplate["agent-infra"] != 1 {
		t.Errorf("overview = %+v, want one agent-infra", ov)
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
		"template": "agent-sandbox",
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

	if w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-sandbox","name":"taken"}`); w.Code != http.StatusCreated {
		t.Fatalf("the first create = %d, want 201: %s", w.Code, w.Body.String())
	}
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-sandbox","name":"taken"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("a create with a taken name = %d, want 409", w.Code)
	}

	// And a second, differently spelled, create of the same name is the same
	// conflict — normalization is visible from outside, which is the point.
	w = call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-sandbox","name":"Taken"}`)
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

// ── exec and files, through the whole stack ─────────────────────────────────

// The path a create leaves a sandbox in, so the pod resolution is exercised
// rather than assumed.
func aRunningSandbox(t *testing.T, s *api.Server) string {
	t.Helper()
	w := call(t, s, http.MethodPost, "/api/v1/sandboxes", `{"template":"agent-sandbox","name":"demo"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a sandbox = %d: %s", w.Code, w.Body)
	}
	return "demo"
}

// TestIntegrationExecGoesThroughEveryLayer is the seam test.
//
// A stub service proves the routing and the cluster tests prove the command;
// only this notices the two disagreeing — a field the handler reads and the
// service never passes down is internally consistent on both sides.
func TestIntegrationExecGoesThroughEveryLayer(t *testing.T) {
	runner := &recorderRunner{reply: "hi\n"}
	s, cs := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)
	addSandboxPod(t, cs, id)

	w := call(t, s, http.MethodPost, "/api/v1/sandboxes/"+id+"/exec", `{"command":["echo","hi"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("exec = %d: %s", w.Code, w.Body)
	}
	got := unmarshal[struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
	}](t, w)
	if got.Stdout != "hi\n" {
		t.Errorf("stdout = %q, want the runner's reply", got.Stdout)
	}
	if got.ExitCode != 0 {
		t.Errorf("exitCode = %d, want 0", got.ExitCode)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("the runner saw %d commands, want 1", len(runner.calls))
	}
	if strings.Join(runner.calls[0], " ") != "echo hi" {
		t.Errorf("command = %v, want the caller's argv", runner.calls[0])
	}
}

// A command that failed comes back as a 200 with its status, all the way up.
func TestIntegrationANonZeroExitIsStillOK(t *testing.T) {
	runner := &recorderRunner{exitCode: 3}
	s, cs := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)
	addSandboxPod(t, cs, id)

	w := call(t, s, http.MethodPost, "/api/v1/sandboxes/"+id+"/exec", `{"command":["grep","x"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("exec = %d, want 200: %s", w.Code, w.Body)
	}
	if got := unmarshal[struct {
		ExitCode int `json:"exitCode"`
	}](t, w); got.ExitCode != 3 {
		t.Errorf("exitCode = %d, want 3", got.ExitCode)
	}
}

// A sandbox with no pod is a 409, not a 500 — it is the ordinary state moments
// after a create, and a caller polling should be told to come back.
func TestIntegrationExecOnASandboxWithNoPodIsAConflict(t *testing.T) {
	runner := &recorderRunner{reply: "x"}
	s, _ := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)

	w := call(t, s, http.MethodPost, "/api/v1/sandboxes/"+id+"/exec", `{"command":["true"]}`)
	if w.Code != http.StatusConflict {
		t.Errorf("exec with no pod = %d, want 409: %s", w.Code, w.Body)
	}
}

// Writing a file ends with the path as an argument and the bytes on stdin.
func TestIntegrationWriteFileReachesTheContainer(t *testing.T) {
	runner := &recorderRunner{}
	s, cs := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)
	addSandboxPod(t, cs, id)

	w := call(t, s, http.MethodPut, "/api/v1/sandboxes/"+id+"/files?path=/workspace/a.txt",
		`{"content":"hello\n"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("write = %d: %s", w.Code, w.Body)
	}
	if string(runner.stdin) != "hello\n" {
		t.Errorf("stdin = %q, want the file's bytes", runner.stdin)
	}
	if got := runner.calls[0][4]; got != "/workspace/a.txt" {
		t.Errorf("the path argument = %q, want it passed as an argument", got)
	}
}

// Reading one comes back with the file's bytes.
func TestIntegrationReadFileReachesTheContainer(t *testing.T) {
	runner := &recorderRunner{reply: "file contents\n"}
	s, cs := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)
	addSandboxPod(t, cs, id)

	w := call(t, s, http.MethodGet, "/api/v1/sandboxes/"+id+"/files?path=/workspace/a.txt", "")
	if w.Code != http.StatusOK {
		t.Fatalf("read = %d: %s", w.Code, w.Body)
	}
	got := unmarshal[struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}](t, w)
	if got.Content != "file contents\n" {
		t.Errorf("content = %q, want the runner's output", got.Content)
	}
	if got.Encoding != "utf8" {
		t.Errorf("encoding = %q, want utf8", got.Encoding)
	}
}

// A file that is not there, all the way up: a 404 naming the file, not the
// sandbox. The two 404s read differently on purpose.
func TestIntegrationAMissingFileIs404AndNamesTheFile(t *testing.T) {
	runner := &recorderRunner{exitCode: 3}
	s, cs := newIntegrationServerWithRunner(t, runner)
	id := aRunningSandbox(t, s)
	addSandboxPod(t, cs, id)

	w := call(t, s, http.MethodGet, "/api/v1/sandboxes/"+id+"/files?path=/workspace/gone.txt", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("read of a missing file = %d, want 404: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/workspace/gone.txt") {
		t.Errorf("body = %s, want it to name the file", body)
	}
	if strings.Contains(body, "no sandbox") {
		t.Errorf("body = %s, which is the message for a missing sandbox", body)
	}
}
