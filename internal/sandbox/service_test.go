package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

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

func testService(t *testing.T, cfg config.Config) *Service {
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
	return New(cfg, testCatalog(t), client)
}

func TestCreateResolvesTheTemplate(t *testing.T) {
	s := testService(t, config.Config{})

	t.Run("a valid template", func(t *testing.T) {
		sb, err := s.Create(context.Background(), CreateInput{Template: "quick", Name: "my-box"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if sb.ID != "my-box" || sb.Template != "quick" {
			t.Errorf("sandbox = %+v, want id my-box and template quick", sb)
		}
	})

	t.Run("an unknown template", func(t *testing.T) {
		_, err := s.Create(context.Background(), CreateInput{Template: "nope"})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Create with an unknown template = %v, want ErrInvalid", err)
		}
	})

	t.Run("no template", func(t *testing.T) {
		if _, err := s.Create(context.Background(), CreateInput{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create with no template = %v, want ErrInvalid", err)
		}
	})
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
			s := testService(t, config.Config{})
			sb, err := s.Create(context.Background(), CreateInput{Template: "quick", Name: tc.given})
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
	s := testService(t, config.Config{})
	ctx := context.Background()

	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "My Box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "my-box"}); !errors.Is(err, ErrConflict) {
		t.Errorf("Create with a differently-spelled same name = %v, want ErrConflict", err)
	}
}

func TestCreateRejectsANameWithNothingUsableInIt(t *testing.T) {
	s := testService(t, config.Config{})
	if _, err := s.Create(context.Background(), CreateInput{Template: "quick", Name: "!!!"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create with an unusable name = %v, want ErrInvalid", err)
	}
}

func TestCreateGeneratesAName(t *testing.T) {
	s := testService(t, config.Config{})
	seen := map[string]bool{}

	for i := 0; i < 5; i++ {
		sb, err := s.Create(context.Background(), CreateInput{Template: "quick"})
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
			s := testService(t, cfg)

			sb, err := s.Create(context.Background(), CreateInput{Template: tc.template, TTL: tc.requested})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := sb.ExpiresAt.Sub(sb.CreatedAt); got-tc.want > time.Second || tc.want-got > time.Second {
				t.Errorf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}
func TestCreateRejectsANegativeTTL(t *testing.T) {
	s := testService(t, config.Config{})
	if _, err := s.Create(context.Background(), CreateInput{Template: "quick", TTL: -time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Create with a negative ttl = %v, want ErrInvalid", err)
	}
}

func TestCreateEnforcesTheDeploymentCeiling(t *testing.T) {
	cfg := config.Config{DefaultTTL: time.Hour, MaxTTL: time.Hour, MaxSandboxes: 2}
	s := testService(t, cfg)
	ctx := context.Background()

	for _, id := range []string{"one", "two"} {
		if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: id}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "three"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("Create past the ceiling = %v, want ErrLimit", err)
	}
	if err := s.Delete(ctx, "one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "three"}); err != nil {
		t.Errorf("Create after freeing a slot: %v", err)
	}
}
func TestCreateReportsATakenName(t *testing.T) {
	s := testService(t, config.Config{})
	ctx := context.Background()
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "mine"}); !errors.Is(err, ErrConflict) {
		t.Errorf("Create with a taken name = %v, want ErrConflict", err)
	}
}

func TestGetNormalizesTheID(t *testing.T) {
	s := testService(t, config.Config{})
	ctx := context.Background()
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "my-box"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, id := range []string{"my-box", "My-Box", "MY-BOX"} {
		if _, err := s.Get(ctx, id); err != nil {
			t.Errorf("Get(%q): %v", id, err)
		}
	}
}

func TestGetRejectsAnUnusableID(t *testing.T) {
	s := testService(t, config.Config{})
	if _, err := s.Get(context.Background(), "!!!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("Get with an unusable id = %v, want ErrInvalid", err)
	}
}

func TestDeleteAndNotFound(t *testing.T) {
	s := testService(t, config.Config{})
	ctx := context.Background()

	if err := s.Delete(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of a missing sandbox = %v, want ErrNotFound", err)
	}
	if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, "mine"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "mine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestRenew(t *testing.T) {
	s := testService(t, config.Config{DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour})
	ctx := context.Background()
	if _, err := s.Create(ctx, CreateInput{Template: "long", Name: "mine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("extends", func(t *testing.T) {
		sb, err := s.Renew(ctx, "mine", RenewInput{TTL: 3 * time.Hour})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 3*time.Hour+time.Second || got < 3*time.Hour-time.Second {
			t.Errorf("ttl after renew = %v, want about 3h", got)
		}
	})

	t.Run("clamped to the deployment ceiling", func(t *testing.T) {
		sb, err := s.Renew(ctx, "mine", RenewInput{TTL: 100 * time.Hour})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if got := sb.ExpiresAt.Sub(sb.CreatedAt); got > 4*time.Hour+time.Second {
			t.Errorf("ttl after renew = %v, want it clamped to the 4h ceiling", got)
		}
	})

	t.Run("zero removes the expiry", func(t *testing.T) {
		sb, err := s.Renew(ctx, "mine", RenewInput{})
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
		if sb.ExpiresAt != nil {
			t.Errorf("ExpiresAt = %v after renewing with no ttl, want no expiry", sb.ExpiresAt)
		}
	})

	t.Run("a negative ttl", func(t *testing.T) {
		if _, err := s.Renew(ctx, "mine", RenewInput{TTL: -time.Hour}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Renew with a negative ttl = %v, want ErrInvalid", err)
		}
	})

	t.Run("a missing sandbox", func(t *testing.T) {
		if _, err := s.Renew(ctx, "nope", RenewInput{TTL: time.Hour}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Renew of a missing sandbox = %v, want ErrNotFound", err)
		}
	})
}
func TestOverview(t *testing.T) {
	s := testService(t, config.Config{MaxSandboxes: 5})
	ctx := context.Background()
	for _, in := range []CreateInput{
		{Template: "quick", Name: "one"},
		{Template: "quick", Name: "two"},
		{Template: "long", Name: "three"},
	} {
		if _, err := s.Create(ctx, in); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	ov, err := s.Overview(ctx)
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
	s := testService(t, config.Config{})
	if s.Catalog().Len() != 3 {
		t.Errorf("Catalog().Len() = %d, want 3", s.Catalog().Len())
	}
}
func TestLogsOfAStartingSandboxIsAConflict(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		available    int32
		wantConflict bool
	}{
		// AvailableReplicas is what decides Pending from Running, and the
		// distinction is the point: only a sandbox still coming up is a
		// conflict, because a running one whose logs fail to read is a genuine
		// error worth surfacing rather than a "come back later".
		{"a container still being created", 0, true},
		{"a running sandbox", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			cs.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
				dep := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
				dep.Status = appsv1.DeploymentStatus{Replicas: 1, AvailableReplicas: tc.available}
				return false, nil, nil
			})
			cfg := config.Config{DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour, SandboxNamespacePrefix: "sbx-"}
			s := New(cfg, testCatalog(t), k8s.NewWithClientset(cs, cfg))

			if _, err := s.Create(ctx, CreateInput{Template: "quick", Name: "my-box"}); err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Nothing plays the controller here, so there is no pod and the
			// cluster will not serve logs — which is the failing read the
			// mapping turns on.
			_, err := s.Logs(ctx, "my-box", 10)
			switch {
			case tc.wantConflict && !errors.Is(err, ErrConflict):
				t.Errorf("Logs of a starting sandbox = %v, want ErrConflict", err)
			case !tc.wantConflict && errors.Is(err, ErrConflict):
				t.Errorf("Logs = %v; a sandbox that is not starting must not read as a conflict", err)
			}
		})
	}
}
