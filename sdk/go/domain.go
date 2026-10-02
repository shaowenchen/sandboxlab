package sdk

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file is hand-written and is not touched by regeneration.
//
// The generator produces the shapes of the types; it cannot produce what they
// mean. Everything here is a rule about the domain: what a valid id is, when a
// sandbox has expired, what a template's lifetime resolves to. Keeping it in
// its own file — rather than beside the types, which are generated into
// client.gen.go — is what makes `make sdk` safe to run at any time.

// ── ids ─────────────────────────────────────────────────────────────────────

// IsValidID reports whether s can be a sandbox id, a template id or a user
// name.
//
// The rule is Kubernetes' own for a namespace name, because a sandbox id becomes
// one. It is enforced here rather than left to the API server so a bad id is
// rejected with a message about the id rather than about a namespace.
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

// NormalizeName is NormalizeID, named for the thing a user has rather than the
// thing a sandbox has. They are the same rule because a user's name becomes a
// label value on every sandbox they create.
func NormalizeName(s string) string { return NormalizeID(s) }

// ── templates ───────────────────────────────────────────────────────────────

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

// Catalog is an ordered set of templates.
//
// It is hand-written because it is a runtime index rather than a wire type: a
// catalog is never serialized, so there is no schema to generate it from.
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

// ── sandboxes ───────────────────────────────────────────────────────────────

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
