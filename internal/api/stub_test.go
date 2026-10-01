package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// stubService is the API layer's test double: the same policy the real service
// applies, over a map instead of a cluster.
//
// It deliberately re-implements the id, TTL and existence rules rather than
// stubbing them out. What the routing tests need is a service that answers the
// way the real one does for the cases they exercise — a taken name, an unknown
// template, a missing sandbox — and a double that accepted everything would let
// the error-mapping tests pass while the real paths 500.
type stubService struct {
	cfg     config.Config
	catalog *model.Catalog
	boxes   map[string]model.Sandbox
	limits  map[string]time.Duration
	quotas  map[string]user.Quota
	// keys maps a user's name to the key that authenticates as them, so a test
	// can drive a request as a user without a key store.
	keys      map[string]string
	clusterUp bool
	logs      string
	now       func() time.Time
}

func newStubService(cfg config.Config, c *model.Catalog) *stubService {
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = time.Hour
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 8 * time.Hour
	}
	return &stubService{
		cfg:       cfg,
		catalog:   c,
		boxes:     map[string]model.Sandbox{},
		limits:    map[string]time.Duration{},
		quotas:    map[string]user.Quota{},
		keys:      map[string]string{},
		clusterUp: true,
		logs:      "sandbox output\n",
		now:       time.Now,
	}
}

func (s *stubService) Catalog() *model.Catalog { return s.catalog }

func (s *stubService) Cluster(context.Context) bool { return s.clusterUp }

func (s *stubService) Create(_ context.Context, who auth.Identity, in sandbox.CreateInput) (model.Sandbox, error) {
	tmpl, ok := s.catalog.Get(strings.TrimSpace(in.Template))
	if !ok {
		return model.Sandbox{}, fmt.Errorf("%w: no template named %q", sandbox.ErrInvalid, in.Template)
	}
	id := model.NormalizeID(in.Name)
	if in.Name == "" {
		id = "generated-" + tmpl.ID
	} else if !model.IsValidID(id) {
		return model.Sandbox{}, fmt.Errorf("%w: %q is not a usable name", sandbox.ErrInvalid, in.Name)
	}
	if in.TTL < 0 {
		return model.Sandbox{}, fmt.Errorf("%w: ttl cannot be negative", sandbox.ErrInvalid)
	}
	if _, exists := s.boxes[id]; exists {
		return model.Sandbox{}, fmt.Errorf("%w: a sandbox named %q already exists", sandbox.ErrConflict, id)
	}
	if s.cfg.MaxSandboxes > 0 && len(s.boxes) >= s.cfg.MaxSandboxes {
		return model.Sandbox{}, fmt.Errorf("%w: %d of %d sandboxes exist", sandbox.ErrLimit, len(s.boxes), s.cfg.MaxSandboxes)
	}

	ttl := in.TTL
	if ttl == 0 {
		if d, err := tmpl.ParsedTTLDefault(); err == nil {
			ttl = s.cfg.DefaultTTLFor(d)
		}
	}
	var tmplMax time.Duration
	if d, err := tmpl.ParsedTTLMax(); err == nil {
		tmplMax = d
	}
	ttl = s.cfg.ClampTTL(ttl, tmplMax)

	sb := model.Sandbox{
		ID:        id,
		Template:  tmpl.ID,
		Image:     tmpl.Image,
		Owner:     stubOwner(who),
		State:     model.StateRunning,
		CreatedAt: s.now(),
		Namespace: s.cfg.SandboxNamespace(id),
	}
	if ttl > 0 {
		sb.ExpiresAt = s.now().Add(ttl)
	}
	for _, p := range tmpl.Ports {
		sb.Endpoints = append(sb.Endpoints, model.Endpoint{
			Name: p.Name,
			Port: p.Port,
			URL:  s.cfg.URL("/sandbox/" + id + "/" + p.Name + "/"),
		})
	}
	s.boxes[id] = sb
	s.limits[id] = ttl
	return sb, nil
}

// stubOwner mirrors the service's rule: a user's sandboxes carry their name and
// an administrator's carry none.
func stubOwner(who auth.Identity) string {
	if who.IsAdmin() {
		return ""
	}
	return who.Name
}

func (s *stubService) Get(_ context.Context, who auth.Identity, id string) (model.Sandbox, error) {
	id = model.NormalizeID(id)
	sb, ok := s.boxes[id]
	if !ok || !who.Owns(sb.Owner) {
		// Not found rather than forbidden, as the real service does — see its
		// owned() for why.
		return model.Sandbox{}, fmt.Errorf("%w: no sandbox named %q", sandbox.ErrNotFound, id)
	}
	return sb, nil
}

func (s *stubService) List(_ context.Context, who auth.Identity) ([]model.Sandbox, error) {
	out := make([]model.Sandbox, 0, len(s.boxes))
	for _, sb := range s.boxes {
		if who.Owns(sb.Owner) {
			out = append(out, sb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *stubService) Delete(ctx context.Context, who auth.Identity, id string) error {
	if _, err := s.Get(ctx, who, id); err != nil {
		return err
	}
	id = model.NormalizeID(id)
	delete(s.boxes, id)
	delete(s.limits, id)
	return nil
}

func (s *stubService) Renew(ctx context.Context, who auth.Identity, id string, in sandbox.RenewInput) (model.Sandbox, error) {
	sb, err := s.Get(ctx, who, id)
	if err != nil {
		return model.Sandbox{}, err
	}
	if in.TTL < 0 {
		return model.Sandbox{}, fmt.Errorf("%w: ttl cannot be negative", sandbox.ErrInvalid)
	}
	if in.TTL == 0 {
		sb.ExpiresAt = time.Time{}
	} else {
		sb.ExpiresAt = s.now().Add(s.cfg.ClampTTL(in.TTL, 0))
	}
	s.boxes[sb.ID] = sb
	return sb, nil
}

func (s *stubService) Logs(ctx context.Context, who auth.Identity, id string, _ int64) (string, error) {
	if _, err := s.Get(ctx, who, id); err != nil {
		return "", err
	}
	return s.logs, nil
}

func (s *stubService) Overview(_ context.Context, who auth.Identity) (sandbox.Overview, error) {
	ov := sandbox.Overview{
		ByState:      map[model.SandboxState]int{},
		ByTemplate:   map[string]int{},
		Cluster:      s.clusterUp,
		MaxSandboxes: s.cfg.MaxSandboxes,
		Scoped:       !who.IsAdmin(),
		User:         who.Name,
	}
	for _, sb := range s.boxes {
		if !who.Owns(sb.Owner) {
			continue
		}
		ov.Total++
		ov.ByState[sb.State]++
		ov.ByTemplate[sb.Template]++
	}
	return ov, nil
}

// QuotaFor implements the sandbox service's quota lookup, backed by the map a
// test fills in.
func (s *stubService) QuotaFor(_ context.Context, name string) (user.Quota, bool) {
	q, ok := s.quotas[name]
	return q, ok
}

// MatchKey implements the authenticator's user lookup over the same map, so a
// test can set a user's key and have it authenticate as that user.
func (s *stubService) MatchKey(_ context.Context, key string) (string, bool) {
	for name, stored := range s.keys {
		if stored == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(stored)) == 1 {
			return name, true
		}
	}
	return "", false
}

// addUser registers a user with a key, for a test that wants to act as one.
func (s *stubService) addUser(name, key string, quota user.Quota) {
	s.keys[name] = key
	s.quotas[name] = quota
}
