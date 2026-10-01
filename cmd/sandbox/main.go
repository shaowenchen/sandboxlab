// Command sandbox is the control plane: it serves the API, the console, and the
// data-plane proxy that reaches into a sandbox.
//
// It has one real command, `serve`, because that is what the Deployment runs.
// Everything about an environment comes from the environment the pod gives it
// (see the internal/config package), so there are no flags to keep in step with
// the chart's values.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/sandboxlab/internal/api"
	"github.com/shaowenchen/sandboxlab/internal/auth"
	"github.com/shaowenchen/sandboxlab/internal/buildinfo"
	"github.com/shaowenchen/sandboxlab/internal/catalog"
	"github.com/shaowenchen/sandboxlab/internal/config"
	"github.com/shaowenchen/sandboxlab/internal/console"
	"github.com/shaowenchen/sandboxlab/internal/k8s"
	"github.com/shaowenchen/sandboxlab/internal/logging"
	"github.com/shaowenchen/sandboxlab/internal/proxy"
	"github.com/shaowenchen/sandboxlab/internal/reaper"
	"github.com/shaowenchen/sandboxlab/internal/sandbox"
	"github.com/shaowenchen/sandboxlab/internal/userservice"
)

func main() {
	// A signal-cancelled context is what lets the reaper stop and the HTTP
	// server drain instead of the process being killed mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:           "sandbox",
		Short:         "Serve a sandbox environment: the API, the console and the data-plane proxy",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd())
	root.AddCommand(catalogCmd())

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var (
		listen      string
		printConfig bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the control plane",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if listen != "" {
				cfg.Listen = listen
			}
			if printConfig {
				// Printed before anything else, so a run that fails to reach a
				// cluster still shows what it was configured with — which is
				// the question being asked at that point.
				printResolved(cfg)
			}
			return serve(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "address to bind, overriding SANDBOX_LISTEN")
	cmd.Flags().BoolVar(&printConfig, "print-config", true, "log the resolved configuration at startup")
	return cmd
}

func serve(ctx context.Context, cfg config.Config) error {
	log := logging.New(cfg.LogLevel, os.Stderr)
	log.Info("starting sandboxlab", "build", buildinfo.String())

	templates, err := catalog.Loader{
		ExtraDir: cfg.CatalogDir,
		Disabled: disabledSet(cfg.DisabledTemplates),
	}.Load()
	if err != nil {
		return err
	}
	log.Info("loaded the catalog", "templates", templates.Len())

	client, err := k8s.New(cfg)
	if err != nil {
		return err
	}

	// The user store is the cluster's Secrets in the control plane's own
	// namespace, so this is the same choice the rest of the deployment makes:
	// the cluster is the record, and a restart loses nothing.
	userStore := client.Users()
	users := userservice.New(userStore, client)
	svc := sandbox.New(cfg, templates, client, users)

	if cfg.GeneratedKey() {
		// Loud, because it is a credential and the only place it appears. It is
		// never masked: hiding it here would hide it from the summary that
		// exists to show it.
		log.Warn("no API key was configured; one was generated", "api_key", cfg.APIKey)
	}

	var dataPlane api.DataPlane
	if cfg.DataPlane {
		dataPlane = proxy.New(cfg, client, log)
	}

	consoleHandler, err := console.New()
	if err != nil {
		return err
	}

	server := api.New(api.Deps{
		Config:    cfg,
		Service:   svc,
		Users:     users,
		Auth:      auth.New(cfg.APIKey, users),
		Log:       log,
		Console:   consoleHandler,
		DataPlane: dataPlane,
	})

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: the data-plane proxy streams a sandbox's output, and
		// a fixed write deadline would cut a long-running command off mid-line.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// The reaper runs for the life of the process. A control plane that
	// restarts does not lose the schedule — the expiry is on the sandbox — so
	// there is nothing to recover here.
	go reaper.New(client, cfg.ReapInterval, log).Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Info("serving", "listen", cfg.Listen, "base_path", cfg.BasePath, "public_url", cfg.PublicURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// A bounded drain: the process should stop, but not so abruptly that a
	// request in flight is cut off with no response at all.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("did not shut down cleanly", "error", err)
	}
	return nil
}

// catalogCmd prints the catalog the current configuration would serve, without
// starting anything. It is how a person answers "why is that template not
// listed" without a cluster.
func catalogCmd() *cobra.Command {
	var asYAML bool
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Print the templates this configuration serves",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			templates, err := catalog.Loader{
				ExtraDir: cfg.CatalogDir,
				Disabled: disabledSet(cfg.DisabledTemplates),
			}.Load()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, t := range templates.List() {
				if asYAML {
					b, err := catalog.Marshal(t)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "---\n%s", b)
					continue
				}
				fmt.Fprintf(out, "%-14s %-22s %s\n", t.ID, t.Title, t.Image)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asYAML, "yaml", false, "print the templates as YAML rather than a table")
	return cmd
}

func printResolved(cfg config.Config) {
	log := logging.New(cfg.LogLevel, os.Stderr)
	log.Info("resolved configuration",
		"listen", cfg.Listen,
		"namespace", cfg.Namespace,
		"namespace_prefix", cfg.SandboxNamespacePrefix,
		"base_path", cfg.BasePath,
		"public_url", cfg.PublicURL,
		"catalog_dir", cfg.CatalogDir,
		"disabled_templates", cfg.DisabledTemplates,
		"default_ttl", cfg.DefaultTTL,
		"max_ttl", cfg.MaxTTL,
		"max_sandboxes", cfg.MaxSandboxes,
		"reap_interval", cfg.ReapInterval,
		"data_plane", cfg.DataPlane,
	)
}

func disabledSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
