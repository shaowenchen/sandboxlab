package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEventsComeFromTheSandboxNamespace(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ns := c.cfg.SandboxNamespace("demo")
	_, err := c.cs.CoreV1().Events(ns).Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "sandbox"},
		Reason:         "Failed",
		Message:        "pull failed",
		Type:           corev1.EventTypeWarning,
		Count:          2,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating an event: %v", err)
	}

	events, err := c.Events(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].Reason != "Failed" || events[0].Object != "Pod/sandbox" || events[0].Count != 2 {
		t.Errorf("event = %+v, want the Failed one about Pod/sandbox", events[0])
	}
}

func TestEventsWarningsComeFirst(t *testing.T) {
	c := newClient()
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := c.cfg.SandboxNamespace("demo")

	mk := func(name, typ string) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: ns},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "sandbox"},
			Reason:         name, Message: name, Type: typ, Count: 1,
		}
	}
	for _, e := range []*corev1.Event{mk("normal", corev1.EventTypeNormal), mk("warn", corev1.EventTypeWarning)} {
		if _, err := c.cs.CoreV1().Events(ns).Create(ctx, e, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating an event: %v", err)
		}
	}

	events, err := c.Events(ctx, "demo", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 || events[0].Type != corev1.EventTypeWarning {
		t.Errorf("events = %+v, want the Warning first", events)
	}
}

// Usage on a client with no metrics API answers "not available" rather than zero
// or an error — the cluster is a legitimate one, it just cannot report usage.
func TestUsageWithoutMetricsIsUnavailable(t *testing.T) {
	c := newClient() // no dynamic client
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	u, err := c.Usage(ctx, "demo")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Available {
		t.Errorf("usage = %+v, want available:false with no metrics API", u)
	}
	if u.CPU != "" || u.Memory != "" {
		t.Errorf("usage reported figures with no metrics API: %+v", u)
	}
}
