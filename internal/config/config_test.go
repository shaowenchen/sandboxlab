package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// envVars are every variable Load reads.
var envVars = []string{
	"SANDBOX_LISTEN", "SANDBOX_NAMESPACE", "SANDBOX_NAMESPACE_PREFIX",
	"SANDBOX_PUBLIC_URL", "SANDBOX_BASE_PATH", "SANDBOX_API_KEY",
	"SANDBOX_LOG_LEVEL",
	"SANDBOX_KUBECONFIG", "SANDBOX_DEFAULT_TTL", "SANDBOX_MAX_TTL",
	"SANDBOX_MAX_SANDBOXES", "SANDBOX_REAP_INTERVAL", "SANDBOX_DATA_PLANE",
}

// load loads a config with the environment given, clearing every variable the
// package reads first so a test never sees the ambient environment.
//
// The clearing is an unset rather than an empty string, and that is the point:
// Load reads a variable with LookupEnv, so an empty value is a value — which is
// how a deliberate "no public URL" is expressed, and not the same thing as a
// variable that was never set.
func load(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	for _, k := range envVars {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetting %s: %v", k, err)
		}
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			t.Fatalf("setting %s: %v", k, err)
		}
	}
	return Load()
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Namespace != "ops-system" {
		t.Errorf("Namespace = %q, want ops-system", cfg.Namespace)
	}
	if cfg.SandboxNamespacePrefix != "sbx-" {
		t.Errorf("SandboxNamespacePrefix = %q, want sbx-", cfg.SandboxNamespacePrefix)
	}
	if cfg.DefaultTTL != time.Hour {
		t.Errorf("DefaultTTL = %v, want 1h", cfg.DefaultTTL)
	}
	if cfg.MaxTTL != 8*time.Hour {
		t.Errorf("MaxTTL = %v, want 8h", cfg.MaxTTL)
	}
	if !cfg.DataPlane {
		t.Error("DataPlane is off by default")
	}
}

func TestGeneratedKey(t *testing.T) {
	t.Run("generated when unset", func(t *testing.T) {
		cfg, err := load(t, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.GeneratedKey() {
			t.Error("GeneratedKey() = false for a key that was not configured")
		}
		if len(cfg.APIKey) != 32 {
			t.Errorf("generated key is %d characters, want 32", len(cfg.APIKey))
		}
	})

	t.Run("configured is not generated", func(t *testing.T) {
		cfg, err := load(t, map[string]string{"SANDBOX_API_KEY": "abc123"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.GeneratedKey() {
			t.Error("GeneratedKey() = true for a configured key")
		}
		if cfg.APIKey != "abc123" {
			t.Errorf("APIKey = %q, want abc123", cfg.APIKey)
		}
	})

	t.Run("two generated keys differ", func(t *testing.T) {
		a, err := load(t, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		b, err := load(t, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if a.APIKey == b.APIKey {
			t.Error("two generated keys are identical")
		}
	})
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "base path must start with a slash",
			env:  map[string]string{"SANDBOX_BASE_PATH": "sandbox"},
			// A base path without a leading slash would be joined onto the
			// public URL to make a nonsense address, and the failure would show
			// up as a 404 somewhere far away.
			wantErr: "must start with '/'",
		},
		{
			name:    "base path must not end with a slash",
			env:     map[string]string{"SANDBOX_BASE_PATH": "/sandbox/"},
			wantErr: "must not end with '/'",
		},
		{
			name:    "public url must not end with a slash",
			env:     map[string]string{"SANDBOX_PUBLIC_URL": "https://example.com/"},
			wantErr: "must not end with '/'",
		},
		{
			name: "namespace prefix must be namespace-safe",
			env:  map[string]string{"SANDBOX_NAMESPACE_PREFIX": "Sbx_"},
			// It becomes part of a namespace name, so an unusable prefix would
			// fail on every create rather than once, here.
			wantErr: "not a valid namespace prefix",
		},
		{
			name:    "default ttl above the ceiling",
			env:     map[string]string{"SANDBOX_DEFAULT_TTL": "24h"},
			wantErr: "exceeds SANDBOX_MAX_TTL",
		},
		{
			name:    "unknown log level",
			env:     map[string]string{"SANDBOX_LOG_LEVEL": "verbose"},
			wantErr: "not one of",
		},
		{
			name:    "negative ttl",
			env:     map[string]string{"SANDBOX_DEFAULT_TTL": "-1h"},
			wantErr: "is negative",
		},
		{
			name:    "unparseable duration",
			env:     map[string]string{"SANDBOX_MAX_TTL": "eight hours"},
			wantErr: "SANDBOX_MAX_TTL",
		},
		{
			name:    "negative max sandboxes",
			env:     map[string]string{"SANDBOX_MAX_SANDBOXES": "-1"},
			wantErr: "is negative",
		},
		{
			name: "a complete valid configuration",
			env: map[string]string{
				"SANDBOX_BASE_PATH":     "/sandbox",
				"SANDBOX_PUBLIC_URL":    "https://example.com",
				"SANDBOX_DEFAULT_TTL":   "30m",
				"SANDBOX_MAX_TTL":       "2h",
				"SANDBOX_MAX_SANDBOXES": "5",
				"SANDBOX_LOG_LEVEL":     "debug",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.env)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Load() returned an unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Load() accepted a configuration with %s", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Load() error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		name   string
		public string
		base   string
		path   string
		want   string
	}{
		{"root, no public url", "", "", "/api/v1/config", "/api/v1/config"},
		{"root, with public url", "https://sandbox.example.com", "", "/api/v1", "https://sandbox.example.com/api/v1"},
		{"base path, no public url", "", "/sandbox", "/api/v1", "/sandbox/api/v1"},
		{"both", "https://sandbox.example.com", "/sandbox", "/sandbox/abc", "https://sandbox.example.com/sandbox/sandbox/abc"},
		{"empty path", "https://sandbox.example.com", "/sandbox", "", "https://sandbox.example.com/sandbox"},
		{"path without a leading slash", "", "/sandbox", "api/v1", "/sandbox/api/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{PublicURL: tc.public, BasePath: tc.base}
			if got := cfg.URL(tc.path); got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestSandboxNamespace(t *testing.T) {
	cfg := Config{SandboxNamespacePrefix: "sbx-"}
	if got := cfg.SandboxNamespace("demo"); got != "sbx-demo" {
		t.Errorf("SandboxNamespace(\"demo\") = %q, want sbx-demo", got)
	}
}

func TestClampTTL(t *testing.T) {
	cfg := Config{MaxTTL: 4 * time.Hour}

	tests := []struct {
		name        string
		requested   time.Duration
		templateMax time.Duration
		want        time.Duration
	}{
		{"no expiry stays no expiry", 0, time.Hour, 0},
		{"under every ceiling", 30 * time.Minute, time.Hour, 30 * time.Minute},
		{"over the template's", 2 * time.Hour, time.Hour, time.Hour},
		{"over the deployment's", 9 * time.Hour, 0, 4 * time.Hour},
		{"over both takes the lower", 9 * time.Hour, time.Hour, time.Hour},
		{"negative is treated as no expiry", -time.Hour, time.Hour, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.ClampTTL(tc.requested, tc.templateMax); got != tc.want {
				t.Errorf("ClampTTL(%v, %v) = %v, want %v", tc.requested, tc.templateMax, got, tc.want)
			}
		})
	}

	t.Run("no deployment ceiling", func(t *testing.T) {
		open := Config{}
		if got := open.ClampTTL(100*time.Hour, 0); got != 100*time.Hour {
			t.Errorf("ClampTTL with no ceiling = %v, want 100h", got)
		}
	})
}

func TestDefaultTTLFor(t *testing.T) {
	cfg := Config{DefaultTTL: time.Hour}
	if got := cfg.DefaultTTLFor(30 * time.Minute); got != 30*time.Minute {
		t.Errorf("DefaultTTLFor(30m) = %v, want the template's 30m", got)
	}
	if got := cfg.DefaultTTLFor(0); got != time.Hour {
		t.Errorf("DefaultTTLFor(0) = %v, want the deployment's 1h", got)
	}
}
