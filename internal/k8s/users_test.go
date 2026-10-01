package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

func userStore(t *testing.T) (*UserStore, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	c := NewWithClientset(cs, config.Config{Namespace: "ops-system", SandboxNamespacePrefix: "sbx-"})
	return c.Users(), cs
}

func alice() user.User {
	return user.User{
		Name: "alice",
		Key:  "alice-key",
		Quota: user.Quota{
			MaxSandboxes: 3,
			MaxTTL:       2 * time.Hour,
			Templates:    []string{"python", "node"},
		},
	}
}

func TestUserCreateAndGet(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()

	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(ctx, "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "alice" || got.Key != "alice-key" {
		t.Errorf("Get = %+v, want alice with her key", got)
	}
	if got.Quota.MaxSandboxes != 3 || got.Quota.MaxTTL != 2*time.Hour {
		t.Errorf("quota did not round trip: %+v", got.Quota)
	}
	if len(got.Quota.Templates) != 2 {
		t.Errorf("templates did not round trip: %v", got.Quota.Templates)
	}
}

func TestUserCreateRejectsATakenName(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("the first Create: %v", err)
	}
	if _, err := store.Create(ctx, alice()); !errors.Is(err, ErrUserExists) {
		t.Errorf("Create with a taken name = %v, want ErrUserExists", err)
	}
}

func TestUserCreateValidates(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		u    user.User
	}{
		{"no name", user.User{Key: "k"}},
		// The name becomes a label value on everything the user creates, so a
		// name that cannot be one has to be refused here rather than at the
		// first create it is used in.
		{"an unusable name", user.User{Name: "Not A Name", Key: "k"}},
		{"no key", user.User{Name: "alice"}},
		{"a negative quota", user.User{Name: "alice", Key: "k", Quota: user.Quota{MaxSandboxes: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Create(ctx, tc.u); err == nil {
				t.Errorf("Create accepted %+v", tc.u)
			}
		})
	}
}

func TestUserGetNotFound(t *testing.T) {
	store, _ := userStore(t)
	if _, err := store.Get(context.Background(), "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Get of a missing user = %v, want ErrUserNotFound", err)
	}
}

func TestUserListOmitsKeys(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	for _, name := range []string{"alice", "bob"} {
		u := alice()
		u.Name = name
		u.Key = name + "-key"
		if _, err := store.Create(ctx, u); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d users, want 2", len(got))
	}
	if got[0].Name != "alice" || got[1].Name != "bob" {
		t.Errorf("List is not sorted by name: %+v", got)
	}
	// The key is dropped by the store rather than by the caller, so a listing
	// that reaches a screen can never carry one.
	for _, u := range got {
		if u.Key != "" {
			t.Errorf("List returned %s's key", u.Name)
		}
	}
}

func TestUserStoreIgnoresForeignSecrets(t *testing.T) {
	store, cs := userStore(t)
	ctx := context.Background()

	// A Secret whose name matches but is not ours must be neither readable as a
	// user nor overwritten.
	if _, err := cs.CoreV1().Secrets("ops-system").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: userSecretPrefix + "alice", Namespace: "ops-system"},
		Data:       map[string][]byte{"something": []byte("else")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the foreign Secret: %v", err)
	}

	if _, err := store.Get(ctx, "alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Get of a foreign Secret = %v, want ErrUserNotFound", err)
	}
	if err := store.Delete(ctx, "alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Delete of a foreign Secret = %v, want ErrUserNotFound", err)
	}
	// And it is still there.
	if _, err := cs.CoreV1().Secrets("ops-system").Get(ctx, userSecretPrefix+"alice", metav1.GetOptions{}); err != nil {
		t.Errorf("the foreign Secret was removed: %v", err)
	}
}

func TestUserUpdateKeepsTheKey(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Update(ctx, "alice", user.Quota{MaxSandboxes: 10})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Quota.MaxSandboxes != 10 {
		t.Errorf("the quota did not change: %+v", got.Quota)
	}
	// A quota change must not touch the credential — invalidating a key every
	// time a limit is edited would be a surprising way to lose a session.
	stored, err := store.Get(ctx, "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Key != "alice-key" {
		t.Errorf("the key changed on an update: %q", stored.Key)
	}
}

func TestUserSetKey(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.SetKey(ctx, "alice", "a-new-key")
	if err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if got.Key != "a-new-key" {
		t.Errorf("SetKey returned %q", got.Key)
	}
	// The quota survives a rotation, which is the bug a naive patch would cause:
	// replacing the whole data map drops everything else in it.
	if got.Quota.MaxSandboxes != 3 {
		t.Errorf("the quota was lost on a rotation: %+v", got.Quota)
	}

	if _, err := store.SetKey(ctx, "alice", ""); err == nil {
		t.Error("SetKey accepted an empty key")
	}
	if _, err := store.SetKey(ctx, "nobody", "k"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetKey on a missing user = %v, want ErrUserNotFound", err)
	}
}

func TestUserDelete(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(ctx, "alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Get after Delete = %v, want ErrUserNotFound", err)
	}
	if err := store.Delete(ctx, "alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("Delete twice = %v, want ErrUserNotFound", err)
	}
}

func TestUserTouch(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := store.Touch(ctx, "alice", when); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, err := store.Get(ctx, "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastUsedAt.Equal(when) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, when)
	}
}

func TestUserMatchKey(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()

	u := alice()
	if _, err := store.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}
	bob := alice()
	bob.Name = "bob"
	bob.Key = "bob-key"
	if _, err := store.Create(ctx, bob); err != nil {
		t.Fatalf("Create bob: %v", err)
	}

	tests := []struct {
		name string
		key  string
		want string
		ok   bool
	}{
		{"alice's key", "alice-key", "alice", true},
		{"bob's key", "bob-key", "bob", true},
		{"an unknown key", "nobody-key", "", false},
		{"an empty key", "", "", false},
		// A prefix of a real key must not match — the comparison is over the
		// whole value.
		{"a prefix of alice's key", "alice", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := store.MatchKey(ctx, tc.key)
			if ok != tc.ok || got != tc.want {
				t.Errorf("MatchKey(%q) = %q, %v; want %q, %v", tc.key, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestUserMatchKeyIsCaseSensitive(t *testing.T) {
	store, _ := userStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, alice()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := store.MatchKey(ctx, "ALICE-KEY"); ok {
		t.Error("MatchKey accepted an uppercased key")
	}
}

func TestCountByOwner(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	for _, req := range []CreateRequest{
		{ID: "one", Template: testTemplate(), Owner: "alice"},
		{ID: "two", Template: testTemplate(), Owner: "alice"},
		{ID: "three", Template: testTemplate(), Owner: "bob"},
		{ID: "four", Template: testTemplate()}, // the administrator's, no owner
	} {
		if _, err := c.Create(ctx, req); err != nil {
			t.Fatalf("Create %s: %v", req.ID, err)
		}
	}

	counts, err := c.CountByOwner(ctx)
	if err != nil {
		t.Fatalf("CountByOwner: %v", err)
	}
	if counts["alice"] != 2 || counts["bob"] != 1 || counts[""] != 1 {
		t.Errorf("CountByOwner = %v, want alice 2, bob 1, empty 1", counts)
	}
}

func TestListFiltersByOwner(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	for _, req := range []CreateRequest{
		{ID: "alices", Template: testTemplate(), Owner: "alice"},
		{ID: "bobs", Template: testTemplate(), Owner: "bob"},
		{ID: "admins", Template: testTemplate()},
	} {
		if _, err := c.Create(ctx, req); err != nil {
			t.Fatalf("Create %s: %v", req.ID, err)
		}
	}

	t.Run("one owner", func(t *testing.T) {
		got, err := c.List(ctx, "alice")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != "alices" {
			t.Errorf("List(alice) = %+v, want only alices", got)
		}
	})

	t.Run("everyone", func(t *testing.T) {
		got, err := c.List(ctx, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("List(\"\") returned %d, want 3", len(got))
		}
	})

	t.Run("an owner with nothing", func(t *testing.T) {
		got, err := c.List(ctx, "nobody")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("List(nobody) = %+v, want none", got)
		}
	})
}
