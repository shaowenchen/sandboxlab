package catalog

import (
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
		for _, want := range []string{"agent-infra", "agent-sandbox", "opensandbox"} {
			if _, ok := c.Get(want); !ok {
				t.Errorf("the built-in catalog has no %q template", want)
			}
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
