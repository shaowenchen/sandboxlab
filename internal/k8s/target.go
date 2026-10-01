package k8s

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Target resolves where a sandbox's port is reached from inside the cluster.
//
// The port is looked up in the sandbox's Service rather than trusted from the
// request: the Service is what exists, and a name it does not carry is a
// sandbox that cannot be reached — which is a 404 about the port, not a proxy
// to somewhere unintended.
func (c *Client) Target(ctx context.Context, id, port string) (*url.URL, error) {
	sb, err := c.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	svc, err := c.cs.CoreV1().Services(sb.Namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the sandbox %q has no service yet", id)
	}
	var resolved int32
	found := false
	for _, p := range svc.Spec.Ports {
		if p.Name == port {
			resolved, found = p.Port, true
			break
		}
	}
	if !found {
		// A numeric port is accepted as a fallback, so an address that was
		// bookmarked before a template changed its port names still works —
		// but only if the Service really carries it.
		if n, err := strconv.Atoi(port); err == nil {
			for _, p := range svc.Spec.Ports {
				if int(p.Port) == n {
					resolved, found = p.Port, true
					break
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("the sandbox %q serves no port named %q", id, port)
	}

	// The sandbox's own Service, by its cluster DNS name. It is reached from
	// this process, which is in the cluster, so no ingress or node port is
	// involved: the hop the caller made through the tunnel ends here. The
	// scheme is plain HTTP because every image in the catalog serves HTTP and
	// TLS would be the proxy's business, not theirs.
	return &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s.%s.svc:%d", sandboxName, sb.Namespace, resolved),
	}, nil
}
