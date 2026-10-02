package reaper

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

// testCluster is a fake cluster plus a client over it.
//
// The clientset is kept so a test can write a namespace directly. That is how
// an *already expired* sandbox is produced: no path through the API can make
// one — a create with a past deadline is rejected, and waiting is not a test —
// so the state is written the way the control plane writes it, and the reaper
// is asked to find it. The label and annotation names are spelled out rather
// than imported because they are the contract between the two packages: if
// either side renames one, this test should fail.
type testCluster struct {
	client *k8s.Client
	cs     *fake.Clientset
}

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelSandboxID = "sandbox.sandboxlab/id"
	labelTemplate  = "sandbox.sandboxlab/template"
	annoExpiresAt  = "sandbox.sandboxlab/expires-at"
)

func newTestCluster() *testCluster {
	cs := fake.NewClientset()
	return &testCluster{
		cs: cs,
		client: k8s.NewWithClientset(cs, config.Config{
			SandboxNamespacePrefix: "sbx-",
			MaxTTL:                 8 * time.Hour,
		}),
	}
}

func newTestReaper(tc *testCluster) *Reaper {
	return New(tc.client, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// create makes a live sandbox through the same path a real create takes.
func (tc *testCluster) create(t *testing.T, id string, ttl time.Duration) {
	t.Helper()
	if _, err := tc.client.Create(context.Background(), k8s.CreateRequest{
		ID:       id,
		Template: model.Template{ID: "python", Image: "python:3.12-slim"},
		TTL:      ttl,
	}); err != nil {
		t.Fatalf("creating sandbox %s: %v", id, err)
	}
}

// expire writes a sandbox that is already past its deadline.
func (tc *testCluster) expire(t *testing.T, id string, expiredAt time.Time) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "sbx-" + id,
		Labels: map[string]string{
			labelManagedBy: "sandboxlab",
			labelSandboxID: id,
			labelTemplate:  "python",
		},
		Annotations: map[string]string{
			annoExpiresAt: expiredAt.UTC().Format(time.RFC3339),
		},
	}}
	if _, err := tc.cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("writing the expired sandbox %s: %v", id, err)
	}
}

func (tc *testCluster) list(t *testing.T) []model.Sandbox {
	t.Helper()
	all, err := tc.client.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return all
}

func TestSweepDeletesOnlyExpiredSandboxes(t *testing.T) {
	tc := newTestCluster()
	ctx := context.Background()

	tc.expire(t, "past", time.Now().Add(-time.Minute))
	tc.create(t, "future", time.Hour)
	tc.create(t, "forever", 0)

	newTestReaper(tc).Sweep(ctx)

	all := tc.list(t)
	if len(all) != 2 {
		t.Fatalf("after the sweep %d sandboxes remain, want 2: %+v", len(all), all)
	}
	for _, sb := range all {
		if sb.ID == "past" {
			t.Error("the expired sandbox survived the sweep")
		}
	}
}

func TestSweepWithNothingDue(t *testing.T) {
	tc := newTestCluster()
	tc.create(t, "future", time.Hour)

	newTestReaper(tc).Sweep(context.Background())

	if all := tc.list(t); len(all) != 1 {
		t.Errorf("a sweep with nothing due removed %d sandbox(es)", 1-len(all))
	}
}

func TestSweepOnAnEmptyCluster(t *testing.T) {
	// The state an environment spends most of its life in. It must not panic
	// or log a failure.
	newTestReaper(newTestCluster()).Sweep(context.Background())
}

func TestSweepDeletesEveryExpiredSandbox(t *testing.T) {
	tc := newTestCluster()
	for _, id := range []string{"one", "two", "three"} {
		tc.expire(t, id, time.Now().Add(-time.Minute))
	}

	newTestReaper(tc).Sweep(context.Background())

	if all := tc.list(t); len(all) != 0 {
		t.Errorf("%d expired sandboxes survived the sweep: %+v", len(all), all)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	tc := newTestCluster()
	tc.expire(t, "past", time.Now().Add(-time.Minute))
	r := newTestReaper(tc)

	// A second sweep finds the sandbox already gone. That is the outcome the
	// loop wanted, not an error worth reporting — a reaper that warned on every
	// tick after a delete would drown its own log.
	r.Sweep(context.Background())
	r.Sweep(context.Background())

	if all := tc.list(t); len(all) != 0 {
		t.Errorf("the sandbox came back: %+v", all)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	r := newTestReaper(newTestCluster())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestRunWithADisabledInterval(t *testing.T) {
	// An interval of zero means the reaper is off, so Run returns at once — a
	// deployment that would rather expire sandboxes by hand is a legitimate
	// configuration, and it should not cost a goroutine that ticks forever.
	r := New(newTestCluster().client, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan struct{})
	go func() {
		r.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with a zero interval did not return")
	}
}

func TestRunSweepsOnItsInterval(t *testing.T) {
	tc := newTestCluster()
	tc.expire(t, "past", time.Now().Add(-time.Minute))

	r := New(tc.client, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	// The first sweep is one interval away. This waits for its effect rather
	// than for a fixed duration, so a slow machine does not make it flaky.
	deadline := time.After(5 * time.Second)
	for {
		all := tc.list(t)
		if len(all) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the reaper did not collect the expired sandbox within 5s: %+v", all)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
