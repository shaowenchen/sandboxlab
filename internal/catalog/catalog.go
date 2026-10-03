// Package catalog loads the sandbox templates a build ships.
//
// A template is a file: the defaults are compiled into the binary (see the seed
// directory), so a sandbox environment works the moment it starts, with no
// volume and nothing to mount. What a deployment offers is not fixed at build
// time, though — templates can also be added and removed on a running control
// plane through the API (see internal/model.Catalog), which is what lets an
// environment be given a new one without a release. Those additions live in
// memory and are gone at restart; the compiled-in templates are what it comes
// back to.
package catalog

import (
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shaowenchen/sandboxlab/internal/model"
)

// Loader builds the catalog a build ships. It has no options today — the
// templates are the compiled-in ones — but it is a named entry point so a
// caller asks for "the catalog" rather than reaching into the embedded
// filesystem, and so a future source has one place to be threaded through.
type Loader struct{}

// Load returns the catalog this build ships.
//
// Every template read here is marked built-in: these are the ones compiled into
// the binary, and the mark is what stops them being removed through the API or
// replaced by a runtime /catalog add of the same id.
func (Loader) Load() (*model.Catalog, error) {
	templates, err := LoadDir(defaultFS())
	if err != nil {
		return nil, fmt.Errorf("loading built-in templates: %w", err)
	}
	for i := range templates {
		templates[i].Builtin = true
	}
	return model.NewCatalog(templates)
}

// LoadDir reads every *.yaml and *.yml file directly in fsys as a template.
//
// It does not recurse, and it does not read a directory that is absent — an
// empty result and no error, so a caller can compose directories without
// checking each one first.
func LoadDir(fsys fs.FS) ([]model.Template, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []model.Template
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		t, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Parse reads one template from YAML.
//
// It is strict, so a misspelled field is a load error rather than a silently
// ignored line. The failure a typo would otherwise cause is a sandbox that
// comes up without the setting that was meant to be there. The same strictness
// applies to a template a caller posts to the API, so a template copied from a
// file and one sent over the wire are held to one standard.
func Parse(data []byte) (model.Template, error) {
	var t model.Template
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return model.Template{}, err
	}
	if err := t.Validate(); err != nil {
		return model.Template{}, err
	}
	return t, nil
}

// Marshal renders a template back to YAML. The console's "copy this template"
// affordance and the tests both use it.
func Marshal(t model.Template) ([]byte, error) { return yaml.Marshal(t) }

// defaultFS is the embedded seed directory, rooted at the files themselves so
// LoadDir reads them as "agent-infra.yaml" rather than "seed/agent-infra.yaml".
func defaultFS() fs.FS {
	sub, err := fs.Sub(seedFS, "seed")
	if err != nil {
		// Unreachable: the embed directive guarantees the directory. A panic
		// here is a build error, not a runtime condition.
		panic("catalog: embedded seed directory is missing: " + err.Error())
	}
	return sub
}
