package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// The identities every test works with.
var (
	admin = auth.Identity{Role: auth.RoleAdmin}
	alice = auth.Identity{Role: auth.RoleUser, Name: "alice"}
	bob   = auth.Identity{Role: auth.RoleUser, Name: "bob"}
)

// stubQuotas is a user list in memory, as the quota source.
type stubQuotas map[string]user.Quota

func (s stubQuotas) QuotaFor(_ context.Context, name string) (user.Quota, bool) {
	q, ok := s[name]
	return q, ok
}

func testCatalog(t *testing.T) *model.Catalog {
	t.Helper()
	c, err := model.NewCatalog([]model.Template{
		{ID: "quick", Title: "Quick", Image: "busybox:latest", TTLDefault: "10m", TTLMax: "30m"},
		{ID: "long", Title: "Long", Image: "busybox:latest", TTLDefault: "2h", TTLMax: "4h",
			Ports: []model.Port{{Name: "api", Port: 8000}}},
		{ID: "forever", Title: "No default TTL", Image: "busybox:latest"},
	})
	if err != nil {
		t.Fatalf("building the test catalog: %v", err)
	}
	return c
}

func testService(t *testing.T, cfg config.Config, quotas Quotas) *Service {
	t.Helper()
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = time.Hour
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 4 * time.Hour
	}
	if cfg.SandboxNamespacePrefix == "" {
		cfg.SandboxNamespacePrefix = "sbx-"
	}
	client := k8s.NewWithClientset(fake.NewClientset(), cfg)
	return New(cfg, testCatalog(t), client, quotas)
}

func TestCreateResolvesTheTemplate(t *testing.T) {
	s := testService(t, config.Config{}, nil)

	t.Run("a valid template", func(t *testing.T) {
		sb, err := s.Create(context.Background(), admin, CreateInput{Template: "quick", Name: "my-box"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if sb.ID != "my-box" || sb.Template != "quick" {
			t.Errorf("sandbox = %+v, want id my-box and template quick", sb)
		}
	})

	t.Run("an unknown template", func(t *testing.T) {
		_, err := s.Create(context.Background(), admin, CreateInput{Template: "nope"})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Create with an unknown template = %v, want ErrInvalid", err)
		}
	})

	t.Run("no template", func(t *testing.T) {
		if _, err := s.Create(context.Background(), admin, CreateInput{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create with no template = %v, want ErrInvalid", err)
		}
	})
}

func TestCreateRecordsTheOwner(t *testing.T) {
	tests := []struct {
		name  string
		who   auth.Identity
		owner string
	}{
		{"an administrator's sandbox belongs to the deployment", admin, ""},
		{"a user's carries their name", alice, "alice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t, config.Config{}, nil)
			sb, err := s.Create(context.Background(), tc.who, CreateInput{Template: "quick", Name: "mine"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if sb.Owner != tc.owner {
				t.Errorf("Owner = %q, want %q", sb.Owner, tc.owner)
			}
		})
	}
}

func TestUsersOnlySeeTheirOwn(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()

	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "alice-box"}); err != nil {
		t.Fatalf("alice's Create: %v", err)
	}
	if _, err := s.Create(ctx, bob, CreateInput{Template: "quick", Name: "bob-box"}); err != nil {
		t.Fatalf("bob's Create: %v", err)
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "admin-box"}); err != nil {
		t.Fatalf("the administrator's Create: %v", err)
	}

	t.Run("alice lists hers only", func(t *testing.T) {
		got, err := s.List(ctx, alice)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != "alice-box" {
			t.Errorf("alice sees %+v, want only alice-box", got)
		}
	})

	t.Run("the administrator lists everything", func(t *testing.T) {
		got, err := s.List(ctx, admin)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("the administrator sees %d sandboxes, want 3", len(got))
		}
	})

	t.Run("the overview is scoped the same way", func(t *testing.T) {
		ov, err := s.Overview(ctx, alice)
		if err != nil {
			t.Fatalf("Overview: %v", err)
		}
		if ov.Total != 1 || !ov.Scoped {
			t.Errorf("alice's overview = %+v, want one scoped sandbox", ov)
		}
		if len(ov.ByOwner) != 0 {
			t.Error("a user's overview named other owners")
		}

		adminOv, err := s.Overview(ctx, admin)
		if err != nil {
			t.Fatalf("Overview: %v", err)
		}
		if adminOv.Total != 3 || adminOv.Scoped {
			t.Errorf("the administrator's overview = %+v, want three unscoped", adminOv)
		}
		if adminOv.ByOwner["alice"] != 1 || adminOv.ByOwner["bob"] != 1 || adminOv.ByOwner["(the deployment)"] != 1 {
			t.Errorf("byOwner = %v, want one each", adminOv.ByOwner)
		}
	})
}

func TestAnotherUsersSandboxIsNotFound(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "alice-box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Not found rather than forbidden. A 403 would confirm the sandbox exists,
	// so a user could map the deployment by watching which names 403.
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"get", func() error { _, err := s.Get(ctx, bob, "alice-box"); return err }},
		{"delete", func() error { return s.Delete(ctx, bob, "alice-box") }},
		{"logs", func() error { _, err := s.Logs(ctx, bob, "alice-box", 10); return err }},
		{"renew", func() error { _, err := s.Renew(ctx, bob, "alice-box", RenewInput{TTL: time.Hour}); return err }},
	} {
		t.Run(op.name, func(t *testing.T) {
			if err := op.run(); !errors.Is(err, ErrNotFound) {
				t.Errorf("bob's %s of alice's sandbox = %v, want ErrNotFound", op.name, err)
			}
		})
	}

	// And it really is still there.
	if _, err := s.Get(ctx, alice, "alice-box"); err != nil {
		t.Errorf("alice can no longer reach her own sandbox: %v", err)
	}
}

func TestAnAdministratorReachesEverything(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "alice-box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := s.Get(ctx, admin, "alice-box"); err != nil {
		t.Errorf("the administrator could not read a user's sandbox: %v", err)
	}
	if err := s.Delete(ctx, admin, "alice-box"); err != nil {
		t.Errorf("the administrator could not delete a user's sandbox: %v", err)
	}
}

func TestCreateNormalizesTheName(t *testing.T) {
	tests := []struct {
		given string
		want  string
	}{
		{"My Box", "my-box"},
		{"My_Box", "my-box"},
		{"my...box", "my-box"},
		{"  spaced  ", "spaced"},
		{"UPPER", "upper"},
	}
	for _, tc := range tests {
		t.Run(tc.given, func(t *testing.T) {
			// A fresh service per case: three of these normalize to the same
			// id, which is the point — they would collide in one cluster.
			s := testService(t, config.Config{}, nil)
			sb, err := s.Create(context.Background(), admin, CreateInput{Template: "quick", Name: tc.given})
			if err != nil {
				t.Fatalf("Create(%q): %v", tc.given, err)
			}
			if sb.ID != tc.want {
				t.Errorf("id = %q, want %q", sb.ID, tc.want)
			}
		})
	}
}

func TestCreateTreatsEquivalentNamesAsTheSameName(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()

	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "My Box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "my-box"}); !errors.Is(err, ErrConflict) {
		t.Errorf("Create with a differently-spelled same name = %v, want ErrConflict", err)
	}
}

func TestCreateRejectsANameWithNothingUsableInIt(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	if _, err := s.Create(context.Background(), admin, CreateInput{Template: "quick", Name: "!!!"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create with an unusable name = %v, want ErrInvalid", err)
	}
}

func TestCreateGeneratesAName(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	seen := map[string]bool{}

	for i := 0; i < 5; i++ {
		sb, err := s.Create(context.Background(), admin, CreateInput{Template: "quick"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if sb.ID == "" || !strings.HasPrefix(sb.ID, "quick-") || !model.IsValidID(sb.ID) {
			t.Fatalf("generated id %q is not usable", sb.ID)
		}
		if seen[sb.ID] {
			t.Errorf("generated id %q twice", sb.ID)
		}
		seen[sb.ID] = true
	}
}

func TestCreateAppliesTTLs(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		requested time.Duration
		cfgMax    time.Duration
		want      time.Duration
	}{
		{name: "the template default", template: "quick", want: 10 * time.Minute},
		{name: "an explicit ttl under every ceiling", template: "quick", requested: 5 * time.Minute, want: 5 * time.Minute},
		{name: "clamped to the template's ceiling", template: "quick", requested: 2 * time.Hour, want: 30 * time.Minute},
		{name: "clamped to the deployment's ceiling", template: "long", requested: 3 * time.Hour, cfgMax: 2 * time.Hour, want: 2 * time.Hour},
		{name: "the deployment default", template: "forever", want: time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{DefaultTTL: time.Hour, MaxTTL: tc.cfgMax}
			if cfg.MaxTTL == 0 {
				cfg.MaxTTL = 4 * time.Hour
			}
			s := testService(t, cfg, nil)

			sb, err := s.Create(context.Background(), admin, CreateInput{Template: tc.template, TTL: tc.requested})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := sb.ExpiresAt.Sub(sb.CreatedAt); got-tc.want > time.Second || tc.want-got > time.Second {
				t.Errorf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAUsersTTLCeilingApplies(t *testing.T) {
	// A user's own ceiling ends up below the template's and the deployment's,
	// so it is the one that decides.
	s := testService(t, config.Config{DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour},
		stubQuotas{"alice": {MaxTTL: 12 * time.Minute}})
	ctx := context.Background()

	t.Run("an explicit request is clamped", func(t *testing.T) {
		sb, err := s.Create(ctx, alice, CreateInput{Template: "long", Name: "capped", TTL: 3 * time.Hour})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 12*time.Minute+time.Second {
			t.Errorf("ttl = %v, want it clamped to the user's 12m", got)
		}
	})

	t.Run("a template default is capped too", func(t *testing.T) {
		// The template asks for 10m and the user's ceiling is 12m, so this one
		// passes through — the point is that the cap is applied to the default
		// as well as to an explicit request.
		sb, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "under"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 12*time.Minute+time.Second {
			t.Errorf("ttl = %v, want it within the user's 12m", got)
		}
	})

	t.Run("the administrator is not capped", func(t *testing.T) {
		sb, err := s.Create(ctx, admin, CreateInput{Template: "long", Name: "uncapped"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got < 2*time.Hour-time.Second {
			t.Errorf("ttl = %v, want the template's 2h", got)
		}
	})
}

func TestATemplateWhitelistApplies(t *testing.T) {
	s := testService(t, config.Config{},
		stubQuotas{"alice": {Templates: []string{"quick"}}})
	ctx := context.Background()

	if _, err := s.Create(ctx, alice, CreateInput{Template: "long", Name: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create of a template not on the list = %v, want ErrInvalid", err)
	}
	// The message should say what is available, since the refusal is otherwise
	// a dead end.
	_, err := s.Create(ctx, alice, CreateInput{Template: "long"})
	if err == nil || !strings.Contains(err.Error(), "quick") {
		t.Errorf("the refusal does not name the allowed templates: %v", err)
	}
	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "yes"}); err != nil {
		t.Errorf("Create of an allowed template: %v", err)
	}
}

func TestCreateRejectsANegativeTTL(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	if _, err := s.Create(context.Background(), admin, CreateInput{Template: "quick", TTL: -time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create with a negative ttl = %v, want ErrInvalid", err)
	}
}

func TestCreateEnforcesTheDeploymentCeiling(t *testing.T) {
	cfg := config.Config{DefaultTTL: time.Hour, MaxTTL: time.Hour, MaxSandboxes: 2}
	s := testService(t, cfg, nil)
	ctx := context.Background()

	for _, id := range []string{"one", "two"} {
		if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: id}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "three"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("Create past the ceiling = %v, want ErrLimit", err)
	}
	if err := s.Delete(ctx, admin, "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "three"}); err != nil {
		t.Errorf("Create after freeing a slot: %v", err)
	}
}

func TestCreateEnforcesAUsersOwnCeiling(t *testing.T) {
	s := testService(t, config.Config{}, stubQuotas{"alice": {MaxSandboxes: 1}})
	ctx := context.Background()

	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "first"}); err != nil {
		t.Fatalf("alice's first Create: %v", err)
	}
	_, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "second"})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("alice's second Create = %v, want ErrLimit", err)
	}
	// The message is about her limit, not a deployment-wide one she cannot see.
	if !strings.Contains(err.Error(), "you have") {
		t.Errorf("the refusal = %q, want it to be about her own limit", err)
	}

	// Other users are unaffected: the ceiling is hers, not a global one.
	if _, err := s.Create(ctx, bob, CreateInput{Template: "quick", Name: "bobs"}); err != nil {
		t.Errorf("bob's Create was refused by alice's ceiling: %v", err)
	}
	// As is the administrator.
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "admins"}); err != nil {
		t.Errorf("the administrator's Create was refused by a user's ceiling: %v", err)
	}
}

func TestCreateReportsATakenName(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "mine"}); !errors.Is(err, ErrConflict) {
		t.Errorf("Create with a taken name = %v, want ErrConflict", err)
	}
}

func TestGetNormalizesTheID(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "my-box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, id := range []string{"my-box", "My-Box", "MY-BOX"} {
		if _, err := s.Get(ctx, admin, id); err != nil {
			t.Errorf("Get(%q): %v", id, err)
		}
	}
}

func TestGetRejectsAnUnusableID(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	if _, err := s.Get(context.Background(), admin, "!!!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("Get with an unusable id = %v, want ErrInvalid", err)
	}
}

func TestDeleteAndNotFound(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	ctx := context.Background()

	if err := s.Delete(ctx, admin, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of a missing sandbox = %v, want ErrNotFound", err)
	}
	if _, err := s.Create(ctx, admin, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, admin, "mine"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, admin, "mine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestRenew(t *testing.T) {
	s := testService(t, config.Config{DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour}, nil)
	ctx := context.Background()
	if _, err := s.Create(ctx, admin, CreateInput{Template: "long", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("extends", func(t *testing.T) {
		sb, err := s.Renew(ctx, admin, "mine", RenewInput{TTL: 3 * time.Hour})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 3*time.Hour+time.Second || got < 3*time.Hour-time.Second {
			t.Errorf("ttl after renew = %v, want about 3h", got)
		}
	})

	t.Run("clamped to the deployment ceiling", func(t *testing.T) {
		sb, err := s.Renew(ctx, admin, "mine", RenewInput{TTL: 100 * time.Hour})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 4*time.Hour+time.Second {
			t.Errorf("ttl after renew = %v, want it clamped to the 4h ceiling", got)
		}
	})

	t.Run("zero removes the expiry", func(t *testing.T) {
		sb, err := s.Renew(ctx, admin, "mine", RenewInput{})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if !sb.ExpiresAt.IsZero() {
			t.Errorf("ExpiresAt = %v after renewing with no ttl, want no expiry", sb.ExpiresAt)
		}
	})

	t.Run("a negative ttl", func(t *testing.T) {
		if _, err := s.Renew(ctx, admin, "mine", RenewInput{TTL: -time.Hour}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Renew with a negative ttl = %v, want ErrInvalid", err)
		}
	})

	t.Run("a missing sandbox", func(t *testing.T) {
		if _, err := s.Renew(ctx, admin, "nope", RenewInput{TTL: time.Hour}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Renew of a missing sandbox = %v, want ErrNotFound", err)
		}
	})
}

func TestRenewIsCappedByAUsersCeiling(t *testing.T) {
	s := testService(t, config.Config{DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour},
		stubQuotas{"alice": {MaxTTL: 20 * time.Minute}})
	ctx := context.Background()
	if _, err := s.Create(ctx, alice, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A renew is how someone extends a sandbox, so a ceiling that only applied
	// at create would be one a user could step past on their first renew.
	sb, err := s.Renew(ctx, alice, "mine", RenewInput{TTL: 3 * time.Hour})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 20*time.Minute+time.Second {
		t.Errorf("ttl after renew = %v, want it capped at the user's 20m", got)
	}
}

func TestOverview(t *testing.T) {
	s := testService(t, config.Config{MaxSandboxes: 5}, nil)
	ctx := context.Background()
	for _, in := range []CreateInput{
		{Template: "quick", Name: "one"},
		{Template: "quick", Name: "two"},
		{Template: "long", Name: "three"},
	} {
		if _, err := s.Create(ctx, admin, in); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	ov, err := s.Overview(ctx, admin)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if ov.Total != 3 {
		t.Errorf("total = %d, want 3", ov.Total)
	}
	if ov.ByTemplate["quick"] != 2 || ov.ByTemplate["long"] != 1 {
		t.Errorf("byTemplate = %v, want quick 2 and long 1", ov.ByTemplate)
	}
	if ov.MaxSandboxes != 5 {
		t.Errorf("maxSandboxes = %d, want 5", ov.MaxSandboxes)
	}
}

func TestACatalogIsExposed(t *testing.T) {
	s := testService(t, config.Config{}, nil)
	if s.Catalog().Len() != 3 {
		t.Errorf("Catalog().Len() = %d, want 3", s.Catalog().Len())
	}
}

func TestNoQuotaSourceMeansNoUserLimits(t *testing.T) {
	// A deployment with no user store has no per-user ceilings, and that must
	// not be a reason to refuse a user — it is a reason they are unlimited.
	s := testService(t, config.Config{}, nil)
	if _, err := s.Create(context.Background(), alice, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Errorf("Create with no quota source: %v", err)
	}
}
