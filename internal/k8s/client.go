// Package k8s is the cluster half of the control plane: it creates, reads and
// removes sandboxes, and it owns the one rule that makes them possible — a
// sandbox is a namespace.
//
// Everything the API reports about a sandbox is read back from the cluster
// here. The control plane keeps no record of its own, deliberately: a sandbox
// that exists in the cluster is real whether or not the process that made it is
// still running, and one that does not is gone whatever a database might say.
// That is what lets this run happily on an ephemeral GitHub runner.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

// Labels and annotations the control plane puts on what it creates. They are
// the interface between this package and a restart of the process: everything
// not derivable from the objects themselves is written here.
const (
	// managedByLabel marks a namespace as one this control plane owns. Listing
	// sandboxes is a label query for it, so nothing else on the cluster is ever
	// a candidate for deletion.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "sandboxlab"

	templateLabel  = "sandbox.sandboxlab/template"
	sandboxIDLabel = "sandbox.sandboxlab/id"
	appLabelKey    = "sandbox.sandboxlab/app"

	createdAtAnnotation = "sandbox.sandboxlab/created-at"
	expiresAtAnnotation = "sandbox.sandboxlab/expires-at"
	imageAnnotation     = "sandbox.sandboxlab/image"

	containerName = "sandbox"
	// sandboxName is the fixed name of the Deployment and Service inside a
	// sandbox's namespace. One sandbox per namespace means one name is enough,
	// and a fixed name is what makes reading them back a lookup rather than a
	// search.
	sandboxName = "sandbox"
)

// ErrNotFound is returned for a sandbox that does not exist.
//
// Callers match it with errors.Is; the message always names the sandbox, which
// is what a person reading an API error is looking for — "not found" alone
// leaves them to guess which of several names they mistyped.
var ErrNotFound = errors.New("sandbox not found")

// notFound builds the error for a missing sandbox.
func notFound(id string) error {
	return fmt.Errorf("%w: no sandbox named %q", ErrNotFound, id)
}

// ErrAlreadyExists is returned when a sandbox's name is taken.
var ErrAlreadyExists = fmt.Errorf("sandbox already exists")

// Client talks to the cluster.
type Client struct {
	cs  kubernetes.Interface
	cfg config.Config
	now func() time.Time

	// rc is the rest config the clientset was built from. It is kept for exec
	// alone: everything else goes through the clientset, and only the upgraded
	// connection of pods/exec needs the transport underneath it. nil when the
	// client was built over an injected clientset.
	rc *rest.Config

	// runner overrides the real executor. nil means "build one from rc" — see
	// execRunner. It exists because neither the clientset interface nor its
	// fake can exec.
	runner Runner

	// dynamic reads the resource metrics API, which is a group the typed
	// clientset does not carry. nil on a client built over an injected
	// clientset, in which case usage is reported as unavailable rather than
	// as zero — see PodUsage.
	dynamic dynamic.Interface
}

// New builds a client from the resolved configuration.
//
// It tries in-cluster configuration first — the case that matters, because the
// control plane runs as a pod — and falls back to a kubeconfig, which is what a
// local run against a kind cluster uses.
func New(cfg config.Config) (*Client, error) {
	rc, err := restConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	rc.UserAgent = "sandboxlab/" + rc.UserAgent
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("building the Kubernetes client: %w", err)
	}
	// The dynamic client is for the resource metrics API alone — see PodUsage.
	// Built only when the typed one was, so a deployment reaches metrics by the
	// same route it reaches everything else, or not at all.
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("building the dynamic client: %w", err)
	}
	// NewForConfig mutates the config it is given — it installs rate limiters
	// and wraps the transport — so the copy kept for exec is taken from what it
	// returns rather than sharing its mutable state with the clientset's.
	return &Client{cs: cs, cfg: cfg, now: time.Now, rc: rest.CopyConfig(rc), dynamic: dyn}, nil
}

// NewWithClientset builds a client over an injected clientset, for tests.
//
// It carries no rest config, so exec is unavailable unless a runner is
// installed with WithRunner. That is deliberate: the fake clientset cannot
// exec, and a client that quietly fell back to the real transport would try to
// reach a cluster the test never had.
func NewWithClientset(cs kubernetes.Interface, cfg config.Config) *Client {
	return &Client{cs: cs, cfg: cfg, now: time.Now}
}

// Ready reports whether the cluster answers. It is what /readyz checks, so a
// control plane that cannot reach a cluster says so rather than serving a
// console that fails on every action.
func (c *Client) Ready(ctx context.Context) bool {
	_, err := c.cs.Discovery().ServerVersion()
	return err == nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if rc, err := rest.InClusterConfig(); err == nil {
		return rc, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("resolving cluster credentials: %w", err)
	}
	return rc, nil
}

// CreateRequest is what a caller asks for, already resolved: template chosen,
// TTL clamped, id decided. This package only has to build the objects.
type CreateRequest struct {
	ID       string
	Template model.Template
	// TTL is how long the sandbox may live. Zero means it has no expiry.
	TTL time.Duration
	// Env is the caller's environment, merged over the template's.
	Env map[string]string
}

// Create builds a sandbox: a namespace of its own, the sandbox Deployment, the
// namespace's own quota and network policy, and — when the template serves
// anything — a Service.
//
// The namespace comes first and is the unit of everything. If any later step
// fails the namespace is removed, so a half-made sandbox is never left behind
// to be discovered later as an orphan.
func (c *Client) Create(ctx context.Context, req CreateRequest) (model.Sandbox, error) {
	now := c.now()
	var expires time.Time
	if req.TTL > 0 {
		expires = now.Add(req.TTL)
	}

	if err := c.createNamespace(ctx, req, now, expires); err != nil {
		return model.Sandbox{}, err
	}

	ns := c.cfg.SandboxNamespace(req.ID)
	// From here on, anything that fails takes the namespace with it — a fresh
	// context, because the caller's may already be cancelled and the cleanup
	// has to happen regardless.
	cleanup := func(err error) (model.Sandbox, error) {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_ = c.cs.CoreV1().Namespaces().Delete(delCtx, ns, metav1.DeleteOptions{})
		return model.Sandbox{}, err
	}

	if err := c.createQuota(ctx, ns, req.Template.Resources); err != nil {
		return cleanup(err)
	}
	if err := c.createNetworkPolicy(ctx, ns); err != nil {
		return cleanup(err)
	}
	if err := c.createDeployment(ctx, ns, req); err != nil {
		return cleanup(err)
	}

	// A Service is created only when the template serves a port. One with no
	// ports is not merely pointless — Kubernetes refuses it, because a Service
	// is an address and ports are what it publishes (`spec.ports` is required
	// unless the Service is headless or ExternalName). A template with no ports
	// runs no service at all and is driven through exec and the file endpoints,
	// so the missing Service is the correct state and `Target` has nothing to
	// resolve for it.
	if hasPorts(req.Template.Ports) {
		if err := c.createService(ctx, ns, req.Template); err != nil {
			return cleanup(err)
		}
	}

	return c.Get(ctx, req.ID)
}

// hasPorts reports whether a template publishes anything to reach over HTTP.
func hasPorts(ports []model.Port) bool { return len(ports) > 0 }

func (c *Client) createNamespace(ctx context.Context, req CreateRequest, created, expires time.Time) error {
	annotations := map[string]string{
		createdAtAnnotation: created.UTC().Format(time.RFC3339),
		imageAnnotation:     req.Template.Image,
	}
	if !expires.IsZero() {
		annotations[expiresAtAnnotation] = expires.UTC().Format(time.RFC3339)
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.cfg.SandboxNamespace(req.ID),
			Labels: map[string]string{
				managedByLabel: managedByValue,
				sandboxIDLabel: req.ID,
				templateLabel:  req.Template.ID,
			},
			Annotations: annotations,
		},
	}
	if _, err := c.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%w: a sandbox named %q already exists", ErrAlreadyExists, req.ID)
		}
		return fmt.Errorf("creating namespace %s: %w", ns.Name, err)
	}
	return nil
}

func (c *Client) createQuota(ctx context.Context, ns string, res model.Resources) error {
	// A template may name cpu/memory, in which case the quota bounds what one
	// pod may ask for and what the namespace may hold in total — without the
	// first a single pod can request more than the node has and stay Pending
	// forever, and without the second several pods can do the same together.
	// Most templates name none: a sandbox then takes what the node has, and the
	// quota bounds only the pod count.
	spec := corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{}}
	if q, err := quantity(res.CPU); err != nil {
		return fmt.Errorf("template cpu %q: %w", res.CPU, err)
	} else if q != nil {
		spec.Hard[corev1.ResourceRequestsCPU] = *q
		spec.Hard[corev1.ResourceLimitsCPU] = *q
	}
	if q, err := quantity(res.Memory); err != nil {
		return fmt.Errorf("template memory %q: %w", res.Memory, err)
	} else if q != nil {
		spec.Hard[corev1.ResourceRequestsMemory] = *q
		spec.Hard[corev1.ResourceLimitsMemory] = *q
	}
	spec.Hard[corev1.ResourcePods] = *resource.NewQuantity(4, resource.DecimalSI)

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: ns, Labels: ownerLabels()},
		Spec:       spec,
	}
	if _, err := c.cs.CoreV1().ResourceQuotas(ns).Create(ctx, quota, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the resource quota: %w", err)
	}
	return nil
}

func (c *Client) createNetworkPolicy(ctx context.Context, ns string) error {
	// Ingress is allowed only from the control plane's namespace, which is what
	// serves the data-plane proxy and the console. Egress is left open: the
	// point of most of these sandboxes is to reach the network — install a
	// package, call an API — and a policy that blocked it would make the
	// environment useless for the reason it exists.
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: ns, Labels: ownerLabels()},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // every pod in the namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": c.cfg.Namespace},
					},
				}},
			}},
		},
	}
	if _, err := c.cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the network policy: %w", err)
	}
	return nil
}

func (c *Client) createDeployment(ctx context.Context, ns string, req CreateRequest) error {
	container := corev1.Container{
		Name:    containerName,
		Image:   req.Template.Image,
		Command: req.Template.Command,
		Args:    req.Template.Args,
		Env:     envVars(mergeEnv(req.Template.Env, req.Env)),
		Ports:   containerPorts(req.Template.Ports),
	}
	container.Resources = resources(req.Template.Resources)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        sandboxName,
			Namespace:   ns,
			Labels:      ownerLabels(),
			Annotations: map[string]string{imageAnnotation: req.Template.Image},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: sandboxName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabelKey: sandboxName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					// A sandbox has no state worth a graceful drain, and a
					// termination that waits out the default thirty seconds is
					// thirty seconds a caller spends waiting for `rm`. Two
					// seconds is enough for a clean shutdown that wants one.
					TerminationGracePeriodSeconds: ptr(int64(2)),
				},
			},
		},
	}
	if _, err := c.cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the sandbox deployment: %w", err)
	}
	return nil
}

func (c *Client) createService(ctx context.Context, ns string, t model.Template) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxName, Namespace: ns, Labels: ownerLabels()},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{appLabelKey: sandboxName},
			Ports:    servicePorts(t.Ports),
		},
	}
	if _, err := c.cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the sandbox service: %w", err)
	}
	return nil
}

// Get reads one sandbox back from the cluster.
func (c *Client) Get(ctx context.Context, id string) (model.Sandbox, error) {
	ns := c.cfg.SandboxNamespace(id)
	obj, err := c.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return model.Sandbox{}, notFound(id)
		}
		return model.Sandbox{}, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if obj.Labels[managedByLabel] != managedByValue {
		// The namespace exists but is not ours. Reporting it as absent is the
		// honest answer: this control plane does not manage it, so it is not a
		// sandbox as far as any caller is concerned — and it must never be
		// deleted by one.
		return model.Sandbox{}, notFound(id)
	}
	return c.describe(ctx, obj)
}

// List returns every sandbox this control plane manages.
func (c *Client) List(ctx context.Context) ([]model.Sandbox, error) {
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("listing sandboxes: %w", err)
	}
	out := make([]model.Sandbox, 0, len(list.Items))
	for i := range list.Items {
		sb, err := c.describe(ctx, &list.Items[i])
		if err != nil {
			// One unreadable sandbox should not hide the rest. It is reported
			// as present-but-broken rather than dropped, because dropping it
			// would make it invisible in the console and therefore un-deletable.
			sb = model.Sandbox{
				ID:        list.Items[i].Labels[sandboxIDLabel],
				Template:  list.Items[i].Labels[templateLabel],
				Namespace: list.Items[i].Name,
				State:     model.StateFailed,
				Message:   err.Error(),
			}
		}
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// describe turns a namespace and what it holds into a Sandbox.
func (c *Client) describe(ctx context.Context, ns *corev1.Namespace) (model.Sandbox, error) {
	sb := model.Sandbox{
		ID:        ns.Labels[sandboxIDLabel],
		Template:  ns.Labels[templateLabel],
		Image:     ns.Annotations[imageAnnotation],
		Namespace: ns.Name,
		State:     model.StatePending,
	}
	if sb.ID == "" {
		// An object from an older version predates the label; the namespace
		// name is the id by construction, so it can be recovered.
		sb.ID = strings.TrimPrefix(ns.Name, c.cfg.SandboxNamespacePrefix)
	}
	if t, err := time.Parse(time.RFC3339, ns.Annotations[createdAtAnnotation]); err == nil {
		sb.CreatedAt = t
	} else {
		sb.CreatedAt = ns.CreationTimestamp.Time
	}
	if v := ns.Annotations[expiresAtAnnotation]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			// A pointer, so "no expiry" stays the absence of the field.
			sb.ExpiresAt = &t
		}
	}

	var ports []model.Port
	dep, err := c.cs.AppsV1().Deployments(ns.Name).Get(ctx, sandboxName, metav1.GetOptions{})
	switch {
	case err == nil:
		sb.State, sb.Message = deploymentState(dep)
		if len(dep.Spec.Template.Spec.Containers) > 0 {
			ctr := dep.Spec.Template.Spec.Containers[0]
			if sb.Image == "" {
				sb.Image = ctr.Image
			}
			sb.Env = envMap(ctr.Env)
		}
	case apierrors.IsNotFound(err):
		sb.State = model.StateFailed
		sb.Message = "the sandbox deployment is missing"
	default:
		return sb, fmt.Errorf("reading the sandbox deployment in %s: %w", ns.Name, err)
	}

	// The ports come from the Service, which is the object that carries them,
	// rather than from the catalog — the template may have moved on since this
	// sandbox was created, and the Service is what is actually there.
	if svc, err := c.cs.CoreV1().Services(ns.Name).Get(ctx, sandboxName, metav1.GetOptions{}); err == nil {
		for _, p := range svc.Spec.Ports {
			ports = append(ports, model.Port{Name: p.Name, Port: p.Port, Protocol: string(p.Protocol)})
		}
	}
	sb.Endpoints = c.endpoints(sb.ID, ports)

	if sb.Expired(c.now()) {
		sb.State = model.StateExpired
	}
	return sb, nil
}

// endpoints builds the reachable URLs for a sandbox, one per declared port.
func (c *Client) endpoints(id string, ports []model.Port) []model.Endpoint {
	if len(ports) == 0 {
		return nil
	}
	// The data plane serves a sandbox at <public>/sandbox/<id>/<port>/ — the
	// port name is in the path because a sandbox with a browser and an API has
	// two addresses and only one hostname to put them on.
	base := c.cfg.URL("/sandbox/" + id)
	out := make([]model.Endpoint, 0, len(ports))
	for _, p := range ports {
		out = append(out, model.Endpoint{Name: p.Name, Port: p.Port, URL: base + "/" + p.Name + "/"})
	}
	return out
}

// Delete removes a sandbox, and everything in it, by removing its namespace.
//
// That is the whole implementation, and it is the reason a sandbox is a
// namespace: one call reclaims the Deployment, the Service, the quota, the
// policy and anything else created inside it, with no list to keep in step and
// nothing to miss.
func (c *Client) Delete(ctx context.Context, id string) error {
	ns := c.cfg.SandboxNamespace(id)
	obj, err := c.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return notFound(id)
		}
		return fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if obj.Labels[managedByLabel] != managedByValue {
		return notFound(id)
	}
	if err := c.cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", ns, err)
	}
	return nil
}

// Renew resets a sandbox's expiry and returns it as it now stands.
func (c *Client) Renew(ctx context.Context, id string, ttl time.Duration) (model.Sandbox, error) {
	ns := c.cfg.SandboxNamespace(id)
	obj, err := c.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return model.Sandbox{}, notFound(id)
		}
		return model.Sandbox{}, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if obj.Labels[managedByLabel] != managedByValue {
		return model.Sandbox{}, notFound(id)
	}
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	if ttl > 0 {
		obj.Annotations[expiresAtAnnotation] = c.now().Add(ttl).UTC().Format(time.RFC3339)
	} else {
		// Zero means "remove the expiry", which is a real request: a sandbox
		// someone is working in should be able to be made to stop disappearing.
		delete(obj.Annotations, expiresAtAnnotation)
	}
	if _, err := c.cs.CoreV1().Namespaces().Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return model.Sandbox{}, fmt.Errorf("updating namespace %s: %w", ns, err)
	}
	return c.describe(ctx, obj)
}

// Expired returns the sandboxes whose TTL has passed.
//
// The check reads the annotation rather than anything a reaper remembers, so it
// is correct after a restart and correct for a sandbox this process never
// created — which is the whole reason the TTL is stored on the object.
func (c *Client) Expired(ctx context.Context) ([]model.Sandbox, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	now := c.now()
	var out []model.Sandbox
	for _, sb := range all {
		if sb.ExpiresAt == nil || now.Before(*sb.ExpiresAt) {
			continue
		}
		out = append(out, sb)
	}
	return out, nil
}

// Logs returns the tail of a sandbox's output.
func (c *Client) Logs(ctx context.Context, id string, tail int64) (string, error) {
	ns := c.cfg.SandboxNamespace(id)
	if _, err := c.Get(ctx, id); err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = 200
	}

	pod, err := c.sandboxPod(ctx, ns)
	if err != nil {
		return "", err
	}

	req := c.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("reading sandbox logs: %w", err)
	}
	defer stream.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, stream); err != nil {
		return "", fmt.Errorf("reading sandbox logs: %w", err)
	}
	return b.String(), nil
}

// sandboxPod returns the name of the pod running a sandbox.
//
// The name is read rather than built: a pod's is its Deployment's plus a suffix
// the controller generates, and guessing that suffix is not possible. It is a
// function of its own because the empty name it replaced could not be caught
// where the logs are read — client-go's fake does not carry the pod name into
// the action it dispatches for GetLogs, so a test there cannot see which pod
// was asked for, while this can.
func (c *Client) sandboxPod(ctx context.Context, ns string) (string, error) {
	pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: appLabelKey + "=" + sandboxName,
	})
	if err != nil {
		return "", fmt.Errorf("finding the sandbox's pod: %w", err)
	}
	// One sandbox is one replica, which is what makes the first match the
	// answer.
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("the sandbox in %s has no pod yet", ns)
	}
	return pods.Items[0].Name, nil
}

// deploymentState reduces a Deployment to the state a caller cares about.
func deploymentState(dep *appsv1.Deployment) (model.SandboxState, string) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return model.StateFailed, strings.TrimSpace(c.Message + " " + c.Reason)
		}
	}
	if dep.Status.AvailableReplicas > 0 {
		return model.StateRunning, ""
	}
	if dep.Status.Replicas == 0 {
		return model.StatePending, "the sandbox is being created"
	}
	// Pods exist but none is available: the useful thing to say is that it is
	// on its way, not a state name.
	return model.StatePending, "the sandbox is starting"
}

func resources(r model.Resources) corev1.ResourceRequirements {
	var out corev1.ResourceRequirements
	if q, err := quantity(r.CPU); err == nil && q != nil {
		out.Requests = corev1.ResourceList{corev1.ResourceCPU: *q}
		out.Limits = corev1.ResourceList{corev1.ResourceCPU: *q}
	}
	if q, err := quantity(r.Memory); err == nil && q != nil {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceMemory] = *q
		out.Limits[corev1.ResourceMemory] = *q
	}
	return out
}

func envVars(m map[string]string) []corev1.EnvVar {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// envMap is envVars reversed, skipping a value a pod spec took from a Secret —
// those are not readable here and must not be reported as if they were.
func envMap(vars []corev1.EnvVar) map[string]string {
	out := make(map[string]string)
	for _, v := range vars {
		if v.ValueFrom != nil {
			continue
		}
		out[v.Name] = v.Value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func containerPorts(ports []model.Port) []corev1.ContainerPort {
	out := make([]corev1.ContainerPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, corev1.ContainerPort{
			Name:          p.Name,
			ContainerPort: p.Port,
			Protocol:      protocol(p.Protocol),
		})
	}
	return out
}

func servicePorts(ports []model.Port) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		out = append(out, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   protocol(p.Protocol),
		})
	}
	return out
}

func protocol(p string) corev1.Protocol {
	if strings.EqualFold(p, "UDP") {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

func ownerLabels() map[string]string {
	return map[string]string{managedByLabel: managedByValue, appLabelKey: sandboxName}
}

func quantity(s string) (*resource.Quantity, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func ptr[T any](v T) *T { return &v }
