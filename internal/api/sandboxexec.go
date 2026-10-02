package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// Running a command, and moving a file, through the JSON API.
//
// These are the two primitives that make a sandbox usable by a program rather
// than by a person with a browser: a portless sandbox — python, node — serves
// no URL at all, so without them it is a workspace with no way in.

// execRequest is the body POST /api/v1/sandboxes/{id}/exec accepts.
type execRequest struct {
	// Command is an argv. A shell is named explicitly, as ["sh", "-c", "..."],
	// rather than assumed: a caller that wants one can ask, and a caller that
	// does not should not have its arguments interpreted by one.
	Command []string `json:"command"`
	// Stdin is fed to the command, decoded per Encoding.
	Stdin string `json:"stdin,omitempty"`
	// Cwd is the working directory. Empty means the image's own.
	Cwd string `json:"cwd,omitempty"`
	// Timeout is how long the command may take, e.g. "30s". Bounded by the
	// deployment's ceiling however large it is.
	Timeout string `json:"timeout,omitempty"`
	// Encoding is "utf8" (the default) or "base64", and applies to Stdin.
	Encoding string `json:"encoding,omitempty"`
}

// execResponse is what POST .../exec returns.
type execResponse struct {
	// ExitCode is the command's status. **A non-zero value is not a failure of
	// the request** — the command ran, and this is what it said. Only an error
	// response means the command could not be run.
	ExitCode int `json:"exitCode"`
	// Stdout and Stderr are the command's output, encoded per Encoding.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Encoding is how Stdout and Stderr are spelled. It is chosen per field, so
	// a command that printed text and a command that printed bytes can differ.
	StdoutEncoding string `json:"stdoutEncoding"`
	StderrEncoding string `json:"stderrEncoding"`
	// Truncated report that the output cap was reached, which is what tells an
	// empty output apart from a cut-off one.
	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool `json:"stderrTruncated,omitempty"`
}

// fileResponse is the body GET /api/v1/sandboxes/{id}/files returns.
type fileResponse struct {
	Path string `json:"path"`
	// Content is the file's bytes, decoded per Encoding.
	Content string `json:"content"`
	// Encoding is "utf8" when the file is text and "base64" when it is not, so
	// a caller decides from a field rather than by sniffing the content.
	Encoding string `json:"encoding"`
	// Size is the length of the file's bytes, before encoding.
	Size int64 `json:"size"`
}

// writeFileRequest is the body PUT /api/v1/sandboxes/{id}/files accepts.
type writeFileRequest struct {
	// Content is the file's bytes, decoded per Encoding.
	Content string `json:"content"`
	// Encoding is "utf8" (the default) or "base64".
	Encoding string `json:"encoding,omitempty"`
	// CreateParents makes the directories above the path if they are missing.
	// It is off by default: a silently created directory turns a typo into a
	// file nobody will find, and saying so is one field away.
	CreateParents bool `json:"createParents,omitempty"`
}

// writeFileResponse is what a write reports.
type writeFileResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// maxFileBody bounds a file write. It is the file limit plus room for the JSON
// envelope and for base64, which inflates by a third.
const maxFileBody = k8s.MaxFileBytes*4/3 + 64*1024

func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request) {
	var body execRequest
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "could not read the request body: " + err.Error()})
		return
	}
	if len(body.Command) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "command is required, and is a list: [\"sh\", \"-c\", \"...\"]"})
		return
	}
	timeout, err := parseTTL(body.Timeout)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	stdin, err := decodeContent(body.Stdin, body.Encoding)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	for _, arg := range body.Command {
		if len(arg) > k8s.MaxPathLength {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "an argument is longer than this API accepts"})
			return
		}
	}

	out, err := s.svc.Exec(r.Context(), r.PathValue("id"), sandbox.ExecInput{
		Command: body.Command,
		Stdin:   stdin,
		Cwd:     body.Cwd,
		Timeout: timeout,
	})
	if err != nil {
		writeError(w, logger(r), err)
		return
	}

	stdout, stdoutEncoding := encodeContent(out.Stdout)
	stderr, stderrEncoding := encodeContent(out.Stderr)
	writeJSON(w, http.StatusOK, execResponse{
		ExitCode:        out.ExitCode,
		Stdout:          stdout,
		Stderr:          stderr,
		StdoutEncoding:  stdoutEncoding,
		StderrEncoding:  stderrEncoding,
		StdoutTruncated: out.StdoutTruncated,
		StderrTruncated: out.StderrTruncated,
	})
}

func (s *Server) sandboxFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.readSandboxFile(w, r)
	case http.MethodPut:
		s.writeSandboxFile(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
	}
}

func (s *Server) readSandboxFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "a path is required, as ?path=/workspace/a.txt"})
		return
	}
	out, err := s.svc.ReadFile(r.Context(), r.PathValue("id"), path)
	if err != nil {
		writeError(w, logger(r), err)
		return
	}
	writeJSON(w, http.StatusOK, fileResponse{
		Path:     path,
		Content:  out.Content,
		Encoding: out.Encoding,
		Size:     out.Size,
	})
}

func (s *Server) writeSandboxFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "a path is required, as ?path=/workspace/a.txt"})
		return
	}
	// Not decodeJSON: that caps the body at 1 MiB, which is under the size a
	// file write is allowed to be. The cap here is the file limit plus what the
	// JSON envelope and base64 add to it, and the ResponseWriter is passed so
	// that going over closes the connection rather than leaving the client
	// writing into a socket nobody is reading.
	var body writeFileRequest
	if err := decodeJSONLimited(w, r, maxFileBody, &body); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "the body is larger than this API accepts"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "could not read the request body: " + err.Error()})
		return
	}
	data, err := decodeContent(body.Content, body.Encoding)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}

	info, err := s.svc.WriteFile(r.Context(), r.PathValue("id"), path, data, body.CreateParents)
	if err != nil {
		writeError(w, logger(r), err)
		return
	}
	writeJSON(w, http.StatusOK, writeFileResponse{Path: path, Size: info.Size})
}

// encodeContent spells bytes so they survive JSON.
//
// Text goes out as it is, because that is what a caller reading a source file
// wants; anything else is base64, because JSON strings cannot carry arbitrary
// bytes. The caller is told which through the encoding field rather than having
// to guess.
func encodeContent(b []byte) (string, string) {
	if utf8.Valid(b) && !containsNUL(b) {
		return string(b), "utf8"
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}

// decodeContent reads bytes that were spelled by encodeContent, or by a caller
// that chose base64 for itself.
func decodeContent(s, encoding string) ([]byte, error) {
	switch encoding {
	case "", "utf8", "utf-8":
		return []byte(s), nil
	case "base64":
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errors.New("the content is not valid base64")
		}
		if len(b) > k8s.MaxFileBytes {
			return nil, errors.New("the content is larger than this API accepts")
		}
		return b, nil
	default:
		return nil, errors.New("encoding must be utf8 or base64, not " + encoding)
	}
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
