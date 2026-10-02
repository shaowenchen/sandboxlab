package k8s

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Running a command, and reaching a file with one.
//
// Kubernetes has no file API, so both directions of file access are a small
// command run through the same executor as Exec: `cat` to read, `cat > "$1"` to
// write. The path is passed as a positional argument and never interpolated
// into the script text — that is the whole reason for the shape, and it is what
// makes a path containing `;`, `$(...)` or a newline ordinary data rather than
// code.

// ErrNoSuchFile is returned for a path that is not there.
//
// It is separate from ErrNotFound, which means "no such sandbox". Both are 404s,
// but the message has to name the right missing thing or a caller cannot tell
// which of the two they hit.
var ErrNoSuchFile = errors.New("no such file")

// ErrInvalid is returned for a request that cannot be satisfied whatever the
// cluster says — a relative path, an empty command.
var ErrInvalid = errors.New("invalid request")

// ErrTooLarge is returned when a file is over the size the API will carry.
var ErrTooLarge = errors.New("too large")

// Caps. A command that prints without end must not grow the control plane's
// heap, and neither must a file read.
const (
	// MaxCommandOutput caps stdout and stderr each.
	MaxCommandOutput = 1 << 20
	// MaxFileBytes caps a file read, and the write body.
	MaxFileBytes = 2 << 20
	// MaxPathLength bounds a path a caller may name.
	MaxPathLength = 4096
)

// Exit codes used by the file scripts below.
//
// They are a contract between this file and the shell text in it, which is why
// they are named rather than written as literals. The alternative — reading the
// message out of stderr — does not work: `cat: /x: No such file or directory`
// is locale-dependent and is not a stable interface.
const (
	exitDirectory  = 2
	exitNotFound   = 3
	exitUnreadable = 4
	exitNoParent   = 5
	exitUnwritable = 6
	exitTooLarge   = 7
	// exitNoCwd is `cd` failing, which is the caller's path rather than a fault.
	exitNoCwd = 125
)

// ExecInput is one command to run.
type ExecInput struct {
	Command []string
	// Stdin is fed to the command. Empty means no stdin stream at all, which is
	// not the same as an empty one — see StreamOptions.Stdin.
	Stdin []byte
	// Cwd, when set, is the working directory.
	Cwd string
	// MaxOutput caps stdout and stderr each. Zero means MaxCommandOutput.
	MaxOutput int64
}

// ExecResult is what a command produced.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Truncated reports that the output cap was reached, so a caller can tell
	// an empty output from a cut-off one.
	StdoutTruncated bool
	StderrTruncated bool
}

// Exec runs one command in a sandbox and waits for it.
//
// A non-zero exit is not an error: the command ran, and its status comes back in
// the result. Only a command that could not be run at all — no pod, no
// permission, the connection dropped — is returned as an error. Reporting a
// failing command as a failure would make `grep` finding nothing look like the
// API being broken.
func (c *Client) Exec(ctx context.Context, id string, in ExecInput) (ExecResult, error) {
	if len(in.Command) == 0 {
		return ExecResult{}, fmt.Errorf("%w: no command to run", ErrInvalid)
	}

	ns := c.cfg.SandboxNamespace(id)
	if _, err := c.Get(ctx, id); err != nil {
		return ExecResult{}, err
	}
	pod, err := c.sandboxPod(ctx, ns)
	if err != nil {
		return ExecResult{}, err
	}
	runner, err := c.execRunner()
	if err != nil {
		return ExecResult{}, err
	}

	command := in.Command
	// A working directory has to go through a shell, because exec has nowhere
	// to put one. `cd -- "$1" || exit; shift; exec "$@"` keeps the caller's
	// argv intact — its arguments are still arguments, not text spliced into a
	// command line — and `exec` replaces the shell so the exit status and the
	// signals are the command's own.
	if in.Cwd != "" {
		command = append([]string{"sh", "-c", `cd -- "$1" || exit 125; shift; exec "$@"`, "sh", in.Cwd}, in.Command...)
	}

	limit := in.MaxOutput
	if limit <= 0 {
		limit = MaxCommandOutput
	}
	stdout := &capWriter{remaining: limit}
	stderr := &capWriter{remaining: limit}
	opts := StreamOptions{Command: command, Stdout: stdout, Stderr: stderr}
	if len(in.Stdin) > 0 {
		opts.Stdin = bytes.NewReader(in.Stdin)
	}

	streamErr := runner.Stream(ctx, ns, pod, opts)

	result := ExecResult{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
	if streamErr == nil {
		return result, nil
	}
	if code, ok := exitCode(streamErr); ok {
		// A `cd` that failed is the caller's path, not a command that ran and
		// failed, so it is reported rather than returned as an exit status.
		if in.Cwd != "" && code == exitNoCwd {
			return result, fmt.Errorf("%w: the working directory %q is not there", ErrInvalid, in.Cwd)
		}
		result.ExitCode = code
		return result, nil
	}
	return result, fmt.Errorf("running a command in %s: %w", id, streamErr)
}

// capWriter collects output up to a limit and never fails.
//
// It deliberately does not return an error past the cap. The stream copy does
// not reliably propagate a writer's error — its cleanup path can swallow it —
// so a writer that errored would be an unreliable way to stop, and the command
// would be killed rather than allowed to finish. Discarding the excess and
// recording that it happened keeps the command's exit status, which the caller
// needs more than the bytes it did not ask for.
type capWriter struct {
	buf       bytes.Buffer
	remaining int64
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.buf.Write(p[:w.remaining])
		w.remaining = 0
		w.truncated = true
		// Claim the whole write: an error here would abort the command, and the
		// point is to let it finish.
		return len(p), nil
	}
	w.remaining -= int64(len(p))
	return w.buf.Write(p)
}

func (w *capWriter) Bytes() []byte { return w.buf.Bytes() }

// FileContent is a file's bytes and how they are spelled.
type FileContent struct {
	// Content is the file's bytes: UTF-8 verbatim when the file is valid UTF-8
	// text, base64 otherwise. Encoding says which, so a caller decides from a
	// field rather than by sniffing.
	Content string
	// Encoding is "utf8" or "base64".
	Encoding string
	// Size is the length of the file's bytes, before any encoding.
	Size int64
}

// ReadFile reads a file out of a sandbox.
//
// The path must be absolute. That is a rule about the interface rather than
// about safety: a relative path would depend on a working directory the caller
// cannot see, and would fail as a confusing 404 rather than as a rejected
// request. A caller holding the key can already run any command, so nothing
// here pretends to be a security boundary — the container's own user, the
// RBAC grant and the key are what bound this.
func (c *Client) ReadFile(ctx context.Context, id, path string, maxBytes int64) (FileContent, error) {
	if err := validatePath(path); err != nil {
		return FileContent{}, err
	}
	if maxBytes <= 0 {
		maxBytes = MaxFileBytes
	}

	ns := c.cfg.SandboxNamespace(id)
	if _, err := c.Get(ctx, id); err != nil {
		return FileContent{}, err
	}
	pod, err := c.sandboxPod(ctx, ns)
	if err != nil {
		return FileContent{}, err
	}
	runner, err := c.execRunner()
	if err != nil {
		return FileContent{}, err
	}

	// The probe and the read are one command, so there is no window between
	// them in which the file could become something else.
	//
	// `wc -c` refuses the read outright rather than letting a huge file stream
	// into this process and be cut off, which would report a size that is the
	// cap rather than the file's.
	script := `[ -d "$1" ] && exit 2
[ -e "$1" ] || exit 3
[ -r "$1" ] || exit 4
size=$(wc -c < "$1" | tr -d ' ')
[ "$size" -le "$2" ] || { printf '%s' "$size" >&2; exit 7; }
cat -- "$1"`

	stdout := &capWriter{remaining: maxBytes + 1}
	stderr := &capWriter{remaining: MaxCommandOutput}
	streamErr := runner.Stream(ctx, ns, pod, StreamOptions{
		Command: []string{"sh", "-c", script, "sh", path, strconv.FormatInt(maxBytes, 10)},
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if code, ok := exitCode(streamErr); ok {
		switch code {
		case exitDirectory:
			return FileContent{}, fmt.Errorf("%w: %q is a directory", ErrInvalid, path)
		case exitNotFound:
			return FileContent{}, fmt.Errorf("%w: no file at %q in sandbox %q", ErrNoSuchFile, path, id)
		case exitUnreadable:
			return FileContent{}, fmt.Errorf("%w: %q is not readable", ErrInvalid, path)
		case exitTooLarge:
			size, _ := strconv.ParseInt(strings.TrimSpace(string(stderr.Bytes())), 10, 64)
			return FileContent{}, fmt.Errorf("%w: %q is %d bytes, over the %d byte limit", ErrTooLarge, path, size, maxBytes)
		}
		return FileContent{}, fmt.Errorf("reading %q: %s", path, strings.TrimSpace(string(stderr.Bytes())))
	}
	if streamErr != nil {
		return FileContent{}, fmt.Errorf("reading %q: %w", path, streamErr)
	}

	raw := stdout.Bytes()
	out := FileContent{Size: int64(len(raw)), Encoding: "utf8"}
	if utf8.Valid(raw) && !bytes.ContainsRune(raw, 0) {
		out.Content = string(raw)
	} else {
		out.Encoding = "base64"
		out.Content = base64.StdEncoding.EncodeToString(raw)
	}
	return out, nil
}

// FileInfo is what a write reports back.
type FileInfo struct {
	Size int64
}

// WriteFile writes bytes to a file in a sandbox.
//
// It creates the file if it is absent and replaces it if it is: there is no
// separate create, because what the caller is expressing is the contents at the
// path, not the act of making it.
//
// Parent directories are not created unless asked. A silent mkdir -p turns a
// mistyped directory into a file nobody will find; refusing names the directory
// that is missing, and the caller is one field away from saying it meant it.
func (c *Client) WriteFile(ctx context.Context, id, path string, data []byte, createParents bool) (FileInfo, error) {
	if err := validatePath(path); err != nil {
		return FileInfo{}, err
	}
	if int64(len(data)) > MaxFileBytes {
		return FileInfo{}, fmt.Errorf("%w: %d bytes is over the %d byte limit", ErrTooLarge, len(data), MaxFileBytes)
	}

	ns := c.cfg.SandboxNamespace(id)
	if _, err := c.Get(ctx, id); err != nil {
		return FileInfo{}, err
	}
	pod, err := c.sandboxPod(ctx, ns)
	if err != nil {
		return FileInfo{}, err
	}
	runner, err := c.execRunner()
	if err != nil {
		return FileInfo{}, err
	}

	script := `target="$1"
if [ "$3" = "true" ]; then mkdir -p -- "$(dirname -- "$target")" || exit 5; fi
[ -d "$(dirname -- "$target")" ] || exit 5
[ -w "$(dirname -- "$target")" ] || [ -w "$target" ] || exit 6
cat > "$target"`

	stderr := &capWriter{remaining: MaxCommandOutput}
	streamErr := runner.Stream(ctx, ns, pod, StreamOptions{
		Command: []string{"sh", "-c", script, "sh", path, strconv.FormatBool(createParents)},
		// The payload is raw bytes on stdin: the stream is 8-bit clean, and
		// `cat` truncates the file to exactly what it reads. It is not base64,
		// because encoding here would buy nothing the transport does not
		// already give and would put a decoder between the caller and the file.
		Stdin:  bytes.NewReader(data),
		Stdout: &capWriter{remaining: MaxCommandOutput},
		Stderr: stderr,
	})
	if code, ok := exitCode(streamErr); ok {
		switch code {
		case exitNoParent:
			return FileInfo{}, fmt.Errorf("%w: the directory for %q does not exist", ErrInvalid, path)
		case exitUnwritable:
			return FileInfo{}, fmt.Errorf("%w: %q is not writable", ErrInvalid, path)
		}
		return FileInfo{}, fmt.Errorf("writing %q: %s", path, strings.TrimSpace(string(stderr.Bytes())))
	}
	if streamErr != nil {
		return FileInfo{}, fmt.Errorf("writing %q: %w", path, streamErr)
	}
	return FileInfo{Size: int64(len(data))}, nil
}

// validatePath rejects a path this API will not carry.
//
// It is input validation, not a security boundary — see ReadFile. A NUL is
// rejected because it truncates inside the container's syscall layer, so the
// file reached would not be the one the string names.
func validatePath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("%w: a path is required", ErrInvalid)
	case !strings.HasPrefix(path, "/"):
		return fmt.Errorf("%w: %q must be an absolute path", ErrInvalid, path)
	case strings.ContainsRune(path, 0):
		return fmt.Errorf("%w: a path cannot contain a NUL byte", ErrInvalid)
	case len(path) > MaxPathLength:
		return fmt.Errorf("%w: the path is longer than %d bytes", ErrInvalid, MaxPathLength)
	}
	return nil
}
