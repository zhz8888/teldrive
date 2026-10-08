// Package main implements the teldrive CLI: the "run" command that starts the
// server, the "check" command that validates configuration and initializes the
// dependencies once, and the "version" command that prints build metadata.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zhz8888/teldrive/v2/internal/app"
	"github.com/zhz8888/teldrive/v2/internal/config"
	"github.com/zhz8888/teldrive/v2/internal/logging"
)

var (
	// version is the release version, overridden at build time via
	// -ldflags "-X main.version=...".
	version = "dev"
	// commit is the source revision reported by the version command, likewise
	// set through -ldflags and left as "unknown" for plain go build.
	commit = "unknown"
	// date is the build timestamp reported by the version command, likewise set
	// through -ldflags and left as "unknown" for plain go build.
	date = "unknown"
)

// main runs the command tree with a context that is canceled on SIGINT or
// SIGTERM, so subcommands propagate shutdown to the running server. Command
// errors are printed to stderr, and any failure exits with status 1.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCommand assembles the command tree. Usage and error output are
// silenced on the root so that main prints each failure exactly once.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "teldrive",
		Short:         "Telegram-backed cloud storage server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newRunCommand(), newCheckCommand(), newVersionCommand())
	return root
}

// newRunCommand builds the "run" command (aliased as "serve"). It loads the
// configuration from the registered flags, creates the logger, installs it as
// the slog default, and runs the application until the command context is
// canceled. A context.Canceled error from a signal-triggered shutdown is
// treated as a clean exit; every other error is returned to main.
func newRunCommand() *cobra.Command {
	loader := config.NewLoader()
	cmd := &cobra.Command{
		Use:     "run",
		Aliases: []string{"serve"},
		Short:   "Start the TelDrive server",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loader.Load(cmd.Flags())
			if err != nil {
				return err
			}
			logger, err := logging.NewLogger(os.Stdout, cfg.Logging.LogLevel, cfg.Logging.LogFormat)
			if err != nil {
				return err
			}
			slog.SetDefault(logger)
			application, err := app.New(cmd.Context(), cfg, app.Dependencies{Logger: logger, Version: buildVersion()})
			if err != nil {
				return fmt.Errorf("initialize TelDrive: %w", err)
			}
			logger.Info("application.starting", "address", cfg.HTTP.Address, "version", buildVersion(), "commit", commit)
			if err := application.Run(cmd.Context()); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("application.stopped", "error", err)
				return err
			}
			logger.Info("application.stopped")
			return nil
		},
	}
	loader.RegisterFlags(cmd.Flags())
	return cmd
}

// newCheckCommand builds the "check" command, which validates the configuration
// and initializes every dependency exactly as run would, then closes the
// application before any traffic is served. It is meant for deployments and
// pre-flight checks: success reports that configuration, migrations, and
// dependency connections all work.
func newCheckCommand() *cobra.Command {
	loader := config.NewLoader()
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate configuration and initialize dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loader.Load(cmd.Flags())
			if err != nil {
				return err
			}
			logger, err := logging.NewLogger(os.Stdout, cfg.Logging.LogLevel, cfg.Logging.LogFormat)
			if err != nil {
				return err
			}
			slog.SetDefault(logger)
			application, err := app.New(cmd.Context(), cfg, app.Dependencies{Logger: logger, Version: buildVersion()})
			if err != nil {
				return fmt.Errorf("initialize TelDrive: %w", err)
			}
			if err := application.Close(); err != nil {
				return fmt.Errorf("close checked application: %w", err)
			}
			logger.Info("application.check.succeeded", "version", buildVersion())
			return nil
		},
	}
	loader.RegisterFlags(cmd.Flags())
	return cmd
}

// newVersionCommand builds the "version" command, which prints the values
// resolved by buildVersion together with the build-injected commit and date.
// It writes to stdout and never returns an error.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Printf("teldrive %s commit=%s built=%s\n", buildVersion(), commit, date)
		},
	}
}

// buildVersion resolves the version reported to users and passed to the
// application. It prefers the linker-injected version and otherwise falls back
// to the module version recorded in the build info, which makes
// `go install`ed binaries report their released tag; "dev" is returned when
// neither is available, as with a plain `go build`.
func buildVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
