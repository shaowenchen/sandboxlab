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

	// CreatedAt and ExpiresAt bracket the sandbox's life. ExpiresAt is the zero
	// time when the sandbox has no TTL.
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`

	// Endpoints are the URLs the sandbox is reached at, one per template port.
	Endpoints []Endpoint `json:"endpoints,omitempty"`

	// Env is the non-secret environment the sandbox was created with.
	Env map[string]string `json:"env,omitempty"`

	// Namespace is the Kubernetes namespace the sandbox owns.
	Namespace string `json:"namespace,omitempty"`

	// Owner is the user who created it. Empty means it was created by the
	// administrator, which is the deployment's own rather than anyone's.
	Owner string `json:"owner,omitempty"`
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

// TTLRemaining is how long the sandbox has left, or zero when it has no TTL.
func (s Sandbox) TTLRemaining(now time.Time) time.Duration {
	if s.ExpiresAt.IsZero() {
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
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// Catalog is an ordered set of templates.
type Catalog struct {
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
func (c *Catalog) Get(id string) (Template, bool) {
	t, ok := c.templates[id]
	return t, ok
}

// List returns every template, ordered by id so the console and the CLI do not
// reshuffle between calls.
func (c *Catalog) List() []Template {
	out := make([]Template, 0, len(c.templates))
	for _, t := range c.templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Len is how many templates the catalog holds.
func (c *Catalog) Len() int { return len(c.templates) }

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
