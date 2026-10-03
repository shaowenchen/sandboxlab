package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

func testConfig() config.Config {
	return config.Config{
		Namespace:              "ops-system",
		SandboxNamespacePrefix: "sbx-",
		PublicURL:              "https://sandbox.example.com",
		BasePath:               "/sandbox",
		APIKey:                 "test-key",
		DefaultTTL:             time.Hour,
		MaxTTL:                 8 * time.Hour,
	}
}

func testTemplate() model.Template {
	return model.Template{
		ID:    "demo",
		Title: "Demo",
		Image: "busybox:latest",
		Ports: []model.Port{
			{Name: "api", Port: 8000},
			{Name: "vnc", Port: 6080},
		},
		Env:       map[string]string{"FROM_TEMPLATE": "yes", "OVERRIDDEN": "template"},
		Resources: model.Resources{CPU: "1", Memory: "512Mi"},
	}
}

// newClient builds a client over a fake clientset, with a reactor that settles
// a created Deployment the way the controller would — otherwise every sandbox
// reads back as Pending and the state assertions test nothing.
func newClient(objects ...runtime.Object) *Client {
	cs := fake.NewClientset(objects...)
	cs.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		dep := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
		dep.Status = appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
		return false, nil, nil
	})
	c := NewWithClientset(cs, testConfig())
	c.now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
	return c
}

func TestCreateBuildsANamespacePerSandbox(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	sb, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate(), TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if sb.Namespace != "sbx-demo" {
		t.Errorf("namespace = %q, want sbx-demo", sb.Namespace)
	}
	if sb.State != model.StateRunning {
		t.Errorf("state = %q, want Running", sb.State)
	}
	if sb.ID != "demo" || sb.Template != "demo" || sb.Image != "busybox:latest" {
		t.Errorf("sandbox = %+v, want id/template demo and the template's image", sb)
	}

	// The namespace carries the labels the list is built from. Without them a
	// sandbox would exist but be invisible, and therefore un-deletable.
	ns, err := c.cs.CoreV1().Namespaces().Get(ctx, "sbx-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the namespace: %v", err)
	}
	if ns.Labels[managedByLabel] != managedByValue {
		t.Errorf("the namespace is not labelled as managed: %v", ns.Labels)
	}
	if ns.Labels[sandboxIDLabel] != "demo" {
		t.Errorf("sandbox id label = %q, want demo", ns.Labels[sandboxIDLabel])
	}

	// Everything else is inside it.
	if _, err := c.cs.AppsV1().Deployments("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); err != nil {
		t.Errorf("the deployment is missing: %v", err)
	}
	if _, err := c.cs.CoreV1().Services("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); err != nil {
		t.Errorf("the service is missing: %v", err)
	}
	if _, err := c.cs.CoreV1().ResourceQuotas("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); err != nil {
		t.Errorf("the resource quota is missing: %v", err)
	}
	if _, err := c.cs.NetworkingV1().NetworkPolicies("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); err != nil {
		t.Errorf("the network policy is missing: %v", err)
	}
}

// TestCreateWithoutPortsMakesNoService covers the templates that publish
// nothing — `opensandbox` is one, driven through exec and the file endpoints,
// so it declares no ports.
//
// It is a regression test for a real failure: the Service was created
// unconditionally, and Kubernetes refuses a Service with no ports ("spec.ports:
// Required value"), so every sandbox from a portless template came back 500.
// A fake clientset does not validate, which is why this asserts on what was
// written rather than on the API rejecting it.
func TestCreateWithoutPortsMakesNoService(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	tmpl := testTemplate()
	tmpl.Ports = nil

	sb, err := c.Create(ctx, CreateRequest{ID: "demo", Template: tmpl, TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := c.cs.CoreV1().Services("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a portless template got a Service (err = %v); Kubernetes refuses one with no ports", err)
	}
	// The rest of the sandbox is unaffected: only the Service is conditional.
	if _, err := c.cs.AppsV1().Deployments("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{}); err != nil {
		t.Errorf("the deployment is missing: %v", err)
	}
	if _, err := c.cs.CoreV1().Namespaces().Get(ctx, "sbx-demo", metav1.GetOptions{}); err != nil {
		t.Errorf("the namespace is missing: %v", err)
	}
	// And it reads back with no endpoints rather than a URL to nothing.
	if len(sb.Endpoints) != 0 {
		t.Errorf("endpoints = %v, want none for a template with no ports", sb.Endpoints)
	}
	if sb.State != model.StateRunning {
		t.Errorf("state = %q, want Running", sb.State)
	}
}

func TestCreateRecordsTheExpiryOnTheNamespace(t *testing.T) {
	// This is the whole reason a restart is a non-event: the expiry lives on
	// the object, so a reaper that never saw the create still knows when the
	// sandbox is due.
	c := newClient()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	sb, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate(), TTL: 2 * time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := now.Add(2 * time.Hour)
	if !sb.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", sb.ExpiresAt, want)
	}

	ns, err := c.cs.CoreV1().Namespaces().Get(ctx, "sbx-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the namespace: %v", err)
	}
	raw := ns.Annotations[expiresAtAnnotation]
	got, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("the expiry annotation %q is not RFC3339: %v", raw, err)
	}
	if !got.Equal(want) {
		t.Errorf("the expiry annotation is %v, want %v", got, want)
	}
}

func TestCreateWithNoTTLHasNoExpiry(t *testing.T) {
	c := newClient()
	sb, err := c.Create(context.Background(), CreateRequest{ID: "demo", Template: testTemplate()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sb.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want no expiry for a TTL of zero", sb.ExpiresAt)
	}
	ns, err := c.cs.CoreV1().Namespaces().Get(context.Background(), "sbx-demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the namespace: %v", err)
	}
	if _, ok := ns.Annotations[expiresAtAnnotation]; ok {
		t.Error("an expiry annotation was written for a sandbox with no TTL")
	}
}

func TestCreateMergesTheCallersEnvironmentOverTheTemplates(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	_, err := c.Create(ctx, CreateRequest{
		ID:       "demo",
		Template: testTemplate(),
		Env:      map[string]string{"OVERRIDDEN": "caller", "EXTRA": "yes"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	dep, err := c.cs.AppsV1().Deployments("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the deployment: %v", err)
	}
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["FROM_TEMPLATE"] != "yes" {
		t.Errorf("a template variable was lost: %v", env)
	}
	if env["OVERRIDDEN"] != "caller" {
		t.Errorf("OVERRIDDEN = %q, want the caller's value", env["OVERRIDDEN"])
	}
	if env["EXTRA"] != "yes" {
		t.Errorf("the caller's EXTRA is missing: %v", env)
	}
}

func TestCreateIsAtomic(t *testing.T) {
	// If any step after the namespace fails, the namespace goes with it. A
	// half-made sandbox left behind would be an orphan nothing lists and
	// nothing cleans up.
	cs := fake.NewClientset()
	cs.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the cluster refused")
	})
	c := NewWithClientset(cs, testConfig())

	_, err := c.Create(context.Background(), CreateRequest{ID: "demo", Template: testTemplate()})
	if err == nil {
		t.Fatal("Create succeeded although creating the service failed")
	}

	_, getErr := cs.CoreV1().Namespaces().Get(context.Background(), "sbx-demo", metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("the namespace survived a failed create (err = %v); it should have been removed", getErr)
	}
}

func TestCreateRejectsATakenName(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("the first Create: %v", err)
	}
	_, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Create with a taken name returned %v, want ErrAlreadyExists", err)
	}
}

func TestGetAndNotFound(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	if _, err := c.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a missing sandbox = %v, want ErrNotFound", err)
	}
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Get(ctx, "demo"); err != nil {
		t.Errorf("Get of an existing sandbox: %v", err)
	}
}

func TestForeignNamespacesAreInvisible(t *testing.T) {
	// A namespace whose name matches the prefix but is not ours must be neither
	// listed nor reachable nor deletable: the control plane has no business
	// touching something it did not create.
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sbx-demo"}}
	c := newClient(foreign)
	ctx := context.Background()

	if _, err := c.Get(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a foreign namespace = %v, want ErrNotFound", err)
	}
	if err := c.Delete(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of a foreign namespace = %v, want ErrNotFound", err)
	}
	all, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("List returned %d sandboxes, want none: %+v", len(all), all)
	}

	// And it is still there.
	if _, err := c.cs.CoreV1().Namespaces().Get(ctx, "sbx-demo", metav1.GetOptions{}); err != nil {
		t.Errorf("the foreign namespace was removed: %v", err)
	}
}

func TestListReturnsOnlyManagedSandboxes(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	for _, id := range []string{"one", "two"} {
		if _, err := c.Create(ctx, CreateRequest{ID: id, Template: testTemplate()}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	// Something else's namespace, and a plain one.
	if _, err := c.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating an unrelated namespace: %v", err)
	}

	got, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d sandboxes, want 2: %+v", len(got), got)
	}
	for _, sb := range got {
		if sb.ID != "one" && sb.ID != "two" {
			t.Errorf("List returned an unexpected sandbox %q", sb.ID)
		}
	}
}

func TestDeleteRemovesEverything(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.Delete(ctx, "demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := c.Delete(ctx, "demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete twice = %v, want ErrNotFound", err)
	}
}

func TestRenew(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate(), TTL: time.Hour}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("extends", func(t *testing.T) {
		sb, err := c.Renew(ctx, "demo", 4*time.Hour)
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if want := now.Add(4 * time.Hour); !sb.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want %v", sb.ExpiresAt, want)
		}
	})

	t.Run("zero removes the expiry", func(t *testing.T) {
		sb, err := c.Renew(ctx, "demo", 0)
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if !sb.ExpiresAt.IsZero() {
			t.Errorf("ExpiresAt = %v after renewing with zero, want no expiry", sb.ExpiresAt)
		}
		ns, err := c.cs.CoreV1().Namespaces().Get(ctx, "sbx-demo", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the namespace: %v", err)
		}
		if _, ok := ns.Annotations[expiresAtAnnotation]; ok {
			t.Error("the expiry annotation survived a renew with zero")
		}
	})

	t.Run("a missing sandbox", func(t *testing.T) {
		if _, err := c.Renew(ctx, "nope", time.Hour); !errors.Is(err, ErrNotFound) {
			t.Errorf("Renew of a missing sandbox = %v, want ErrNotFound", err)
		}
	})
}

func TestExpired(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if _, err := c.Create(ctx, CreateRequest{ID: "short", Template: testTemplate(), TTL: time.Minute}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Create(ctx, CreateRequest{ID: "long", Template: testTemplate(), TTL: time.Hour}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Create(ctx, CreateRequest{ID: "forever", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("nothing is due yet", func(t *testing.T) {
		got, err := c.Expired(ctx)
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Expired returned %d sandboxes before any was due: %+v", len(got), got)
		}
	})

	t.Run("only the due one", func(t *testing.T) {
		// Move the clock past the short one's expiry but not the long one's.
		c.now = func() time.Time { return now.Add(5 * time.Minute) }
		got, err := c.Expired(ctx)
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		if len(got) != 1 || got[0].ID != "short" {
			t.Fatalf("Expired = %+v, want only \"short\"", got)
		}
	})

	t.Run("a sandbox with no expiry is never due", func(t *testing.T) {
		c.now = func() time.Time { return now.Add(1000 * time.Hour) }
		got, err := c.Expired(ctx)
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		for _, sb := range got {
			if sb.ID == "forever" {
				t.Error("a sandbox with no expiry was reported as expired")
			}
		}
	})
}

func TestEndpointsNameEveryPort(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	sb, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sb.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2: %+v", len(sb.Endpoints), sb.Endpoints)
	}
	byName := map[string]model.Endpoint{}
	for _, ep := range sb.Endpoints {
		byName[ep.Name] = ep
	}
	for _, want := range []struct {
		name string
		port int32
		url  string
	}{
		{"api", 8000, "https://sandbox.example.com/sandbox/sandbox/demo/api/"},
		{"vnc", 6080, "https://sandbox.example.com/sandbox/sandbox/demo/vnc/"},
	} {
		ep, ok := byName[want.name]
		if !ok {
			t.Errorf("no endpoint named %q", want.name)
			continue
		}
		if ep.Port != want.port {
			t.Errorf("%s port = %d, want %d", want.name, ep.Port, want.port)
		}
		if ep.URL != want.url {
			t.Errorf("%s url = %q, want %q", want.name, ep.URL, want.url)
		}
	}
}

func TestTarget(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("by port name", func(t *testing.T) {
		u, err := c.Target(ctx, "demo", "api")
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		if u.Host != "sandbox.sbx-demo.svc:8000" {
			t.Errorf("Target host = %q, want sandbox.sbx-demo.svc:8000", u.Host)
		}
		if u.Scheme != "http" {
			t.Errorf("Target scheme = %q, want http", u.Scheme)
		}
	})

	t.Run("by port number", func(t *testing.T) {
		u, err := c.Target(ctx, "demo", "6080")
		if err != nil {
			t.Fatalf("Target: %v", err)
		}
		if u.Host != "sandbox.sbx-demo.svc:6080" {
			t.Errorf("Target host = %q, want sandbox.sbx-demo.svc:6080", u.Host)
		}
	})

	t.Run("a port the sandbox does not serve", func(t *testing.T) {
		if _, err := c.Target(ctx, "demo", "9999"); err == nil {
			t.Error("Target resolved a port the service does not carry")
		}
	})

	t.Run("a port name that does not exist", func(t *testing.T) {
		if _, err := c.Target(ctx, "demo", "nope"); err == nil {
			t.Error("Target resolved a port name the service does not carry")
		}
	})

	t.Run("a missing sandbox", func(t *testing.T) {
		if _, err := c.Target(ctx, "nope", "api"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Target of a missing sandbox = %v, want ErrNotFound", err)
		}
	})
}

func TestNetworkPolicyAllowsOnlyTheControlPlane(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	np, err := c.cs.NetworkingV1().NetworkPolicies("sbx-demo").Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the network policy: %v", err)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("policy types = %v, want only Ingress — egress is left open on purpose", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
		t.Fatalf("ingress rules = %+v, want exactly one source", np.Spec.Ingress)
	}
	sel := np.Spec.Ingress[0].From[0].NamespaceSelector
	if sel == nil || sel.MatchLabels["kubernetes.io/metadata.name"] != "ops-system" {
		t.Errorf("ingress is not limited to the control plane's namespace: %+v", sel)
	}
}

func TestDeploymentState(t *testing.T) {
	tests := []struct {
		name  string
		dep   appsv1.Deployment
		state model.SandboxState
	}{
		{
			name:  "available",
			dep:   appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: 1}},
			state: model.StateRunning,
		},
		{
			name:  "not created yet",
			dep:   appsv1.Deployment{},
			state: model.StatePending,
		},
		{
			name:  "created but not available",
			dep:   appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: 1}},
			state: model.StatePending,
		},
		{
			name: "replica failure",
			dep: appsv1.Deployment{Status: appsv1.DeploymentStatus{
				Conditions: []appsv1.DeploymentCondition{{
					Type:    appsv1.DeploymentReplicaFailure,
					Status:  corev1.ConditionTrue,
					Reason:  "FailedCreate",
					Message: "quota exceeded",
				}},
			}},
			state: model.StateFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := deploymentState(&tc.dep)
			if state != tc.state {
				t.Errorf("deploymentState = %q, want %q", state, tc.state)
			}
		})
	}
}

func TestExpiredSandboxReportsAsExpired(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate(), TTL: time.Minute}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The container is still running — the reaper has not swept yet — but its
	// time is up, and that is what a caller should be told.
	c.now = func() time.Time { return now.Add(2 * time.Minute) }
	sb, err := c.Get(ctx, "demo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sb.State != model.StateExpired {
		t.Errorf("state = %q, want Expired", sb.State)
	}
}

func TestLogsRequiresTheSandbox(t *testing.T) {
	c := newClient()
	if _, err := c.Logs(context.Background(), "nope", 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("Logs of a missing sandbox = %v, want ErrNotFound", err)
	}
}

// TestLogsFailsWithoutAPod covers the path a missing-pod check exists for.
//
// It is a regression test for a real failure: the pod name was passed to
// GetLogs as the empty string, so `sandbox logs` returned 500 for every
// sandbox. A fake clientset does not reject an empty name and returns no
// content, so the assertion is that the pod is looked up at all — with no pod
// there is nothing to read, and saying so is the difference between the two
// implementations.
func TestLogsFailsWithoutAPod(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Create writes a Deployment, and a real cluster's controller turns that
	// into a Pod; nothing here plays that controller, so there is none.
	if _, err := c.Logs(ctx, "demo", 100); err == nil {
		t.Error("Logs with no pod returned no error; the pod name was never resolved")
	} else if !strings.Contains(err.Error(), "no pod") {
		t.Errorf("Logs with no pod = %v, want an error about the missing pod", err)
	}
}

// TestSandboxPodReadsTheRunningPod asserts the pod is found in the cluster
// rather than built from the Deployment's name — a pod's name carries a suffix
// the controller generates, which nothing outside it can predict.
func TestSandboxPodReadsTheRunningPod(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const podName = "sandbox-7d9f8c4b5-x2jql"
	if _, err := c.cs.CoreV1().Pods("sbx-demo").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   podName,
			Labels: map[string]string{appLabelKey: sandboxName},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	got, err := c.sandboxPod(ctx, "sbx-demo")
	if err != nil {
		t.Fatalf("sandboxPod: %v", err)
	}
	if got != podName {
		t.Errorf("sandboxPod = %q, want %q", got, podName)
	}
}

// TestSandboxPodIgnoresAnotherSandboxesPod asserts the lookup is by the label
// the control plane sets, not by "whatever pod is in the namespace" — the
// namespace holds one sandbox, and reading another's output would be a leak.
func TestSandboxPodIgnoresUnrelatedPods(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, pod := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Labels: map[string]string{"app": "other"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1", Labels: map[string]string{appLabelKey: sandboxName}}},
	} {
		if _, err := c.cs.CoreV1().Pods("sbx-demo").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating %s: %v", pod.Name, err)
		}
	}

	got, err := c.sandboxPod(ctx, "sbx-demo")
	if err != nil {
		t.Fatalf("sandboxPod: %v", err)
	}
	if got != "sandbox-1" {
		t.Errorf("sandboxPod = %q, want sandbox-1", got)
	}
}

// TestLogsSucceedsWithAPod is the other half: the lookup finds one, and the
// stream is read. The content is the fake's placeholder — what is asserted is
// that the path returns rather than erroring.
func TestLogsSucceedsWithAPod(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.cs.CoreV1().Pods("sbx-demo").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "sandbox-abc-1",
			Labels: map[string]string{appLabelKey: sandboxName},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	// A tail of zero is the default path, and was in the same few lines as the
	// bug this covers.
	if _, err := c.Logs(ctx, "demo", 0); err != nil {
		t.Errorf("Logs with a pod: %v", err)
	}
}

func TestResourceQuantityValidation(t *testing.T) {
	// A template with an unparseable quantity should fail the create with a
	// message about the template, not a panic or a 500.
	cs := fake.NewClientset()
	c := NewWithClientset(cs, testConfig())
	tmpl := testTemplate()
	tmpl.Resources.CPU = "not a quantity"

	_, err := c.Create(context.Background(), CreateRequest{ID: "demo", Template: tmpl})
	if err == nil {
		t.Fatal("Create accepted an unparseable cpu quantity")
	}
	if !strings.Contains(err.Error(), "not a quantity") {
		t.Errorf("error = %q, want it to name the bad value", err)
	}
	// And the namespace is not left behind.
	_, getErr := cs.CoreV1().Namespaces().Get(context.Background(), "sbx-demo", metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Error("the namespace survived a failed create")
	}
}
