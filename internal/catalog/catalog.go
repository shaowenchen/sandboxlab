// Package catalog loads the sandbox templates an environment offers.
//
// A template is a file. The defaults are compiled into the binary (see the seed
// directory) and an installation can add to or replace them with its own, which
// is what makes this a debugging environment rather than a fixed product: the
// case that needs an unusual image is the case that is worth being able to
// express without a release.
package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shaowenchen/sandboxlab/internal/model"
)

// Loader builds a catalog from the compiled-in defaults plus, optionally, a
// directory of files on disk.
type Loader struct {
	// ExtraDir is a directory of additional template YAML files. A file whose
	// template id matches a default replaces it, which is how an installation
	// overrides one without restating the rest.
	ExtraDir string
	// Disabled is a set of template ids to leave out. An id in here that is not
	// in the catalog is not an error — a default may simply not ship in some
	// future version — but a typo is exactly the kind of thing this would hide,
	// so the loader reports unknown ids.
	Disabled map[string]bool
}

// Load returns the catalog the configuration describes.
func (l Loader) Load() (*model.Catalog, error) {
	templates, err := LoadDir(defaultFS())
	if err != nil {
		return nil, fmt.Errorf("loading built-in templates: %w", err)
	}

	if l.ExtraDir != "" {
		extra, err := LoadDir(os.DirFS(l.ExtraDir))
		if err != nil {
			// A missing extra directory is not an error: it is the normal state
			// of an installation that adds nothing, and a volume mount that has
			// not been populated yet. A directory that is there and unreadable
			// is, and LoadDir says which.
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("loading templates from %s: %w", l.ExtraDir, err)
			}
		}
		templates = merge(templates, extra)
	}

	if len(l.Disabled) > 0 {
		templates, err = disable(templates, l.Disabled)
		if err != nil {
			return nil, err
		}
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
func Parse(data []byte) (model.Template, error) {
	var t model.Template
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// Strict, so a misspelled field is a load error rather than a silently
	// ignored line. The failure a typo would otherwise cause is a sandbox that
	// comes up without the setting that was meant to be there.
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

// merge overlays extra on base by id: an id in both takes extra's value, an id
// only in extra is added. Order does not matter — the catalog sorts.
func merge(base, extra []model.Template) []model.Template {
	byID := make(map[string]model.Template, len(base)+len(extra))
	for _, t := range base {
		byID[t.ID] = t
	}
	for _, t := range extra {
		byID[t.ID] = t
	}
	out := make([]model.Template, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// disable removes templates by id, reporting ids that matched nothing.
func disable(templates []model.Template, disabled map[string]bool) ([]model.Template, error) {
	present := make(map[string]bool, len(templates))
	for _, t := range templates {
		present[t.ID] = true
	}
	var unknown []string
	for id := range disabled {
		if !present[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("disabled template ids match nothing in the catalog: %s", strings.Join(unknown, ", "))
	}
	out := make([]model.Template, 0, len(templates))
	for _, t := range templates {
		if disabled[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// defaultFS is the embedded seed directory, rooted at the files themselves so
// LoadDir reads them as "all-in-one.yaml" rather than "seed/all-in-one.yaml".
func defaultFS() fs.FS {
	sub, err := fs.Sub(seedFS, "seed")
	if err != nil {
		// Unreachable: the embed directive above guarantees the directory. A
		// panic here is a build error, not a runtime condition.
		panic("catalog: embedded seed directory is missing: " + err.Error())
	}
	return sub
}

// ExtraDirFromPath resolves a directory path relative to a working directory,
// so a configuration can say "catalog" and mean the one beside the binary's
// working directory.
func ExtraDirFromPath(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, nil
}
