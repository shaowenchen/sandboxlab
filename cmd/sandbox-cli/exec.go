package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/sandboxlab/internal/client"
)

// execCmd runs a command inside a sandbox.
//
// It is the way into a sandbox whose template serves no port: `python` and
// `node` are a workspace with no URL, and this is what reaches them.
func execCmd() *cobra.Command {
	var (
		cwd      string
		timeout  string
		useStdin bool
	)
	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command in a sandbox and print what it produced",
		Long: "exec runs one command in a sandbox and waits for it.\n\n" +
			"The command's exit status becomes this command's, so a script can test\n" +
			"it. `--` is not required, but it is what keeps a flag meant for the\n" +
			"command from being read as one of sandbox's own.",
		Example: "  sandbox exec demo -- python3 -c 'print(1)'\n" +
			"  sandbox exec demo -- ls -la /workspace\n" +
			"  echo 'print(1)' | sandbox exec --stdin demo -- python3 -",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			// `--` is a separator for whoever is reading the command line, not
			// an argument. It survives into args here, so it is dropped: the
			// command is what comes after it.
			command := args[1:]
			if len(command) > 0 && command[0] == "--" {
				command = command[1:]
			}
			if len(command) == 0 {
				return errors.New("no command to run")
			}
			in := client.ExecInput{Command: command, Cwd: cwd, Timeout: timeout}
			if useStdin {
				raw, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				in.Stdin = string(raw)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), execTimeout)
			defer cancel()

			out, err := c.Exec(ctx, args[0], in)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, out)
			}

			// The streams go to the streams they belong to, so a pipeline can
			// take the command's output without this command's noise in it.
			writeDecoded(cmd.OutOrStdout(), out.Stdout, out.StdoutEncoding)
			writeDecoded(cmd.ErrOrStderr(), out.Stderr, out.StderrEncoding)

			if out.ExitCode != 0 {
				// The status is the command's, and the shell should see it as
				// its own — that is what makes `sandbox exec x -- test -f y` a
				// condition rather than something to parse the output of.
				return exitCodeError{code: out.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cwd, "cwd", "", "the working directory; the image's own by default")
	cmd.Flags().StringVar(&timeout, "timeout", "", "how long the command may take, e.g. 30s")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "feed this command's stdin to the sandbox command")
	// Flags belong before the first positional argument. Without this, `sandbox
	// exec demo ls -la` would have cobra read -la as an unknown flag for exec
	// rather than as an argument to ls.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// execTimeout bounds a command when the caller names no time of its own. It is
// longer than requestTimeout because running a build is not a lookup.
const execTimeout = 10 * time.Minute

// cpCmd copies a file into or out of a sandbox.
func cpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp <name>:<path> <local> | <local> <name>:<path>",
		Short: "Copy a file into or out of a sandbox",
		Long: "cp moves one file across the boundary, in whichever direction the\n" +
			"side with a sandbox name on it says. Other commands can use the\n" +
			"result: `sandbox cp demo:/etc/hostname -` writes to stdout.",
		Example: "  sandbox cp demo:/workspace/report.csv .\n" +
			"  sandbox cp ./setup.sh demo:/workspace/setup.sh\n" +
			"  sandbox cp demo:/etc/hostname -",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			from, to := args[0], args[1]
			fromID, fromPath, fromIsSandbox := splitSandboxPath(from)
			toID, toPath, toIsSandbox := splitSandboxPath(to)

			switch {
			case fromIsSandbox && toIsSandbox:
				return errors.New("both sides name a sandbox; one of them has to be a local path")
			case fromIsSandbox:
				return copyOut(cmd, c, fromID, fromPath, to)
			case toIsSandbox:
				return copyIn(cmd, c, toID, toPath, from)
			}
			return fmt.Errorf("%q and %q are both local paths; one side has to be <name>:<path>", from, to)
		},
	}
	return cmd
}

// splitSandboxPath reads "<name>:<path>" apart.
//
// The name is everything before the first colon and the path is everything
// after, which is why a local path is recognised by not having the shape rather
// than by having it — a Windows drive letter would look the same otherwise, and
// this CLI is not the place to guess.
func splitSandboxPath(s string) (id, path string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", "", false
	}
	id, path = s[:i], s[i+1:]
	if id == "" || path == "" {
		return "", "", false
	}
	return id, path, true
}

func copyOut(cmd *cobra.Command, c *client.Client, id, path, dest string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
	defer cancel()

	file, err := c.ReadFile(ctx, id, path)
	if err != nil {
		return err
	}
	data, err := decodeFile(file.Content, file.Encoding)
	if err != nil {
		return err
	}

	if dest == "-" {
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s:%s → %s (%d bytes)\n", id, path, dest, len(data))
	return nil
}

func copyIn(cmd *cobra.Command, c *client.Client, id, path, src string) error {
	var data []byte
	var err error
	if src == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
	defer cancel()

	// Text goes as it is, so the wire body is readable; anything else is
	// base64, because a JSON string cannot carry arbitrary bytes.
	content, encoding := string(data), "utf8"
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		content, encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}

	if err := c.WriteFile(ctx, id, path, content, encoding, false); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s → %s:%s (%d bytes)\n", src, id, path, len(data))
	return nil
}

func decodeFile(content, encoding string) ([]byte, error) {
	if encoding != "base64" {
		return []byte(content), nil
	}
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("the server sent content its encoding says is base64, and it is not: %w", err)
	}
	return data, nil
}

// writeDecoded writes a stream the way the server said it was spelled.
func writeDecoded(w io.Writer, content, encoding string) {
	data, err := decodeFile(content, encoding)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return
	}
	_, _ = w.Write(data)
}
