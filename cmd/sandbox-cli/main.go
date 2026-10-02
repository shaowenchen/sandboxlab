// Command sandbox-cli drives a sandbox environment from a terminal.
//
// The commands are shaped around what someone actually does, not around the
// API:
//
//	sandbox create      start a sandbox from a template
//	sandbox list        what is running, and where
//	sandbox url         the address to open a sandbox at
//	sandbox logs        what a sandbox has printed
//	sandbox rm          stop one
//
// `create` and `url` are the pair that matters. Everything else exists so a
// person can answer a question without opening a browser, and each command is a
// thin layer over internal/client — the same code the console's fetch calls
// reach the same endpoints — so the CLI cannot drift from the API.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/sandboxlab/internal/buildinfo"
	"github.com/shaowenchen/sandboxlab/internal/client"
)

func main() {
	// A signal-cancelled context is what lets a Ctrl-C stop a followed log or
	// an in-flight request cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:           "sandbox",
		Short:         "Manage sandboxes in a sandboxlab environment",
		Long:          "sandbox talks to a sandboxlab control plane: it creates sandboxes,\nlists them, and tells you where to reach them.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("url", "", "the control plane's address; defaults to $SANDBOX_URL")
	root.PersistentFlags().String("key", "", "the API key; defaults to $SANDBOX_KEY")
	root.PersistentFlags().Bool("json", false, "print the server's JSON instead of a table")

	root.AddCommand(
		catalogCmd(),
		createCmd(),
		listCmd(),
		getCmd(),
		urlCmd(),
		logsCmd(),
		execCmd(),
		cpCmd(),
		renewCmd(),
		rmCmd(),
		envCmd(),
		describeCmd(),
	)

	if err := root.ExecuteContext(ctx); err != nil {
		// `exec` ran a command and is reporting its status, which is not this
		// program's failure and needs no message of its own.
		var ec exitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		// A command that has already explained itself — `rm` reporting which
		// names it could not delete, one line each — returns this to set a
		// non-zero status without a second, empty message under its own.
		if !errors.Is(err, errExitQuiet) {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
		}
		os.Exit(1)
	}
}

// Environment variables the CLI reads, so a shell that has been set up once
// needs no flags.
const (
	urlEnv = "SANDBOX_URL"
	keyEnv = "SANDBOX_KEY"
)

// newClient builds a client from the flags and the environment.
func newClient(cmd *cobra.Command) (*client.Client, error) {
	baseURL, _ := cmd.Flags().GetString("url")
	if baseURL == "" {
		baseURL = os.Getenv(urlEnv)
	}
	if baseURL == "" {
		return nil, fmt.Errorf("no control plane address: pass --url or set %s (it looks like https://<host>/sandbox)", urlEnv)
	}
	key, _ := cmd.Flags().GetString("key")
	if key == "" {
		key = os.Getenv(keyEnv)
	}
	return client.New(baseURL, key), nil
}

// jsonOut reports whether the caller asked for raw JSON.
func jsonOut(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	return v
}

// isNotFound reports whether an error is the API's 404, so a command can turn
// it into its own message rather than echoing a status.
func isNotFound(err error) bool { return client.IsNotFound(err) }

// exitCodeError carries a child process's exit status up to main, so `sandbox
// exec` becomes the command it ran rather than always reporting 1.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// errExitQuiet is returned when a command has already printed what went wrong,
// line by line, and only needs to set a non-zero status.
var errExitQuiet = errors.New("")
