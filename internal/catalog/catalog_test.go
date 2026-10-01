package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParse(t *testing.T) {
	t.Run("a full template", func(t *testing.T) {
		data := []byte(`
id: demo
title: Demo
description: A demo.
image: busybox:latest
ports:
  - name: api
    port: 8000
env:
  GREETING: hello
resources:
  cpu: "1"
  memory: 512Mi
ttlDefault: 30m
ttlMax: 2h
`)
		tmpl, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if tmpl.ID != "demo" || tmpl.Image != "busybox:latest" {
			t.Errorf("parsed id/image = %q/%q, want demo/busybox:latest", tmpl.ID, tmpl.Image)
		}
		if len(tmpl.Ports) != 1 || tmpl.Ports[0].Name != "api" || tmpl.Ports[0].Port != 8000 {
			t.Errorf("parsed ports = %+v, want one named api on 8000", tmpl.Ports)
		}
		if tmpl.Env["GREETING"] != "hello" {
			t.Errorf("parsed env = %v, want GREETING=hello", tmpl.Env)
		}
		if d, err := tmpl.ParsedTTLDefault(); err != nil || d.String() != "30m0s" {
			t.Errorf("ParsedTTLDefault = %v, %v, want 30m", d, err)
		}
	})

	t.Run("a misspelled field is an error", func(t *testing.T) {
		// The failure a silently-ignored field would cause is a sandbox that
		// comes up without the setting that was meant to be there, which is
		// much harder to find than a load error naming the file.
		_, err := Parse([]byte("id: demo\nimage: busybox\nttlDefualt: 30m\n"))
		if err == nil {
			t.Fatal("Parse accepted a misspelled field")
		}
		if !strings.Contains(err.Error(), "ttlDefualt") {
			t.Errorf("Parse error = %q, want it to name the unknown field", err)
		}
	})

	t.Run("an invalid template is rejected", func(t *testing.T) {
		if _, err := Parse([]byte("id: demo\n")); err == nil {
			t.Fatal("Parse accepted a template with no image")
		}
	})
}

func TestLoadDir(t *testing.T) {
	fsys := fstest.MapFS{
		"one.yaml":       {Data: []byte("id: one\nimage: busybox\n")},
		"two.yml":        {Data: []byte("id: two\nimage: alpine\n")},
		"notes.txt":      {Data: []byte("not a template")},
		"sub/three.yaml": {Data: []byte("id: three\nimage: busybox\n")},
	}
	got, err := LoadDir(fsys)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	// .txt is ignored and subdirectories are not recursed into: a directory of
	// templates should not pick up a README, and should not wander.
	if len(got) != 2 {
		t.Fatalf("LoadDir returned %d templates, want 2: %+v", len(got), got)
	}
}

func TestLoadDirReportsABadFileByName(t *testing.T) {
	fsys := fstest.MapFS{
		"good.yaml": {Data: []byte("id: good\nimage: busybox\n")},
		"bad.yaml":  {Data: []byte("id: bad\n")}, // no image
	}
	_, err := LoadDir(fsys)
	if err == nil {
		t.Fatal("LoadDir accepted a directory with an invalid template")
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("LoadDir error = %q, want it to name bad.yaml", err)
	}
}

func TestLoad(t *testing.T) {
	t.Run("the built-in catalog", func(t *testing.T) {
		c, err := Loader{}.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Len() == 0 {
			t.Fatal("the built-in catalog is empty")
		}
		for _, want := range []string{"all-in-one", "python", "node", "code-server"} {
			if _, ok := c.Get(want); !ok {
				t.Errorf("the built-in catalog has no %q template", want)
			}
		}
	})

	t.Run("an extra directory adds to the catalog", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "extra.yaml", "id: extra\nimage: busybox\n")

		c, err := Loader{ExtraDir: dir}.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := c.Get("extra"); !ok {
			t.Error("a template in the extra directory is missing from the catalog")
		}
		// The built-ins are still there: an extra directory adds, it does not
		// replace the catalog wholesale.
		if _, ok := c.Get("python"); !ok {
			t.Error("the built-in templates disappeared when an extra directory was added")
		}
	})

	t.Run("an extra directory overrides by id", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "python.yaml", "id: python\nimage: my/python:latest\n")

		c, err := Loader{ExtraDir: dir}.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got, ok := c.Get("python")
		if !ok {
			t.Fatal("python is missing after being overridden")
		}
		if got.Image != "my/python:latest" {
			t.Errorf("python image = %q, want the override my/python:latest", got.Image)
		}
	})

	t.Run("a missing extra directory is not an error", func(t *testing.T) {
		// The normal state of an installation that adds nothing, and of a
		// volume that has not been populated yet.
		c, err := Loader{ExtraDir: filepath.Join(t.TempDir(), "does-not-exist")}.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Len() == 0 {
			t.Error("the catalog is empty after loading a missing extra directory")
		}
	})

	t.Run("disabled templates are left out", func(t *testing.T) {
		c, err := Loader{Disabled: map[string]bool{"node": true}}.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := c.Get("node"); ok {
			t.Error("node is in the catalog after being disabled")
		}
		if _, ok := c.Get("python"); !ok {
			t.Error("disabling one template removed another")
		}
	})

	t.Run("a disabled template that does not exist is an error", func(t *testing.T) {
		// A typo here would otherwise silently disable nothing, which is the
		// kind of thing that is discovered much later.
		_, err := Loader{Disabled: map[string]bool{"pythn": true}}.Load()
		if err == nil {
			t.Fatal("Load accepted a disabled id that matches nothing")
		}
		if !strings.Contains(err.Error(), "pythn") {
			t.Errorf("Load error = %q, want it to name pythn", err)
		}
	})
}

func TestMarshalRoundTrips(t *testing.T) {
	original, err := Loader{}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, want := range original.List() {
		data, err := Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", want.ID, err)
		}
		got, err := Parse(data)
		if err != nil {
			t.Fatalf("Parse(Marshal(%s)): %v", want.ID, err)
		}
		if got.Image != want.Image || len(got.Ports) != len(want.Ports) {
			t.Errorf("round trip of %s changed it: %+v vs %+v", want.ID, got, want)
		}
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
