package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/shaowenchen/sandboxlab/internal/sandbox"
	"github.com/shaowenchen/sandboxlab/internal/userservice"
)

// writeJSON writes a value as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Nothing useful can be done about a failure here — the status line is
	// already written — so it is deliberately dropped. The alternative is a
	// second status line, which corrupts the response.
	_ = enc.Encode(v)
}

// errorBody is the shape of every error response.
type errorBody struct {
	Error string `json:"error"`
}

// writeError maps an error to a status and writes it.
//
// The mapping is by the sentinel the service wrapped, so the HTTP layer never
// has to match on message text — which is the thing that makes an API's error
// handling rot quietly.
//
// Both services' sentinels are here, and that is deliberate rather than an
// oversight: the sandbox and user services are separate layers and neither
// should have to import the other to share an error vocabulary. What they do
// share is the HTTP layer, so the translation lives where both are visible —
// and a service added later that forgets to is a 500 that a test like this one
// catches rather than a status that quietly means nothing.
func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, sandbox.ErrInvalid), errors.Is(err, userservice.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, sandbox.ErrNotFound), errors.Is(err, userservice.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, sandbox.ErrConflict), errors.Is(err, userservice.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, sandbox.ErrLimit):
		status = http.StatusTooManyRequests
	}
	if status >= 500 {
		log.Error("request failed", "error", err)
	}
	writeJSON(w, status, errorBody{Error: err.Error()})
}

// decodeJSON reads a JSON request body into v, rejecting anything unexpected.
//
// DisallowUnknownFields is on so a misspelled field is an error rather than a
// silently ignored line — the failure it would otherwise cause is a create call
// that succeeds without the setting that was meant to be there.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("a request body is required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
