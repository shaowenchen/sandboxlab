// Package user is the control plane's record of who may use it.
//
// There are two kinds of caller and one credential each. An administrator holds
// the deployment's own key and can do anything: see every sandbox, create users,
// change what they may do. A user holds a key issued to them and can create
// sandboxes and see their own — and only their own.
//
// A user is stored as a Secret in the control plane's own namespace, which is
// the same choice the rest of this makes: the cluster is the record, so a
// restart loses nothing and there is no database to back up. It is also the
// right object — a user record holds a credential, and a Secret is where a
// credential goes.
package user

import (
	"fmt"
	"strings"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/model"
)

// Quota is what a user may do. The zero value is "no limits from this user",
// which still leaves the deployment's own ceilings in force.
//
// The JSON names are the same ones UserRequest takes, because they describe the
// same three limits: a client reads a user, edits a limit, and sends the result
// back. When these had no tags they went out capitalized — `MaxSandboxes` where
// the request accepts `maxSandboxes` — so the console read undefined fields,
// showed every user as unlimited, and wrote the blanks back, clearing the real
// quota. One field with two wire spellings is the bug that caused it.
type Quota struct {
	// MaxSandboxes is how many may exist at once. Zero is no limit.
	MaxSandboxes int `json:"maxSandboxes"`
	// MaxTTL caps how long a sandbox may be asked for, in nanoseconds — what a
	// Go time.Duration is on the wire. Zero is no limit — but the deployment's
	// own maximum always applies, so a zero here does not mean unbounded.
	MaxTTL time.Duration `json:"maxTTL"`
	// Templates whitelists what may be created. Empty means every template in
	// the catalog, which is the useful default for a debugging environment
	// where the catalog is already curated.
	//
	// Omitted rather than null when there is no whitelist: the two are the same
	// thing to this service, but only one of them is a JSON array, and a client
	// generated from the spec types the field as an array.
	Templates []string `json:"templates,omitempty"`
}

// Allows reports whether a template may be created under this quota.
func (q Quota) Allows(template string) bool {
	if len(q.Templates) == 0 {
		return true
	}
	for _, t := range q.Templates {
		if t == template {
			return true
		}
	}
	return false
}

// Describe renders the quota for a table, so an administrator reading a list of
// users does not have to interpret zeros.
func (q Quota) Describe() string {
	var parts []string
	if q.MaxSandboxes > 0 {
		parts = append(parts, fmt.Sprintf("%d sandbox(es)", q.MaxSandboxes))
	}
	if q.MaxTTL > 0 {
		parts = append(parts, "up to "+q.MaxTTL.String())
	}
	if len(q.Templates) > 0 {
		parts = append(parts, strings.Join(q.Templates, ", "))
	}
	if len(parts) == 0 {
		return "the deployment's own limits"
	}
	return strings.Join(parts, "; ")
}

// User is one issued key.
type User struct {
	// Name identifies the user, and becomes the owner recorded on everything
	// they create. It is validated the same way a sandbox id is — see Validate.
	Name string `json:"name"`
	// Key is the credential. It is only present on a create or a rotate
	// response and on an explicit reveal; a listing omits it, so a list is safe
	// to leave on a screen.
	Key string `json:"key,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	// LastUsedAt is the last request made with this key, to within the store's
	// write throttle. Absent means it has never been used, which is the thing an
	// administrator most wants to know about a key.
	//
	// A pointer rather than a value: `omitempty` has no effect on a struct, so
	// the zero time went out as "0001-01-01T00:00:00Z" and every unused key read
	// as one last used in the year 1. Absence is what a caller checks, so
	// absence is what has to be sent — the same bug expiresAt had.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`

	Quota Quota `json:"quota"`

	// Sandboxes is how many the user currently has. It is filled in by the
	// service when listing, not stored — a count that is stored is a count that
	// goes wrong.
	Sandboxes int `json:"sandboxes,omitempty"`
}

// Validate rejects a name a user could not have.
//
// A user's name becomes the owner label on every sandbox they create, and a
// Kubernetes label value has to be a valid one — so this is the same rule a
// sandbox id follows, for the same reason: a name that cannot be a label would
// fail at create time on every request rather than once, here.
func (u User) Validate() error {
	if u.Name == "" {
		return fmt.Errorf("a user needs a name")
	}
	if !model.IsValidID(u.Name) {
		return fmt.Errorf("the name %q is not usable; use lowercase letters, digits and '-', up to 40 characters", u.Name)
	}
	// A user exists to hold a key. One without a key is a record nobody can
	// authenticate as — every request with an empty key is refused, so it would
	// be listed in the console as a user who cannot do anything and there would
	// be nothing to say about why.
	if u.Key == "" {
		return fmt.Errorf("a user needs a key")
	}
	if u.Quota.MaxSandboxes < 0 {
		return fmt.Errorf("a quota cannot be negative")
	}
	if u.Quota.MaxTTL < 0 {
		return fmt.Errorf("a quota cannot be negative")
	}
	for _, t := range u.Quota.Templates {
		if !model.IsValidID(t) {
			return fmt.Errorf("the template name %q is not usable", t)
		}
	}
	return nil
}

// NormalizeName lowercases a name and replaces anything else with '-', so a name
// someone typed becomes one a label can hold.
func NormalizeName(s string) string { return model.NormalizeID(s) }
