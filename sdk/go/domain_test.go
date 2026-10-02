package sdk

import (
	"testing"
	"time"
)

// The rules this package owns, as opposed to the shapes it generates.

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

func TestTemplateParsedTTL(t *testing.T) {
	t.Run("empty means no bound", func(t *testing.T) {
		tmpl := Template{ID: "demo", Image: "busybox"}
		if d, err := tmpl.ParsedTTLDefault(); err != nil || d != 0 {
			t.Errorf("ParsedTTLDefault = %v, %v; want 0, nil", d, err)
		}
		if d, err := tmpl.ParsedTTLMax(); err != nil || d != 0 {
			t.Errorf("ParsedTTLMax = %v, %v; want 0, nil", d, err)
		}
	})

	t.Run("parses", func(t *testing.T) {
		tmpl := Template{ID: "demo", Image: "busybox", TTLDefault: "30m", TTLMax: "2h"}
		if d, err := tmpl.ParsedTTLDefault(); err != nil || d != 30*time.Minute {
			t.Errorf("ParsedTTLDefault = %v, %v; want 30m", d, err)
		}
		if d, err := tmpl.ParsedTTLMax(); err != nil || d != 2*time.Hour {
			t.Errorf("ParsedTTLMax = %v, %v; want 2h", d, err)
		}
	})
}

func TestTemplateParsedWorkDir(t *testing.T) {
	if got := (Template{}).ParsedWorkDir(); got != "/workspace" {
		t.Errorf("ParsedWorkDir with no value = %q, want /workspace", got)
	}
	if got := (Template{WorkDir: "/data"}).ParsedWorkDir(); got != "/data" {
		t.Errorf("ParsedWorkDir = %q, want /data", got)
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

func TestSandboxExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("no ttl never expires", func(t *testing.T) {
		// A sandbox with no expiry has no expiresAt at all, which is why the
		// field is a pointer: a zero time would serialize as year 1 and read as
		// long-expired to anything that did not special-case it.
		sb := Sandbox{}
		if sb.Expired(now) {
			t.Error("a sandbox with no expiry reported as expired")
		}
		if got := sb.TTLRemaining(now); got != 0 {
			t.Errorf("TTLRemaining = %v, want 0", got)
		}
	})

	t.Run("before expiry", func(t *testing.T) {
		expires := now.Add(30 * time.Minute)
		sb := Sandbox{ExpiresAt: &expires}
		if sb.Expired(now) {
			t.Error("a sandbox with 30m left reported as expired")
		}
		if got := sb.TTLRemaining(now); got != 30*time.Minute {
			t.Errorf("TTLRemaining = %v, want 30m", got)
		}
	})

	t.Run("after expiry", func(t *testing.T) {
		expires := now.Add(-time.Minute)
		sb := Sandbox{ExpiresAt: &expires}
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
		at := now
		sb := Sandbox{ExpiresAt: &at}
		if !sb.Expired(now) {
			t.Error("a sandbox at its expiry instant did not report as expired")
		}
	})
}

func TestQuota(t *testing.T) {
	t.Run("allows everything by default", func(t *testing.T) {
		q := Quota{}
		for _, name := range []string{"python", "node", "anything"} {
			if !q.Allows(name) {
				t.Errorf("an empty quota refused %q", name)
			}
		}
	})

	t.Run("a list is a whitelist", func(t *testing.T) {
		q := Quota{Templates: []string{"python"}}
		if !q.Allows("python") {
			t.Error("the whitelisted template was refused")
		}
		if q.Allows("node") {
			t.Error("a template not on the list was allowed")
		}
	})

	t.Run("describes itself for a table", func(t *testing.T) {
		// The wire type is nanoseconds, so this is also the check that the
		// conversion back to a duration is right — a quota of 30m reading as
		// "30ns" or "30s" would be a wire bug the console shares.
		tests := []struct {
			name string
			q    Quota
			want string
		}{
			{"no limits", Quota{}, "the deployment's own limits"},
			{"count only", Quota{MaxSandboxes: 3}, "3 sandbox(es)"},
			{"ttl only", Quota{MaxTTL: int64(30 * time.Minute)}, "up to 30m0s"},
			{"templates only", Quota{Templates: []string{"python", "node"}}, "python, node"},
			{
				"all three",
				Quota{MaxSandboxes: 2, MaxTTL: int64(2 * time.Hour), Templates: []string{"python"}},
				"2 sandbox(es); up to 2h0m0s; python",
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.q.Describe(); got != tc.want {
					t.Errorf("Describe() = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("nanoseconds round-trip to the duration they came from", func(t *testing.T) {
		d := 90 * time.Minute
		q := Quota{MaxTTL: int64(d)}
		if got := q.MaxTTLDuration(); got != d {
			t.Errorf("MaxTTLDuration() = %v, want %v", got, d)
		}
	})
}

func TestUserValidate(t *testing.T) {
	base := func() User {
		return User{Name: "alice", Key: "abc123"}
	}

	tests := []struct {
		name    string
		mutate  func(*User)
		wantErr bool
	}{
		{"minimal", func(*User) {}, false},
		{"no name", func(u *User) { u.Name = "" }, true},
		{"unusable name", func(u *User) { u.Name = "Alice Smith" }, true},
		{"no key", func(u *User) { u.Key = "" }, true},
		{"negative sandboxes", func(u *User) { u.Quota.MaxSandboxes = -1 }, true},
		{"negative ttl", func(u *User) { u.Quota.MaxTTL = -1 }, true},
		{"bad template name", func(u *User) { u.Quota.Templates = []string{"Not A Template"} }, true},
		{"with limits", func(u *User) {
			u.Quota = Quota{MaxSandboxes: 5, MaxTTL: int64(time.Hour), Templates: []string{"python"}}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := base()
			tc.mutate(&u)
			err := u.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
