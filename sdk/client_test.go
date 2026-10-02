package sdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shaowenchen/sandboxlab/sdk"
)

// The client's own behaviour: the request it builds, the key it presents, and
// the error it reports when the server refuses. What the endpoints *do* is the
// server's business and is tested there; what is tested here is that a caller of
// this package reaches them correctly.
//
// It goes over a real HTTP server rather than a fake transport, because the
// things most likely to break are the ones a fake hides: the path a request is
// built from, the header it carries, and how a status is turned into an error.

// recorded is one request the test server received.
type recorded struct {
	method string
	path   string
	query  string
	key    string
	bearer string
	body   string
}

// newServer starts a server that answers with the given status and body, and
// records what it was asked.
func newServer(t *testing.T, status int, body string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := new(strings.Builder)
		if r.Body != nil {
			_, _ = fmt.Fprint(raw, readAll(r))
		}
		got = append(got, recorded{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			key:    r.Header.Get("X-Sandbox-Key"),
			bearer: r.Header.Get("Authorization"),
			body:   raw.String(),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func readAll(r *http.Request) string {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}

func TestClientSendsTheKeyAndThePath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		body   string
		call   func(c *sdk.ClientWithResponses) (*http.Response, error)
	}{
		{
			name:   "whoami",
			path:   "/api/v1/whoami",
			status: http.StatusOK,
			body:   `{"role":"admin","admin":true}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.Whoami(context.Background()) },
		},
		{
			name:   "catalog",
			path:   "/api/v1/catalog",
			status: http.StatusOK,
			body:   `{"templates":[]}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.ListCatalog(context.Background()) },
		},
		{
			name:   "sandboxes",
			path:   "/api/v1/sandboxes",
			status: http.StatusOK,
			body:   `{"sandboxes":[],"count":0}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.ListSandboxes(context.Background()) },
		},
		{
			name:   "one sandbox",
			path:   "/api/v1/sandboxes/demo",
			status: http.StatusOK,
			body:   `{"id":"demo","template":"python","image":"python:3.12","state":"Running","createdAt":"2026-01-01T00:00:00Z"}`,
			call: func(c *sdk.ClientWithResponses) (*http.Response, error) {
				return c.GetSandbox(context.Background(), "demo")
			},
		},
		{
			name:   "overview",
			path:   "/api/v1/overview",
			status: http.StatusOK,
			body:   `{"total":0,"byState":{},"byTemplate":{},"cluster":true,"scoped":false}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.GetOverview(context.Background()) },
		},
		{
			name:   "config",
			path:   "/api/v1/config",
			status: http.StatusOK,
			body:   `{"apiVersion":"v1"}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.GetConfig(context.Background()) },
		},
		{
			name:   "describe",
			path:   "/api/v1/describe",
			status: http.StatusOK,
			body:   `{"name":"sandboxlab","version":"v1","build":"dev","basePath":"","endpoints":[]}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.Describe(context.Background()) },
		},
		{
			name:   "health",
			path:   "/healthz",
			status: http.StatusOK,
			body:   `{"status":"ok"}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.Health(context.Background()) },
		},
		{
			name:   "ready",
			path:   "/readyz",
			status: http.StatusOK,
			body:   `{"status":"ready"}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.Ready(context.Background()) },
		},
		{
			name:   "users",
			path:   "/api/v1/users",
			status: http.StatusOK,
			body:   `{"users":[],"count":0}`,
			call:   func(c *sdk.ClientWithResponses) (*http.Response, error) { return c.ListUsers(context.Background()) },
		},
		{
			name:   "one user",
			path:   "/api/v1/users/alice",
			status: http.StatusOK,
			body:   `{"name":"alice","key":"k","createdAt":"2026-01-01T00:00:00Z","quota":{}}`,
			call: func(c *sdk.ClientWithResponses) (*http.Response, error) {
				return c.GetUser(context.Background(), "alice")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := newServer(t, tc.status, tc.body)
			client, err := sdk.New(sdk.Options{BaseURL: srv.URL, Key: "test-key"})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			resp, err := tc.call(client)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			resp.Body.Close()

			if len(*got) != 1 {
				t.Fatalf("the server saw %d requests, want 1", len(*got))
			}
			r := (*got)[0]
			if r.path != tc.path {
				t.Errorf("path = %q, want %q", r.path, tc.path)
			}
			if r.key != "test-key" {
				t.Errorf("the key was sent as %q, want test-key", r.key)
			}
			if r.method != http.MethodGet {
				t.Errorf("method = %q, want GET", r.method)
			}
		})
	}
}

func TestDeleteUsesTheRightVerbAndPath(t *testing.T) {
	srv, got := newServer(t, http.StatusOK, `{"deleted":"demo"}`)
	c, err := sdk.New(sdk.Options{BaseURL: srv.URL, Key: "k"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp, err := c.DeleteSandbox(context.Background(), "demo")
	if err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	resp.Body.Close()

	r := (*got)[0]
	if r.method != http.MethodDelete || r.path != "/api/v1/sandboxes/demo" {
		t.Errorf("got %s %s, want DELETE /api/v1/sandboxes/demo", r.method, r.path)
	}
}

func TestCreateSendsTheBody(t *testing.T) {
	srv, got := newServer(t, http.StatusCreated, `{"id":"demo","template":"python","image":"i","state":"Pending","createdAt":"2026-01-01T00:00:00Z"}`)
	c, err := sdk.New(sdk.Options{BaseURL: srv.URL, Key: "k"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	body := sdk.CreateSandboxJSONRequestBody{Template: "python", Name: "demo"}
	resp, err := c.CreateSandbox(context.Background(), body)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	resp.Body.Close()

	r := (*got)[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %q, want POST", r.method)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(r.body), &sent); err != nil {
		t.Fatalf("the body was not JSON: %q", r.body)
	}
	if sent["template"] != "python" {
		t.Errorf("the body carried template=%v, want python", sent["template"])
	}
}

func TestLogsSendsTheTailParameter(t *testing.T) {
	srv, got := newServer(t, http.StatusOK, `{"logs":"hello"}`)
	c, err := sdk.New(sdk.Options{BaseURL: srv.URL, Key: "k"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	tail := 50
	resp, err := c.GetSandboxLogs(context.Background(), "demo", &sdk.GetSandboxLogsParams{Tail: tail})
	if err != nil {
		t.Fatalf("GetSandboxLogs: %v", err)
	}
	resp.Body.Close()

	if q := (*got)[0].query; q != "tail=50" {
		t.Errorf("query = %q, want tail=50", q)
	}
}

// TestCheckTurnsAStatusIntoAnError is the behaviour the CLI depends on: it does
// not inspect status codes, it asks whether the error was a 404.
func TestCheckTurnsAStatusIntoAnError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantErr  bool
		notFound bool
		conflict bool
		limit    bool
		message  string
	}{
		{"ok", http.StatusOK, `{}`, false, false, false, false, ""},
		{"created", http.StatusCreated, `{}`, false, false, false, false, ""},
		{"missing", http.StatusNotFound, `{"error":"no sandbox named \"ghost\""}`, true, true, false, false, `no sandbox named "ghost"`},
		{"conflict", http.StatusConflict, `{"error":"that name is taken"}`, true, false, true, false, "that name is taken"},
		{"limit", http.StatusTooManyRequests, `{"error":"you have 3 of 3"}`, true, false, false, true, "you have 3 of 3"},
		{"bad request", http.StatusBadRequest, `{"error":"bad ttl"}`, true, false, false, false, "bad ttl"},
		{"not json", http.StatusBadGateway, `<html>nope</html>`, true, false, false, false, "<html>nope</html>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServer(t, tc.status, tc.body)
			client, err := sdk.New(sdk.Options{BaseURL: srv.URL, Key: "k"})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			// A generated call, then the check the CLI performs on it.
			resp, err := client.GetSandbox(context.Background(), "ghost")
			if err != nil {
				t.Fatalf("GetSandbox: %v", err)
			}
			defer resp.Body.Close()
			raw := []byte(readBody(t, resp))

			checkErr := sdk.Check(resp, raw)
			if (checkErr != nil) != tc.wantErr {
				t.Fatalf("Check() error = %v, wantErr %v", checkErr, tc.wantErr)
			}
			if checkErr == nil {
				return
			}
			if sdk.IsNotFound(checkErr) != tc.notFound {
				t.Errorf("IsNotFound = %v, want %v", sdk.IsNotFound(checkErr), tc.notFound)
			}
			if sdk.IsConflict(checkErr) != tc.conflict {
				t.Errorf("IsConflict = %v, want %v", sdk.IsConflict(checkErr), tc.conflict)
			}
			if sdk.IsLimitReached(checkErr) != tc.limit {
				t.Errorf("IsLimitReached = %v, want %v", sdk.IsLimitReached(checkErr), tc.limit)
			}
			if checkErr.Error() != tc.message {
				t.Errorf("message = %q, want %q", checkErr.Error(), tc.message)
			}
		})
	}
}

func TestNewRefusesAnEmptyAddress(t *testing.T) {
	if _, err := sdk.New(sdk.Options{Key: "k"}); err == nil {
		t.Fatal("New with no BaseURL should fail rather than build relative requests")
	}
}

func TestNewTrimsATrailingSlash(t *testing.T) {
	// A base URL with a trailing slash would otherwise produce "//api/v1/...",
	// which some gateways route differently from "/api/v1/...".
	srv, got := newServer(t, http.StatusOK, `{"role":"admin","admin":true}`)
	c, err := sdk.New(sdk.Options{BaseURL: srv.URL + "/", Key: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if p := (*got)[0].path; p != "/api/v1/whoami" {
		t.Errorf("path = %q, want /api/v1/whoami", p)
	}
}

func TestSandboxURL(t *testing.T) {
	got := sdk.SandboxURL("https://sandbox.example.com/sandbox", "my-key", "demo", "vnc")
	want := "https://sandbox.example.com/sandbox/sandbox/demo/vnc/?key=my-key"
	if got != want {
		t.Errorf("SandboxURL = %q, want %q", got, want)
	}

	// The key goes in the query string because a browser navigation cannot set
	// a header, so it has to survive being put in a URL.
	got = sdk.SandboxURL("https://x/sandbox", "a b&c", "demo", "8000")
	if !strings.Contains(got, "key=a+b%26c") && !strings.Contains(got, "key=a%20b%26c") {
		t.Errorf("SandboxURL did not escape the key: %q", got)
	}
}
