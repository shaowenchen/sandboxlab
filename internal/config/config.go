// Package config resolves the control plane's settings from the environment.
//
// Everything is an environment variable, set by the Helm chart. That keeps the
// binary free of a config file format and of flags that would have to be kept
// in step with the chart's values, and it means the deployment's settings are
// readable from the pod spec — which is where someone debugging a deployment
// looks first.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved configuration.
type Config struct {
	// Listen is the address the HTTP server binds, in Go's ":port" form.
	Listen string

	// Namespace is the namespace the control plane itself runs in — the one the
	// Deployment, ServiceAccount and Role live in. Sandboxes do not run here;
	// each gets its own namespace. It defaults to ops-system.
	Namespace string

	// SandboxNamespacePrefix is prepended to a sandbox id to name the namespace
	// that sandbox owns. "sbx-" plus an id, by default.
	SandboxNamespacePrefix string

	// PublicURL is the address the console and sandboxes are reached at, with
	// scheme and without a trailing slash: "https://sandbox.example.com". It is
	// used to build the URLs reported for a sandbox. Empty means the URLs are
	// reported relative, which is what a local run wants.
	PublicURL string

	// BasePath is a path prefix the whole deployment is served under, without a
	// trailing slash: "/sandbox" for a deployment reached at
	// "https://host/sandbox/...". Empty means the root.
	BasePath string

	// APIKey authenticates every route but /healthz, /readyz and the describe
	// endpoint. It is generated when unset, and the generated value is reported
	// so a caller can use it.
	APIKey string
	// keyGenerated records that APIKey was made up rather than configured, so
	// the caller can tell the operator where it came from.
	keyGenerated bool

	// CatalogDir is a directory of extra template files. Its ids override the
	// built-in ones.
	CatalogDir string
	// DisabledTemplates are template ids to leave out of the catalog.
	DisabledTemplates []string

	// DefaultTTL applies to a sandbox whose template names no default.
	DefaultTTL time.Duration
	// MaxTTL is a ceiling no template or caller can exceed. Zero is no ceiling.
	MaxTTL time.Duration
	// MaxSandboxes caps how many sandboxes may exist at once. Zero is no cap.
	MaxSandboxes int

	// ExecTimeout is how long a command run in a sandbox may take when the
	// caller names no time of its own.
	ExecTimeout time.Duration
	// MaxExecTimeout is the ceiling a caller cannot ask past. Zero is no
	// ceiling, which is how a request holds a connection indefinitely.
	MaxExecTimeout time.Duration
	// MaxFileBytes caps a file read and a write body. Zero means the package
	// default in internal/k8s.
	MaxFileBytes int64

	// ReapInterval is how often expired sandboxes are collected.
	ReapInterval time.Duration

	// DataPlane turns on proxying /sandbox/<id>/... to the sandbox's own port.
	DataPlane bool

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// Kubeconfig is an explicit kubeconfig path. Empty means in-cluster, then
	// the usual default.
	Kubeconfig string
}

// GeneratedKey reports whether the API key was generated rather than configured.
func (c Config) GeneratedKey() bool { return c.keyGenerated }

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	var cfg Config
	var err error

	// The default is 8080 rather than 80, and that is not arbitrary: the image
	// runs as a non-root user, which cannot bind a port below 1024. The
	// Dockerfile and the chart both set this explicitly, so the value here only
	// matters when the binary is run bare — where 80 would fail with a
	// permission error that says nothing about the cause.
	setString(&cfg.Listen, "SANDBOX_LISTEN", ":8080")
	setString(&cfg.Namespace, "SANDBOX_NAMESPACE", "ops-system")
	setString(&cfg.SandboxNamespacePrefix, "SANDBOX_NAMESPACE_PREFIX", "sbx-")
	setString(&cfg.PublicURL, "SANDBOX_PUBLIC_URL", "")
	setString(&cfg.BasePath, "SANDBOX_BASE_PATH", "")
	setString(&cfg.APIKey, "SANDBOX_API_KEY", "")
	setString(&cfg.CatalogDir, "SANDBOX_CATALOG_DIR", "")
	setString(&cfg.LogLevel, "SANDBOX_LOG_LEVEL", "info")
	setString(&cfg.Kubeconfig, "SANDBOX_KUBECONFIG", "")

	if cfg.APIKey == "" {
		cfg.APIKey, err = generateKey()
		if err != nil {
			return Config{}, fmt.Errorf("generating an API key: %w", err)
		}
		cfg.keyGenerated = true
	}

	cfg.DisabledTemplates = splitList(os.Getenv("SANDBOX_DISABLE_TEMPLATES"))

	if cfg.DefaultTTL, err = durationEnv("SANDBOX_DEFAULT_TTL", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.MaxTTL, err = durationEnv("SANDBOX_MAX_TTL", 8*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ReapInterval, err = durationEnv("SANDBOX_REAP_INTERVAL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.MaxSandboxes, err = intEnv("SANDBOX_MAX_SANDBOXES", 0); err != nil {
		return Config{}, err
	}
	if cfg.ExecTimeout, err = durationEnv("SANDBOX_EXEC_TIMEOUT", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.MaxExecTimeout, err = durationEnv("SANDBOX_MAX_EXEC_TIMEOUT", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.MaxFileBytes, err = intEnv64("SANDBOX_MAX_FILE_BYTES", 2<<20); err != nil {
		return Config{}, err
	}
	if cfg.DataPlane, err = boolEnv("SANDBOX_DATA_PLANE", true); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects a configuration the control plane could not run under.
func (c Config) Validate() error {
	switch {
	case c.Listen == "":
		return fmt.Errorf("SANDBOX_LISTEN is empty")
	case c.Namespace == "":
		return fmt.Errorf("SANDBOX_NAMESPACE is empty")
	case c.SandboxNamespacePrefix == "":
		return fmt.Errorf("SANDBOX_NAMESPACE_PREFIX is empty")
	case c.APIKey == "":
		return fmt.Errorf("SANDBOX_API_KEY is empty")
	}
	// A namespace name has to be lowercase and dash-safe, and it is used as a
	// prefix on ids that are already constrained — so a prefix that could not
	// form a valid name would fail at create time on every call.
	if !validNamespacePart(c.SandboxNamespacePrefix) {
		return fmt.Errorf("SANDBOX_NAMESPACE_PREFIX %q is not a valid namespace prefix (lowercase alphanumerics and '-')", c.SandboxNamespacePrefix)
	}
	if c.BasePath != "" {
		if !strings.HasPrefix(c.BasePath, "/") {
			return fmt.Errorf("SANDBOX_BASE_PATH %q must start with '/'", c.BasePath)
		}
		if strings.HasSuffix(c.BasePath, "/") {
			return fmt.Errorf("SANDBOX_BASE_PATH %q must not end with '/'", c.BasePath)
		}
	}
	if c.PublicURL != "" && strings.HasSuffix(c.PublicURL, "/") {
		return fmt.Errorf("SANDBOX_PUBLIC_URL %q must not end with '/'", c.PublicURL)
	}
	if c.MaxTTL != 0 && c.DefaultTTL > c.MaxTTL {
		return fmt.Errorf("SANDBOX_DEFAULT_TTL %s exceeds SANDBOX_MAX_TTL %s", c.DefaultTTL, c.MaxTTL)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("SANDBOX_LOG_LEVEL %q is not one of debug, info, warn, error", c.LogLevel)
	}
	return nil
}

// SandboxNamespace is the namespace a sandbox id owns.
func (c Config) SandboxNamespace(id string) string { return c.SandboxNamespacePrefix + id }

// URL joins a path onto the deployment's public address and base path.
//
// It is the one place a reported URL is built, so the console, the API and the
// CLI cannot disagree about where a sandbox lives. A deployment with no public
// URL reports a root-relative path instead, which is what a local run renders
// as a working link.
func (c Config) URL(path string) string {
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.PublicURL + c.BasePath + path
}

// DefaultTTLFor returns the TTL a create call gets when it names none: the
// template's default, the deployment's, then the ceiling, in that order.
func (c Config) DefaultTTLFor(templateDefault time.Duration) time.Duration {
	if templateDefault > 0 {
		return templateDefault
	}
	return c.DefaultTTL
}

// ClampTTL bounds a requested TTL by the deployment ceiling and the template's.
// A zero result means "no expiry".
func (c Config) ClampTTL(requested, templateMax time.Duration) time.Duration {
	if requested <= 0 {
		return 0
	}
	if templateMax > 0 && requested > templateMax {
		requested = templateMax
	}
	if c.MaxTTL > 0 && requested > c.MaxTTL {
		requested = c.MaxTTL
	}
	return requested
}

func setString(dst *string, key, def string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
		return
	}
	*dst = def
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %s is negative", key, v)
	}
	return d, nil
}

func intEnv(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: %d is negative", key, n)
	}
	return n, nil
}

func intEnv64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: %d is negative", key, n)
	}
	return n, nil
}

func boolEnv(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validNamespacePart reports whether s can appear inside a namespace name.
func validNamespacePart(s string) bool {
	if len(s) > 253 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// generateKey mints an API key. It is 32 hex characters — 16 bytes — which is
// long enough that guessing is not a threat model, and short enough to read off
// a job summary and paste.
func generateKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
