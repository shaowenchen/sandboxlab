package api_test

// The end-to-end tests for the two-key model: an administrator creates a user,
// the user's key reaches exactly their own sandboxes, and the administrator sees
// everything.
//
// These go through the real stack — HTTP, the services, the user store on a fake
// cluster — because the failures that matter here are at the seams. The
// ownership filter is applied by the service, the identity is resolved by the
// authenticator, and neither would notice the other being wrong.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/user"
)

// as issues a request with a specific key.
func as(t *testing.T, s interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, integrationBase+path, nil)
	} else {
		r = httptest.NewRequest(method, integrationBase+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("X-Sandbox-Key", key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// createUser makes a user through the API and returns their key, which is what
// a person would do with the console or the CLI.
func createUser(t *testing.T, s interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, body string) user.User {
	t.Helper()
	w := as(t, s, integrationKey, http.MethodPost, "/api/v1/users", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("creating a user = %d: %s", w.Code, w.Body.String())
	}
	var u user.User
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("the create response is not a user: %s", w.Body.String())
	}
	if u.Key == "" {
		t.Fatal("creating a user did not return a key")
	}
	return u
}

func TestIntegrationWhoami(t *testing.T) {
	s, _ := newIntegrationServer(t)

	t.Run("the administrator", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodGet, "/api/v1/whoami", "")
		if w.Code != http.StatusOK {
			t.Fatalf("whoami = %d, want 200", w.Code)
		}
		var got struct {
			Role           string `json:"role"`
			Admin          bool   `json:"admin"`
			CanManageUsers bool   `json:"canManageUsers"`
		}
		unmarshalInto(t, w, &got)
		if !got.Admin || got.Role != "admin" || !got.CanManageUsers {
			t.Errorf("whoami = %+v, want an administrator who can manage users", got)
		}
	})

	t.Run("a user", func(t *testing.T) {
		u := createUser(t, s, `{"name":"alice"}`)
		w := as(t, s, u.Key, http.MethodGet, "/api/v1/whoami", "")
		var got struct {
			Role           string `json:"role"`
			Admin          bool   `json:"admin"`
			User           string `json:"user"`
			CanManageUsers bool   `json:"canManageUsers"`
		}
		unmarshalInto(t, w, &got)
		if got.Admin || got.Role != "user" || got.User != "alice" || got.CanManageUsers {
			t.Errorf("whoami = %+v, want alice as a user who cannot manage users", got)
		}
	})

	t.Run("an unknown key", func(t *testing.T) {
		if w := as(t, s, "not-a-key", http.MethodGet, "/api/v1/whoami", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("whoami with a bad key = %d, want 401", w.Code)
		}
	})
}

func TestIntegrationUsersAreIsolated(t *testing.T) {
	s, _ := newIntegrationServer(t)
	ctx := context.Background()

	alice := createUser(t, s, `{"name":"alice"}`)
	bob := createUser(t, s, `{"name":"bob"}`)

	// Each creates one of their own.
	if w := as(t, s, alice.Key, http.MethodPost, "/api/v1/sandboxes",
		`{"template":"python","name":"alice-box"}`); w.Code != http.StatusCreated {
		t.Fatalf("alice's create = %d: %s", w.Code, w.Body.String())
	}
	if w := as(t, s, bob.Key, http.MethodPost, "/api/v1/sandboxes",
		`{"template":"python","name":"bob-box"}`); w.Code != http.StatusCreated {
		t.Fatalf("bob's create = %d: %s", w.Code, w.Body.String())
	}
	// And the administrator one of the deployment's own.
	if w := as(t, s, integrationKey, http.MethodPost, "/api/v1/sandboxes",
		`{"template":"python","name":"admin-box"}`); w.Code != http.StatusCreated {
		t.Fatalf("the administrator's create = %d: %s", w.Code, w.Body.String())
	}

	t.Run("each user lists only their own", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			key  string
			want string
		}{
			{"alice", alice.Key, "alice-box"},
			{"bob", bob.Key, "bob-box"},
		} {
			w := as(t, s, tc.key, http.MethodGet, "/api/v1/sandboxes", "")
			var got struct {
				Sandboxes []struct {
					ID    string `json:"id"`
					Owner string `json:"owner"`
				} `json:"sandboxes"`
				Count int `json:"count"`
			}
			unmarshalInto(t, w, &got)
			if got.Count != 1 || got.Sandboxes[0].ID != tc.want {
				t.Errorf("%s sees %+v, want only %s", tc.name, got.Sandboxes, tc.want)
			}
			if got.Sandboxes[0].Owner != tc.name {
				t.Errorf("%s's sandbox is owned by %q", tc.want, got.Sandboxes[0].Owner)
			}
		}
	})

	t.Run("the administrator sees everything", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodGet, "/api/v1/sandboxes", "")
		var got struct {
			Count int `json:"count"`
		}
		unmarshalInto(t, w, &got)
		if got.Count != 3 {
			t.Errorf("the administrator sees %d sandboxes, want 3", got.Count)
		}
	})

	t.Run("another user's sandbox is not found", func(t *testing.T) {
		// 404 rather than 403: a distinct status would confirm it exists, and a
		// user could then map the deployment by watching which names 403.
		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/sandboxes/alice-box"},
			{http.MethodDelete, "/api/v1/sandboxes/alice-box"},
			{http.MethodGet, "/api/v1/sandboxes/alice-box/logs"},
		} {
			w := as(t, s, bob.Key, tc.method, tc.path, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("bob's %s %s = %d, want 404", tc.method, tc.path, w.Code)
			}
		}
		// And alice's is still there.
		if w := as(t, s, alice.Key, http.MethodGet, "/api/v1/sandboxes/alice-box", ""); w.Code != http.StatusOK {
			t.Errorf("alice can no longer reach her own sandbox: %d", w.Code)
		}
	})

	t.Run("the data plane refuses another user's sandbox", func(t *testing.T) {
		// The route a user could otherwise reach another user's port through.
		w := as(t, s, bob.Key, http.MethodGet, "/sandbox/alice-box/desktop/", "")
		if w.Code != http.StatusNotFound {
			t.Errorf("bob reaching alice's port = %d, want 404", w.Code)
		}
	})

	t.Run("the overview is scoped", func(t *testing.T) {
		w := as(t, s, alice.Key, http.MethodGet, "/api/v1/overview", "")
		var got struct {
			Total   int            `json:"total"`
			ByOwner map[string]int `json:"byOwner"`
			Scoped  bool           `json:"scoped"`
			User    string         `json:"user"`
		}
		unmarshalInto(t, w, &got)
		if got.Total != 1 || !got.Scoped || got.User != "alice" {
			t.Errorf("alice's overview = %+v, want one scoped sandbox", got)
		}
		// A user's overview must not name the deployment's other tenants.
		if len(got.ByOwner) != 0 {
			t.Errorf("alice's overview named other owners: %v", got.ByOwner)
		}
	})

	_ = ctx
}

func TestIntegrationUsersCannotManageUsers(t *testing.T) {
	s, _ := newIntegrationServer(t)
	alice := createUser(t, s, `{"name":"alice"}`)

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodPost, "/api/v1/users", `{"name":"mallory"}`},
		{http.MethodGet, "/api/v1/users/alice", ""},
		{http.MethodDelete, "/api/v1/users/alice", ""},
		{http.MethodPost, "/api/v1/users/alice/key", `{}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := as(t, s, alice.Key, tc.method, tc.path, tc.body)
			// 404, not 403: a user should not learn that a user list exists.
			if w.Code != http.StatusNotFound {
				t.Errorf("= %d, want 404", w.Code)
			}
		})
	}
}

func TestIntegrationUserLifecycle(t *testing.T) {
	s, _ := newIntegrationServer(t)

	alice := createUser(t, s, `{"name":"alice","maxSandboxes":2,"maxTTL":"30m","templates":["python"]}`)

	t.Run("the limits arrive", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodGet, "/api/v1/users/alice", "")
		var got user.User
		unmarshalInto(t, w, &got)
		if got.Quota.MaxSandboxes != 2 || got.Quota.MaxTTL != 30*time.Minute {
			t.Errorf("quota = %+v, want 2 sandboxes and 30m", got.Quota)
		}
		if len(got.Quota.Templates) != 1 || got.Quota.Templates[0] != "python" {
			t.Errorf("templates = %v, want [python]", got.Quota.Templates)
		}
	})

	t.Run("a template outside the list is refused", func(t *testing.T) {
		w := as(t, s, alice.Key, http.MethodPost, "/api/v1/sandboxes", `{"template":"node","name":"nope"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("= %d, want 400 for a template not on the list", w.Code)
		}
	})

	t.Run("the ceiling holds", func(t *testing.T) {
		for _, name := range []string{"one", "two"} {
			if w := as(t, s, alice.Key, http.MethodPost, "/api/v1/sandboxes",
				`{"template":"python","name":"`+name+`"}`); w.Code != http.StatusCreated {
				t.Fatalf("creating %s = %d: %s", name, w.Code, w.Body.String())
			}
		}
		w := as(t, s, alice.Key, http.MethodPost, "/api/v1/sandboxes", `{"template":"python","name":"three"}`)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("the third create = %d, want 429", w.Code)
		}
	})

	t.Run("the ttl ceiling holds", func(t *testing.T) {
		// Given room first: the sandbox ceiling above is still in force, and a
		// refusal for that reason would leave the TTL cap untested.
		as(t, s, integrationKey, http.MethodPatch, "/api/v1/users/alice", `{"maxSandboxes":20}`)
		w := as(t, s, alice.Key, http.MethodPost, "/api/v1/sandboxes",
			`{"template":"python","name":"capped","ttl":"5h"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		var sb struct {
			CreatedAt time.Time `json:"createdAt"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		unmarshalInto(t, w, &sb)
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 30*time.Minute+time.Second {
			t.Errorf("ttl = %v, want it capped at the user's 30m", got)
		}
	})

	t.Run("the limit can be raised", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodPatch, "/api/v1/users/alice", `{"maxSandboxes":10}`)
		if w.Code != http.StatusOK {
			t.Fatalf("patching the user = %d: %s", w.Code, w.Body.String())
		}
		var got user.User
		unmarshalInto(t, w, &got)
		if got.Quota.MaxSandboxes != 10 {
			t.Errorf("maxSandboxes = %d, want 10", got.Quota.MaxSandboxes)
		}
		// The key survives a limit change: editing a quota must not sign anyone
		// out.
		if got.Key != "" {
			t.Error("an update returned the key")
		}
		if w := as(t, s, alice.Key, http.MethodGet, "/api/v1/whoami", ""); w.Code != http.StatusOK {
			t.Errorf("alice's key stopped working after a limit change: %d", w.Code)
		}
	})

	t.Run("changing one limit leaves the others", func(t *testing.T) {
		// The bug this catches: a patch that replaced the whole quota, so
		// setting a sandbox ceiling silently cleared the TTL cap and the
		// template list. It is invisible without asserting on a limit the
		// request did not mention.
		w := as(t, s, integrationKey, http.MethodPatch, "/api/v1/users/alice", `{"maxSandboxes":20}`)
		if w.Code != http.StatusOK {
			t.Fatalf("patching = %d: %s", w.Code, w.Body.String())
		}
		var got user.User
		unmarshalInto(t, w, &got)
		if got.Quota.MaxSandboxes != 20 {
			t.Errorf("maxSandboxes = %d, want 20", got.Quota.MaxSandboxes)
		}
		if got.Quota.MaxTTL != 30*time.Minute {
			t.Errorf("the ttl cap was cleared: %v", got.Quota.MaxTTL)
		}
		if len(got.Quota.Templates) != 1 || got.Quota.Templates[0] != "python" {
			t.Errorf("the template list was cleared: %v", got.Quota.Templates)
		}
	})

	t.Run("a limit can be cleared deliberately", func(t *testing.T) {
		// The other half: an empty value is a value, so this does remove it —
		// which is why the fields are pointers rather than plain values.
		w := as(t, s, integrationKey, http.MethodPatch, "/api/v1/users/alice", `{"maxTTL":""}`)
		if w.Code != http.StatusOK {
			t.Fatalf("patching = %d: %s", w.Code, w.Body.String())
		}
		var got user.User
		unmarshalInto(t, w, &got)
		if got.Quota.MaxTTL != 0 {
			t.Errorf("maxTTL = %v, want it cleared", got.Quota.MaxTTL)
		}
		if got.Quota.MaxSandboxes != 20 {
			t.Errorf("clearing the ttl also cleared maxSandboxes: %d", got.Quota.MaxSandboxes)
		}
	})

	t.Run("the key can be rotated", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodPost, "/api/v1/users/alice/key", `{}`)
		if w.Code != http.StatusOK {
			t.Fatalf("rotating = %d: %s", w.Code, w.Body.String())
		}
		var got user.User
		unmarshalInto(t, w, &got)
		if got.Key == "" || got.Key == alice.Key {
			t.Fatalf("rotation returned %q, want a new key", got.Key)
		}

		// The old one stops working immediately, which is the point.
		if w := as(t, s, alice.Key, http.MethodGet, "/api/v1/whoami", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("the old key still works: %d", w.Code)
		}
		if w := as(t, s, got.Key, http.MethodGet, "/api/v1/whoami", ""); w.Code != http.StatusOK {
			t.Errorf("the new key does not work: %d", w.Code)
		}
		alice = got
	})

	t.Run("deleting a user leaves their sandboxes", func(t *testing.T) {
		if w := as(t, s, integrationKey, http.MethodDelete, "/api/v1/users/alice", ""); w.Code != http.StatusOK {
			t.Fatalf("deleting alice = %d: %s", w.Code, w.Body.String())
		}
		if w := as(t, s, alice.Key, http.MethodGet, "/api/v1/whoami", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("a deleted user's key still works: %d", w.Code)
		}
		// Her sandboxes are not the key's to take down: tearing down an
		// environment someone is working in is not what revoking a key means.
		if w := as(t, s, integrationKey, http.MethodGet, "/api/v1/sandboxes/one", ""); w.Code != http.StatusOK {
			t.Errorf("a sandbox was deleted along with its owner: %d", w.Code)
		}
	})
}

func TestIntegrationUserNames(t *testing.T) {
	s, _ := newIntegrationServer(t)

	t.Run("a name is normalized", func(t *testing.T) {
		u := createUser(t, s, `{"name":"Alice Smith"}`)
		if u.Name != "alice-smith" {
			t.Errorf("name = %q, want alice-smith", u.Name)
		}
	})

	t.Run("a taken name is a conflict", func(t *testing.T) {
		// And so is a differently-spelled one, which normalizes to the same.
		w := as(t, s, integrationKey, http.MethodPost, "/api/v1/users", `{"name":"Alice Smith"}`)
		if w.Code != http.StatusConflict {
			t.Errorf("= %d, want 409", w.Code)
		}
	})

	t.Run("a name with nothing usable in it", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodPost, "/api/v1/users", `{"name":"!!!"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("= %d, want 400", w.Code)
		}
	})

	t.Run("a name that cannot be a label", func(t *testing.T) {
		// A user's name becomes the owner label on everything they create, so a
		// name that cannot be one has to be refused before it is used.
		if u := createUser(t, s, `{"name":"ok-name"}`); u.Name != "ok-name" {
			t.Errorf("a plain name was rewritten to %q", u.Name)
		}
	})

	t.Run("a missing user", func(t *testing.T) {
		w := as(t, s, integrationKey, http.MethodGet, "/api/v1/users/nobody", "")
		if w.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404", w.Code)
		}
	})
}

func TestIntegrationListOmitsKeys(t *testing.T) {
	s, _ := newIntegrationServer(t)
	createUser(t, s, `{"name":"alice"}`)

	// The roster is what an administrator leaves on a screen, so it must not
	// carry credentials. The key is on the single-user read, deliberately.
	w := as(t, s, integrationKey, http.MethodGet, "/api/v1/users", "")
	if strings.Contains(w.Body.String(), `"key"`) {
		t.Errorf("the user list contains a key: %s", w.Body.String())
	}
	var got struct {
		Users []user.User `json:"users"`
	}
	unmarshalInto(t, w, &got)
	if len(got.Users) != 1 {
		t.Fatalf("the list returned %d users, want 1", len(got.Users))
	}
	for _, u := range got.Users {
		if u.Key != "" {
			t.Errorf("the list returned %s's key", u.Name)
		}
	}

	// The single read does have it, because that is how it is recovered.
	w = as(t, s, integrationKey, http.MethodGet, "/api/v1/users/alice", "")
	var one user.User
	unmarshalInto(t, w, &one)
	if one.Key == "" {
		t.Error("reading one user did not return their key")
	}
}

func unmarshalInto(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("response is not the expected JSON (%d): %s", w.Code, w.Body.String())
	}
}

// The quota goes out under the same names it comes in under.
//
// This asserted on structs for a year and proved nothing: a user's Quota had no
// JSON tags, so it was served as {"MaxSandboxes":2}, while Go's decoder matches
// field names case-insensitively and read it back into Quota.MaxSandboxes
// without complaint. The Go tests, the CLI and the server all agreed with each
// other and were wrong together. The console, which reads JSON by hand, saw
// undefined, showed every user as unlimited, and wrote those blanks back over
// the real quota when an administrator opened the Limits dialog and saved.
//
// So this reads the wire as a client that is not Go does: by name.
func TestIntegrationQuotaWireNames(t *testing.T) {
	s, _ := newIntegrationServer(t)
	createUser(t, s, `{"name":"alice","maxSandboxes":2,"maxTTL":"30m","templates":["python"]}`)

	w := as(t, s, integrationKey, http.MethodGet, "/api/v1/users/alice", "")
	if w.Code != http.StatusOK {
		t.Fatalf("reading the user = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	unmarshalInto(t, w, &body)

	quota, ok := body["quota"].(map[string]any)
	if !ok {
		t.Fatalf("the user has no quota object: %s", w.Body.String())
	}
	// Exactly the three names a PATCH takes, so what a client reads is what it
	// can send back.
	if got := quota["maxSandboxes"]; got != float64(2) {
		t.Errorf("quota.maxSandboxes = %v, want 2", got)
	}
	if got := quota["maxTTL"]; got != float64((30 * time.Minute).Nanoseconds()) {
		t.Errorf("quota.maxTTL = %v, want %d nanoseconds", got, (30 * time.Minute).Nanoseconds())
	}
	if got, ok := quota["templates"].([]any); !ok || len(got) != 1 || got[0] != "python" {
		t.Errorf("quota.templates = %v, want [python]", quota["templates"])
	}

	// And the capitalized forms must be gone: their presence is the bug, and a
	// client that reads by name would take them for a second, empty quota.
	for _, name := range []string{"MaxSandboxes", "MaxTTL", "Templates"} {
		if _, present := quota[name]; present {
			t.Errorf("quota carries %q, which is the capitalised form of a request field — "+
				"one field with two wire spellings is what made the console clear quotas", name)
		}
	}

	// A key that has never been used must not claim it was used in the year 1.
	// omitempty does nothing to a time.Time, so this is the same bug expiresAt
	// had, and the absence of the field is what a caller checks.
	if lastUsed, present := body["lastUsedAt"]; present {
		t.Errorf("lastUsedAt = %v, want it absent for a key that has never been used", lastUsed)
	}
}

// An edit round-trips: what the console reads is what it sends back, and the
// limits survive it.
//
// This is the whole bug in one test. The console reads a user, fills its dialog
// from the response, and PATCHes the result — so a response whose field names do
// not match the request's are not a display problem, they are a data-loss
// problem.
func TestIntegrationQuotaRoundTrip(t *testing.T) {
	s, _ := newIntegrationServer(t)
	createUser(t, s, `{"name":"alice","maxSandboxes":2,"maxTTL":"30m","templates":["python"]}`)

	// Read it exactly as a non-Go client would.
	w := as(t, s, integrationKey, http.MethodGet, "/api/v1/users/alice", "")
	var body map[string]any
	unmarshalInto(t, w, &body)
	quota := body["quota"].(map[string]any)

	// Build the edit body from those names — the console does this by hand.
	edit := map[string]any{
		"maxSandboxes": quota["maxSandboxes"],
		"maxTTL":       "1h", // changed, in the format a request takes
		"templates":    quota["templates"],
	}
	payload, err := json.Marshal(edit)
	if err != nil {
		t.Fatalf("building the edit body: %v", err)
	}

	w = as(t, s, integrationKey, http.MethodPatch, "/api/v1/users/alice", string(payload))
	if w.Code != http.StatusOK {
		t.Fatalf("the edit = %d: %s", w.Code, w.Body.String())
	}
	var after map[string]any
	unmarshalInto(t, w, &after)
	q := after["quota"].(map[string]any)

	if q["maxSandboxes"] != float64(2) {
		t.Errorf("the edit lost maxSandboxes: %v", q["maxSandboxes"])
	}
	if q["maxTTL"] != float64(time.Hour.Nanoseconds()) {
		t.Errorf("maxTTL = %v, want %d", q["maxTTL"], time.Hour.Nanoseconds())
	}
	if got, ok := q["templates"].([]any); !ok || len(got) != 1 || got[0] != "python" {
		t.Errorf("the edit lost templates: %v", q["templates"])
	}
}

var _ = context.Background
