package model

import (
	"testing"
	"time"
)

func TestIsValidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"simple", "demo", true},
		{"with dashes", "my-sandbox-1", true},
		{"digits", "sbx123", true},
		{"single character", "a", true},
		{"empty", "", false},
		{"uppercase", "Demo", false},
		{"leading dash", "-demo", false},
		{"trailing dash", "demo-", false},
		{"underscore", "my_sandbox", false},
		{"dot", "my.sandbox", false},
		{"slash", "my/sandbox", false},
		{"space", "my sandbox", false},
		{"unicode", "sandböx", false},
		{"at the limit", "a234567890123456789012345678901234567890", true},     // 40
		{"over the limit", "a2345678901234567890123456789012345678901", false}, // 41
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidID(tc.id); got != tc.want {
				t.Errorf("IsValidID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Demo", "demo"},
		{"my sandbox", "my-sandbox"},
		{"my_sandbox", "my-sandbox"},
		{"my...sandbox", "my-sandbox"},
		{"My Shop", "my-shop"},
		{"--leading and trailing--", "leading-and-trailing"},
		{"already-fine", "already-fine"},
		{"", ""},
		{"!!!", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeID(tc.in); got != tc.want {
				t.Errorf("NormalizeID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeIDTruncates(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz0123456789abcdefghij"
	got := NormalizeID(long)
	if len(got) > 40 {
		t.Fatalf("NormalizeID returned %d characters, want at most 40", len(got))
	}
	if !IsValidID(got) {
		t.Errorf("NormalizeID(%q) = %q, which is not a valid id", long, got)
	}
}

func TestTemplateValidate(t *testing.T) {
	base := func() Template {
		return Template{ID: "demo", Title: "Demo", Image: "busybox:latest"}
	}

	tests := []struct {
		name    string
		mutate  func(*Template)
		wantErr bool
	}{
		{"minimal", func(*Template) {}, false},
		{"with ports", func(tt *Template) { tt.Ports = []Port{{Name: "api", Port: 8000}} }, false},
		{"no id", func(tt *Template) { tt.ID = "" }, true},
		{"bad id", func(tt *Template) { tt.ID = "Not An Id" }, true},
		{"no image", func(tt *Template) { tt.Image = "" }, true},
		{"port with no name", func(tt *Template) { tt.Ports = []Port{{Port: 80}} }, true},
		{"port out of range", func(tt *Template) { tt.Ports = []Port{{Name: "api", Port: 70000}} }, true},
		{"port zero", func(tt *Template) { tt.Ports = []Port{{Name: "api", Port: 0}} }, true},
		{"duplicate port name", func(tt *Template) {
			tt.Ports = []Port{{Name: "api", Port: 1}, {Name: "api", Port: 2}}
		}, true},
		{"bad ttl default", func(tt *Template) { tt.TTLDefault = "soon" }, true},
		{"bad ttl max", func(tt *Template) { tt.TTLMax = "forever" }, true},
		{"good ttls", func(tt *Template) { tt.TTLDefault = "30m"; tt.TTLMax = "2h" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := base()
			tc.mutate(&tmpl)
			err := tmpl.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCatalogRejectsDuplicates(t *testing.T) {
	_, err := NewCatalog([]Template{
		{ID: "demo", Image: "busybox"},
		{ID: "demo", Image: "alpine"},
	})
	if err == nil {
		t.Fatal("NewCatalog accepted two templates with the same id")
	}
}

func TestCatalogListIsSorted(t *testing.T) {
	c, err := NewCatalog([]Template{
		{ID: "zulu", Image: "busybox"},
		{ID: "alpha", Image: "busybox"},
		{ID: "mike", Image: "busybox"},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	got := c.List()
	want := []string{"alpha", "mike", "zulu"}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("List()[%d].ID = %q, want %q", i, got[i].ID, want[i])
		}
	}
}

func TestCatalogGet(t *testing.T) {
	c, err := NewCatalog([]Template{{ID: "demo", Image: "busybox"}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if _, ok := c.Get("demo"); !ok {
		t.Error("Get(\"demo\") did not find the template")
	}
	if _, ok := c.Get("nope"); ok {
		t.Error("Get(\"nope\") found a template that does not exist")
	}
}

func TestCatalogAddUpserts(t *testing.T) {
	c, err := NewCatalog([]Template{{ID: "demo", Image: "busybox"}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	replaced, err := c.Add(Template{ID: "extra", Image: "alpine"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if replaced {
		t.Error("Add reported replacing a template that was not there")
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}

	// Adding the same id again replaces it rather than being an error: with no
	// persistence, re-adding is how a template is edited.
	replaced, err = c.Add(Template{ID: "demo", Image: "busybox:1.36"})
	if err != nil {
		t.Fatalf("Add (replace): %v", err)
	}
	if !replaced {
		t.Error("Add did not report replacing an existing template")
	}
	if c.Len() != 2 {
		t.Errorf("Len after replace = %d, want 2", c.Len())
	}
	if got, _ := c.Get("demo"); got.Image != "busybox:1.36" {
		t.Errorf("demo image = %q, want the replacement busybox:1.36", got.Image)
	}

	// An invalid template is rejected and leaves the catalog alone.
	if _, err := c.Add(Template{ID: "bad"}); err == nil {
		t.Error("Add accepted a template with no image")
	}
	if c.Len() != 2 {
		t.Errorf("Len after a rejected add = %d, want 2", c.Len())
	}
}

func TestCatalogRemove(t *testing.T) {
	c, err := NewCatalog([]Template{{ID: "demo", Image: "busybox"}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if !c.Remove("demo") {
		t.Error("Remove reported a template that was there as missing")
	}
	if c.Len() != 0 {
		t.Errorf("Len after remove = %d, want 0", c.Len())
	}
	if c.Remove("demo") {
		t.Error("Remove reported removing a template that was already gone")
	}
}

func TestCatalogKeepsBuiltins(t *testing.T) {
	c, err := NewCatalog([]Template{{ID: "shipped", Image: "busybox", Builtin: true}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	if c.Remove("shipped") {
		t.Error("Remove reported removing a built-in template")
	}
	if _, ok := c.Get("shipped"); !ok {
		t.Error("a built-in template was removed")
	}

	// A built-in can be edited — a new image, a different port — and the edit
	// keeps the mark, so an edited built-in is still not removable.
	if _, err := c.Add(Template{ID: "shipped", Image: "busybox:1.36"}); err != nil {
		t.Fatalf("Add over a built-in: %v", err)
	}
	got, _ := c.Get("shipped")
	if got.Image != "busybox:1.36" {
		t.Errorf("image = %q, want the edit busybox:1.36", got.Image)
	}
	if !got.Builtin {
		t.Error("editing a built-in cleared its Builtin mark")
	}
	if c.Remove("shipped") {
		t.Error("an edited built-in became removable")
	}
}

// TestCatalogConcurrentAccess is what makes `-race` mean something here: the
// HTTP handlers read and write the catalog from many goroutines at once.
func TestCatalogConcurrentAccess(t *testing.T) {
	c, err := NewCatalog([]Template{{ID: "demo", Image: "busybox"}})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			c.Add(Template{ID: "demo", Image: "busybox"})
			c.Remove("demo")
		}
	}()
	for i := 0; i < 200; i++ {
		c.List()
		c.Get("demo")
		c.Len()
	}
	<-done
}

func TestSandboxExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("no ttl never expires", func(t *testing.T) {
		sb := Sandbox{}
		if sb.Expired(now) {
			t.Error("a sandbox with no expiry reported as expired")
		}
		if got := sb.TTLRemaining(now); got != 0 {
			t.Errorf("TTLRemaining = %v, want 0", got)
		}
	})

	t.Run("before expiry", func(t *testing.T) {
		sb := Sandbox{ExpiresAt: now.Add(30 * time.Minute)}
		if sb.Expired(now) {
			t.Error("a sandbox with 30m left reported as expired")
		}
		if got := sb.TTLRemaining(now); got != 30*time.Minute {
			t.Errorf("TTLRemaining = %v, want 30m", got)
		}
	})

	t.Run("after expiry", func(t *testing.T) {
		sb := Sandbox{ExpiresAt: now.Add(-time.Minute)}
		if !sb.Expired(now) {
			t.Error("a sandbox past its expiry did not report as expired")
		}
		// Remaining is clamped at zero rather than going negative: a caller
		// rendering it should not have to guard against "-3m left".
		if got := sb.TTLRemaining(now); got != 0 {
			t.Errorf("TTLRemaining = %v, want 0", got)
		}
	})

	t.Run("exactly at expiry", func(t *testing.T) {
		sb := Sandbox{ExpiresAt: now}
		if !sb.Expired(now) {
			t.Error("a sandbox at its expiry instant did not report as expired")
		}
	})
}
