package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/sandboxlab/internal/client"
	"github.com/shaowenchen/sandboxlab/internal/user"
)

// The user commands are the administrator's. A user running one gets the same
// answer the API gives — a 404 — rather than a message explaining the
// restriction, because the point of the split is that a user cannot tell what
// is on the other side of it.

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show which key you are using",
		Long: "Report whether this key is the administrator's or a user's, and which user\n" +
			"it is. It is the way to answer \"why can I not see that sandbox\" without\n" +
			"guessing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			me, err := c.Whoami(ctx)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, me)
			}
			out := cmd.OutOrStdout()
			if me.Admin {
				fmt.Fprintln(out, "administrator")
				fmt.Fprintln(out, "  may see every sandbox and manage users")
			} else {
				fmt.Fprintf(out, "user %s\n", me.User)
				fmt.Fprintln(out, "  may create sandboxes and see its own")
			}
			return nil
		},
	}
}

func usersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "users",
		Aliases: []string{"user"},
		Short:   "Manage the users who may use this deployment",
		Long: "Administrator only. Each user gets a key of their own and can create\n" +
			"sandboxes and see only theirs.",
	}
	cmd.AddCommand(
		usersListCmd(),
		usersCreateCmd(),
		usersShowCmd(),
		usersLimitsCmd(),
		usersKeyCmd(),
		usersRmCmd(),
	)
	return cmd
}

func usersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			users, err := c.Users(ctx)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, users)
			}
			out := cmd.OutOrStdout()
			if len(users) == 0 {
				fmt.Fprintln(out, "no users. Create one with `sandbox users create <name>`.")
				return nil
			}
			rows := [][]string{{"user", "sandboxes", "limits", "last used"}}
			for _, u := range users {
				rows = append(rows, []string{
					u.Name,
					sandboxCount(u),
					userLimits(u),
					lastUsed(u),
				})
			}
			printTable(out, rows)
			return nil
		},
	}
}

func usersCreateCmd() *cobra.Command {
	var (
		maxSandboxes int
		maxTTL       string
		templates    string
		key          string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user and print its key",
		Long: "Create a user, print the key it was given, and stop.\n\n" +
			"The key is shown here and is readable later with `sandbox users show <name>`,\n" +
			"so losing the output is recoverable — but it is a credential, and handing it\n" +
			"over is the point of this command.\n\n" +
			"With no limits given, the user gets the deployment's own: every template, the\n" +
			"deployment's maximum lifetime, and no separate ceiling on how many sandboxes\n" +
			"they may have.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			u, err := c.CreateUser(ctx, client.CreateUserInput{
				Name:         args[0],
				Key:          key,
				MaxSandboxes: maxSandboxes,
				MaxTTL:       maxTTL,
				Templates:    templates,
			})
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, u)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "created %s\n\n", u.Name)
			fmt.Fprintf(out, "  key   %s\n", u.Key)
			fmt.Fprintf(out, "  give this to them; `sandbox users show %s` reads it back\n", u.Name)
			if u.Quota.Describe() != "" {
				fmt.Fprintf(out, "  limits  %s\n", u.Quota.Describe())
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&maxSandboxes, "max-sandboxes", 0, "how many sandboxes they may have at once; 0 is no limit")
	cmd.Flags().StringVar(&maxTTL, "max-ttl", "", "the longest a sandbox of theirs may live, e.g. 2h")
	cmd.Flags().StringVar(&templates, "templates", "", "comma-separated templates they may use; empty is all of them")
	cmd.Flags().StringVar(&key, "key", "", "use this key instead of generating one")
	return cmd
}

func usersShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one user, with its key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			u, err := c.User(ctx, args[0])
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, u)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n", u.Name)
			fmt.Fprintf(out, "  key         %s\n", u.Key)
			fmt.Fprintf(out, "  sandboxes   %s\n", sandboxCount(u))
			fmt.Fprintf(out, "  limits      %s\n", userLimits(u))
			fmt.Fprintf(out, "  last used   %s\n", lastUsed(u))
			return nil
		},
	}
}

func usersLimitsCmd() *cobra.Command {
	var (
		maxSandboxes int
		maxTTL       string
		templates    string
	)
	cmd := &cobra.Command{
		Use:   "limit <name>",
		Short: "Change what a user may do",
		Long: "Set a user's limits. The key is not touched, so a limit change does not sign\n" +
			"anyone out.\n\n" +
			"Passing 0 or an empty value removes that particular limit — the user then gets\n" +
			"the deployment's own for it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			// Only the flags that were given are sent, so changing one limit
			// does not clear the others.
			in := client.CreateUserInput{}
			if cmd.Flags().Changed("max-sandboxes") {
				in.SetMaxSandboxes = true
				in.MaxSandboxes = maxSandboxes
			}
			if cmd.Flags().Changed("max-ttl") {
				in.SetMaxTTL = true
				in.MaxTTL = maxTTL
			}
			if cmd.Flags().Changed("templates") {
				in.SetTemplates = true
				in.Templates = templates
			}
			u, err := c.UpdateUser(ctx, args[0], in)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, u)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now limited to: %s\n", u.Name, userLimits(u))
			return nil
		},
	}
	// The flags default to empty rather than to a zero value, so "not given"
	// and "set to no limit" are different things — without this, running the
	// command to change one limit would silently clear the others.
	cmd.Flags().IntVar(&maxSandboxes, "max-sandboxes", 0, "how many sandboxes they may have at once; 0 is no limit")
	cmd.Flags().StringVar(&maxTTL, "max-ttl", "", "the longest a sandbox of theirs may live; empty is no limit")
	cmd.Flags().StringVar(&templates, "templates", "", "comma-separated templates they may use; empty is all of them")
	return cmd
}

func usersKeyCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "key <name>",
		Short: "Issue a new key for a user",
		Long: "Replace a user's key. The old one stops working immediately, which is what\n" +
			"makes this the command to reach for when a key has leaked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()

			u, err := c.RotateUserKey(ctx, args[0], key)
			if err != nil {
				return err
			}
			if jsonOut(cmd) {
				return printJSON(cmd, u)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n  key   %s\n", u.Name, u.Key)
			return nil
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "use this key instead of generating one")
	return cmd
}

func usersRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <name> [name...]",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete users",
		Long: "Delete users. Their keys stop working; their sandboxes do not — those have\n" +
			"lifetimes of their own, and tearing down an environment someone is working in\n" +
			"is not what revoking a key means. Delete them by id if you want them gone.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if len(args) > 1 && !yes {
				fmt.Fprintf(out, "delete %d users: %s? [y/N] ", len(args), joinComma(args))
				var answer string
				if _, err := fmt.Fscan(cmd.InOrStdin(), &answer); err != nil {
					return fmt.Errorf("reading the answer: %w", err)
				}
				if !hasPrefixFold(answer, "y") {
					fmt.Fprintln(out, "nothing was deleted")
					return nil
				}
			}

			var failed int
			for _, name := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
				err := c.DeleteUser(ctx, name)
				cancel()
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not delete %s: %v\n", name, err)
					failed++
					continue
				}
				fmt.Fprintf(out, "deleted %s\n", name)
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

// ── rendering ───────────────────────────────────────────────────────────────

func sandboxCount(u user.User) string {
	if u.Quota.MaxSandboxes > 0 {
		return strconv.Itoa(u.Sandboxes) + " / " + strconv.Itoa(u.Quota.MaxSandboxes)
	}
	return strconv.Itoa(u.Sandboxes)
}

func userLimits(u user.User) string {
	return u.Quota.Describe()
}

func lastUsed(u user.User) string {
	if u.LastUsedAt == nil || u.LastUsedAt.IsZero() {
		return "never"
	}
	return u.LastUsedAt.Local().Format(time.RFC3339)
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[0] == prefix[0] || s[0] == prefix[0]-32
}
