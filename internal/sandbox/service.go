// Package sandbox is the control plane's own logic: turning a request into a
// sandbox. It sits between the HTTP layer, which knows about JSON, and the
// cluster, which knows about namespaces.
//
// Everything that is a policy decision lives here — which ids are allowed, how
// long a sandbox may live, how many may exist, and who may see which — so the
// same decisions apply however a sandbox was asked for, the console and the CLI
// included. A handler that forgot an ownership check would still be handed a
// sandbox it should not have.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// ErrNotFound is returned for a sandbox that does not exist.
var ErrNotFound = k8s.ErrNotFound

// ErrInvalid is wrapped by every error that is the caller's fault, so the HTTP
// layer can answer 400 rather than 500 without matching on message text.
var ErrInvalid = errors.New("invalid request")

// ErrConflict is wrapped by errors where the request was well formed but could
// not be satisfied — an id already taken, a sandbox past its TTL.
var ErrConflict = errors.New("conflict")

// ErrLimit is returned when a deployment or a user is at its sandbox ceiling.
var ErrLimit = errors.New("sandbox limit reached")

// Quotas resolves the quota a user is under. The user store implements it.
type Quotas interface {
	// QuotaFor returns a user's quota, and whether the user exists.
	QuotaFor(ctx context.Context, name string) (user.Quota, bool)
}

// Service creates and manages sandboxes.
type Service struct {
	cfg     config.Config
	catalog *model.Catalog
	client  *k8s.Client
	quotas  Quotas
	now     func() time.Time
}

// New builds the service.
func New(cfg config.Config, catalog *model.Catalog, client *k8s.Client, quotas Quotas) *Service {
	return &Service{cfg: cfg, catalog: catalog, client: client, quotas: quotas, now: time.Now}
}

// Catalog returns the templates a sandbox can be created from.
func (s *Service) Catalog() *model.Catalog { return s.catalog }

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

// Create resolves an input into a sandbox and creates it, on behalf of who.
func (s *Service) Create(ctx context.Context, who auth.Identity, in CreateInput) (model.Sandbox, error) {
	quota := s.quotaFor(ctx, who)

	tmpl, ok := s.catalog.Get(strings.TrimSpace(in.Template))
	if !ok {
		return model.Sandbox{}, fmt.Errorf("%w: no template named %q", ErrInvalid, in.Template)
	}
	// Checked before anything is built, so a template a user may not have is
	// refused without a namespace ever being created for it.
	if !quota.Allows(tmpl.ID) {
		return model.Sandbox{}, fmt.Errorf("%w: the template %q is not available to you; your templates are: %s",
			ErrInvalid, tmpl.ID, strings.Join(quota.Templates, ", "))
	}

	id, err := s.resolveID(in.Name, tmpl)
	if err != nil {
		return model.Sandbox{}, err
	}

	ttl, err := s.resolveTTL(tmpl, in.TTL, quota)
	if err != nil {
		return model.Sandbox{}, err
	}

	if err := s.checkLimit(ctx, who, quota); err != nil {
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
		Owner:    ownerOf(who),
	})
	if err != nil {
		if errors.Is(err, k8s.ErrAlreadyExists) {
			return model.Sandbox{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		return model.Sandbox{}, err
	}
	return sb, nil
}

// ownerOf is the owner recorded on a sandbox: a user's name, or empty for the
// administrator.
//
// Empty rather than "admin" is deliberate. An administrator's sandboxes belong
// to the deployment, and a listing for a user named "admin" — which they could
// be — must not turn up the deployment's own.
func ownerOf(who auth.Identity) string {
	if who.IsAdmin() {
		return ""
	}
	return who.Name
}

// quotaFor is the quota a caller is under. An administrator has none, and a
// user's comes from their record — which is looked up per request rather than
// cached, so revoking it takes effect on the next call rather than on a restart.
func (s *Service) quotaFor(ctx context.Context, who auth.Identity) user.Quota {
	if who.IsAdmin() || who.Name == "" || s.quotas == nil {
		return user.Quota{}
	}
	q, _ := s.quotas.QuotaFor(ctx, who.Name)
	return q
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

// resolveTTL applies the template default and every ceiling: the template's, the
// user's, and the deployment's.
//
// The user's is applied last so it cannot be raised by a template's, and the
// deployment's last of all so nothing can exceed it.
func (s *Service) resolveTTL(tmpl model.Template, requested time.Duration, quota user.Quota) (time.Duration, error) {
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
	ttl := s.cfg.ClampTTL(requested, tmplMax)
	if quota.MaxTTL > 0 && (ttl == 0 || ttl > quota.MaxTTL) {
		ttl = quota.MaxTTL
	}
	return ttl, nil
}

// checkLimit enforces both ceilings: the deployment's, and the caller's own.
//
// It is a count of what exists rather than a reservation, so two creates racing
// can both pass and land one over the limit. That is the right trade for a
// ceiling that exists to stop a runaway loop, not to enforce a hard quota —
// nothing here is billing anyone.
func (s *Service) checkLimit(ctx context.Context, who auth.Identity, quota user.Quota) error {
	// The user's own ceiling first, so the message is about the limit they hit
	// rather than about a deployment-wide one they cannot see.
	if !who.IsAdmin() && quota.MaxSandboxes > 0 {
		mine, err := s.client.List(ctx, who.Name)
		if err != nil {
			return err
		}
		if len(mine) >= quota.MaxSandboxes {
			return fmt.Errorf("%w: you have %d of %d sandboxes; delete one first", ErrLimit, len(mine), quota.MaxSandboxes)
		}
	}
	if s.cfg.MaxSandboxes <= 0 {
		return nil
	}
	all, err := s.client.List(ctx, "")
	if err != nil {
		return err
	}
	if len(all) >= s.cfg.MaxSandboxes {
		return fmt.Errorf("%w: %d of %d sandboxes exist in this deployment; delete one first", ErrLimit, len(all), s.cfg.MaxSandboxes)
	}
	return nil
}

// Get returns one sandbox, if the caller may see it.
func (s *Service) Get(ctx context.Context, who auth.Identity, id string) (model.Sandbox, error) {
	return s.owned(ctx, who, id)
}

// owned reads a sandbox and checks the caller may have it.
//
// A sandbox the caller does not own is reported as missing rather than as
// forbidden. That is the deliberate half of this: a 403 would confirm the
// sandbox exists, so a user could enumerate the deployment's other users' names
// by watching which ones 403 instead of 404.
func (s *Service) owned(ctx context.Context, who auth.Identity, id string) (model.Sandbox, error) {
	id = model.NormalizeID(id)
	if !model.IsValidID(id) {
		return model.Sandbox{}, fmt.Errorf("%w: %q is not a sandbox id", ErrInvalid, id)
	}
	sb, err := s.client.Get(ctx, id)
	if err != nil {
		return model.Sandbox{}, err
	}
	if !who.Owns(sb.Owner) {
		return model.Sandbox{}, fmt.Errorf("%w: no sandbox named %q", ErrNotFound, id)
	}
	return sb, nil
}

// List returns the sandboxes the caller may see: all of them for an
// administrator, their own for a user.
func (s *Service) List(ctx context.Context, who auth.Identity) ([]model.Sandbox, error) {
	return s.client.List(ctx, ownerOf(who))
}

// Delete removes a sandbox the caller may see.
func (s *Service) Delete(ctx context.Context, who auth.Identity, id string) error {
	sb, err := s.owned(ctx, who, id)
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
// The new lifetime is bounded by the deployment's ceiling and the caller's own,
// the same as at create. It is not additionally bounded by the template's,
// because a template bounds what it costs to start a sandbox and a renew costs
// nothing — the template's ceiling is a default, not a property of the sandbox.
func (s *Service) Renew(ctx context.Context, who auth.Identity, id string, in RenewInput) (model.Sandbox, error) {
	sb, err := s.owned(ctx, who, id)
	if err != nil {
		return model.Sandbox{}, err
	}
	if in.TTL < 0 {
		return model.Sandbox{}, fmt.Errorf("%w: ttl cannot be negative", ErrInvalid)
	}
	if in.TTL > 0 {
		quota := s.quotaFor(ctx, who)
		in.TTL = s.cfg.ClampTTL(in.TTL, 0)
		if quota.MaxTTL > 0 && in.TTL > quota.MaxTTL {
			in.TTL = quota.MaxTTL
		}
	}
	return s.client.Renew(ctx, sb.ID, in.TTL)
}

// Logs returns the tail of a sandbox's output, if the caller may see it.
func (s *Service) Logs(ctx context.Context, who auth.Identity, id string, tail int64) (string, error) {
	sb, err := s.owned(ctx, who, id)
	if err != nil {
		return "", err
	}
	return s.client.Logs(ctx, sb.ID, tail)
}

// Overview summarises the deployment for whoever is looking.
type Overview struct {
	Total        int                        `json:"total"`
	ByState      map[model.SandboxState]int `json:"byState"`
	ByTemplate   map[string]int             `json:"byTemplate"`
	ByOwner      map[string]int             `json:"byOwner,omitempty"`
	Cluster      bool                       `json:"cluster"`
	MaxSandboxes int                        `json:"maxSandboxes,omitempty"`
	// Scoped says whether these counts are the caller's own or the whole
	// deployment's, so the console can label them rather than guessing.
	Scoped bool `json:"scoped"`
	// User is the caller's name, for the same reason.
	User string `json:"user,omitempty"`
}

// Overview computes the summary for the caller.
//
// A user's overview counts their own sandboxes, and the roster of owners is left
// out — it would be a list of everyone else on the deployment, which is exactly
// what the per-user split is meant not to hand out.
func (s *Service) Overview(ctx context.Context, who auth.Identity) (Overview, error) {
	all, err := s.client.List(ctx, ownerOf(who))
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{
		Total:        len(all),
		ByState:      map[model.SandboxState]int{},
		ByTemplate:   map[string]int{},
		Cluster:      s.client.Ready(ctx),
		MaxSandboxes: s.cfg.MaxSandboxes,
		Scoped:       !who.IsAdmin(),
		User:         who.Name,
	}
	if who.IsAdmin() {
		ov.ByOwner = map[string]int{}
	}
	for _, sb := range all {
		ov.ByState[sb.State]++
		ov.ByTemplate[sb.Template]++
		if who.IsAdmin() {
			owner := sb.Owner
			if owner == "" {
				owner = "(the deployment)"
			}
			ov.ByOwner[owner]++
		}
	}
	// A user's own ceiling is what they can see; the deployment's is not their
	// business until they hit it.
	if !who.IsAdmin() && s.quotas != nil {
		if q, ok := s.quotas.QuotaFor(ctx, who.Name); ok && q.MaxSandboxes > 0 {
			ov.MaxSandboxes = q.MaxSandboxes
		}
	}
	return ov, nil
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
