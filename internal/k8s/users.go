package k8s

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shaowenchen/sandboxlab/internal/user"
)

// Users are Secrets in the control plane's own namespace.
//
// The choice is the same one the rest of this makes — the cluster is the record
// — and it is also the right object: a user record holds a credential, and a
// Secret is where a credential belongs. A ConfigMap would be readable by anyone
// who can read the namespace.
//
// The alternative, holding users in memory, would lose every issued key on a
// restart, which on an ephemeral runner means every key dies with the process.
const (
	// userSecretPrefix names the Secret for a user: sandbox-user-<name>.
	userSecretPrefix = "sandbox-user-"
	// userLabel marks a Secret as a user record, so listing them is a label
	// query and nothing else in the namespace is ever a candidate.
	userLabel    = "sandbox.sandboxlab/user"
	keyDataKey   = "key"
	quotaDataKey = "quota"
	lastUsedKey  = "last-used"
)

// UserStore reads and writes user records.
type UserStore struct {
	client *Client
}

// Users returns the store.
func (c *Client) Users() *UserStore { return &UserStore{client: c} }

// ErrUserExists is returned when a user's name is taken.
var ErrUserExists = fmt.Errorf("user already exists")

// ErrUserNotFound is returned for a user that does not exist.
var ErrUserNotFound = fmt.Errorf("user not found")

func (s *UserStore) secretName(name string) string { return userSecretPrefix + name }

// NameFromSecret recovers a user's name from a Secret's name.
func (s *UserStore) NameFromSecret(secretName string) string {
	return strings.TrimPrefix(secretName, userSecretPrefix)
}

// MatchKey returns the name of the user a presented key belongs to.
//
// It is a linear scan with a constant-time comparison per user. That is
// deliberate rather than lazy: the alternative is an index keyed by a hash of
// the key, which is a second structure to keep in step with the Secrets for a
// deployment whose user list is a handful of people on a runner that lives for
// four hours. The comparison is constant time because the cost of not doing so
// is a timing oracle on a credential, and the cost of doing so is nothing.
//
// The scan is not weakened by the timing of the *list*: which users exist is
// not a secret — an administrator sees them in the console.
func (s *UserStore) MatchKey(ctx context.Context, key string) (string, bool) {
	if key == "" {
		return "", false
	}
	list, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: userLabel,
	})
	if err != nil {
		return "", false
	}
	for i := range list.Items {
		stored := list.Items[i].Data[keyDataKey]
		if len(stored) == 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key), stored) == 1 {
			return s.NameFromSecret(list.Items[i].Name), true
		}
	}
	return "", false
}

// Create stores a new user.
func (s *UserStore) Create(ctx context.Context, u user.User) (user.User, error) {
	if err := u.Validate(); err != nil {
		return user.User{}, err
	}
	secret, err := s.toSecret(u)
	if err != nil {
		return user.User{}, err
	}
	if _, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return user.User{}, fmt.Errorf("%w: a user named %q already exists", ErrUserExists, u.Name)
		}
		return user.User{}, fmt.Errorf("creating the user %q: %w", u.Name, err)
	}
	return u, nil
}

// Get reads one user, with the key included.
func (s *UserStore) Get(ctx context.Context, name string) (user.User, error) {
	secret, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Get(ctx, s.secretName(name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return user.User{}, fmt.Errorf("%w: no user named %q", ErrUserNotFound, name)
		}
		return user.User{}, fmt.Errorf("reading the user %q: %w", name, err)
	}
	if secret.Labels[userLabel] == "" {
		// A Secret whose name matches but is not ours. It is not a user as far
		// as this control plane is concerned, and must never be handed out or
		// overwritten.
		return user.User{}, fmt.Errorf("%w: no user named %q", ErrUserNotFound, name)
	}
	return s.fromSecret(secret)
}

// List returns every user, without their keys.
//
// The key is dropped here rather than at the handler so a listing can never
// leak one by accident: there is no caller of List that has the key to forget
// to omit.
func (s *UserStore) List(ctx context.Context) ([]user.User, error) {
	list, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: userLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	out := make([]user.User, 0, len(list.Items))
	for i := range list.Items {
		u, err := s.fromSecret(&list.Items[i])
		if err != nil {
			// One unreadable record should not hide the rest. It is reported as
			// present-but-broken rather than dropped, because dropping it would
			// make it invisible and therefore impossible to fix from here.
			out = append(out, user.User{Name: s.NameFromSecret(list.Items[i].Name)})
			continue
		}
		u.Key = ""
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Update replaces a user's quota, leaving the key alone.
func (s *UserStore) Update(ctx context.Context, name string, quota user.Quota) (user.User, error) {
	current, err := s.Get(ctx, name)
	if err != nil {
		return user.User{}, err
	}
	current.Quota = quota
	if err := current.Validate(); err != nil {
		return user.User{}, err
	}
	secret, err := s.toSecret(current)
	if err != nil {
		return user.User{}, err
	}
	if _, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return user.User{}, fmt.Errorf("updating the user %q: %w", name, err)
	}
	current.Key = ""
	return current, nil
}

// SetKey replaces a user's key.
//
// It reads and writes rather than patching, because the value has to be
// re-encoded and the quota has to survive — a patch that replaced the whole
// data map would drop it.
func (s *UserStore) SetKey(ctx context.Context, name, key string) (user.User, error) {
	current, err := s.Get(ctx, name)
	if err != nil {
		return user.User{}, err
	}
	if key == "" {
		return user.User{}, fmt.Errorf("a key cannot be empty")
	}
	current.Key = key
	secret, err := s.toSecret(current)
	if err != nil {
		return user.User{}, err
	}
	if _, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return user.User{}, fmt.Errorf("updating the user %q: %w", name, err)
	}
	return current, nil
}

// Delete removes a user. It does not touch their sandboxes, which have their
// own lifetimes.
func (s *UserStore) Delete(ctx context.Context, name string) error {
	secret, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Get(ctx, s.secretName(name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: no user named %q", ErrUserNotFound, name)
		}
		return fmt.Errorf("reading the user %q: %w", name, err)
	}
	if secret.Labels[userLabel] == "" {
		return fmt.Errorf("%w: no user named %q", ErrUserNotFound, name)
	}
	if err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the user %q: %w", name, err)
	}
	return nil
}

// Touch records that a user's key was used.
//
// A failure here is not returned: it is a bookkeeping write on a request that
// has already been authorised, and failing a user's request because their
// "last used" could not be stamped would be a poor trade. It is the caller's to
// log.
func (s *UserStore) Touch(ctx context.Context, name string, now time.Time) error {
	secret, err := s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Get(ctx, s.secretName(name), metav1.GetOptions{})
	if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[lastUsedKey] = []byte(now.UTC().Format(time.RFC3339))
	_, err = s.client.cs.CoreV1().Secrets(s.client.cfg.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func (s *UserStore) toSecret(u user.User) (*corev1.Secret, error) {
	quota, err := json.Marshal(u.Quota)
	if err != nil {
		return nil, fmt.Errorf("encoding the quota: %w", err)
	}
	data := map[string][]byte{
		keyDataKey:   []byte(u.Key),
		quotaDataKey: quota,
	}
	if !u.LastUsedAt.IsZero() {
		data[lastUsedKey] = []byte(u.LastUsedAt.UTC().Format(time.RFC3339))
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.secretName(u.Name),
			Namespace: s.client.cfg.Namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				userLabel:      u.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}, nil
}

func (s *UserStore) fromSecret(secret *corev1.Secret) (user.User, error) {
	u := user.User{
		Name: secret.Labels[userLabel],
	}
	if u.Name == "" {
		u.Name = s.NameFromSecret(secret.Name)
	}
	u.Key = string(secret.Data[keyDataKey])
	if raw, ok := secret.Data[quotaDataKey]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &u.Quota); err != nil {
			return u, fmt.Errorf("the quota on %q is unreadable: %w", u.Name, err)
		}
	}
	if raw, ok := secret.Data[lastUsedKey]; ok {
		if t, err := time.Parse(time.RFC3339, string(raw)); err == nil {
			u.LastUsedAt = t
		}
	}
	u.CreatedAt = secret.CreationTimestamp.Time
	return u, nil
}
