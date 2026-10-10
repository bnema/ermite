// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bnema/zerowrap"
	"github.com/spf13/cobra"

	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/config"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the server",
		Long: "Run the server. It is configured by the environment only:\n" +
			config.EnvAddr + ", " + config.EnvDataDir + ", " + config.EnvPublicURL + ", " + config.EnvAdminToken + ",\n" +
			config.EnvTrustedProxies + ", " + config.EnvGit + ", " + config.EnvSourceURL + ", " +
			config.EnvLogLevel + ", " + config.EnvLogFormat + ".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context())
		},
	}
}

// serve wires the adapters and runs the server until SIGINT or SIGTERM.
func serve(ctx context.Context) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	// The logger is built from the validated settings, which carry the
	// defaults of ERMITE_LOG_LEVEL and ERMITE_LOG_FORMAT.
	log := zerowrap.New(zerowrap.Config{Level: cfg.LogLevel, Format: cfg.LogFormat})
	ctx = zerowrap.WithCtx(ctx, log)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore the default signal handling as soon as the first signal
	// arrives, so a second one kills the process during the shutdown.
	go func() {
		<-ctx.Done()
		stop()
	}()

	log.Info().Str("version", version()).Str("addr", cfg.Addr).Str("data_dir", cfg.DataDir).Msg("starting")

	if err := prepareDataDir(cfg.DataDir); err != nil {
		return err
	}
	store, err := kvstore.Open(ctx, filepath.Join(cfg.DataDir, "kv"))
	if err != nil {
		return err
	}
	// The server has stopped by the time this runs; close even when ctx ended.
	defer func() {
		if cerr := store.Close(context.WithoutCancel(ctx)); cerr != nil {
			log.Error().Err(cerr).Msg("close metadata store")
		}
	}()
	// The kv store holds the directory lock: only now is run/ ours to reset.
	if err := resetRunDir(cfg.DataDir); err != nil {
		return err
	}

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	git := gitcli.New(gitcli.Config{Bin: cfg.Git, DataDir: cfg.DataDir, Path: path})
	gitVersion, err := git.CheckVersion(ctx)
	if err != nil {
		return fmt.Errorf("%s=%q: %w", config.EnvGit, cfg.Git, err)
	}
	log.Info().Str("git", gitVersion.String()).Msg("git checked")

	repos := app.NewRepos(store, git, systemClock{})
	report, err := repos.Reconcile(ctx)
	if err != nil {
		return fmt.Errorf("reconcile repositories: %w", err)
	}
	log.Info().
		Int("registered", len(report.Registered)).
		Int("missing_dirs", len(report.MissingDirs)).
		Int("invalid_dirs", len(report.InvalidDirs)).
		Msg("repositories reconciled")

	srv := httpserver.New(httpserver.Config{
		Addr:         cfg.Addr,
		HealthChecks: []httpserver.Check{store.Check, httpserver.DirWritable(cfg.DataDir)},
	})
	if err := srv.Run(ctx); err != nil {
		return err
	}
	log.Info().Msg("stopped")
	return nil
}
