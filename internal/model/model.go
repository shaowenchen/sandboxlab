// Package model holds the domain types the API, the CLI and the console all
// speak: a sandbox template is what can be created, a sandbox is what was.
//
// The types are deliberately plain — no Kubernetes types leak through — so the
// HTTP layer can marshal them directly and the CLI can share them with the
// console without either importing client-go.
package model

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Template is one kind of environment that can be created: a name, the image it
// runs, the ports it serves, and the lifetime bounds a caller may ask for.
//
// It is the catalog's unit. Templates are defined in YAML (see the catalog
// package) so an environment can be extended without rebuilding the binary.
type Template struct {
	// ID is the stable identifier a create call names, and the value of the
	// sandbox.sandboxlab/template annotation. It is unique within the catalog.
	ID string `yaml:"id" json:"id"`
	// Title is the human name the console lists.
	Title string `yaml:"title" json:"title"`
	// Description says what the environment is for.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Image is the container image the sandbox runs.
	Image string `yaml:"image" json:"image"`

	// Ports are the container ports the sandbox serves. The first is the one
	// the data-plane proxy sends traffic to when a path carries no port name.
	Ports []Port `yaml:"ports,omitempty" json:"ports,omitempty"`

	// Env is the environment a template sets. It is merged under whatever the
	// caller passes, so a caller can override a template default.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// Command and Args override the image's own entrypoint, for images whose
	// default is not what a sandbox wants.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string `yaml:"args,omitempty" json:"args,omitempty"`

	// Resources is the sandbox's quota, applied both to the pod and to the
	// namespace it lives in.
	Resources Resources `yaml:"resources,omitempty" json:"resources,omitempty"`

	// TTLDefault is the lifetime a create call gets when it asks for none.
	// TTLMax is the ceiling a caller cannot exceed. Both are durations in text
	// ("30m", "2h"); zero means "no bound from this side".
	TTLDefault string `yaml:"ttlDefault,omitempty" json:"ttlDefault,omitempty"`
	TTLMax     string `yaml:"ttlMax,omitempty" json:"ttlMax,omitempty"`

	// Persistent asks for a volume mounted at WorkDir, for environments where
	// the workspace must outlive the container process. It is off by default:
	// most sandboxes are disposable, and a volume is the one object that does
	// not go away with the namespace unless it is deleted with it.
	Persistent bool `yaml:"persistent,omitempty" json:"persistent,omitempty"`
	// WorkDir is where a persistent volume is mounted.
	WorkDir string `yaml:"workDir,omitempty" json:"workDir,omitempty"`

	// Builtin marks a template that was compiled into the control plane rather
	// than added to it at runtime. It is not read from a document — a caller
	// cannot make a template built-in by setting it — but is set when the
	// catalog is loaded and reported over the API, where it is what protects
	// the shipped templates from being removed or overwritten by name.
	Builtin bool `yaml:"-" json:"builtin,omitempty"`
}

// Port is one port a sandbox serves.
type Port struct {
	// Name identifies the port within a template, and is the segment a
	// data-plane URL may carry to pick a non-default port.
	Name string `yaml:"name" json:"name"`
	// Port is the container port.
	Port int32 `yaml:"port" json:"port"`
	// Protocol is "TCP" or "UDP"; empty means TCP.
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	// Description is shown in the console beside the port.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Resources is a sandbox's resource envelope.
type Resources struct {
	// CPU and Memory are the requests/limits, in Kubernetes quantity syntax
	// ("500m", "2", "1Gi"). Empty leaves the field unset, which on a node with
	// no other shape is effectively unlimited — a template that cares should
	// set them.
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// SandboxState is where a sandbox is in its life, as far as the cluster shows.
type SandboxState string

const (
	// StatePending means the objects exist but the container is not running yet.
	StatePending SandboxState = "Pending"
	// StateRunning means the container is up and the sandbox can be reached.
	StateRunning SandboxState = "Running"
	// StateFailed means the container could not start or has died.
	StateFailed SandboxState = "Failed"
	// StateExpired means its TTL has passed and it is due for collection. It is
	// derived from the expiry time, not from the cluster, so it is reported
	// even before the reaper has removed the objects.
	StateExpired SandboxState = "Expired"
)

// Sandbox is one created environment.
//
// Everything here is derived: the control plane keeps no record of a sandbox,
// it reads the cluster. The fields that are not in the cluster (Template,
// ExpiresAt) live in annotations on the objects themselves, which is what makes
// a control-plane restart a non-event.
type Sandbox struct {
	// ID is both the name and the identity. It is cluster-safe (lowercase
	// alphanumeric and '-') because it is a namespace name.
	ID string `json:"id"`
	// Template is the catalog id this was created from.
	Template string `json:"template"`
	// Image is the running image, carried for display.
	Image string `json:"image"`
	// State is what the cluster shows, with Expired layered on top.
	State SandboxState `json:"state"`
	// Message explains a state that is not Running, when the cluster says why.
	Message string `json:"message,omitempty"`

	// CreatedAt and ExpiresAt bracket the sandbox's life. ExpiresAt is nil when
	// the sandbox has no TTL — a pointer, not a zero time, because `omitempty`
	// does not omit a struct and a zero time would go out as
	// "0001-01-01T00:00:00Z". The spec and the generated SDKs promise the field
	// is *absent* in that case, and a client that keys "no expiry" off its
	// absence would read the zero time as long expired.
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`

	// Endpoints are the URLs the sandbox is reached at, one per template port.
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// Env is the non-secret environment the sandbox was created with.
	Env map[string]string `json:"env,omitempty"`

	// Namespace is the Kubernetes namespace the sandbox owns.
	Namespace string `json:"namespace,omitempty"`
}

// Endpoint is one way to reach a sandbox.
type Endpoint struct {
	// Name is the port name it serves.
	Name string `json:"name"`
	// Port is the container port behind it.
	Port int32 `json:"port"`
	// URL is the address a caller opens. Empty when the deployment has no
	// public URL yet.
	URL string `json:"url,omitempty"`
}

// TimePtr returns a pointer to t. ExpiresAt is a pointer so that "no expiry" is
// the absence of the field, and this is what builds one for a value that is
// known to be set.
func TimePtr(t time.Time) *time.Time { return &t }

// TTLRemaining is how long the sandbox has left, or zero when it has no TTL.
func (s Sandbox) TTLRemaining(now time.Time) time.Duration {
	if s.ExpiresAt == nil {
		return 0
	}
	d := s.ExpiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Expired reports whether the sandbox's TTL has passed.
func (s Sandbox) Expired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// Catalog is an ordered set of templates.
//
// It is mutable: templates can be added and removed while a deployment is
// running, which is what makes a sandbox environment extensible without a
// release. It is therefore safe for concurrent use — the HTTP handlers read it
// on every request and write it when a template is added or removed — and it
// must not be copied, because the mutex lives inside it (a pointer is what the
// service, the console's API and the CLI all share).
type Catalog struct {
	mu        sync.RWMutex
	templates map[string]Template
}

// NewCatalog builds a catalog from templates, rejecting duplicates and
// templates that could not be created.
func NewCatalog(templates []Template) (*Catalog, error) {
	c := &Catalog{templates: make(map[string]Template, len(templates))}
	for _, t := range templates {
		if err := t.Validate(); err != nil {
			return nil, err
		}
		if _, ok := c.templates[t.ID]; ok {
			return nil, fmt.Errorf("duplicate template id %q", t.ID)
		}
		c.templates[t.ID] = t
	}
	return c, nil
}

// Get returns the template with the given id.
//
// The value is a snapshot; it is safe to read without holding the lock, but its
// slices and maps still alias the stored template's. Templates are treated as
// immutable once stored — Add replaces the whole value rather than editing one
// in place — so a reader never observes a half-changed template.
func (c *Catalog) Get(id string) (Template, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.templates[id]
	return t, ok
}

// List returns every template, ordered by id so the console and the CLI do not
// reshuffle between calls.
func (c *Catalog) List() []Template {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Template, 0, len(c.templates))
	for _, t := range c.templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Len is how many templates the catalog holds.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.templates)
}

// Add puts a template in the catalog, replacing any template with the same id.
//
// It is an upsert rather than an insert because templates are not persisted: a
// deployment that wants to change a template it added earlier — a new image, a
// different port — does it by adding the same id again, and the alternative
// would be delete-then-add for the common case of fixing a typo. The template
// is validated before the lock is taken, so an invalid one is rejected without
// disturbing the catalog.
//
// A built-in template is replaced too, but the replacement is marked Builtin:
// editing a shipped template is allowed, moving it is not.
//
// It reports whether an existing template was replaced, which is what lets the
// HTTP layer answer 200 for an edit and 201 for a new id.
func (c *Catalog) Add(t Template) (replaced bool, err error) {
	if err := t.Validate(); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, replaced := c.templates[t.ID]
	if replaced {
		t.Builtin = old.Builtin
	}
	c.templates[t.ID] = t
	return replaced, nil
}

// Remove takes a template out of the catalog, reporting whether it was there.
// A built-in template is not removed — it is marked Builtin and left alone, so
// the templates the binary ships cannot be deleted through the API.
func (c *Catalog) Remove(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.templates[id]
	if !ok || t.Builtin {
		return false
	}
	delete(c.templates, id)
	return true
}

// Validate checks a template is one a sandbox could actually be created from.
//
// It is strict on purpose: a template that is wrong here is wrong once, at
// load, rather than at every create call — and the person who can fix it is
// looking at the message.
func (t Template) Validate() error {
	switch {
	case t.ID == "":
		return fmt.Errorf("template has no id")
	case !IsValidID(t.ID):
		return fmt.Errorf("template id %q is not a valid id (lowercase alphanumerics and '-')", t.ID)
	case t.Image == "":
		return fmt.Errorf("template %q has no image", t.ID)
	}
	seen := make(map[string]bool, len(t.Ports))
	for _, p := range t.Ports {
		if p.Name == "" {
			return fmt.Errorf("template %q has a port with no name", t.ID)
		}
		if seen[p.Name] {
			return fmt.Errorf("template %q declares port %q twice", t.ID, p.Name)
		}
		seen[p.Name] = true
		if p.Port <= 0 || p.Port > 65535 {
			return fmt.Errorf("template %q port %q is not a valid port (%d)", t.ID, p.Name, p.Port)
		}
	}
	if _, err := t.ParsedTTLDefault(); err != nil {
		return fmt.Errorf("template %q has an invalid ttlDefault: %w", t.ID, err)
	}
	if _, err := t.ParsedTTLMax(); err != nil {
		return fmt.Errorf("template %q has an invalid ttlMax: %w", t.ID, err)
	}
	return nil
}

// ParsedTTLDefault is TTLDefault as a duration. Empty means no default.
func (t Template) ParsedTTLDefault() (time.Duration, error) {
	return parseOptionalDuration(t.TTLDefault)
}

// ParsedTTLMax is TTLMax as a duration. Empty means no ceiling.
func (t Template) ParsedTTLMax() (time.Duration, error) { return parseOptionalDuration(t.TTLMax) }

// ParsedWorkDir is WorkDir, or the image's conventional working directory.
func (t Template) ParsedWorkDir() string {
	if t.WorkDir != "" {
		return t.WorkDir
	}
	return "/workspace"
}

func parseOptionalDuration(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// IsValidID reports whether s can be a sandbox or template id.
//
// The rule is Kubernetes' own for a namespace name, because a sandbox id becomes
// one: lowercase alphanumerics and '-', beginning and ending alphanumeric, at
// most 40 characters. It is enforced here rather than left to the API server so
// a bad id is rejected with a message about the id rather than about a namespace.
func IsValidID(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// NormalizeID lowercases s and replaces runs of anything else with '-', so a
// name a person typed becomes one a namespace can hold. It does not promise the
// result is valid — call IsValidID — only that it is the closest a name gets.
func NormalizeID(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}
