// Package userservice is the administrator's view of the user list: creating
// users, rotating their keys, and counting what they have.
//
// It exists as its own layer because a user is not a sandbox — the rules are
// different (a name is unique, a key is a credential, a quota is changed rather
// than a sandbox created) and mixing them into the sandbox service would make
// both harder to read. The two meet only at the sandbox service's quota lookup.
package userservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// Errors the HTTP layer maps to statuses, by sentinel rather than by message.
var (
	// ErrInvalid is the caller's fault.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound is a user that does not exist.
	ErrNotFound = k8s.ErrUserNotFound
	// ErrConflict is a name that is taken.
	ErrConflict = k8s.ErrUserExists
)

// SandboxCounter counts what a user has, for the roster.
type SandboxCounter interface {
	CountByOwner(ctx context.Context) (map[string]int, error)
}

// Service manages users.
type Service struct {
	store *k8s.UserStore
	// counter counts sandboxes per owner. Nil means counts are left out, which
	// is what a deployment with no cluster access would pass.
	counter SandboxCounter
}

// New builds the service.
func New(store *k8s.UserStore, counter SandboxCounter) *Service {
	return &Service{store: store, counter: counter}
}

// Store returns the store, so the authenticator can resolve keys through it.
func (s *Service) Store() *k8s.UserStore { return s.store }

// QuotaFor implements the sandbox service's Quotas interface.
func (s *Service) QuotaFor(ctx context.Context, name string) (user.Quota, bool) {
	u, err := s.store.Get(ctx, name)
	if err != nil {
		return user.Quota{}, false
	}
	return u.Quota, true
}

// MatchKey implements the authenticator's UserLookup interface.
func (s *Service) MatchKey(ctx context.Context, key string) (string, bool) {
	return s.store.MatchKey(ctx, key)
}

// CreateInput is what an administrator supplies to make a user.
type CreateInput struct {
	Name  string
	Quota user.Quota
	// Key is the credential to issue. Empty generates one, which is the usual
	// case and the reason the generated value is returned once.
	Key string
}

// Create makes a user and returns it with its key.
//
// The key is in the response and nowhere else: it is not stored in the clear
// anywhere a later read would surface it except the Secret itself, and a
// listing omits it. Losing it means rotating, which is the intended workflow.
func (s *Service) Create(ctx context.Context, in CreateInput) (user.User, error) {
	name := user.NormalizeName(in.Name)
	if name == "" {
		return user.User{}, fmt.Errorf("%w: a user needs a name", ErrInvalid)
	}
	u := user.User{
		Name:  name,
		Quota: normalizeQuota(in.Quota),
		Key:   in.Key,
	}
	if u.Key == "" {
		key, err := generateKey()
		if err != nil {
			return user.User{}, err
		}
		u.Key = key
	}
	if err := u.Validate(); err != nil {
		return user.User{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	created, err := s.store.Create(ctx, u)
	if err != nil {
		if errors.Is(err, k8s.ErrUserExists) {
			return user.User{}, fmt.Errorf("%w: a user named %q already exists", ErrConflict, name)
		}
		return user.User{}, err
	}
	return created, nil
}

// Get returns one user, with the key — it is the administrator's own record of
// a credential they issued, and they may need to see it.
func (s *Service) Get(ctx context.Context, name string) (user.User, error) {
	u, err := s.store.Get(ctx, user.NormalizeName(name))
	if err != nil {
		if errors.Is(err, k8s.ErrUserNotFound) {
			return user.User{}, fmt.Errorf("%w: %s", ErrNotFound, err)
		}
		return user.User{}, err
	}
	if s.counter != nil {
		if counts, err := s.counter.CountByOwner(ctx); err == nil {
			u.Sandboxes = counts[u.Name]
		}
	}
	return u, nil
}

// List returns every user, without keys, with their sandbox counts.
func (s *Service) List(ctx context.Context) ([]user.User, error) {
	users, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	if s.counter == nil {
		return users, nil
	}
	// A failure to count is not a failure to list: the roster is still useful
	// without the column, and refusing to show users because a count could not
	// be read would be the wrong trade.
	counts, err := s.counter.CountByOwner(ctx)
	if err != nil {
		return users, nil
	}
	for i := range users {
		users[i].Sandboxes = counts[users[i].Name]
	}
	return users, nil
}

// Update changes a user's quota, leaving the key alone.
func (s *Service) Update(ctx context.Context, name string, quota user.Quota) (user.User, error) {
	u, err := s.store.Update(ctx, user.NormalizeName(name), normalizeQuota(quota))
	if err != nil {
		if errors.Is(err, k8s.ErrUserNotFound) {
			return user.User{}, fmt.Errorf("%w: %s", ErrNotFound, err)
		}
		return user.User{}, err
	}
	return u, nil
}

// Rotate issues a new key for a user and returns it.
func (s *Service) Rotate(ctx context.Context, name, key string) (user.User, error) {
	if key == "" {
		generated, err := generateKey()
		if err != nil {
			return user.User{}, err
		}
		key = generated
	}
	u, err := s.store.SetKey(ctx, user.NormalizeName(name), key)
	if err != nil {
		if errors.Is(err, k8s.ErrUserNotFound) {
			return user.User{}, fmt.Errorf("%w: %s", ErrNotFound, err)
		}
		return user.User{}, err
	}
	return u, nil
}

// Delete removes a user.
//
// It does not delete their sandboxes, which is deliberate. A sandbox is a
// running environment someone may be in the middle of using, and revoking a key
// is not the same act as tearing that down. They have lifetimes of their own;
// an administrator who wants them gone can delete them by id.
func (s *Service) Delete(ctx context.Context, name string) error {
	err := s.store.Delete(ctx, user.NormalizeName(name))
	if err != nil {
		if errors.Is(err, k8s.ErrUserNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, err)
		}
		return err
	}
	return nil
}

// normalizeQuota tidies a quota that arrived from JSON.
func normalizeQuota(q user.Quota) user.Quota {
	out := user.Quota{
		MaxSandboxes: q.MaxSandboxes,
		MaxTTL:       q.MaxTTL,
	}
	for _, t := range q.Templates {
		if t = strings.TrimSpace(t); t != "" {
			out.Templates = append(out.Templates, t)
		}
	}
	return out
}

// ParseTemplates reads a comma-separated template list, which is what both the
// CLI flag and the console's text field give.
func ParseTemplates(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// generateKey mints a user's key: 24 hex characters, 12 bytes.
//
// Shorter than the administrator's 32 because it is one of many and is meant to
// be passed around a team; still far past guessing, and the comparison is
// constant time.
func generateKey() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ValidName reports whether a name can be a user's, for a caller that wants to
// check before trying.
func ValidName(name string) bool {
	return model.IsValidID(user.NormalizeName(name))
}
