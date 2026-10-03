// Package sandbox is the control plane's own logic: turning a request into a
// sandbox. It sits between the HTTP layer, which knows about JSON, and the
// cluster, which knows about namespaces.
//
// Everything that is a policy decision lives here — which ids are allowed, how
// long a sandbox may live, and how many may exist — so the same decisions apply
// however a sandbox was asked for, the console and the CLI included.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

// ErrNotFound is returned for a sandbox that does not exist.
var ErrNotFound = k8s.ErrNotFound

// ErrInvalid is wrapped by every error that is the caller's fault, so the HTTP
// layer can answer 400 rather than 500 without matching on message text.
var ErrInvalid = errors.New("invalid request")

// ErrConflict is wrapped by errors where the request was well formed but could
// not be satisfied — an id already taken, a sandbox past its TTL.
var ErrConflict = errors.New("conflict")

// ErrLimit is returned when the deployment is at its sandbox ceiling.
var ErrLimit = errors.New("sandbox limit reached")

// ErrTimeout is returned when a command outlived the time it was given.
//
// It is separate from the other failures because nothing is wrong: the command
// is still running, and the caller can ask again with longer — or set it going
// in the background. Reporting it as a server error would suggest the control
// plane broke.
var ErrTimeout = errors.New("the command did not finish in time")

// ErrTooLarge is returned when a file is over the size this API carries.
var ErrTooLarge = k8s.ErrTooLarge

// ErrNoSuchFile is returned for a path that is not there.
//
// Distinct from ErrNotFound, which means "no such sandbox". Both are 404s, and
// the message has to name the right missing thing.
var ErrNoSuchFile = k8s.ErrNoSuchFile

// Service creates and manages sandboxes.
type Service struct {
	cfg     config.Config
	catalog *model.Catalog
	client  *k8s.Client
	now     func() time.Time
}

// New builds the service.
func New(cfg config.Config, catalog *model.Catalog, client *k8s.Client) *Service {
	return &Service{cfg: cfg, catalog: catalog, client: client, now: time.Now}
}

// Catalog returns the templates a sandbox can be created from.
func (s *Service) Catalog() *model.Catalog { return s.catalog }

// AddTemplate puts a template in the running catalog, replacing any template
// with the same id.
//
// The catalog is shared with the API layer, which reads it on every request, so
// this is the one way a template arrives at runtime. Nothing is persisted: the
// catalog returns to the compiled-in templates when the process restarts, which
// is the trade this deployment makes for not needing a store.
//
// It reports whether an existing template was replaced, so the HTTP layer can
// answer 201 for a new id and 200 for an edit.
func (s *Service) AddTemplate(t model.Template) (replaced bool, err error) {
	return s.catalog.Add(t)
}

// RemoveTemplate takes a template out of the running catalog, reporting whether
// it was there.
func (s *Service) RemoveTemplate(id string) bool {
	return s.catalog.Remove(id)
}

// Cluster reports whether the cluster is reachable.
func (s *Service) Cluster(ctx context.Context) bool { return s.client.Ready(ctx) }

// CreateInput is a create request as it arrives, before policy is applied.
type CreateInput struct {
	// Template is the catalog id to create from. Required.
	Template string
	// Name is the id to give the sandbox. Empty means one is generated from the
	// template and a random suffix.
	Name string
	// TTL is how long it should live. Zero means the template's default.
	TTL time.Duration
	// Env is extra environment, merged over the template's.
	Env map[string]string
}

// Create resolves an input into a sandbox and creates it.
func (s *Service) Create(ctx context.Context, in CreateInput) (model.Sandbox, error) {
	tmpl, ok := s.catalog.Get(strings.TrimSpace(in.Template))
	if !ok {
		return model.Sandbox{}, fmt.Errorf("%w: no template named %q", ErrInvalid, in.Template)
	}

	id, err := s.resolveID(in.Name, tmpl)
	if err != nil {
		return model.Sandbox{}, err
	}

	ttl, err := s.resolveTTL(tmpl, in.TTL)
	if err != nil {
		return model.Sandbox{}, err
	}

	if err := s.checkLimit(ctx); err != nil {
		return model.Sandbox{}, err
	}

	// Whether the id is already taken is discovered by the create itself, which
	// is the only check that cannot race another caller doing the same thing —
	// so the cluster's answer is translated rather than pre-empted.
	sb, err := s.client.Create(ctx, k8s.CreateRequest{
		ID:       id,
		Template: tmpl,
		TTL:      ttl,
		Env:      in.Env,
	})
	if err != nil {
		if errors.Is(err, k8s.ErrAlreadyExists) {
			return model.Sandbox{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		return model.Sandbox{}, err
	}
	return sb, nil
}

// resolveID decides what a sandbox is called.
func (s *Service) resolveID(name string, tmpl model.Template) (string, error) {
	if strings.TrimSpace(name) == "" {
		return generateID(tmpl.ID)
	}
	id := model.NormalizeID(name)
	if !model.IsValidID(id) {
		return "", fmt.Errorf("%w: %q is not a usable name; use lowercase letters, digits and '-', up to 40 characters", ErrInvalid, name)
	}
	return id, nil
}

// resolveTTL applies the template's default and then every ceiling: the
// template's own, and the deployment's.
//
// The deployment's is applied last so nothing can exceed it.
func (s *Service) resolveTTL(tmpl model.Template, requested time.Duration) (time.Duration, error) {
	tmplDefault, err := tmpl.ParsedTTLDefault()
	if err != nil {
		return 0, fmt.Errorf("template %q: %w", tmpl.ID, err)
	}
	tmplMax, err := tmpl.ParsedTTLMax()
	if err != nil {
		return 0, fmt.Errorf("template %q: %w", tmpl.ID, err)
	}
	if requested < 0 {
		return 0, fmt.Errorf("%w: ttl cannot be negative", ErrInvalid)
	}
	if requested == 0 {
		requested = s.cfg.DefaultTTLFor(tmplDefault)
	}
	return s.cfg.ClampTTL(requested, tmplMax), nil
}

// checkLimit enforces the deployment's ceiling.
//
// It is a count of what exists rather than a reservation, so two creates racing
// can both pass and land one over the limit. That is the right trade for a
// ceiling that exists to stop a runaway loop rather than to enforce a hard
// quota — nothing here is billing anyone.
func (s *Service) checkLimit(ctx context.Context) error {
	if s.cfg.MaxSandboxes <= 0 {
		return nil
	}
	all, err := s.client.List(ctx)
	if err != nil {
		return err
	}
	if len(all) >= s.cfg.MaxSandboxes {
		return fmt.Errorf("%w: %d of %d sandboxes exist in this deployment; delete one first", ErrLimit, len(all), s.cfg.MaxSandboxes)
	}
	return nil
}

// Get returns one sandbox.
func (s *Service) Get(ctx context.Context, id string) (model.Sandbox, error) {
	return s.lookup(ctx, id)
}

// lookup reads a sandbox by id, rejecting an id that could not name one.
//
// The id is validated before the cluster is asked, so a malformed one is a 400
// rather than a 404 — the request was wrong, and saying "no such sandbox" would
// suggest a well-formed name that simply is not there.
func (s *Service) lookup(ctx context.Context, id string) (model.Sandbox, error) {
	id = model.NormalizeID(id)
	if !model.IsValidID(id) {
		return model.Sandbox{}, fmt.Errorf("%w: %q is not a sandbox id", ErrInvalid, id)
	}
	return s.client.Get(ctx, id)
}

// List returns every sandbox.
func (s *Service) List(ctx context.Context) ([]model.Sandbox, error) {
	return s.client.List(ctx)
}

// Delete removes a sandbox.
func (s *Service) Delete(ctx context.Context, id string) error {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return err
	}
	return s.client.Delete(ctx, sb.ID)
}

// RenewInput extends, or removes, a sandbox's TTL.
type RenewInput struct {
	// TTL is the new lifetime from now. Zero means "no expiry" — a sandbox
	// someone is working in should be able to be made to stop disappearing.
	TTL time.Duration
	// KeepTemplateMax is unused by the service and kept for the interface; see
	// Renew for which ceilings apply.
	KeepTemplateMax bool
}

// Renew resets a sandbox's expiry.
//
// The new lifetime is bounded by the deployment's ceiling, the same as at
// create. It is not additionally bounded by the template's, because a template
// bounds what it costs to start a sandbox and a renew costs nothing — the
// template's ceiling is a default, not a property of the sandbox.
func (s *Service) Renew(ctx context.Context, id string, in RenewInput) (model.Sandbox, error) {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return model.Sandbox{}, err
	}
	if in.TTL < 0 {
		return model.Sandbox{}, fmt.Errorf("%w: ttl cannot be negative", ErrInvalid)
	}
	if in.TTL > 0 {
		in.TTL = s.cfg.ClampTTL(in.TTL, 0)
	}
	return s.client.Renew(ctx, sb.ID, in.TTL)
}

// Logs returns the tail of a sandbox's output.
//
// A sandbox that cannot be read because it is still coming up is reported as a
// conflict rather than a server error. This is the ordinary state right after a
// create: the pod exists and its container is being pulled, so there is nothing
// to read yet, and a caller polling for output should be told to come back
// rather than that something broke.
//
// The state decides it, not the error's wording — and the state is what makes
// the distinction safe: a sandbox whose container has *exited* is Failed or
// Running and reads as a plain error, because its logs are empty rather than
// unavailable, and a failing container is exactly the one worth reading.
func (s *Service) Logs(ctx context.Context, id string, tail int64) (string, error) {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return "", err
	}
	logs, err := s.client.Logs(ctx, sb.ID, tail)
	if err != nil && sb.State == model.StatePending {
		return "", fmt.Errorf("%w: the sandbox %q is still starting; its output is not available yet", ErrConflict, sb.ID)
	}
	return logs, err
}

// Overview summarises the deployment.
type Overview struct {
	Total        int                        `json:"total"`
	ByState      map[model.SandboxState]int `json:"byState"`
	ByTemplate   map[string]int             `json:"byTemplate"`
	Cluster      bool                       `json:"cluster"`
	MaxSandboxes int                        `json:"maxSandboxes,omitempty"`
}

// Overview computes the summary.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	all, err := s.client.List(ctx)
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{
		Total:        len(all),
		ByState:      map[model.SandboxState]int{},
		ByTemplate:   map[string]int{},
		Cluster:      s.client.Ready(ctx),
		MaxSandboxes: s.cfg.MaxSandboxes,
	}
	for _, sb := range all {
		ov.ByState[sb.State]++
		ov.ByTemplate[sb.Template]++
	}
	return ov, nil
}

// Exec runs one command in a sandbox and waits for it.
//
// The timeout is applied here rather than left to the caller, because nothing
// else bounds it: the HTTP server has no write deadline (the data-plane proxy
// streams for as long as it likes) so a command that never returns would hold a
// goroutine and a connection until the process restarts.
//
// A command that exits non-zero is not an error — see k8s.Exec. Only running it
// at all can fail.
func (s *Service) Exec(ctx context.Context, id string, in ExecInput) (ExecResult, error) {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = s.cfg.ExecTimeout
	}
	if s.cfg.MaxExecTimeout > 0 && timeout > s.cfg.MaxExecTimeout {
		timeout = s.cfg.MaxExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := s.client.Exec(ctx, sb.ID, k8s.ExecInput{
		Command: in.Command,
		Stdin:   in.Stdin,
		Cwd:     in.Cwd,
	})
	if err != nil {
		return ExecResult{}, wrapExecErr(err, id)
	}
	return ExecResult{
		Stdout:          out.Stdout,
		Stderr:          out.Stderr,
		ExitCode:        out.ExitCode,
		StdoutTruncated: out.StdoutTruncated,
		StderrTruncated: out.StderrTruncated,
	}, nil
}

// ReadFile reads a file out of a sandbox.
func (s *Service) ReadFile(ctx context.Context, id, path string) (k8s.FileContent, error) {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return k8s.FileContent{}, err
	}
	return s.client.ReadFile(ctx, sb.ID, path, s.cfg.MaxFileBytes)
}

// WriteFile writes a file into a sandbox.
func (s *Service) WriteFile(ctx context.Context, id, path string, data []byte, createParents bool) (k8s.FileInfo, error) {
	sb, err := s.lookup(ctx, id)
	if err != nil {
		return k8s.FileInfo{}, err
	}
	return s.client.WriteFile(ctx, sb.ID, path, data, createParents)
}

// wrapExecErr turns a transport failure into something the HTTP layer can map.
//
// The one worth naming is a sandbox whose pod is not up yet. A new sandbox is
// Pending for a few seconds while its image is pulled, and an exec into it
// fails — that is the ordinary state right after a create, not a fault, and a
// caller polling should be told to come back rather than that something broke.
// It is the same distinction Logs makes, for the same reason.
func wrapExecErr(err error, id string) error {
	if errors.Is(err, k8s.ErrInvalid) || errors.Is(err, k8s.ErrNoSuchFile) || errors.Is(err, k8s.ErrTooLarge) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the command in %q was still running when its time ran out", ErrTimeout, id)
	}
	// The transport reports "no pod yet" as a plain error, so the message is the
	// only signal — but it is this package's own message from sandboxPod, not a
	// cluster's, which is what makes matching on it safe here and nowhere else.
	if strings.Contains(err.Error(), "has no pod yet") {
		return fmt.Errorf("%w: the sandbox %q is still starting; try again in a moment", ErrConflict, id)
	}
	return err
}

// ExecInput is one command to run in a sandbox.
type ExecInput struct {
	// Command is an argv, not a command line. A shell is something a caller
	// asks for by naming one — ["sh", "-c", "..."] — rather than something this
	// API imposes on every command.
	Command []string
	// Stdin is fed to the command. Empty means no stdin stream at all.
	Stdin []byte
	// Cwd is the working directory. Empty means the image's own.
	Cwd string
	// Timeout bounds the command. Zero means the deployment's default, and the
	// deployment's ceiling caps whatever is asked for.
	Timeout time.Duration
}

// ExecResult is what a command produced.
//
// A non-zero ExitCode is not an error: the command ran. Only a command that
// could not be run at all is an error.
type ExecResult struct {
	Stdout          []byte `json:"-"`
	Stderr          []byte `json:"-"`
	ExitCode        int    `json:"exitCode"`
	StdoutTruncated bool   `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool   `json:"stderrTruncated,omitempty"`
}

// generateID makes an id for a sandbox nobody named: the template's first word
// and four random bytes, so it is readable in a namespace list and unique
// enough that a collision is not worth a retry loop around.
func generateID(template string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a sandbox id: %w", err)
	}
	prefix := template
	if i := strings.IndexByte(prefix, '-'); i > 0 {
		prefix = prefix[:i]
	}
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	id := prefix + "-" + hex.EncodeToString(b)
	if !model.IsValidID(id) {
		// A template id that does not survive being used as a prefix is a
		// catalog problem, not a request problem.
		return "", fmt.Errorf("template id %q cannot form a sandbox id", template)
	}
	return id, nil
}
