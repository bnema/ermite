// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bnema/zerowrap"
	"github.com/spf13/cobra"

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
	cfg, err := config.Load(os.Getenv)
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
	f, err := newForge(ctx, cfg, options{})
	if err != nil {
		return err
	}
	// The server has stopped by the time this runs; close even when ctx ended.
	defer f.Close(context.WithoutCancel(ctx))

	if err := f.Run(ctx); err != nil {
		return err
	}
	log.Info().Msg("stopped")
	return nil
}
