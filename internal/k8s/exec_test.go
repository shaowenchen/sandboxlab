package k8s

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/exec"
)

// stubRunner stands in for the one thing the fake clientset cannot do.
//
// It records what it was asked to run, which is the point: the exec path's
// correctness is mostly about the command it builds and what it does with the
// answer, and both are visible here without a cluster.
type stubRunner struct {
	// calls is every invocation, in order.
	calls []stubCall
	// reply, when set, is written to stdout on each call.
	reply string
	// replyStderr is written to stderr on each call.
	replyStderr string
	// err is returned from each call, after any reply is written.
	err error
}

type stubCall struct {
	ns      string
	pod     string
	command []string
	stdin   []byte
}

func (s *stubRunner) Stream(_ context.Context, ns, pod string, opts StreamOptions) error {
	var stdin []byte
	if opts.Stdin != nil {
		stdin, _ = io.ReadAll(opts.Stdin)
	}
	s.calls = append(s.calls, stubCall{ns: ns, pod: pod, command: opts.Command, stdin: stdin})
	if s.reply != "" && opts.Stdout != nil {
		_, _ = io.WriteString(opts.Stdout, s.reply)
	}
	if s.replyStderr != "" && opts.Stderr != nil {
		_, _ = io.WriteString(opts.Stderr, s.replyStderr)
	}
	return s.err
}

// execClient is a client over the fake clientset with a pod already running in
// a sandbox named demo, and a runner installed.
func execClient(t *testing.T, runner Runner) *Client {
	t.Helper()
	c := newClient().WithRunner(runner)
	ctx := context.Background()
	if _, err := c.Create(ctx, CreateRequest{ID: "demo", Template: testTemplate()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.cs.CoreV1().Pods("sbx-demo").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "sandbox-7d9f8c4b5-x2jql",
			Labels: map[string]string{appLabelKey: sandboxName},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}
	return c
}

// ── the transport's request ─────────────────────────────────────────────────

// TestTheExecRequestIsWhatKubectlWouldSend pins the address and the query.
//
// Everything up to the connection upgrade is an ordinary REST request, so this
// is the half of the exec transport that can be checked without a kubelet —
// the handshake itself is the only part that needs a cluster.
func TestTheExecRequestIsWhatKubectlWouldSend(t *testing.T) {
	got := execURL(execRESTClient(), "sbx-demo", "sandbox-abc-123", StreamOptions{
		Command: []string{"sh", "-c", "echo hi"},
		Stdin:   strings.NewReader("input"),
	})

	if want := "/api/v1/namespaces/sbx-demo/pods/sandbox-abc-123/exec"; got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}

	q := got.Query()
	if q.Get("container") != containerName {
		t.Errorf("container = %q, want %q", q.Get("container"), containerName)
	}
	if got := q["command"]; len(got) != 3 || got[0] != "sh" || got[1] != "-c" || got[2] != "echo hi" {
		t.Errorf("command = %v, want [sh -c \"echo hi\"] as separate values", got)
	}
	// Both output streams are always asked for; one-shot exec never wants a TTY.
	for _, key := range []string{"stdout", "stderr", "stdin"} {
		if q.Get(key) != "true" {
			t.Errorf("%s = %q, want true", key, q.Get(key))
		}
	}
	if q.Get("tty") != "" {
		t.Errorf("tty = %q; a false value is omitted, and anything else means a terminal", q.Get("tty"))
	}
}

// A command with no stdin must not declare one.
//
// The flag is what tells the kubelet whether to attach a stream, and a
// container that reads its input would wait forever on a stdin that never
// closes.
func TestNoStdinMeansNoStdinStream(t *testing.T) {
	got := execURL(execRESTClient(), "sbx-demo", "pod", StreamOptions{Command: []string{"true"}})
	// client-go omits a false boolean rather than sending "false", so the
	// absence of the key is what says there is no stdin stream.
	if v, present := got.Query()["stdin"]; present {
		t.Errorf("stdin = %v with no stdin, want the key absent", v)
	}
}

// TestAnExecWithNoRunnerSaysSo asserts the failure is a sentence, not a nil
// dereference or a request to a cluster that is not there.
func TestAnExecWithNoRunnerSaysSo(t *testing.T) {
	// A sandbox that exists, so the request reaches the transport rather than
	// stopping at "no such sandbox" — which is what a nil runner has to be told
	// apart from.
	c := execClient(t, nil)
	if _, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"true"}}); err == nil {
		t.Fatal("Exec with no runner returned no error")
	} else if !strings.Contains(err.Error(), "exec is unavailable") {
		t.Errorf("error = %v, want it to say exec is unavailable", err)
	}
}

// The runner is only reached for a sandbox that exists.
func TestExecRefusesAnUnknownSandbox(t *testing.T) {
	runner := &stubRunner{}
	c := newClient().WithRunner(runner)
	if _, err := c.Exec(context.Background(), "ghost", ExecInput{Command: []string{"true"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Exec of an unknown sandbox = %v, want ErrNotFound", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("the runner was called %d times for a sandbox that does not exist", len(runner.calls))
	}
}

// ── exec ────────────────────────────────────────────────────────────────────

func TestExecRunsInTheSandboxsPod(t *testing.T) {
	runner := &stubRunner{reply: "hello\n"}
	c := execClient(t, runner)

	out, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("the runner was called %d times, want 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.ns != "sbx-demo" {
		t.Errorf("namespace = %q, want sbx-demo", call.ns)
	}
	if call.pod != "sandbox-7d9f8c4b5-x2jql" {
		t.Errorf("pod = %q, want the pod the label selected", call.pod)
	}
	if strings.Join(call.command, " ") != "echo hello" {
		t.Errorf("command = %v, want the caller's argv unchanged", call.command)
	}
	if string(out.Stdout) != "hello\n" {
		t.Errorf("stdout = %q, want hello", out.Stdout)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", out.ExitCode)
	}
}

// A command that ran and failed is a successful exec.
//
// The status is data. Reporting it as an error would make `grep` finding
// nothing indistinguishable from the API being broken.
func TestANonZeroExitIsNotAnError(t *testing.T) {
	runner := &stubRunner{reply: "partial\n", err: exec.CodeExitError{Err: errors.New("exit status 3"), Code: 3}}
	c := execClient(t, runner)

	out, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"false"}})
	if err != nil {
		t.Fatalf("Exec returned an error for a command that merely failed: %v", err)
	}
	if out.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", out.ExitCode)
	}
	if string(out.Stdout) != "partial\n" {
		t.Errorf("stdout = %q; the output of a failing command is still its answer", out.Stdout)
	}
}

// A transport failure is an error, and says so.
func TestATransportFailureIsAnError(t *testing.T) {
	runner := &stubRunner{err: errors.New("connection refused")}
	c := execClient(t, runner)

	if _, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"true"}}); err == nil {
		t.Fatal("Exec returned no error for a transport failure")
	}
}

func TestAnEmptyCommandIsRejected(t *testing.T) {
	runner := &stubRunner{}
	c := execClient(t, runner)
	if _, err := c.Exec(context.Background(), "demo", ExecInput{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Exec with no command = %v, want ErrInvalid", err)
	}
	if len(runner.calls) != 0 {
		t.Error("the runner was called for an empty command")
	}
}

// Output past the cap is cut off, and the caller is told.
func TestExecOutputIsCapped(t *testing.T) {
	runner := &stubRunner{reply: strings.Repeat("x", 5000)}
	c := execClient(t, runner)

	out, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"yes"}, MaxOutput: 100})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(out.Stdout) != 100 {
		t.Errorf("stdout is %d bytes, want it capped at 100", len(out.Stdout))
	}
	if !out.StdoutTruncated {
		t.Error("stdout was cut off but Truncated is false")
	}
	if out.ExitCode != 0 {
		t.Errorf("exit code = %d; a command that printed too much still ran", out.ExitCode)
	}
}

// A working directory goes through a shell, and the caller's argv survives.
func TestAWorkingDirectoryKeepsTheArgv(t *testing.T) {
	runner := &stubRunner{}
	c := execClient(t, runner)

	if _, err := c.Exec(context.Background(), "demo", ExecInput{
		Command: []string{"ls", "-la", "/tmp"},
		Cwd:     "/workspace",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := runner.calls[0].command
	if len(got) != 8 {
		t.Fatalf("command = %v, want a shell wrapper around three arguments", got)
	}
	if got[0] != "sh" || got[1] != "-c" || got[4] != "/workspace" {
		t.Errorf("command = %v, want sh -c <script> sh /workspace ...", got)
	}
	// The arguments are arguments: they are not spliced into the script text,
	// which is what keeps a value with a space or a `;` in it intact.
	if strings.Join(got[5:], " ") != "ls -la /tmp" {
		t.Errorf("the arguments after the script = %v, want them passed through", got[5:])
	}
	if strings.Contains(got[2], "/tmp") {
		t.Errorf("the script text contains the caller's arguments: %q", got[2])
	}
}

// A cd that failed is the caller's path, not a command that ran.
func TestAnUnreachableWorkingDirectoryIsRejected(t *testing.T) {
	runner := &stubRunner{err: exec.CodeExitError{Err: errors.New("exit status 125"), Code: 125}}
	c := execClient(t, runner)

	_, err := c.Exec(context.Background(), "demo", ExecInput{Command: []string{"ls"}, Cwd: "/nope"})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("Exec with a missing cwd = %v, want ErrInvalid", err)
	}
}

// ── paths ───────────────────────────────────────────────────────────────────

// The path is validated before anything is run, and the rule is the interface's
// rather than a security boundary — see the note on ReadFile.
func TestPathValidation(t *testing.T) {
	c := execClient(t, &stubRunner{})
	ctx := context.Background()

	bad := map[string]string{
		"empty":    "",
		"relative": "etc/hostname",
		"dot":      "./x",
		"nul":      "/tmp/a\x00b",
		"too long": "/" + strings.Repeat("x", MaxPathLength),
	}
	for name, path := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ReadFile(ctx, "demo", path, 0); !errors.Is(err, ErrInvalid) {
				t.Errorf("ReadFile(%q) = %v, want ErrInvalid", path, err)
			}
			if _, err := c.WriteFile(ctx, "demo", path, []byte("x"), false); !errors.Is(err, ErrInvalid) {
				t.Errorf("WriteFile(%q) = %v, want ErrInvalid", path, err)
			}
		})
	}
}

// The script never contains the path: it is an argument.
//
// This is the property the whole shape exists for. A path with a `;` or a
// `$(...)` in it is a path, not a command, and if it ever reached the script
// text this test is what notices.
func TestThePathIsAnArgumentAndNeverPartOfTheScript(t *testing.T) {
	// A path that would run `id` if it were ever interpolated.
	const evil = "/tmp/$(id);rm -rf /`whoami`"

	t.Run("read", func(t *testing.T) {
		runner := &stubRunner{}
		c := execClient(t, runner)
		_, _ = c.ReadFile(context.Background(), "demo", evil, 0)

		command := runner.calls[0].command
		if !strings.Contains(command[2], "$1") {
			t.Errorf("the read script does not take the path as $1: %q", command[2])
		}
		if strings.Contains(command[2], "whoami") || strings.Contains(command[2], "rm -rf") {
			t.Errorf("the path was interpolated into the read script: %q", command[2])
		}
		if command[4] != evil {
			t.Errorf("argv = %q, want the path passed through verbatim", command[4])
		}
	})

	t.Run("write", func(t *testing.T) {
		runner := &stubRunner{}
		c := execClient(t, runner)
		_, _ = c.WriteFile(context.Background(), "demo", evil, []byte("x"), false)

		command := runner.calls[0].command
		if strings.Contains(command[2], "whoami") || strings.Contains(command[2], "rm -rf") {
			t.Errorf("the path was interpolated into the write script: %q", command[2])
		}
		if command[4] != evil {
			t.Errorf("argv = %q, want the path passed through verbatim", command[4])
		}
	})
}

// ── files ───────────────────────────────────────────────────────────────────

func TestReadFileReturnsText(t *testing.T) {
	c := execClient(t, &stubRunner{reply: "hello\n"})

	out, err := c.ReadFile(context.Background(), "demo", "/workspace/a.txt", 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if out.Content != "hello\n" || out.Encoding != "utf8" {
		t.Errorf("ReadFile = %q/%q, want the text as utf8", out.Content, out.Encoding)
	}
	if out.Size != 6 {
		t.Errorf("size = %d, want 6", out.Size)
	}
}

func TestReadFileReturnsBase64ForBinary(t *testing.T) {
	// Invalid UTF-8, which is what a PNG or a compiled object looks like.
	c := execClient(t, &stubRunner{reply: "\x89PNG\r\n\x1a\n\x00\xff"})

	out, err := c.ReadFile(context.Background(), "demo", "/workspace/a.png", 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if out.Encoding != "base64" {
		t.Errorf("encoding = %q for binary content, want base64", out.Encoding)
	}
	if out.Content == "" {
		t.Error("the content is empty")
	}
}

// The read script refuses a directory and says which of the two 404s this is.
func TestReadFileExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		wantIs  error
		wantMsg string
	}{
		{"a directory", exitDirectory, ErrInvalid, "is a directory"},
		{"not there", exitNotFound, ErrNoSuchFile, "no file at"},
		{"not readable", exitUnreadable, ErrInvalid, "not readable"},
		{"too large", exitTooLarge, ErrTooLarge, "over the"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &stubRunner{
				replyStderr: "1234567",
				err:         exec.CodeExitError{Err: errors.New("exit status"), Code: tc.code},
			}
			c := execClient(t, runner)
			_, err := c.ReadFile(context.Background(), "demo", "/workspace/a.txt", 100)
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("error = %v, want %v", err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// A missing file must not borrow the sandbox's wording: both are 404s, and a
// message about a sandbox named "/workspace/a.txt" would be nonsense.
func TestAMissingFileNamesTheFileNotTheSandbox(t *testing.T) {
	runner := &stubRunner{err: exec.CodeExitError{Err: errors.New("exit status 3"), Code: exitNotFound}}
	c := execClient(t, runner)

	_, err := c.ReadFile(context.Background(), "demo", "/workspace/gone.txt", 0)
	if err == nil {
		t.Fatal("ReadFile of a missing file returned no error")
	}
	if !strings.Contains(err.Error(), "/workspace/gone.txt") {
		t.Errorf("error = %q, want it to name the file", err)
	}
	if strings.Contains(err.Error(), "no sandbox") {
		t.Errorf("error = %q, which reads as a missing sandbox", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a missing file matched the sandbox sentinel; the two 404s must stay distinguishable")
	}
}

func TestWriteFileSendsTheBytes(t *testing.T) {
	runner := &stubRunner{}
	c := execClient(t, runner)

	if _, err := c.WriteFile(context.Background(), "demo", "/workspace/a.txt", []byte("hello\n"), false); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	call := runner.calls[0]
	if string(call.stdin) != "hello\n" {
		t.Errorf("stdin = %q, want the file's bytes", call.stdin)
	}
	if call.command[4] != "/workspace/a.txt" {
		t.Errorf("argv = %v, want the path", call.command)
	}
	// Parents are not created unless asked.
	if call.command[5] != "false" {
		t.Errorf("createParents = %q, want false", call.command[5])
	}
}

func TestWriteFileCanCreateParents(t *testing.T) {
	runner := &stubRunner{}
	c := execClient(t, runner)

	if _, err := c.WriteFile(context.Background(), "demo", "/workspace/a/b/c.txt", []byte("x"), true); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := runner.calls[0].command[5]; got != "true" {
		t.Errorf("createParents = %q, want true", got)
	}
}

func TestWriteFileExitCodes(t *testing.T) {
	tests := []struct {
		name string
		code int
		want string
	}{
		{"no parent", exitNoParent, "does not exist"},
		{"not writable", exitUnwritable, "not writable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &stubRunner{err: exec.CodeExitError{Err: errors.New("exit status"), Code: tc.code}}
			c := execClient(t, runner)
			_, err := c.WriteFile(context.Background(), "demo", "/workspace/a.txt", []byte("x"), false)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWriteFileRefusesAnOversizeBody(t *testing.T) {
	c := execClient(t, &stubRunner{})
	_, err := c.WriteFile(context.Background(), "demo", "/workspace/big", make([]byte, MaxFileBytes+1), false)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("WriteFile of an oversize body = %v, want ErrTooLarge", err)
	}
}

// ── the transport, against an http server ───────────────────────────────────

// TestTheRemoteRunnerReachesTheExecSubresource drives remoteRunner against a
// real HTTP server. The upgrade is refused — that needs a kubelet — but the
// request the transport builds is exercised end to end rather than as a URL in
// isolation.
func TestTheRemoteRunnerReachesTheExecSubresource(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		// No upgrade: the client is expected to report the failure, which is
		// what a non-exec-capable endpoint does.
		http.Error(w, "no upgrade here", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// A real clientset over the test server, because the fake's REST client is
	// nil and the request is what this is checking.
	rc := &rest.Config{Host: srv.URL}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatalf("building a clientset: %v", err)
	}
	runner := &remoteRunner{cs: cs, rc: rc}
	streamErr := runner.Stream(context.Background(), "sbx-demo", "sandbox-abc", StreamOptions{
		Command: []string{"echo", "hi"},
		Stdout:  io.Discard,
	})
	if streamErr == nil {
		t.Fatal("Stream succeeded against a server that refused the upgrade")
	}

	if want := "/api/v1/namespaces/sbx-demo/pods/sandbox-abc/exec"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	values, perr := url.ParseQuery(gotQuery)
	if perr != nil {
		t.Fatalf("the query string did not parse: %v", perr)
	}
	if values.Get("container") != containerName {
		t.Errorf("container = %q, want %q", values.Get("container"), containerName)
	}
	if got := values["command"]; len(got) != 2 || got[0] != "echo" || got[1] != "hi" {
		t.Errorf("command = %v, want [echo hi]", got)
	}
}

// execRESTClient is a real REST client, for the tests that build a request.
//
// The fake clientset's is nil — it implements the typed surface and nothing
// underneath it — so a request cannot be built from one. The host is never
// contacted here: the tests that use this inspect the URL, and the one that
// does call out has its own server.
func execRESTClient() rest.Interface {
	rc, _ := rest.RESTClientFor(&rest.Config{
		Host:    "https://cluster.invalid",
		APIPath: "/api",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &corev1.SchemeGroupVersion,
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		},
	})
	return rc
}
