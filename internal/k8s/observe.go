package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// podMetricsGVR names the resource metrics API for the dynamic client.
//
// v1beta1, not v1: metrics-server registers exactly one version, and in every
// released version that version is v1beta1 — only master adds a v1 line. A
// request for a version a group does not serve is answered 404, which PodUsage
// treats as "this cluster has no metrics API", so asking for v1 would fail
// silently on every real cluster and look identical to an idle one.
// `kubectl top` reads v1beta1 for the same reason.
//
// The dynamic client rather than the k8s.io/metrics module, because this reads
// two numbers out of one resource; the module would be a second Kubernetes API
// surface to keep in step with client-go to avoid reading a map.
var podMetricsGVR = schema.GroupVersionResource{
	Group:    "metrics.k8s.io",
	Version:  "v1beta1",
	Resource: "pods",
}

// Usage is how much CPU and memory a sandbox is using right now, summed across
// its pod's containers.
//
// The quantities are the strings the metrics API reports — "12m", "48Mi" —
// rather than parsed numbers, because CPU is a ratio and memory is bytes and
// one field holding either would have to say which.
type Usage struct {
	// Available is whether the cluster could report usage at all. False on a
	// cluster with no metrics-server, which is a legitimate way to run one — a
	// caller then says "unavailable" rather than showing zeros that read as an
	// idle sandbox.
	Available bool   `json:"available"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// Event is one Kubernetes event about a sandbox's objects.
type Event struct {
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Object    string    `json:"object"`
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Usage reads what a sandbox's pod is using.
//
// A sandbox is a namespace, so this is one lookup rather than a filter across a
// shared one. A cluster without the metrics API answers 404 for the whole
// resource — a missing optional component, not a fault — which is reported as
// unavailable rather than as an error, so a console can say "no metrics here"
// instead of "0%". Anything else is returned.
func (c *Client) Usage(ctx context.Context, id string) (Usage, error) {
	if c.dynamic == nil {
		return Usage{}, nil
	}
	ns := c.cfg.SandboxNamespace(id)
	list, err := c.dynamic.Resource(podMetricsGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: appLabelKey + "=" + sandboxName,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Usage{}, nil // no metrics API this client can read
		}
		return Usage{}, fmt.Errorf("reading usage in %s: %w", ns, err)
	}

	out := Usage{Available: true}
	totalCPU := resource.NewQuantity(0, resource.DecimalSI)
	totalMem := resource.NewQuantity(0, resource.BinarySI)
	for i := range list.Items {
		item := &list.Items[i]
		if stamp, _, _ := unstructured.NestedString(item.Object, "timestamp"); stamp != "" {
			out.Timestamp = stamp
		}
		// Usage is per container, and a pod's is their sum. Summed rather than
		// picked from one container because a sidecar is still the sandbox's
		// cost; see sumContainerUsage for why an unparsed value is skipped
		// rather than failing the read.
		if q, ok := sumContainerUsage(item.Object, "cpu"); ok {
			totalCPU.Add(q)
		}
		if q, ok := sumContainerUsage(item.Object, "memory"); ok {
			totalMem.Add(q)
		}
	}
	// A zero that was actually summed from samples is meaningful; a cluster that
	// answered with no samples reports nothing rather than zeros.
	if !totalCPU.IsZero() {
		out.CPU = totalCPU.String()
	}
	if !totalMem.IsZero() {
		out.Memory = totalMem.String()
	}
	return out, nil
}

// sumContainerUsage adds one resource across a PodMetrics' containers.
//
// The second return says whether any container reported it, which keeps "no
// sample yet" apart from a real zero. A quantity that does not parse is skipped
// rather than failing the read: this is a display path, and the rest of the
// containers are still worth reporting.
func sumContainerUsage(item map[string]any, name string) (resource.Quantity, bool) {
	containers, ok := item["containers"].([]any)
	if !ok {
		return resource.Quantity{}, false
	}
	total := resource.NewQuantity(0, resource.DecimalSI)
	seen := false
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Read as a string, not a number: the API reports quantities with
		// suffixes ("12m", "48Mi") and a numeric accessor would fail on all of
		// them.
		value, _, _ := unstructured.NestedString(container, "usage", name)
		if value == "" {
			continue
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			continue
		}
		total.Add(q)
		seen = true
	}
	if !seen {
		return resource.Quantity{}, false
	}
	return *total, true
}

// Events reads recent Kubernetes events about a sandbox's objects.
//
// Events are how a sandbox explains itself when the pod list cannot: why it was
// never scheduled, why an image pull failed, why a probe killed the container.
// They expire after about an hour, so this reads what is there now. A sandbox is
// a namespace, so every event in it is the sandbox's and no attribution is
// needed.
func (c *Client) Events(ctx context.Context, id string, limit int) ([]Event, error) {
	ns := c.cfg.SandboxNamespace(id)
	list, err := c.cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing events in %s: %w", ns, err)
	}

	out := make([]Event, 0, len(list.Items))
	for i := range list.Items {
		e := &list.Items[i]
		lastSeen := firstNonZeroTime(e.LastTimestamp.Time, e.EventTime.Time, e.CreationTimestamp.Time)
		firstSeen := firstNonZeroTime(e.FirstTimestamp.Time, e.CreationTimestamp.Time)
		out = append(out, Event{
			Type:      e.Type,
			Reason:    e.Reason,
			Message:   e.Message,
			Object:    e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count:     e.Count,
			FirstSeen: firstSeen.UTC(),
			LastSeen:  lastSeen.UTC(),
		})
	}

	// Warnings first, then newest first within each group. A purely recent-first
	// list buries the event that explains a failure among routine pull and
	// scheduling notices, which are the majority in a healthy namespace and are
	// never what a reader is looking for.
	sort.Slice(out, func(i, j int) bool {
		iWarn := out[i].Type == corev1.EventTypeWarning
		jWarn := out[j].Type == corev1.EventTypeWarning
		if iWarn != jWarn {
			return iWarn
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// firstNonZeroTime returns the first time that is set. An event that has not
// been updated yet carries only its creation time, and without this fallback it
// would sort as if it were ancient.
func firstNonZeroTime(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
