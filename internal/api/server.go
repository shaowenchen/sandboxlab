package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/buildinfo"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// APIVersion tracks the route surface, not the build version: it changes when a
// client that understood the old shape would be wrong, and not otherwise.
const APIVersion = "v1"

// Deps is what the server needs.
type Deps struct {
	Config    config.Config
	Service   SandboxService
	Auth      *auth.Authenticator
	Log       *slog.Logger
	Console   http.Handler // the console, already built; nil serves a placeholder
	DataPlane DataPlane    // nil disables the /sandbox/ proxy
}

// SandboxService is the control plane's behaviour, as the HTTP layer uses it.
//
// It is an interface so the routes can be exercised without a cluster. The
// sandbox package's own logic is tested against a fake clientset; this layer's
// job is routing, authentication and JSON, and a stub proves those just as well
// and far faster.
type SandboxService interface {
	Catalog() *model.Catalog
	Cluster(ctx context.Context) bool
	Create(ctx context.Context, in sandbox.CreateInput) (model.Sandbox, error)
	Get(ctx context.Context, id string) (model.Sandbox, error)
	List(ctx context.Context) ([]model.Sandbox, error)
	Delete(ctx context.Context, id string) error
	Renew(ctx context.Context, id string, in sandbox.RenewInput) (model.Sandbox, error)
	Logs(ctx context.Context, id string, tail int64) (string, error)
	Overview(ctx context.Context) (sandbox.Overview, error)
	// The three that reach into a running sandbox rather than describing it.
	Exec(ctx context.Context, id string, in sandbox.ExecInput) (sandbox.ExecResult, error)
	ReadFile(ctx context.Context, id, path string) (k8s.FileContent, error)
	WriteFile(ctx context.Context, id, path string, data []byte, createParents bool) (k8s.FileInfo, error)
}

// DataPlane reaches into a sandbox. The API layer owns routing and
// authentication; the implementation owns talking to the cluster.
type DataPlane interface {
	// Serve proxies a request for the sandbox named by id to the port named by
	// port, with the rest of the path preserved.
	Serve(w http.ResponseWriter, r *http.Request, id, port string, rest string)
}

// Server is the HTTP handler.
type Server struct {
	cfg   config.Config
	svc   SandboxService
	auth  *auth.Authenticator
	log   *slog.Logger
	mux   *http.ServeMux
	start time.Time

	// declared is every route this server registered, recorded as it was. It is
	// what Routes() reports and therefore what the spec is checked against —
	// kept beside registration so the two cannot drift.
	declared []declaredRoute
}

// declaredRoute is a registration Routes() can report.
type declaredRoute struct {
	// pattern is what the mux was given.
	pattern string
	// documented is the shape the spec describes, which is the same string
	// except for a mount whose interior the handler parses itself.
	documented string
	methods    []string
}

// New builds the server and its routes.
func New(d Deps) *Server {
	s := &Server{
		cfg:   d.Config,
		svc:   d.Service,
		auth:  d.Auth,
		log:   d.Log,
		mux:   http.NewServeMux(),
		start: time.Now(),
	}
	s.routes(d)
	return s
}

// methods applies the method dispatch that the by-id handlers do themselves,
// and refuses the rest.
//
// It exists so that a route which serves more than GET — /api/v1/sandboxes
// takes GET and POST, /api/v1/sandboxes/{id} takes GET and DELETE — says so in
// one place rather than inside a switch a reader has to go find. What it
// enables is Routes(), below: without a declaration there is nothing for the
// spec check to compare against, and the spec would be free to describe a method
// this server does not answer.
func methods(allowed string, m map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, ok := m[r.Method]; ok {
			h(w, r)
			return
		}
		w.Header().Set("Allow", allowed)
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
	}
}

// handle registers a route and records it, so Routes() cannot disagree with
// what was actually registered. The pattern is both what the mux is given and
// what the spec knows the route by.
func (s *Server) handle(pattern, allowed string, m map[string]http.HandlerFunc) {
	s.declared = append(s.declared, declaredRoute{
		pattern:    pattern,
		documented: pattern,
		methods:    strings.Split(allowed, ", "),
	})
	s.mux.HandleFunc(pattern, methods(allowed, m))
}

// handleAny registers a route that takes every method and whose interior the
// handler parses itself, so the pattern the mux gets is a mount point rather
// than the shape a caller sees.
//
// The data plane is the one of those. It forwards to a sandbox's own port, and
// the methods there belong to whatever the sandbox runs — the all-in-one
// image's API is driven entirely by POSTs — so this API cannot list them
// without being wrong about someone else's program. What it can say is the
// shape, which is what documented names: "/sandbox/{id}/{port}/...".
//
// Declared rather than assumed, so a future mount has to state its shape for
// the spec check to compare against. A mount that quietly answered more than
// the spec describes is exactly what that check exists to catch.
func (s *Server) handleAny(pattern, documented string, h http.HandlerFunc) {
	s.declared = append(s.declared, declaredRoute{
		pattern:    pattern,
		documented: documented,
		methods:    []string{"*"},
	})
	s.mux.HandleFunc(pattern, h)
}

// routes registers everything. Patterns are written root-relative; the base
// path is stripped once, in ServeHTTP, so no handler has to know the deployment
// is served under a prefix — and a handler cannot forget to.
func (s *Server) routes(d Deps) {
	// Unauthenticated. A health check comes from a kubelet that has no key, and
	// the describe endpoint is what a client reads to learn the API's shape
	// before it has been given one.
	s.handle("/healthz", "GET", map[string]http.HandlerFunc{"GET": s.health})
	s.handle("/readyz", "GET", map[string]http.HandlerFunc{"GET": s.ready})
	s.handle("/api/v1/describe", "GET", map[string]http.HandlerFunc{"GET": s.describe})
	s.handle("/api/v1/config", "GET", map[string]http.HandlerFunc{"GET": s.getConfig})

	// The templates and the sandboxes. There is one caller, so these are every
	// template and every sandbox.
	s.handle("/api/v1/catalog", "GET", map[string]http.HandlerFunc{"GET": s.authenticated(s.listCatalog)})
	s.handle("/api/v1/catalog/{id}", "GET", map[string]http.HandlerFunc{"GET": s.authenticated(s.getCatalogEntry)})
	s.handle("/api/v1/overview", "GET", map[string]http.HandlerFunc{"GET": s.authenticated(s.overview)})
	s.handle("/api/v1/sandboxes", "GET, POST", map[string]http.HandlerFunc{
		"GET":  s.authenticated(s.sandboxes),
		"POST": s.authenticated(s.sandboxes),
	})
	s.handle("/api/v1/sandboxes/{id}", "GET, DELETE", map[string]http.HandlerFunc{
		"GET":    s.authenticated(s.sandboxByID),
		"DELETE": s.authenticated(s.sandboxByID),
	})
	s.handle("/api/v1/sandboxes/{id}/renew", "POST", map[string]http.HandlerFunc{"POST": s.authenticated(s.renewSandbox)})
	s.handle("/api/v1/sandboxes/{id}/logs", "GET", map[string]http.HandlerFunc{"GET": s.authenticated(s.sandboxLogs)})
	// Reaching into a running sandbox. These are the control plane's own
	// operations on the pod, not proxying to something the sandbox serves, so
	// they are here rather than under /sandbox/ — a portless sandbox has no URL
	// for a data-plane route to reach it at all.
	s.handle("/api/v1/sandboxes/{id}/exec", "POST", map[string]http.HandlerFunc{"POST": s.authenticated(s.execSandbox)})
	s.handle("/api/v1/sandboxes/{id}/files", "GET, PUT", map[string]http.HandlerFunc{
		"GET": s.authenticated(s.sandboxFiles),
		"PUT": s.authenticated(s.sandboxFiles),
	})

	// The data plane: a browser opens these directly, so the key may come from
	// the query string as well as a header. Every method is forwarded — see
	// handleAny.
	if d.DataPlane != nil && s.cfg.DataPlane {
		s.handleAny("/sandbox/", "/sandbox/{id}/{port}/", s.dataPlane(d.DataPlane))
	}

	// The console is the fallback, so a client-side route inside it resolves on
	// a reload rather than 404ing.
	//
	// Served without a key, deliberately, and it is the only route that is. The
	// document is a static page carrying nothing: everything it shows comes from
	// /api/v1, and every one of those calls is authenticated as before. A caller
	// with no key gets an empty console and a sign-in form.
	//
	// It has to be this way round for the sign-in form to exist at all. Serving
	// the page only to an already-authenticated caller means the browser — which
	// has no way to send a key on a navigation — is refused before it can render
	// the form that would ask for one. The console behind a key is a console
	// nobody can reach.
	//
	// The earlier note here said the opposite, on the reasoning that a readable
	// console "would list every sandbox to anyone who found the hostname". It
	// would not: listing is an API call, and the API still answers 401.
	//
	// Registered directly rather than through handle(): it answers every path in
	// the deployment, so it is the backdrop rather than a route, and Routes()
	// leaves it out.
	//
	// The backdrop refuses everything under /api/ first, which is the one thing
	// a catch-all would otherwise get wrong. Without it a path that was removed
	// — /api/v1/users, say — is not a 404 but the console's HTML with a 200, and
	// a client generated from the old spec reads that as success. The console's
	// own client-side routes are all outside /api/, so nothing is lost by
	// refusing the prefix.
	if d.Console != nil {
		s.mux.Handle("/", apiNotFound(d.Console))
	} else {
		s.mux.Handle("/", apiNotFound(s.placeholder()))
	}
}

// apiNotFound answers unknown /api/ paths with a 404 rather than handing them to
// the fallback.
func apiNotFound(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + r.URL.Path})
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

// Route is one thing this server answers: the shape a caller sees, and the
// methods it accepts there.
type Route struct {
	Pattern string
	Methods []string
}

// Routes lists every route the server registered, in registration order, root
// relative and without the deployment's base path.
//
// The spec check reads this and compares it against api/openapi.yaml, which is
// what makes "the spec is the source of truth" a property rather than a claim:
// a route the spec describes and this does not register is a client generated
// against an endpoint that does not exist, and a route registered here and not
// in the spec is one nobody can discover. Neither is visible from either side
// alone.
func (s *Server) Routes() []Route {
	out := make([]Route, 0, len(s.declared))
	for _, r := range s.declared {
		out = append(out, Route{Pattern: r.documented, Methods: append([]string(nil), r.methods...)})
	}
	return out
}

// ServeHTTP strips the deployment's base path and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(context.WithValue(r.Context(), logKey{}, s.log))
	if base := s.cfg.BasePath; base != "" {
		trimmed, ok := trimBasePath(r.URL.Path, base)
		if !ok {
			http.NotFound(w, r)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path = trimmed
	}
	s.mux.ServeHTTP(w, r)
}

// trimBasePath removes base from path, reporting whether path was under it.
//
// "/sandbox" and "/sandbox/" both become "/", so the bare URL of a
// sub-path deployment reaches the console rather than a 404.
func trimBasePath(path, base string) (string, bool) {
	switch {
	case path == base:
		return "/", true
	case strings.HasPrefix(path, base+"/"):
		return strings.TrimPrefix(path, base), true
	default:
		return "", false
	}
}

type logKey struct{}

func logger(r *http.Request) *slog.Logger {
	if l, ok := r.Context().Value(logKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// ── unauthenticated ─────────────────────────────────────────────────────────

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	// Readiness is "can I reach a cluster". A control plane that is up but
	// cannot do anything should not be sent traffic, and a 503 here is how the
	// Service takes it out of rotation.
	if !s.svc.Cluster(r.Context()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"reason": "the cluster is not reachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// describe is the API's own contract. It is unauthenticated so a client can
// read the shape of the interface before it has a key — which is what makes it
// usable as documentation rather than as a second thing to configure.
func (s *Server) describe(w http.ResponseWriter, r *http.Request) {
	endpoints := []map[string]string{
		{"method": "GET", "path": "/api/v1/describe", "description": "this document; no key required"},
		{"method": "GET", "path": "/api/v1/config", "description": "the interface version and deployment shape"},
		// The health routes are part of the contract a deployment depends on —
		// the chart's probes are these — so a client reading this document to
		// find out what it may call should find them.
		{"method": "GET", "path": "/healthz", "description": "the process is up; no key required"},
		{"method": "GET", "path": "/readyz", "description": "the process can reach the cluster; no key required"},
		{"method": "GET", "path": "/api/v1/catalog", "description": "the templates a sandbox can be created from"},
		{"method": "GET", "path": "/api/v1/catalog/{id}", "description": "one template"},
		{"method": "GET", "path": "/api/v1/overview", "description": "counts of sandboxes by state and template"},
		{"method": "GET", "path": "/api/v1/sandboxes", "description": "every sandbox in the deployment"},
		{"method": "POST", "path": "/api/v1/sandboxes", "description": "create one; body {template, name?, ttl?, env?}"},
		{"method": "GET", "path": "/api/v1/sandboxes/{id}", "description": "one sandbox"},
		{"method": "DELETE", "path": "/api/v1/sandboxes/{id}", "description": "delete one"},
		{"method": "POST", "path": "/api/v1/sandboxes/{id}/renew", "description": "reset its expiry; body {ttl}"},
		{"method": "GET", "path": "/api/v1/sandboxes/{id}/logs", "description": "the tail of its output; ?tail=<lines>"},
		{"method": "POST", "path": "/api/v1/sandboxes/{id}/exec", "description": "run a command in it and wait; body {command, stdin?, cwd?, timeout?}"},
		{"method": "GET", "path": "/api/v1/sandboxes/{id}/files", "description": "read a file: ?path=/abs/path"},
		{"method": "PUT", "path": "/api/v1/sandboxes/{id}/files", "description": "write a file: ?path=/abs/path, body {content, encoding?, createParents?}"},
		{"method": "GET", "path": "/sandbox/{id}/{port}/", "description": "proxy to a sandbox's own port"},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     "sandboxlab",
		"version":  APIVersion,
		"build":    buildinfo.String(),
		"basePath": s.cfg.BasePath,
		"auth": map[string]any{
			"scheme": "key",
			"headers": []string{
				auth.AuthorizationHeader + ": Bearer <key>",
				auth.APIKeyHeader + ": <key>",
			},
			"note": "the data-plane routes under /sandbox/ also accept ?" + auth.QueryParam + "=<key>, for a browser that cannot set a header",
		},
		"endpoints": endpoints,
	})
}

// getConfig reports the interface contract and the deployment's shape.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion":   APIVersion,
		"build":        buildinfo.String(),
		"basePath":     s.cfg.BasePath,
		"publicURL":    s.cfg.PublicURL,
		"namespace":    s.cfg.Namespace,
		"dataPlane":    s.cfg.DataPlane && s.cfg.PublicURL != "",
		"defaultTTL":   s.cfg.DefaultTTL.String(),
		"maxTTL":       s.cfg.MaxTTL.String(),
		"maxSandboxes": s.cfg.MaxSandboxes,
		"templates":    s.svc.Catalog().Len(),
	})
}

// ── catalog ─────────────────────────────────────────────────────────────────

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": s.svc.Catalog().List()})
}

func (s *Server) getCatalogEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, ok := s.svc.Catalog().Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no template named " + id})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// ── overview ────────────────────────────────────────────────────────────────

// OverviewResponse is what GET /api/v1/overview returns.
//
// It mirrors the service's own summary rather than embedding it, so the wire
// shape is declared where the route is — a field renamed in the service is a
// compile error here rather than a silently missing key in the console.
type OverviewResponse struct {
	Total        int                        `json:"total"`
	ByState      map[model.SandboxState]int `json:"byState"`
	ByTemplate   map[string]int             `json:"byTemplate"`
	Cluster      bool                       `json:"cluster"`
	MaxSandboxes int                        `json:"maxSandboxes,omitempty"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.svc.Overview(r.Context())
	if err != nil {
		writeError(w, logger(r), err)
		return
	}
	writeJSON(w, http.StatusOK, OverviewResponse{
		Total:        ov.Total,
		ByState:      ov.ByState,
		ByTemplate:   ov.ByTemplate,
		Cluster:      ov.Cluster,
		MaxSandboxes: ov.MaxSandboxes,
	})
}

// ── sandboxes ───────────────────────────────────────────────────────────────

// createRequest is the body POST /api/v1/sandboxes accepts.
type createRequest struct {
	Template string            `json:"template"`
	Name     string            `json:"name,omitempty"`
	TTL      string            `json:"ttl,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

func (s *Server) sandboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := s.svc.List(r.Context())
		if err != nil {
			writeError(w, logger(r), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sandboxes": all, "count": len(all)})
	case http.MethodPost:
		var body createRequest
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "could not read the request body: " + err.Error()})
			return
		}
		ttl, err := parseTTL(body.TTL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
			return
		}
		sb, err := s.svc.Create(r.Context(), sandbox.CreateInput{
			Template: body.Template,
			Name:     body.Name,
			TTL:      ttl,
			Env:      body.Env,
		})
		if err != nil {
			writeError(w, logger(r), err)
			return
		}
		logger(r).Info("created a sandbox", "sandbox", sb.ID, "template", sb.Template)
		writeJSON(w, http.StatusCreated, sb)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
	}
}

func (s *Server) sandboxByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		sb, err := s.svc.Get(r.Context(), id)
		if err != nil {
			writeError(w, logger(r), err)
			return
		}
		writeJSON(w, http.StatusOK, sb)
	case http.MethodDelete:
		if err := s.svc.Delete(r.Context(), id); err != nil {
			writeError(w, logger(r), err)
			return
		}
		logger(r).Info("deleted a sandbox", "sandbox", id)
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
	}
}

// renewRequest is the body POST /api/v1/sandboxes/{id}/renew accepts.
type renewRequest struct {
	TTL string `json:"ttl"`
}

func (s *Server) renewSandbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
		return
	}
	var body renewRequest
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "could not read the request body: " + err.Error()})
		return
	}
	ttl, err := parseTTL(body.TTL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	sb, err := s.svc.Renew(r.Context(), r.PathValue("id"), sandbox.RenewInput{TTL: ttl})
	if err != nil {
		writeError(w, logger(r), err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	tail := int64(200)
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := parsePositiveInt(v); err == nil {
			tail = n
		}
	}
	out, err := s.svc.Logs(r.Context(), r.PathValue("id"), tail)
	if err != nil {
		writeError(w, logger(r), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

func (s *Server) placeholder() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("sandboxlab " + buildinfo.String() + "\n\nthe console is not installed in this build; use the API under /api/v1/\n"))
	}
}

// ── auth middleware ─────────────────────────────────────────────────────────

// authenticated serves the request only if it carries the deployment's key.
//
// It is the only place a request is authorised: everything behind it has already
// been checked, so a route registered without this wrapper is the only way to
// get it wrong — and it is one line away rather than a check inside each handler.
func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.OK(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandboxlab"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "a valid API key is required"})
			return
		}
		next(w, r)
	}
}
