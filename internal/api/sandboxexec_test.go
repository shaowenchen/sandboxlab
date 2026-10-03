package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
)

// A server with one sandbox already in it, and the stub's exec surface.
func execServer(t *testing.T) (*Server, *stubService) {
	t.Helper()
	s, svc := newTestServerWithStub(t, config.Config{}, nil)
	if _, err := svc.Create(t.Context(), sandbox.CreateInput{Template: "agent-sandbox", Name: "demo"}); err != nil {
		t.Fatalf("creating the sandbox: %v", err)
	}
	return s, svc
}

// Both new routes are behind the key, like everything else that reaches into
// the deployment.
func TestTheExecAndFileRoutesNeedAKey(t *testing.T) {
	s, _ := execServer(t)

	tests := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/sandboxes/demo/exec", `{"command":["true"]}`},
		{http.MethodGet, "/api/v1/sandboxes/demo/files?path=/etc/hostname", ""},
		{http.MethodPut, "/api/v1/sandboxes/demo/files?path=/tmp/a", `{"content":"x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			for _, key := range []string{"", "wrong"} {
				w := do(t, s, tc.method, tc.path, key, tc.body)
				if w.Code != http.StatusUnauthorized {
					t.Errorf("%s with key %q = %d, want 401", tc.method, key, w.Code)
				}
			}
		})
	}
}

// ── exec ────────────────────────────────────────────────────────────────────

func TestExecRunsTheCommandAndReturnsItsOutput(t *testing.T) {
	s, _ := execServer(t)

	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey,
		`{"command":["sh","-c","echo hi"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var got struct {
		ExitCode       int    `json:"exitCode"`
		Stdout         string `json:"stdout"`
		StdoutEncoding string `json:"stdoutEncoding"`
	}
	decode(t, w, &got)
	if got.ExitCode != 0 {
		t.Errorf("exitCode = %d, want 0", got.ExitCode)
	}
	if !strings.Contains(got.Stdout, "echo hi") {
		t.Errorf("stdout = %q, want the command echoed back", got.Stdout)
	}
	if got.StdoutEncoding != "utf8" {
		t.Errorf("stdoutEncoding = %q, want utf8 for text", got.StdoutEncoding)
	}
}

// A command that failed is a 200 carrying its status.
//
// This is the assertion that keeps `grep` finding nothing from looking like a
// broken API — and the one a naive implementation gets wrong by letting the
// exit status become the HTTP status.
func TestANonZeroExitIsAnOKResponse(t *testing.T) {
	s, svc := execServer(t)
	svc.execExitCode = 7

	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey, `{"command":["false"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a command that ran and exited 7 is not an API failure. %s", w.Code, w.Body)
	}
	var got struct {
		ExitCode int `json:"exitCode"`
	}
	decode(t, w, &got)
	if got.ExitCode != 7 {
		t.Errorf("exitCode = %d, want 7", got.ExitCode)
	}
}

func TestExecRejectsAnEmptyCommand(t *testing.T) {
	s, _ := execServer(t)
	for _, body := range []string{`{}`, `{"command":[]}`} {
		w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST exec with %s = %d, want 400", body, w.Code)
		}
	}
}

func TestExecRejectsAMisspelledField(t *testing.T) {
	// DisallowUnknownFields: a typo must be an error, not a command that ran
	// without the argument that was meant to be there.
	s, _ := execServer(t)
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey,
		`{"command":["true"],"workdir":"/tmp"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d for an unknown field, want 400", w.Code)
	}
}

func TestExecOnAnUnknownSandbox(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/ghost/exec", testKey, `{"command":["true"]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// The status mapping for the ways a command can fail to run at all.
func TestExecFailureStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"a sandbox that is still starting", sandbox.ErrConflict, http.StatusConflict},
		{"a command that outlived its time", sandbox.ErrTimeout, http.StatusGatewayTimeout},
		{"a request the service refused", sandbox.ErrInvalid, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, svc := execServer(t)
			svc.execErr = tc.err
			w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey, `{"command":["sleep"]}`)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestExecAcceptsBase64Stdin(t *testing.T) {
	s, svc := execServer(t)
	// The stub ignores stdin, so what is asserted is that it decoded rather
	// than rejected: bad base64 is the failure this rules out.
	body := `{"command":["cat"],"stdin":"` + base64.StdEncoding.EncodeToString([]byte("hi")) + `","encoding":"base64"}`
	if w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey, body); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", w.Code, w.Body)
	}
	_ = svc
}

func TestExecRejectsBadBase64(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey,
		`{"command":["cat"],"stdin":"not base64!!","encoding":"base64"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestExecRejectsAnUnknownEncoding(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodPost, "/api/v1/sandboxes/demo/exec", testKey,
		`{"command":["cat"],"stdin":"eA==","encoding":"rot13"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ── files ───────────────────────────────────────────────────────────────────

func TestReadFileReturnsTheContent(t *testing.T) {
	s, svc := execServer(t)
	svc.fileContent = "hello\n"

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/demo/files?path=/workspace/a.txt", testKey, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var got struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	decode(t, w, &got)
	if got.Content != "hello\n" || got.Encoding != "utf8" {
		t.Errorf("got %q/%q, want the content as utf8", got.Content, got.Encoding)
	}
	if got.Path != "/workspace/a.txt" {
		t.Errorf("path = %q, want it echoed", got.Path)
	}
	if got.Size != 6 {
		t.Errorf("size = %d, want 6", got.Size)
	}
}

func TestReadFileNeedsAPath(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/demo/files", testKey, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d with no path, want 400", w.Code)
	}
}

// A missing file is a 404 whose message names the file, not the sandbox.
func TestAMissingFileSaysWhichFile(t *testing.T) {
	s, svc := execServer(t)
	// The wrapped form the service produces. That k8s.ReadFile actually names
	// the file is checked in internal/k8s; what this checks is that the API maps
	// the sentinel to a 404 and passes the sentence through rather than
	// replacing it with a generic one.
	svc.readErr = fmt.Errorf("%w: no file at %q in sandbox %q", k8s.ErrNoSuchFile, "/gone.txt", "demo")

	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/demo/files?path=/gone.txt", testKey, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/gone.txt") {
		t.Errorf("body = %s, want it to keep the service's message", w.Body)
	}
	if strings.Contains(w.Body.String(), "no sandbox") {
		t.Errorf("body = %s, which reads as a missing sandbox", w.Body)
	}
}

func TestAnOversizeReadIs413(t *testing.T) {
	s, svc := execServer(t)
	svc.readErr = k8s.ErrTooLarge
	w := do(t, s, http.MethodGet, "/api/v1/sandboxes/demo/files?path=/big", testKey, "")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestWriteFileStoresTheBytes(t *testing.T) {
	s, svc := execServer(t)

	w := do(t, s, http.MethodPut, "/api/v1/sandboxes/demo/files?path=/workspace/a.txt", testKey,
		`{"content":"hello\n"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if len(svc.wrote) != 1 {
		t.Fatalf("the service recorded %d writes, want 1", len(svc.wrote))
	}
	got := svc.wrote[0]
	if string(got.data) != "hello\n" {
		t.Errorf("wrote %q, want hello", got.data)
	}
	if got.path != "/workspace/a.txt" {
		t.Errorf("path = %q", got.path)
	}
	if got.parents {
		t.Error("createParents was passed as true when it was not asked for")
	}
}

func TestWriteFilePassesCreateParents(t *testing.T) {
	s, svc := execServer(t)
	body := `{"content":"x","createParents":true}`
	if w := do(t, s, http.MethodPut, "/api/v1/sandboxes/demo/files?path=/a/b/c.txt", testKey, body); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if !svc.wrote[0].parents {
		t.Error("createParents was not passed through")
	}
}

// A body over the file limit is refused before it reaches the cluster.
//
// This is also the test that would catch the write route being wired through
// the shared 1 MiB decoder by mistake.
func TestAnOversizeWriteBodyIs413(t *testing.T) {
	s, _ := execServer(t)

	big := strings.Repeat("x", maxFileBody+1024)
	body, err := json.Marshal(map[string]string{"content": big})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	w := do(t, s, http.MethodPut, "/api/v1/sandboxes/demo/files?path=/big", testKey, string(body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// And a body that is over 1 MiB but under the file limit still gets through,
// which is what makes the cap above a cap for files rather than for JSON.
func TestAWriteOverOneMebibyteStillWorks(t *testing.T) {
	s, svc := execServer(t)

	big := strings.Repeat("x", 1500*1024)
	body, err := json.Marshal(map[string]string{"content": big})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	w := do(t, s, http.MethodPut, "/api/v1/sandboxes/demo/files?path=/big", testKey, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if len(svc.wrote) != 1 || len(svc.wrote[0].data) != len(big) {
		t.Errorf("wrote %d bytes, want %d", len(svc.wrote[0].data), len(big))
	}
}

func TestWriteFileNeedsAPath(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodPut, "/api/v1/sandboxes/demo/files", testKey, `{"content":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d with no path, want 400", w.Code)
	}
}

func TestFilesRejectsAnUnsupportedMethod(t *testing.T) {
	s, _ := execServer(t)
	w := do(t, s, http.MethodDelete, "/api/v1/sandboxes/demo/files?path=/tmp/a", testKey, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "PUT") {
		t.Errorf("Allow = %q, want it to name the methods that work", allow)
	}
}

// ── the encoding round trip ─────────────────────────────────────────────────

func TestEncodingPicksBase64ForBytes(t *testing.T) {
	// A PNG header: valid bytes, invalid UTF-8.
	raw := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}

	content, encoding := encodeContent(raw)
	if encoding != "base64" {
		t.Fatalf("encoding = %q for binary, want base64", encoding)
	}
	back, err := decodeContent(content, encoding)
	if err != nil {
		t.Fatalf("decodeContent: %v", err)
	}
	if string(back) != string(raw) {
		t.Errorf("the round trip changed the bytes: %v → %v", raw, back)
	}
}

func TestEncodingKeepsTextReadable(t *testing.T) {
	content, encoding := encodeContent([]byte("hello\n"))
	if encoding != "utf8" || content != "hello\n" {
		t.Errorf("got %q/%q, want the text as utf8", content, encoding)
	}
}

// A NUL makes a byte string binary even when it is valid UTF-8, because a
// caller that put it in a JSON string would lose it.
func TestEncodingTreatsNULAsBinary(t *testing.T) {
	if _, encoding := encodeContent([]byte("a\x00b")); encoding != "base64" {
		t.Errorf("encoding = %q for content with a NUL, want base64", encoding)
	}
}
