package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file is hand-written. It adds the few things a generator cannot know:
// how a key is presented, what a failure means to a caller, and how to build
// the one address that is not a typed call.

// APIError is a non-2xx response from the control plane.
//
// The generated methods return their own per-status types (JSON404 and the
// rest), which is precise but awkward to act on: a caller asking "was this
// missing, or was I wrong?" should not have to type-switch on eight structs.
// Check collapses them into one value carrying the status and the server's
// message, so the predicates below work the same everywhere.
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

// IsNotFound reports whether an error is a 404 — no such object, or not one
// this caller may see.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsConflict reports whether an error is a 409: most often a name that is
// taken, or a sandbox that is not ready to be read yet.
func IsConflict(err error) bool { return statusIs(err, http.StatusConflict) }

// IsLimitReached reports whether an error is a 429: the caller, or the
// deployment, is at its ceiling.
func IsLimitReached(err error) bool { return statusIs(err, http.StatusTooManyRequests) }

func statusIs(err error, status int) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.Status == status
}

// Options configure a client.
type Options struct {
	// BaseURL is the deployment's address, including its base path, e.g.
	// "https://sandbox.example.com/sandbox". A trailing slash is ignored.
	BaseURL string

	// Key is the deployment's key. It is sent on every request; leave it empty
	// only to call the routes that take no key — /healthz, /readyz, /describe
	// and the console.
	Key string

	// HTTPClient overrides the transport. Leave it nil for the default.
	HTTPClient *http.Client
}

// New builds a client for one deployment.
//
// It returns the generated client rather than a wrapper around it: every
// operation is already a method there, and a second type with the same methods
// would be one more thing to keep in step.
//
// It fails rather than guessing when the address is missing — an empty base URL
// produces requests to relative paths, which fail in a way that says nothing
// about the configuration that caused it.
func New(opts Options) (*ClientWithResponses, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("no control plane address: set BaseURL to something like https://<host>/sandbox")
	}

	key := opts.Key
	options := []ClientOption{
		WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			if key != "" {
				req.Header.Set("X-Sandbox-Key", key)
			}
			return nil
		}),
	}
	if opts.HTTPClient != nil {
		options = append(options, WithHTTPClient(opts.HTTPClient))
	}
	return NewClientWithResponses(base, options...)
}

// SandboxURL is the address to open a sandbox's port at, with the key in the
// query string so the address works when a browser opens it.
//
// The data plane is proxied rather than typed — what comes back is whatever the
// sandbox serves — so there is no generated method for it, and this is the
// useful thing to do with it instead. The key is in the URL because a
// navigation cannot set a header.
func SandboxURL(baseURL, key, id, port string) string {
	return fmt.Sprintf("%s/sandbox/%s/%s/?key=%s",
		strings.TrimRight(baseURL, "/"),
		url.PathEscape(id), url.PathEscape(port), url.QueryEscape(key))
}

// Check turns a non-2xx response into an *APIError, and returns nil for one
// that succeeded.
//
// The generated methods return a typed value per status rather than an error,
// which pushes the same decision onto every caller. This is the one place it is
// made, so the predicates above behave consistently.
func Check(resp *http.Response, body []byte) error {
	if resp == nil {
		return fmt.Errorf("no response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
		message = envelope.Error
	}
	return &APIError{Status: resp.StatusCode, Message: message}
}
