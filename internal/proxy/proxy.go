// Package proxy serves a sandbox's own ports to a caller outside the cluster.
//
// It exists because of where the sandbox runs. A sandbox has a Service inside
// the cluster and nothing outside it: it is created on demand, so there is no
// ingress rule per sandbox waiting on a hostname, and no node port to hand out.
// The control plane is the one thing that already has an address, so it is what
// forwards — one VirtualService for the whole deployment instead of one object
// per sandbox, which is also why creating a sandbox needs no ingress write.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
)

// Resolver finds where a sandbox's port is reached inside the cluster.
type Resolver interface {
	Target(ctx context.Context, id, port string) (*url.URL, error)
}

// Handler is the data-plane proxy.
type Handler struct {
	cfg      config.Config
	resolver Resolver
	log      *slog.Logger
}

// New builds the proxy.
func New(cfg config.Config, resolver Resolver, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, resolver: resolver, log: log}
}

// Serve implements api.DataPlane.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, id, port, rest string) {
	// The deadline is on the lookup, not on the proxied request: a sandbox may
	// stream for an hour, but finding where to send it is a cluster read that
	// should not hang.
	ctx, cancel := context.WithTimeout(r.Context(), resolveTimeout)
	target, err := h.resolver.Target(ctx, id, port)
	cancel()
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, k8s.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	// The public address is /sandbox/<id>/<port>/<rest>; the sandbox serves
	// <rest> at its own root. The path is rewritten explicitly rather than left
	// to the reverse proxy's join, because SetURL appends the *incoming* path —
	// which would send "/sandbox/demo/api/health" to a sandbox that knows only
	// "/health", and every request would 404.
	outPath := "/" + rest

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = outPath
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
			// The Host is set explicitly rather than left as the caller's. A
			// sandbox that trusts its Host header — the agent-infra image's
			// VS Code and desktop, notably — would otherwise see the public
			// hostname and refuse, or build redirects pointing outside. It
			// sees the address it is served at from the cluster, which is the
			// one it can check.
			pr.Out.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A sandbox that is not up yet, or has died, is the ordinary cause
			// here — worth saying plainly rather than as a bare 502.
			h.log.Info("proxying to a sandbox failed", "sandbox", id, "port", port, "error", err)
			http.Error(w, fmt.Sprintf("could not reach sandbox %q on %q: %v", id, port, err), http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			h.rewriteLocation(resp, id, port)
			// A sandbox's responses are its own; nothing here is cached, and a
			// stale console page for a sandbox that has been recreated is
			// exactly the confusion this avoids.
			resp.Header.Set("Cache-Control", "no-store")
			return nil
		},
	}
	rp.ServeHTTP(w, r)
}

// rewriteLocation makes a redirect a sandbox issues reachable by the caller.
//
// A sandbox that redirects to "/login" means "login on me" — but the caller
// asked for "/sandbox/<id>/<port>/...", so a bare "/login" sends them to the
// control plane's own root. Relative locations are prefixed with the public
// path; absolute ones pointing at the sandbox itself are rewritten too. A
// redirect to somewhere else entirely is left alone.
func (h *Handler) rewriteLocation(resp *http.Response, id, port string) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		return
	}
	prefix := h.cfg.URL("/sandbox/" + id + "/" + port)
	if u.Host == "" {
		// Relative: "/login" or "login". Absolute-path forms are the common
		// case and the one that breaks without this.
		if strings.HasPrefix(loc, "/") {
			resp.Header.Set("Location", prefix+loc)
		} else {
			resp.Header.Set("Location", prefix+"/"+loc)
		}
		return
	}
	// Absolute. Rewrite only if it points at the sandbox's own in-cluster
	// address, which is what a service that builds absolute URLs from Host
	// will produce once Host has been set to that address above.
	if resp.Request != nil && u.Host == resp.Request.Host {
		resp.Header.Set("Location", prefix+u.Path)
	}
}

// resolveTimeout is the deadline on the lookup, not on the proxied request: a
// sandbox may stream for an hour, but finding where to send it is a cluster
// read that should not hang.
const resolveTimeout = 10 * time.Second
