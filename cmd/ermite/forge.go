// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/adapters/web"
	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/ports"
)

// maxReceiveBytes bounds the body of a push, both at the HTTP handler and in
// Git (receive.maxInputSize).
const maxReceiveBytes = githttp.DefaultMaxBodyBytes

// options are the parts of the wiring that tests replace.
type options struct {
	// hookExe is the executable the hook scripts exec; empty means this
	// process's own executable.
	hookExe string
	// maintenanceDelay overrides app.MaintenanceDelay when positive.
	maintenanceDelay time.Duration
	// maxBodyBytes overrides the limit of a push body when positive.
	maxBodyBytes int64
	// eventBuffer overrides app.DefaultEventBuffer when positive.
	eventBuffer int
	// revalidateEvery overrides app.DefaultRevalidateEvery when not zero.
	revalidateEvery time.Duration
}

// forge is the wired server: every adapter and use case, ready to serve.
type forge struct {
	cfg    config.Config
	store  *kvstore.Store
	server *httpserver.Server
	hooks  *githttp.HookServer
	maint  *app.Maintainer
	git    *gitcli.Git

	// The use cases and the API are exposed for tests and for the phases
	// that add the web.
	Repos    *app.Repos
	Tokens   *app.Tokens
	Auth     *app.Auth
	Pushes   *app.Pushes
	Browse   *app.Browse
	EventLog *app.Events
	API      *rpc.API
	Bus      ports.EventBus
}

// serverVersion is the version GetVersion reports: the module version of the
// build, or "devel" for an untagged one.
func serverVersion() string {
	if v := buildVersion(); v != "(devel)" {
		return v
	}
	return "devel"
}

// newForge opens the metadata store, resets run/, checks git, reconciles the repositories,
// writes the hook scripts, opens the hook socket and builds the HTTP server.
// The data directory must be prepared (prepareDataDir). Close it after the
// server stopped.
func newForge(ctx context.Context, cfg config.Config, opts options) (_ *forge, err error) {
	log := zerowrap.FromCtx(ctx)
	store, err := kvstore.Open(ctx, filepath.Join(cfg.DataDir, "kv"))
	if err != nil {
		return nil, err
	}
	f := &forge{cfg: cfg, store: store}
	defer func() {
		if err != nil {
			f.Close(context.WithoutCancel(ctx))
		}
	}()
	// The kv store holds the directory lock: only now is run/ ours to reset.
	// The hook scripts and the socket are written into it afterwards.
	if err := resetRunDir(cfg.DataDir); err != nil {
		return nil, err
	}

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	maxBody := maxReceiveBytes
	if opts.maxBodyBytes > 0 {
		maxBody = opts.maxBodyBytes
	}
	git := gitcli.New(gitcli.Config{Bin: cfg.Git, DataDir: cfg.DataDir, Path: path, MaxReceiveBytes: maxBody})
	f.git = git
	gitVersion, err := git.CheckVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", config.EnvGit, cfg.Git, err)
	}
	log.Info().Str("git", gitVersion.String()).Msg("git checked")
	if _, err := git.HTTPBackend(ctx, false); err != nil {
		return nil, fmt.Errorf("%s=%q: %w", config.EnvGit, cfg.Git, err)
	}

	clock := systemClock{}
	f.Bus = eventbus.New()
	f.Repos = app.NewRepos(store, git, f.Bus, clock)
	report, err := f.Repos.Reconcile(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile repositories: %w", err)
	}
	log.Info().
		Int("registered", len(report.Registered)).
		Int("missing_dirs", len(report.MissingDirs)).
		Int("invalid_dirs", len(report.InvalidDirs)).
		Msg("repositories reconciled")

	f.Tokens = app.NewTokens(store.Tokens(), clock)
	f.Auth = app.NewAuth(cfg.AdminTokenHash, store.Tokens(), clock)
	f.maint = app.NewMaintainer(ctx, git, opts.maintenanceDelay)
	f.Pushes = app.NewPushes(store, f.Repos, git, f.Bus, clock, f.maint, f.Auth)

	exe := opts.hookExe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("find the ermite executable for the hooks: %w", err)
		}
	}
	if err := githttp.WriteHookScripts(gitcli.HooksDir(cfg.DataDir), exe); err != nil {
		return nil, err
	}
	f.hooks = githttp.NewHookServer(filepath.Join(cfg.DataDir, "run", "hook.sock"), f.Pushes)
	if err := f.hooks.Listen(ctx); err != nil {
		return nil, err
	}

	f.Browse = app.NewBrowse(store, git)
	f.EventLog = app.NewEvents(ctx, f.Repos, f.Bus, clock, app.EventsConfig{
		Size: opts.eventBuffer, Auth: f.Auth, RevalidateEvery: opts.revalidateEvery,
	})

	// One budget of failed authentications per client, for Git and the API.
	limiter := clientlimit.NewLimiter(nil)
	f.API = rpc.New(rpc.Config{
		Auth:           f.Auth,
		Repos:          f.Repos,
		Tokens:         f.Tokens,
		Browse:         f.Browse,
		Events:         f.EventLog,
		Version:        serverVersion(),
		TrustedProxies: cfg.TrustedProxies,
		Limiter:        limiter,
	})

	gh := githttp.New(githttp.Config{
		Auth:           f.Auth,
		Repos:          f.Repos,
		Pushes:         f.Pushes,
		Backend:        git,
		Hooks:          f.hooks,
		TrustedProxies: cfg.TrustedProxies,
		Limiter:        limiter,
		MaxBodyBytes:   maxBody,
	})
	routes := make([]httpserver.Route, 0, len(githttp.Patterns)+len(web.Patterns))
	for _, p := range githttp.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: gh})
	}
	site, err := newWeb(cfg, f.API)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	routes = append(routes, webRoutes(site)...)
	f.server = httpserver.New(httpserver.Config{
		Addr:         cfg.Addr,
		HealthChecks: []httpserver.Check{store.Check, httpserver.DirWritable(cfg.DataDir)},
		Routes:       routes,
		API:          f.API.Handler(),
		// Event streams never end by themselves: close them when the
		// shutdown begins, or it would wait for them until its timeout.
		OnShutdown: []func(){func() { f.EventLog.Close(context.WithoutCancel(ctx)) }},
	})
	return f, nil
}

// Run listens on the configured address and serves until ctx ends.
func (f *forge) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", f.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.cfg.Addr, err)
	}
	return f.Serve(ctx, ln)
}

// Serve serves HTTP on ln and the hook socket until ctx ends. The hook socket
// outlives the HTTP server: requests still in flight while it drains are
// pushes whose hooks need it. A failure of either stops both.
func (f *forge) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hookCtx, stopHooks := context.WithCancel(context.WithoutCancel(ctx))
	defer stopHooks()

	hookErr := make(chan error, 1)
	go func() {
		err := f.hooks.Serve(hookCtx)
		cancel() // a dead hook socket would reject every push: stop serving
		hookErr <- err
	}()

	httpErr := f.server.Serve(ctx, ln)
	stopHooks()
	return errors.Join(httpErr, <-hookErr)
}

// Close releases what newForge opened. It is safe to call twice. Call it after
// the HTTP server stopped: it stops the event log and the long-lived Git
// processes of the read side, which requests in flight may still use.
func (f *forge) Close(ctx context.Context) {
	log := zerowrap.FromCtx(ctx)
	if f.hooks != nil {
		f.hooks.Close(ctx)
	}
	if f.EventLog != nil {
		f.EventLog.Close(ctx)
	}
	if f.git != nil {
		if err := f.git.Close(ctx); err != nil {
			log.Error().Err(err).Msg("close git reader")
		}
	}
	if f.maint != nil {
		f.maint.Close(ctx)
	}
	if f.store != nil {
		if err := f.store.Close(ctx); err != nil {
			log.Error().Err(err).Msg("close metadata store")
		}
		f.store = nil
	}
}
