package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
)

// Executing into a pod is the one thing a clientset cannot do.
//
// `pods/exec` is a subresource reached over an upgraded connection — SPDY on
// the clusters here — and client-go's clientset implements the REST surface,
// not the upgrade. So this file holds the transport. The type that carries it
// is an interface for a second reason: the fake clientset cannot exec either,
// which means the only way to test anything above the transport is to hand it
// something that can pretend.

// Runner performs one command in one container and reports what it printed.
//
// A command that ran and failed is returned as an error — that is how the
// transport reports it — so callers read the exit code out of it with exitCode
// rather than treating every error as a fault.
type Runner interface {
	Stream(ctx context.Context, ns, pod string, opts StreamOptions) error
}

// StreamOptions is one command to run.
type StreamOptions struct {
	Command []string
	// Stdin is nil when the caller sends none, and the distinction matters: the
	// exec request only declares a stdin stream when this is non-nil, and a
	// container that reads its input would otherwise wait on a stream that
	// never closes.
	Stdin io.Reader
	// Stdout and Stderr are nil to discard. Callers in this package always pass
	// a capped writer; a test's runner sees whatever it was handed.
	Stdout io.Writer
	Stderr io.Writer
}

// WithRunner installs an executor, for tests.
//
// A setter rather than another constructor, because NewWithClientset has nine
// call sites across five packages and a new parameter would break all of them
// to serve the few that exec.
func (c *Client) WithRunner(r Runner) *Client {
	c.runner = r
	return c
}

// execRunner is the executor to use: an injected one, or the real transport.
func (c *Client) execRunner() (Runner, error) {
	if c.runner != nil {
		return c.runner, nil
	}
	if c.rc == nil {
		// Reachable only from a client built over an injected clientset with no
		// runner — a test that asked to exec without providing a way to.
		return nil, errors.New("exec is unavailable: this client was built without cluster configuration")
	}
	return &remoteRunner{cs: c.cs, rc: c.rc}, nil
}

// remoteRunner is the real executor.
type remoteRunner struct {
	cs kubernetes.Interface
	rc *rest.Config
}

func (r *remoteRunner) Stream(ctx context.Context, ns, pod string, opts StreamOptions) error {
	executor, err := remotecommand.NewSPDYExecutor(r.rc, http.MethodPost,
		execURL(r.cs.CoreV1().RESTClient(), ns, pod, opts))
	if err != nil {
		return fmt.Errorf("preparing to exec into %s: %w", pod, err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  opts.Stdin,
		Stdout: opts.Stdout,
		Stderr: opts.Stderr,
	})
}

// execURL is the address of a pod's exec subresource, with the command and the
// stream flags in the query string.
//
// It takes the REST client rather than the whole clientset because that is all
// it uses, and because the fake clientset's is nil — a test that wants to check
// the request has to bring a real one, and the narrow parameter says so.
//
// Split out from Stream so the request can be asserted without an upgraded
// connection: everything up to the handshake is an ordinary REST request, and
// the handshake is the one part that needs a real kubelet.
func execURL(restClient rest.Interface, ns, pod string, opts StreamOptions) *url.URL {
	req := restClient.Post().
		Resource("pods").
		Namespace(ns).
		Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   opts.Command,
			Stdin:     opts.Stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)
	return req.URL()
}

// exitCode reads the exit status out of a runner's error.
//
// The status arrives as client-go's exec.ExitError, so a command that exited
// non-zero is distinguishable from one that never ran — which is the whole
// difference between reporting an exit code and reporting a broken cluster.
func exitCode(err error) (int, bool) {
	var e exec.ExitError
	if errors.As(err, &e) {
		return e.ExitStatus(), true
	}
	return 0, false
}
