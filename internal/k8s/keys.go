// Per-sandbox API keys.
//
// A sandbox's key is a second tier of credential: it reaches that sandbox and
// nothing else, where the deployment's own key reaches everything. It lives in
// the sandbox's namespace, which is what makes its lifetime free — the key is
// created with the Deployment and goes away with the namespace, so deleting a
// sandbox revokes its key with no separate step and nothing to keep in step.
//
// The value is stored in the clear beside a digest of itself. Reversibly,
// deliberately: the deployment promises a key can be read back, and a key nobody
// can recover is one that has to be rotated the moment it is mislaid. The digest
// is what a presented key is matched against, so resolving one is a fixed-length
// comparison rather than a credential held in memory for every request.
//
// The cost, said plainly: the control plane reads Secrets in the namespaces it
// manages, so the deployment's key — and the cluster grants behind it — reach
// every sandbox's key. This tier separates one *caller* from another; it does
// not, and cannot, separate a caller from the operator.
package k8s

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shaowenchen/sandboxlab/internal/model"
)

const (
	// sandboxKeySecret is the fixed name of the Secret holding a sandbox's key.
	// One sandbox is one namespace, so one name is enough — the same reasoning
	// that gives the Deployment and the Service their fixed names.
	sandboxKeySecret = "sandbox-apikey"

	// sandboxKeyLabel marks a Secret as one of these. It is the selector the
	// resolver lists by: it has to find every sandbox's key without knowing
	// which sandboxes exist, and this is what narrows the list to ours.
	sandboxKeyLabel      = "sandbox.sandboxlab/key"
	sandboxKeyLabelValue = "true"

	// The two fields of the Secret's data.
	sandboxKeyValueKey  = "key"
	sandboxKeyDigestKey = "digest"

	// sandboxKeyBytes is the entropy behind a key. 32 bytes is what the applab
	// deployment uses for its app keys, which is the model this follows.
	sandboxKeyBytes = 32
)

// mintKey generates a key for a sandbox and stores it, replacing any key the
// sandbox already has.
//
// It is an upsert rather than an insert so that create and rotate are one
// operation: a sandbox being created has no key and gets one, and a sandbox
// being rotated has one and is given another. The alternative — a create that
// fails when a key exists — would make the second case an error the caller has
// to unwrap into the first.
func (c *Client) mintKey(ctx context.Context, ns, id string) (string, error) {
	key, err := generateSandboxKey()
	if err != nil {
		return "", err
	}
	data := map[string][]byte{
		sandboxKeyValueKey:  []byte(key),
		sandboxKeyDigestKey: []byte(digestOf(key)),
	}
	labels := keyLabels(id)

	secrets := c.cs.CoreV1().Secrets(ns)
	existing, err := secrets.Get(ctx, sandboxKeySecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sandboxKeySecret, Namespace: ns, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("creating the key for %s: %w", id, err)
		}
	case err != nil:
		return "", fmt.Errorf("reading the key for %s: %w", id, err)
	default:
		// The whole Data map is replaced rather than merged, so a key that was
		// once written cannot survive alongside the new one in a second field —
		// which would leave the rotated-away key still resolving.
		existing.Data = data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range labels {
			existing.Labels[k] = v
		}
		if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("updating the key for %s: %w", id, err)
		}
	}
	return key, nil
}

// Key returns a sandbox's key.
//
// The value is returned in full, because it is stored reversibly precisely so it
// can be read back — a key nobody can recover is one that has to be rotated the
// moment it is lost.
func (c *Client) Key(ctx context.Context, id string) (string, error) {
	secret, err := c.cs.CoreV1().Secrets(c.cfg.SandboxNamespace(id)).Get(ctx, sandboxKeySecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: the sandbox %q has no key", ErrNotFound, id)
		}
		return "", fmt.Errorf("reading the key for %s: %w", id, err)
	}
	key := string(secret.Data[sandboxKeyValueKey])
	if key == "" {
		// Stored but empty is treated as absent rather than as an empty
		// credential, which would authenticate nothing and be confusing to
		// debug.
		return "", fmt.Errorf("%w: the sandbox %q has no key", ErrNotFound, id)
	}
	return key, nil
}

// RotateKey replaces a sandbox's key, invalidating the previous one at once.
//
// There is no grace period and the old value is not retained: a rotation is
// normally performed because a key leaked, and a key that still works after
// being rotated away from has not been rotated.
func (c *Client) RotateKey(ctx context.Context, id string) (string, error) {
	return c.mintKey(ctx, c.cfg.SandboxNamespace(id), id)
}

// ResolveSandboxKey finds the sandbox a presented key belongs to.
//
// It lists every sandbox's key record, filtered by this deployment's own label,
// and compares digests rather than reading each key — which is what makes the
// comparison possible without the keys themselves being in memory for every
// request.
//
// The list is the cost, and it grows with the number of sandboxes: one small
// Secret per sandbox. That is the price of a per-sandbox credential with no
// index, and it is paid on every authenticated request that presents one. A
// deployment with hundreds of sandboxes would want the digest indexed somewhere
// — a change of layout, not of interface.
func (c *Client) ResolveSandboxKey(ctx context.Context, presented string) (string, bool, error) {
	// Trimmed before it is hashed, because the value reaching here has usually
	// been through a shell: `SANDBOX_KEY=$(cat keyfile)` keeps the file's
	// trailing newline, and a client turns that into a header without removing
	// it. A key that fails to resolve over a byte the caller cannot see is a
	// failure with no useful diagnostic.
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return "", false, nil
	}
	want := digestOf(presented)

	secrets, err := c.cs.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: sandboxKeyLabel + "=" + sandboxKeyLabelValue,
	})
	if err != nil {
		return "", false, fmt.Errorf("listing sandbox keys: %w", err)
	}

	var found string
	matched := false
	for i := range secrets.Items {
		s := &secrets.Items[i]
		stored := s.Data[sandboxKeyDigestKey]
		if len(stored) == 0 {
			continue
		}
		// Every candidate is compared even once one has matched, rather than
		// stopping at the first: returning early would make the response time
		// depend on which key matched — that is, on its position in the list —
		// and a caller has no business learning that. The candidate is the
		// stored digest, so the comparison is over fixed-length values and does
		// not leak a length either.
		if subtle.ConstantTimeCompare(stored, []byte(want)) == 1 && !matched {
			// The sandbox is derived from the namespace's NAME, never from a
			// label. Labels are written by whoever writes the object, and a
			// sandbox's own namespace is one the sandbox can write in — so a
			// Secret merely labelled with someone else's id would resolve its
			// holder as that sandbox, which is a way into an environment that is
			// not theirs. The namespace name is fixed at creation and cannot be
			// renamed, so it is the only trustworthy source.
			id, ok := c.sandboxIDFromNamespace(s.Namespace)
			if !ok {
				continue
			}
			found, matched = id, true
		}
	}
	return found, matched, nil
}

// sandboxIDFromNamespace recovers a sandbox id from the namespace that holds it,
// reporting whether the namespace is one of ours.
//
// The label query that produced the Secret is not proof that this namespace is a
// sandbox: anything that can write in any namespace can write this label. The
// prefix and the id's own shape are what establish it, and a namespace that is
// not ours simply resolves to nothing.
func (c *Client) sandboxIDFromNamespace(ns string) (string, bool) {
	if !strings.HasPrefix(ns, c.cfg.SandboxNamespacePrefix) {
		return "", false
	}
	id := strings.TrimPrefix(ns, c.cfg.SandboxNamespacePrefix)
	if !model.IsValidID(id) {
		return "", false
	}
	return id, true
}

// keyLabels is what a sandbox's key Secret carries: the same ownership labels as
// everything else the control plane creates, plus the one the resolver selects
// on.
func keyLabels(id string) map[string]string {
	return map[string]string{
		managedByLabel:  managedByValue,
		appLabelKey:     sandboxName,
		sandboxIDLabel:  id,
		sandboxKeyLabel: sandboxKeyLabelValue,
	}
}

// generateSandboxKey returns a new key.
//
// base64url rather than hex: the same entropy is 43 characters instead of 64,
// and every character is safe to paste into a URL, a header or a shell without
// escaping — which is exactly where a key ends up.
func generateSandboxKey() (string, error) {
	buf := make([]byte, sandboxKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a sandbox key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// digestOf is what a presented key is matched by.
func digestOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
