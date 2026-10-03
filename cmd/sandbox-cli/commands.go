package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/sandboxlab/internal/client"
	"github.com/shaowenchen/sandboxlab/internal/model"
)

// requestTimeout bounds a call that should answer promptly. Logs and anything
// streaming get their own handling; everything here is a single request.
const requestTimeout = 30 * time.Second

func catalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "List the templates a sandbox can be created from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			templates, err := c.Catalog(ctx)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, templates)
			}
			out := cmd.OutOrStdout()
			rows := [][]string{{"template", "image", "ttl", "ports"}}
			for _, t := range templates {
				rows = append(rows, []string{
					t.ID,
					t.Image,
					ttlSummary(t),
					portSummary(t.Ports),
				})
			}
			printTable(out, rows)
			return nil
		},
	}
	// Bare `catalog` still lists, which is what the README, the CI check and
	// anyone's fingers expect; the subcommands are for changing what is listed.
	cmd.AddCommand(catalogAddCmd(), catalogRmCmd())
	return cmd
}

// catalogAddCmd adds a template to a running control plane from a file or stdin.
//
// It comes from a file because that is where a template lives — the same YAML
// the built-ins are written in — and adding one is otherwise the same act as
// adding a seed file, minus the release.
func catalogAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <file|->",
		Short: "Add or replace a template on a running control plane",
		Long: "Add a template from a YAML file, or from stdin with '-'. Adding an id\n" +
			"that already exists replaces it, so this is also how a template is\n" +
			"edited.\n\n" +
			"Nothing is persisted: the catalog returns to the templates compiled into\n" +
			"the control plane when it restarts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}

			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			tmpl, err := c.AddTemplate(ctx, string(data))
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, tmpl)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s (%s)\n", tmpl.ID, tmpl.Image)
			return nil
		},
	}
}

// catalogRmCmd removes templates from a running control plane.
//
// No confirmation, unlike deleting a sandbox: removing a template touches no
// running sandbox and is undone by adding it again.
func catalogRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id> [id...]",
		Aliases: []string{"remove"},
		Short:   "Remove templates from a running control plane",
		Long: "Remove one or more templates. Sandboxes already created from them are\n" +
			"untouched. Nothing is persisted: a template compiled into the control\n" +
			"plane comes back when it restarts.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			var failed int
			for _, id := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
				err := c.DeleteTemplate(ctx, id)
				cancel()
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not remove %s: %v\n", id, err)
					failed++
					continue
				}
				fmt.Fprintf(out, "removed %s\n", id)
			}
			if failed > 0 {
				return errExitQuiet
			}
			return nil
		},
	}
}

func createCmd() *cobra.Command {
	var (
		template string
		name     string
		ttl      string
		envPairs []string
		wait     bool
		waitFor  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a sandbox",
		Long: "Create a sandbox from a template and print its address.\n\n" +
			"With --wait, the command blocks until the sandbox is running rather than\n" +
			"as soon as it is created, which is what a script that is about to use it\n" +
			"wants.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			env, err := parseEnv(envPairs)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			sb, err := c.Create(ctx, client.CreateInput{
				Template: template,
				Name:     name,
				TTL:      ttl,
				Env:      env,
			})
			if err != nil {
				return err
			}

			if wait {
				if err := waitForRunning(cmd, c, sb.ID, waitFor); err != nil {
					return err
				}
				// Re-read: the wait watches the sandbox, and the endpoints in
				// the create response predate the container being up.
				if refreshed, err := c.Get(cmd.Context(), sb.ID); err == nil {
					sb = refreshed
				}
			}

			if jsonOut(cmd) {
				return printJSON(cmd, sb)
			}
			printSandboxCard(cmd, c, sb)
			return nil
		},
	}
	cmd.Flags().StringVarP(&template, "template", "t", "", "the template to create from (required)")
	cmd.Flags().StringVar(&name, "name", "", "the sandbox's name; generated when empty")
	cmd.Flags().StringVar(&ttl, "ttl", "", "how long it may live, e.g. 90m or 2h; the template's default when empty")
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "an environment variable, KEY=VALUE; may be repeated")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the sandbox is running")
	cmd.Flags().DurationVar(&waitFor, "wait-timeout", 2*time.Minute, "how long --wait may wait")
	_ = cmd.MarkFlagRequired("template")
	return cmd
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the sandboxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			boxes, err := c.List(ctx)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, boxes)
			}

			out := cmd.OutOrStdout()
			if len(boxes) == 0 {
				fmt.Fprintln(out, "no sandboxes. Create one with `sandbox create -t <template>`.")
				return nil
			}
			now := time.Now()
			rows := [][]string{{"name", "template", "state", "lifetime", "address"}}
			for _, sb := range boxes {
				rows = append(rows, []string{
					sb.ID,
					sb.Template,
					string(sb.State),
					ttlRemaining(sb, now),
					firstURL(sb),
				})
			}
			printTable(out, rows)
			return nil
		},
	}
}

func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show one sandbox in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			sb, err := c.Get(ctx, args[0])
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, sb)
			}
			printSandboxCard(cmd, c, sb)
			return nil
		},
	}
}

// urlCmd prints just the address, which is what a script wants: no table, no
// labels, one line that can be assigned.
func urlCmd() *cobra.Command {
	var port string
	cmd := &cobra.Command{
		Use:   "url <name>",
		Short: "Print the address to open a sandbox at",
		Long: "Print a sandbox's address, one line and nothing else, so a shell can\n" +
			"capture it:\n\n" +
			"  open \"$(sandbox url myshop --port vnc)\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			sb, err := c.Get(ctx, args[0])
			if err != nil {
				return err
			}
			if len(sb.Endpoints) == 0 {
				return fmt.Errorf("the sandbox %q serves no port, so it has no address; use `sandbox logs` or the API to reach it", sb.ID)
			}
			if port == "" {
				fmt.Fprintln(cmd.OutOrStdout(), sb.Endpoints[0].URL)
				return nil
			}
			for _, ep := range sb.Endpoints {
				if ep.Name == port {
					fmt.Fprintln(cmd.OutOrStdout(), ep.URL)
					return nil
				}
			}
			return fmt.Errorf("the sandbox %q has no port named %q; it has %s", sb.ID, port, portNames(sb.Endpoints))
		},
	}
	cmd.Flags().StringVar(&port, "port", "", "which port's address to print; the first when empty")
	return cmd
}

func logsCmd() *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Print what a sandbox has output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			logs, err := c.Logs(ctx, args[0], tail)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), logs)
			return nil
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 200, "how many lines to fetch")
	return cmd
}

// eventsCmd prints the cluster events about a sandbox, which is what explains a
// sandbox that will not start — an image pull that failed, a probe that killed
// the container, a pod that was never scheduled.
func eventsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "events <name>",
		Short: "Print the cluster events about a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			events, err := c.Events(ctx, args[0], limit)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, events)
			}
			if len(events) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no events")
				return nil
			}
			rows := [][]string{{"type", "reason", "object", "count", "last seen", "message"}}
			for _, e := range events {
				rows = append(rows, []string{
					e.Type, e.Reason, e.Object, fmt.Sprint(e.Count),
					e.LastSeen.Local().Format("15:04:05"), e.Message,
				})
			}
			printTable(cmd.OutOrStdout(), rows)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "how many events to fetch")
	return cmd
}

func renewCmd() *cobra.Command {
	var ttl string
	cmd := &cobra.Command{
		Use:   "renew <name>",
		Short: "Reset a sandbox's lifetime",
		Long: "Give a sandbox a new lifetime, measured from now. Pass no --ttl to remove its\n" +
			"expiry entirely, which is how a sandbox someone is working in is kept from\n" +
			"disappearing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			sb, err := c.Renew(ctx, args[0], ttl)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, sb)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s now expires %s\n", sb.ID, expiryText(sb))
			return nil
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "", "the new lifetime, e.g. 2h; empty removes the expiry")
	return cmd
}

func rmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <name> [name...]",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete sandboxes",
		Long: "Delete one or more sandboxes. Everything in them goes too — a sandbox is a\n" +
			"namespace, so this is one operation, not a list of objects to get right.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// Deleting is irreversible and a name is easy to mistype, so more
			// than one at a time is confirmed unless it was asked for with --yes.
			if len(args) > 1 && !yes {
				fmt.Fprintf(out, "delete %d sandboxes: %s? [y/N] ", len(args), strings.Join(args, ", "))
				var answer string
				if _, err := fmt.Fscan(cmd.InOrStdin(), &answer); err != nil {
					return fmt.Errorf("reading the answer: %w", err)
				}
				if !strings.HasPrefix(strings.ToLower(answer), "y") {
					fmt.Fprintln(out, "nothing was deleted")
					return nil
				}
			}

			var failed int
			for _, id := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
				err := c.Delete(ctx, id)
				cancel()
				if err != nil {
					// One failure should not stop the rest: the names were
					// asked for individually, and the ones that worked are done.
					fmt.Fprintf(cmd.ErrOrStderr(), "could not delete %s: %v\n", id, err)
					failed++
					continue
				}
				fmt.Fprintf(out, "deleted %s\n", id)
			}
			if failed > 0 {
				return errExitQuiet
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask before deleting several")
	return cmd
}

// envCmd prints the environment a shell needs to talk to this deployment, so
// setting up a session is one eval rather than three exports typed by hand.
func envCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print the environment variables that point at this deployment",
		Long: "Print SANDBOX_URL and SANDBOX_KEY for the deployment you are talking to:\n\n" +
			"  eval \"$(sandbox env --key <key>)\"",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch shell {
			case "fish":
				fmt.Fprintf(out, "set -gx %s %s\n", urlEnv, shellQuote(c.BaseURL()))
				fmt.Fprintf(out, "set -gx %s %s\n", keyEnv, shellQuote(os.Getenv(keyEnv)))
			default:
				fmt.Fprintf(out, "export %s=%s\n", urlEnv, shellQuote(c.BaseURL()))
				fmt.Fprintf(out, "export %s=%s\n", keyEnv, shellQuote(os.Getenv(keyEnv)))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "sh", "which shell's syntax to print: sh or fish")
	return cmd
}

func describeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe",
		Short: "Print the control plane's own description of its API",
		Long: "Read /api/v1/describe from the deployment. It needs no key, which makes it\n" +
			"the way to check an address before configuring one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			d, err := c.GetDescribe(ctx)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, d)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s %s (%s)\n\n", d.Name, d.Version, d.Build)
			rows := [][]string{{"method", "path", "description"}}
			for _, ep := range d.Endpoints {
				rows = append(rows, []string{ep.Method, ep.Path, ep.Description})
			}
			printTable(out, rows)
			return nil
		},
	}
}

// waitForRunning polls until a sandbox is running, or the deadline passes.
//
// It is a poll rather than a watch because the API has no watch: a sandbox is
// up within seconds and a create happens rarely, so a request every second is
// neither a load nor a latency problem worth a streaming endpoint.
func waitForRunning(cmd *cobra.Command, c *client.Client, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
		sb, err := c.Get(ctx, id)
		cancel()

		switch {
		case err != nil && !isNotFound(err):
			return err
		case err == nil && sb.State == model.StateRunning:
			return nil
		case err == nil && sb.State == model.StateFailed:
			return fmt.Errorf("the sandbox %q failed to start: %s", id, sb.Message)
		case cmd.Context().Err() != nil:
			return cmd.Context().Err()
		case time.Now().After(deadline):
			return fmt.Errorf("the sandbox %q was not running after %s", id, timeout)
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(time.Second):
		}
	}
}

// ── rendering ───────────────────────────────────────────────────────────────

func printSandboxCard(cmd *cobra.Command, c *client.Client, sb model.Sandbox) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s  %s  %s\n", sb.ID, sb.Template, sb.State)
	fmt.Fprintf(out, "  image      %s\n", sb.Image)
	fmt.Fprintf(out, "  lifetime   %s\n", expiryText(sb))
	if sb.Message != "" {
		fmt.Fprintf(out, "  note       %s\n", sb.Message)
	}
	if len(sb.Endpoints) == 0 {
		fmt.Fprintf(out, "  address    (this template serves no port; use the API or `sandbox logs`)\n")
	}
	for _, ep := range sb.Endpoints {
		// The key is appended because these addresses are reachable directly in
		// a browser, which cannot set a header. It is what makes the printed
		// URL something you can click rather than something you have to finish.
		fmt.Fprintf(out, "  %-10s %s?key=%s\n", ep.Name, ep.URL, c.Key())
	}
}

func expiryText(sb model.Sandbox) string {
	if sb.ExpiresAt.IsZero() {
		return "no expiry"
	}
	left := time.Until(sb.ExpiresAt)
	if left <= 0 {
		return "expired"
	}
	return fmt.Sprintf("%s left (until %s)", left.Truncate(time.Second), sb.ExpiresAt.Local().Format(time.RFC3339))
}

func ttlRemaining(sb model.Sandbox, now time.Time) string {
	if sb.ExpiresAt.IsZero() {
		return "—"
	}
	left := sb.ExpiresAt.Sub(now)
	if left <= 0 {
		return "expired"
	}
	return left.Truncate(time.Minute).String()
}

func ttlSummary(t model.Template) string {
	switch {
	case t.TTLDefault == "":
		return "the deployment's default"
	case t.TTLMax == "":
		return t.TTLDefault
	default:
		return t.TTLDefault + " (max " + t.TTLMax + ")"
	}
}

func portSummary(ports []model.Port) string {
	if len(ports) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%s:%d", p.Name, p.Port))
	}
	return strings.Join(parts, " ")
}

func portNames(endpoints []model.Endpoint) string {
	if len(endpoints) == 0 {
		return "none"
	}
	names := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		names = append(names, ep.Name)
	}
	return strings.Join(names, ", ")
}

func firstURL(sb model.Sandbox) string {
	if len(sb.Endpoints) == 0 {
		return "—"
	}
	return sb.Endpoints[0].URL
}

// parseEnv turns repeated KEY=VALUE flags into a map.
func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--env expects KEY=VALUE, got %q", pair)
		}
		out[key] = value
	}
	return out, nil
}

func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shellQuote wraps a value in single quotes, so an eval of the printed
// environment cannot be split by a space or interpreted by the shell. A value
// containing a single quote is closed, escaped and reopened — the one case
// single-quoting alone does not survive.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printTable prints aligned columns.
//
// The widths are computed from the content rather than fixed, so a table stays
// readable whatever the names are — and tabs are never used, because a terminal
// expands them to a width nothing here can predict.
func printTable(out interface{ Write([]byte) (int, error) }, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
				break
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
		}
		fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	}
}
