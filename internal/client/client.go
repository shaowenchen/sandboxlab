// Package client is the typed HTTP client the CLI and the console both reach
// the control plane through.
//
// It exists so the CLI cannot drift from the API: every command is a thin layer
// over a method here, and every method hits the same endpoint the console's
// fetch does. When a route changes, this package stops compiling — which is a
// much better failure than a command that quietly stops working.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/shaowenchen/sandboxlab/internal/model"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// Client talks to one deployment.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New builds a client. The base URL may carry the deployment's base path and a
// trailing slash; the key is sent on every request that needs one.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http: &http.Client{
			// No global timeout: `sandbox logs -f` follows a stream, and a
			// deadline here would cut it off. Each call that can hang takes its
			// own context instead.
			Timeout: 0,
		},
	}
}

// APIError is a non-2xx response from the control plane.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the server returned %d", e.Status)
	}
	return e.Message
}

// IsNotFound reports whether an error is a 404.
func IsNotFound(err error) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.Status == http.StatusNotFound
}

// Describe is the API's own contract. It needs no key, which is what makes it
// usable to check an address before configuring one.
type Describe struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Build     string `json:"build"`
	BasePath  string `json:"basePath"`
	Endpoints []struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
	} `json:"endpoints"`
}

// GetDescribe reads the contract document.
func (c *Client) GetDescribe(ctx context.Context) (Describe, error) {
	var out Describe
	return out, c.do(ctx, http.MethodGet, "/api/v1/describe", nil, &out, false)
}

// Catalog lists the templates.
func (c *Client) Catalog(ctx context.Context) ([]model.Template, error) {
	var out struct {
		Templates []model.Template `json:"templates"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/catalog", nil, &out, true)
	return out.Templates, err
}

// List returns every sandbox.
func (c *Client) List(ctx context.Context) ([]model.Sandbox, error) {
	var out struct {
		Sandboxes []model.Sandbox `json:"sandboxes"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes", nil, &out, true)
	return out.Sandboxes, err
}

// Get returns one sandbox.
func (c *Client) Get(ctx context.Context, id string) (model.Sandbox, error) {
	var out model.Sandbox
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(id), nil, &out, true)
	return out, err
}

// CreateInput is what a create call sends.
type CreateInput struct {
	Template string            `json:"template"`
	Name     string            `json:"name,omitempty"`
	TTL      string            `json:"ttl,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// Create makes a sandbox.
func (c *Client) Create(ctx context.Context, in CreateInput) (model.Sandbox, error) {
	var out model.Sandbox
	err := c.do(ctx, http.MethodPost, "/api/v1/sandboxes", in, &out, true)
	return out, err
}

// Delete removes a sandbox.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/sandboxes/"+url.PathEscape(id), nil, nil, true)
}

// Renew resets a sandbox's expiry. An empty ttl removes it.
func (c *Client) Renew(ctx context.Context, id, ttl string) (model.Sandbox, error) {
	var out model.Sandbox
	body := map[string]string{"ttl": ttl}
	err := c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(id)+"/renew", body, &out, true)
	return out, err
}

// Logs returns the tail of a sandbox's output.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	path := fmt.Sprintf("/api/v1/sandboxes/%s/logs?tail=%d", url.PathEscape(id), tail)
	var out struct {
		Logs string `json:"logs"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out, true)
	return out.Logs, err
}

// Overview summarises the deployment.
func (c *Client) Overview(ctx context.Context) (sandbox.Overview, error) {
	var out sandbox.Overview
	err := c.do(ctx, http.MethodGet, "/api/v1/overview", nil, &out, true)
	return out, err
}

// BaseURL is the deployment's address, for a command that prints it.
func (c *Client) BaseURL() string { return c.baseURL }

// Key is the key this client authenticates with.
//
// It is read back rather than kept by the caller so a command can build a URL a
// browser can open — the data-plane routes take the key in the query string
// because a navigation cannot set a header, and the address a command prints is
// meant to be clickable.
func (c *Client) Key() string { return c.key }

// do performs one request.
func (c *Client) do(ctx context.Context, method, path string, body, out any, needsKey bool) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if needsKey {
		if c.key == "" {
			return fmt.Errorf("no API key is configured; set SANDBOX_KEY or pass --key")
		}
		req.Header.Set("X-Sandbox-Key", c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	// Read with a cap: an error page from something that is not the control
	// plane — a proxy, most likely — should be reported, not streamed into
	// memory until the process dies.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message := strings.TrimSpace(string(raw))
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error != "" {
			message = envelope.Error
		}
		return &APIError{Status: resp.StatusCode, Message: message}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the response was not the JSON expected: %w", err)
	}
	return nil
}

// ── identity ────────────────────────────────────────────────────────────────

// Whoami is what GET /api/v1/whoami returns.
type Whoami struct {
	Role           string `json:"role"`
	Admin          bool   `json:"admin"`
	User           string `json:"user,omitempty"`
	CanManageUsers bool   `json:"canManageUsers,omitempty"`
}

// Whoami reports which kind of key this client holds.
func (c *Client) Whoami(ctx context.Context) (Whoami, error) {
	var out Whoami
	err := c.do(ctx, http.MethodGet, "/api/v1/whoami", nil, &out, true)
	return out, err
}

// ── users ───────────────────────────────────────────────────────────────────

// CreateUserInput is what creating or updating a user sends.
//
// The Set fields exist because a limit of zero is a real value — "no limit" —
// so an absent flag and a zero one are different things, and an update that
// could not tell them apart would clear every limit it was not asked about.
type CreateUserInput struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`

	SetMaxSandboxes bool `json:"-"`
	SetMaxTTL       bool `json:"-"`
	SetTemplates    bool `json:"-"`

	MaxSandboxes int    `json:"maxSandboxes"`
	MaxTTL       string `json:"maxTTL,omitempty"`
	Templates    string `json:"templates,omitempty"`
}

// Users lists the users.
func (c *Client) Users(ctx context.Context) ([]user.User, error) {
	var out struct {
		Users []user.User `json:"users"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/users", nil, &out, true)
	return out.Users, err
}

// User reads one user, with its key.
func (c *Client) User(ctx context.Context, name string) (user.User, error) {
	var out user.User
	err := c.do(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(name), nil, &out, true)
	return out, err
}

// CreateUser makes a user.
func (c *Client) CreateUser(ctx context.Context, in CreateUserInput) (user.User, error) {
	var out user.User
	err := c.do(ctx, http.MethodPost, "/api/v1/users", in, &out, true)
	return out, err
}

// UpdateUser changes a user's limits, sending only the ones that were set.
func (c *Client) UpdateUser(ctx context.Context, name string, in CreateUserInput) (user.User, error) {
	body := map[string]any{}
	if in.SetMaxSandboxes {
		body["maxSandboxes"] = in.MaxSandboxes
	}
	if in.SetMaxTTL {
		body["maxTTL"] = in.MaxTTL
	}
	if in.SetTemplates {
		body["templates"] = splitTemplates(in.Templates)
	}
	var out user.User
	err := c.do(ctx, http.MethodPatch, "/api/v1/users/"+url.PathEscape(name), body, &out, true)
	return out, err
}

// RotateUserKey issues a new key for a user.
func (c *Client) RotateUserKey(ctx context.Context, name, key string) (user.User, error) {
	var out user.User
	err := c.do(ctx, http.MethodPost, "/api/v1/users/"+url.PathEscape(name)+"/key",
		map[string]string{"key": key}, &out, true)
	return out, err
}

// DeleteUser removes a user.
func (c *Client) DeleteUser(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/users/"+url.PathEscape(name), nil, nil, true)
}

// splitTemplates reads a comma-separated list into the array the API expects.
func splitTemplates(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
