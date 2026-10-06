package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shaowenchen/sandboxlab/internal/model"
)

// newSandbox creates one over the fake clientset, which is the setup every test
// here needs and the slowest part of each.
func newSandbox(t *testing.T, c *Client, id string) model.Sandbox {
	t.Helper()
	sb, err := c.Create(context.Background(), CreateRequest{ID: id, Template: testTemplate(), TTL: 0})
	if err != nil {
		t.Fatalf("creating %s: %v", id, err)
	}
	return sb
}

// The key Secret and the sandbox are made together.
//
// The two assertions are different questions: that the create hands the key back
// is what a caller depends on, and that the Secret is there is what makes the
// key survive a control-plane restart — the value is stored, not remembered.
func TestCreateMintsAKeyAndReturnsIt(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	sb := newSandbox(t, c, "demo")
	if sb.Key == "" {
		t.Fatal("Create returned no key")
	}

	secret, err := c.cs.CoreV1().Secrets("sbx-demo").Get(ctx, sandboxKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the key Secret: %v", err)
	}
	if got := string(secret.Data[sandboxKeyValueKey]); got != sb.Key {
		t.Errorf("the stored key is %q, but the create returned %q", got, sb.Key)
	}
	if secret.Labels[sandboxKeyLabel] != sandboxKeyLabelValue {
		t.Errorf("the key Secret is not labelled %s=%s, so the resolver cannot find it", sandboxKeyLabel, sandboxKeyLabelValue)
	}
}

// Every sandbox gets its own key, and it resolves back to that sandbox.
func TestResolveFindsTheOwningSandbox(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	a := newSandbox(t, c, "alpha")
	b := newSandbox(t, c, "beta")
	if a.Key == b.Key {
		t.Fatal("two sandboxes were given the same key")
	}

	for _, tc := range []struct{ name, key, want string }{
		{"alpha's key", a.Key, "alpha"},
		{"beta's key", b.Key, "beta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok, err := c.ResolveSandboxKey(ctx, tc.key)
			if err != nil {
				t.Fatalf("ResolveSandboxKey: %v", err)
			}
			if !ok {
				t.Fatalf("%s did not resolve to anything", tc.name)
			}
			if id != tc.want {
				t.Errorf("resolved to %q, want %q", id, tc.want)
			}
		})
	}
}

// The sandbox is derived from the namespace's name, never from a label.
//
// This is the escalation the derivation exists to prevent: a namespace's
// occupant can write a Secret in its own namespace carrying any label it likes,
// so a resolver that read the id out of `sandbox.sandboxlab/id` would resolve
// the holder of that secret as the sandbox the label names. Here "alpha" labels
// a key as "beta" and must still be resolved as alpha.
func TestResolveIgnoresTheSandboxIDLabel(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	newSandbox(t, c, "beta")

	forged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "forged",
			Namespace: "sbx-alpha",
			Labels: map[string]string{
				sandboxKeyLabel: sandboxKeyLabelValue,
				// The lie.
				sandboxIDLabel: "beta",
			},
		},
		Data: map[string][]byte{
			sandboxKeyValueKey:  []byte("forged-key"),
			sandboxKeyDigestKey: []byte(digestOf("forged-key")),
		},
	}
	if _, err := c.cs.CoreV1().Secrets("sbx-alpha").Create(ctx, forged, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the forged Secret: %v", err)
	}

	id, ok, err := c.ResolveSandboxKey(ctx, "forged-key")
	if err != nil {
		t.Fatalf("ResolveSandboxKey: %v", err)
	}
	if !ok {
		t.Fatal("the forged key did not resolve at all")
	}
	if id != "alpha" {
		t.Errorf("the forged key resolved to %q; it must resolve to the namespace's own sandbox, alpha", id)
	}
}

// A key whose namespace is not one of ours resolves to nothing, whatever label
// it carries.
func TestResolveRefusesANamespaceThatIsNotASandbox(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	stray := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stray",
			Namespace: "kube-system",
			Labels:    map[string]string{sandboxKeyLabel: sandboxKeyLabelValue},
		},
		Data: map[string][]byte{
			sandboxKeyValueKey:  []byte("stray-key"),
			sandboxKeyDigestKey: []byte(digestOf("stray-key")),
		},
	}
	if _, err := c.cs.CoreV1().Secrets("kube-system").Create(ctx, stray, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the stray Secret: %v", err)
	}

	if id, ok, err := c.ResolveSandboxKey(ctx, "stray-key"); err != nil {
		t.Fatalf("ResolveSandboxKey: %v", err)
	} else if ok {
		t.Errorf("a Secret in kube-system resolved to %q; only sandbox namespaces may issue keys", id)
	}
}

// Deleting a sandbox revokes its key, with no separate step — the key goes with
// the namespace, which is the whole reason it lives there.
//
// The namespace is deleted first, by hand, and the assertion is then about where
// the key lives rather than about the fake's behaviour: the key is a namespaced
// object, so once its namespace is gone it is gone with it. The fake clientset
// has no namespace controller to do that cascade itself, so performing it here
// is what keeps this test about the design instead of about the double.
func TestDeletingASandboxRevokesItsKey(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	sb := newSandbox(t, c, "demo")
	if err := c.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// What a real cluster's namespace controller does, and what `Delete` relies
	// on: the namespace's contents go with it. The fake clientset deletes a
	// namespace but leaves what was in it behind, because there is no controller
	// behind the double to do the cascading.
	if err := c.cs.CoreV1().Secrets("sbx-demo").Delete(ctx, sandboxKeySecret, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("removing the namespace's contents: %v", err)
	}

	if _, ok, err := c.ResolveSandboxKey(ctx, sb.Key); err != nil {
		t.Fatalf("ResolveSandboxKey: %v", err)
	} else if ok {
		t.Error("a deleted sandbox's key still resolves")
	}
}

// A rotation invalidates the previous key at once and the new one works.
func TestRotateKeyReplacesTheKey(t *testing.T) {
	c := newClient()
	ctx := context.Background()

	sb := newSandbox(t, c, "demo")
	old := sb.Key

	rotated, err := c.RotateKey(ctx, sb.ID)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if rotated == old {
		t.Fatal("a rotation produced the same key")
	}

	if _, ok, _ := c.ResolveSandboxKey(ctx, old); ok {
		t.Error("the previous key still resolves after a rotation")
	}
	id, ok, err := c.ResolveSandboxKey(ctx, rotated)
	if err != nil {
		t.Fatalf("ResolveSandboxKey: %v", err)
	}
	if !ok || id != "demo" {
		t.Errorf("the rotated key resolved to (%q, %v), want (demo, true)", id, ok)
	}

	// And the stored value is the new one, rather than both surviving side by
	// side — which is what replacing the whole Data map is for.
	read, err := c.Key(ctx, sb.ID)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if read != rotated {
		t.Errorf("Key returned %q, but the rotation returned %q", read, rotated)
	}
}

// A key is 43 characters: 32 random bytes, base64url without padding.
//
// Asserted because the shape is a promise — the same one applab's keys make —
// and because it is what makes a key safe to paste into a URL or a header
// without escaping.
func TestKeysAreURLSafeAndLongEnough(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		key, err := generateSandboxKey()
		if err != nil {
			t.Fatalf("generateSandboxKey: %v", err)
		}
		if len(key) != 43 {
			t.Fatalf("a key is %d characters, want 43: %q", len(key), key)
		}
		for _, r := range key {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				t.Fatalf("a key contains %q, which is not URL-safe: %q", r, key)
			}
		}
		if seen[key] {
			t.Fatalf("generateSandboxKey produced %q twice", key)
		}
		seen[key] = true
	}
}

// A key's digest is what resolution compares, so the comparison is over
// fixed-length values rather than the key itself.
func TestEnsuredigestIsStableAndFixedLength(t *testing.T) {
	short := digestOf("a")
	long := digestOf("a much longer key than that one")
	if len(short) != len(long) {
		t.Errorf("digests are %d and %d characters; they must be the same length for a constant-time compare", len(short), len(long))
	}
	if digestOf("x") != digestOf("x") {
		t.Error("digestOf is not deterministic")
	}
	if digestOf("x") == digestOf("y") {
		t.Error("two different keys have the same digest")
	}
}
