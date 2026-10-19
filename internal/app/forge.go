// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/adapters/ciartifacts"
	"bnema.dev/ermite/internal/adapters/cilogs"
	"bnema.dev/ermite/internal/adapters/cisummaries"
	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/adapters/githubapi"
	"bnema.dev/ermite/internal/adapters/githubraw"
	"bnema.dev/ermite/internal/adapters/gitremote"
	"bnema.dev/ermite/internal/adapters/httpserver"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/adapters/oidchttp"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/adapters/secretbox"
	"bnema.dev/ermite/internal/adapters/sigverify"
	"bnema.dev/ermite/internal/adapters/sshsig"
	"bnema.dev/ermite/internal/adapters/web"
	"bnema.dev/ermite/internal/adapters/webauthn"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// maxReceiveBytes bounds the body of a push, both at the HTTP handler and in
// Git (receive.maxInputSize).
const maxReceiveBytes = githttp.DefaultMaxBodyBytes

// options are the parts of the wiring that tests replace.
type options struct {
	// hookExe is the executable the hook scripts exec; empty means this
	// process's own executable.
	hookExe string
	// maintenanceDelay overrides usecase.MaintenanceDelay when positive.
	maintenanceDelay time.Duration
	// maxBodyBytes overrides the limit of a push body when positive.
	maxBodyBytes int64
	// eventBuffer overrides usecase.DefaultEventBuffer when positive.
	eventBuffer int
	// revalidateEvery overrides usecase.DefaultRevalidateEvery when not zero.
	revalidateEvery time.Duration
	// archivesPerClient overrides the archives one client downloads at once
	// when positive.
	archivesPerClient int
	// remote replaces the lister of github.com refs (tests never reach the
	// network).
	remoteLister ports.GitRemote
	// workflowFetcher replaces the downloader of github.com reusable workflows
	// (tests never reach the network).
	workflowFetcher ports.WorkflowFetcher
	// ci overrides the settings of the CI engine (tests shorten its
	// durations).
	ci func(*usecase.CIConfig)
	// oidcKey generates the signing keys of the ID tokens; nil generates RSA
	// keys (tests give prepared ones: a 3072-bit key takes a while).
	oidcKey func() (*rsa.PrivateKey, error)
}

// remote returns the lister of github.com refs: the test's, else def.
func (o options) remote(def ports.GitRemote) ports.GitRemote {
	if o.remoteLister != nil {
		return o.remoteLister
	}
	return def
}

// workflows returns the downloader of github.com reusable workflows: the
// test's, else def.
func (o options) workflows(def ports.WorkflowFetcher) ports.WorkflowFetcher {
	if o.workflowFetcher != nil {
		return o.workflowFetcher
	}
	return def
}

// forge is the wired server: every adapter and use case, ready to serve.
type forge struct {
	cfg    config.Config
	store  *kvstore.Store
	server *httpserver.Server
	hooks  *githttp.HookServer
	maint  *usecase.Maintainer
	git    *gitcli.Git
	// reader is the ermite-git reader when ERMITE_GIT_READER selects it.
	reader *ermitegit.Adapter

	// The use cases and the API are exposed for tests and for the phases
	// that add the web.
	Repos  *usecase.Repos
	Tokens *usecase.Tokens
	Auth   *usecase.Auth
	Pushes *usecase.Pushes
	Browse *usecase.Browse
	// MergeRequests and SigningKey serve MergeRequestService and
	// ServerService.GetSigningKey.
	MergeRequests *usecase.MergeRequests
	// Issues serves IssueService, and tells what a number names.
	Issues     *usecase.Issues
	SigningKey *usecase.SigningKey
	// Signatures are the trusted keys and the commit signature checks
	// (ADR 0030).
	Signatures *usecase.Signatures
	Sessions   *usecase.Sessions
	// Passkeys is nil when passkeys are not available (passkeysAvailable).
	Passkeys *usecase.Passkeys
	EventLog *usecase.Events
	// CI is the CI engine: workflow trigger, planner, runner protocol and the
	// owner side. It starts with the server and stops with it.
	CI *usecase.CI
	// Settings are the secrets, variables and environments of CI.
	Settings *usecase.CISettings
	// OIDC is the provider of the ID tokens of CI jobs; nil without a secrets
	// key.
	OIDC *usecase.OIDC
	API  *rpc.API
	Bus  ports.EventBus
}

// newSigningSeed makes the seed of a new ed25519 signing key.
func newSigningSeed() ([]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return seed, nil
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
	f.Repos = usecase.NewRepos(store, git, f.Bus, clock)
	report, err := f.Repos.Reconcile(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile repositories: %w", err)
	}
	log.Info().
		Int("registered", len(report.Registered)).
		Int("missing_dirs", len(report.MissingDirs)).
		Int("invalid_dirs", len(report.InvalidDirs)).
		Msg("repositories reconciled")

	f.Tokens = usecase.NewTokens(store.Tokens(), clock, store.Sessions())
	f.Auth = usecase.NewAuth(cfg.AdminTokenHash, store.Tokens(), clock).WithRepoTokens(store.CI())
	f.maint = usecase.NewMaintainer(ctx, git, opts.maintenanceDelay)
	f.Pushes = usecase.NewPushes(store, f.Repos, git, f.Bus, clock, f.maint, f.Auth)

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

	var reader ports.RepoReader = git
	if cfg.GitReader == config.GitReaderErmiteGit {
		f.reader = ermitegit.New(ermitegit.Config{
			Fallback: git,
			RepoDir:  func(name domain.RepoName) string { return gitcli.RepoDir(cfg.DataDir, name) },
		})
		git.OnTrash(f.reader.Forget)
		f.maint.WithFreshener(f.reader)
		f.Pushes.WithFreshener(f.reader)
		reader = f.reader
	}
	log.Info().Str("reader", cfg.GitReader).Msg("git reader selected")
	f.Browse = usecase.NewBrowse(store, reader)

	// Secrets, variables and environments (ADR 0019, CI-3). With a key the
	// secrets are sealed under it, and the server refuses to start when the
	// key is not the one they were sealed with; without one they cannot be set
	// and jobs receive none.
	var sealer ports.SecretSealer // a nil *secretbox.Box must not become a non-nil interface
	if cfg.SecretsKey != nil {
		box, err := secretbox.New(cfg.SecretsKey)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", config.EnvSecretsKey, err)
		}
		sealer = box
	}
	f.Settings, err = usecase.NewCISettings(ctx, store, store.Settings(), sealer, clock)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvSecretsKey, err)
	}
	// The OIDC provider of CI jobs (CI-4): its signing key is sealed with the
	// secrets key, so there is none without one.
	f.OIDC, err = usecase.NewOIDC(ctx, usecase.OIDCConfig{
		Store: store, Sealer: sealer, Tokens: store.CI(), Clock: clock, PublicURL: cfg.PublicURL, NewKey: opts.oidcKey,
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}

	// CI (ADR 0025): the engine reads workflows through the reader, and plans
	// them in the background; a push only queues its refs.
	committer := domain.Identity{Name: cfg.CommitterName, Email: cfg.CommitterEmail}
	ciCfg := usecase.CIConfig{
		Store: store.CI(), Logs: cilogs.New(cilogs.Dir(cfg.DataDir)), Summaries: cisummaries.New(cisummaries.Dir(cfg.DataDir)), Meta: store, Repos: git, Reader: reader,
		Git: git, Diff: git, Bus: f.Bus, Clock: clock, Auth: f.Auth,
		Owner: cfg.Owner, PublicURL: cfg.PublicURL, Retention: cfg.CIRetention,
		PollTimeout: cfg.CIPollTimeout, Archive: git,
		Remote: opts.remote(gitremote.New(gitremote.Config{
			Bin: cfg.Git, Path: path, Home: filepath.Join(cfg.DataDir, "run", "git-home"),
			Proxy: gitremote.ProxyEnv(os.Getenv),
		})),
		Tokens:        store.Tokens(),
		RefTips:       store.CI(),
		WorkflowState: store.CI(),
		Settings:      f.Settings,
		Workflows:     opts.workflows(githubraw.New(githubraw.Config{})),
		OIDC:          f.OIDC,
	}
	artifacts(&ciCfg, cfg, store)
	f.pullRequests(&ciCfg, store, git, committer)
	if opts.ci != nil {
		opts.ci(&ciCfg)
	}
	f.CI = usecase.NewCI(ctx, ciCfg)
	f.Pushes.WithWorkflowTrigger(f.CI).WithWorkflowGuard(git).WithRefTips(store.CI())
	f.Repos.WithCIRunCleaner(f.CI)
	f.CI.Start()
	f.EventLog = usecase.NewEvents(ctx, f.Repos, f.Bus, clock, usecase.EventsConfig{
		Size: opts.eventBuffer, Auth: f.Auth, RevalidateEvery: opts.revalidateEvery,
	})

	// The instance signing key (ADR 0022): generated on the first start.
	seed, err := store.SigningSeed(ctx, newSigningSeed, clock.Now().UTC())
	if err != nil {
		return nil, err
	}
	signer, err := sshsig.New(seed)
	if err != nil {
		return nil, err
	}
	f.SigningKey = usecase.NewSigningKey(signer, committer)
	if err := f.signatures(store, reader, signer.PublicKey(), committer, clock); err != nil {
		return nil, err
	}
	f.MergeRequests = usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: store, Store: store.MergeRequests(), Repos: git, Git: git, Signer: signer,
		Committer: committer, Bus: f.Bus, Clock: clock, Maint: f.maint,
		CI: f.CI, Checks: f.CI, PullRequests: f.CI, RefTips: store.CI(),
	})
	f.MergeRequests.WithVerifier(f.Signatures)
	f.Issues = usecase.NewIssues(usecase.IssuesConfig{
		Meta: store, Store: store.Issues(), MergeRequests: store.MergeRequests(), Bus: f.Bus, Clock: clock,
	})
	f.CI.SetPullGate(f.MergeRequests)
	log.Info().Str("fingerprint", signer.PublicKey().Fingerprint).Msg("commit signing key ready")

	f.Sessions, err = usecase.NewSessions(ctx, store.Sessions(), clock, cfg.PublicURL)
	if err != nil {
		return nil, err
	}

	// Owner passkeys (ADR 0026): built only when the public URL allows them.
	// When it does, a verifier that cannot be built is fatal.
	if ok, reason := passkeysAvailable(cfg.PublicURL); ok {
		verifier, err := webauthn.New(cfg.PublicURL, cfg.Owner)
		if err != nil {
			return nil, fmt.Errorf("passkeys: %w", err)
		}
		f.Passkeys = usecase.NewPasskeys(store.Passkeys(), verifier, f.Sessions, clock)
		log.Info().Str("public_url", cfg.PublicURL).Msg("passkeys enabled")
	} else {
		log.Info().Str("reason", reason).Msg("passkeys disabled")
	}

	// One budget of failed authentications per client, for Git, the API and
	// the web login.
	limiter := clientlimit.NewLimiter(nil)
	// A nil *usecase.Passkeys must not become a non-nil interface: the API
	// tests for nil to answer failed_precondition and to leave the feature out.
	var apiPasskeys ports.PasskeyService
	if f.Passkeys != nil {
		apiPasskeys = f.Passkeys
	}
	var apiOIDC ports.OIDCKeyService // same rule as apiPasskeys
	if f.OIDC != nil {
		apiOIDC = f.OIDC
	}
	var jobArtifacts ports.JobArtifactService
	var artifacts ports.ArtifactService
	if cfg.CIArtifacts {
		jobArtifacts, artifacts = f.CI, f.CI
	}
	f.API = rpc.New(rpc.Config{
		Auth:           f.Auth,
		Repos:          f.Repos,
		Tokens:         f.Tokens,
		Browse:         f.Browse,
		Events:         f.EventLog,
		MergeRequests:  f.MergeRequests,
		Issues:         f.Issues,
		SigningKey:     f.SigningKey,
		TrustedKeys:    f.Signatures,
		Sessions:       f.Sessions,
		Passkeys:       apiPasskeys,
		Runner:         f.CI,
		Runs:           f.CI,
		Secrets:        f.Settings,
		Variables:      f.Settings,
		Environments:   f.Settings,
		OIDCKeys:       apiOIDC,
		JobArtifacts:   jobArtifacts,
		Artifacts:      artifacts,
		Version:        serverVersion(),
		TrustedProxies: cfg.TrustedProxies,
		Limiter:        limiter,
	})

	gh := githttp.New(githttp.Config{
		Auth:           f.Auth,
		Repos:          f.Repos,
		Pushes:         f.Pushes,
		Backend:        git,
		Owner:          cfg.Owner,
		Hooks:          f.hooks,
		TrustedProxies: cfg.TrustedProxies,
		Limiter:        limiter,
		MaxBodyBytes:   maxBody,
	})
	routes := make([]httpserver.Route, 0, 2*len(githttp.Patterns)+len(githubapi.Patterns)+len(oidchttp.Patterns)+len(web.Patterns))
	for _, p := range githttp.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: gh})
	}
	// The owner form of the clone URL (github.repository), which GitHub's
	// actions use. config.Owner follows the rules of repository names, so it is
	// a literal segment that never overlaps /-/.
	for _, p := range githttp.OwnerPatterns(cfg.Owner) {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: gh})
	}
	// The REST subset the actions call on github.api_url.
	api := githubapi.New(githubapi.Config{Auth: f.Auth, API: f.CI, TrustedProxies: cfg.TrustedProxies, Limiter: limiter, MaxArchivesPerClient: opts.archivesPerClient})
	for _, p := range githubapi.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: api})
	}
	// The OIDC provider of CI jobs (issuer <public URL>/-/oidc).
	idp := oidchttp.New(oidchttp.Config{OIDC: f.CI, TrustedProxies: cfg.TrustedProxies, Limiter: limiter})
	for _, p := range oidchttp.Patterns {
		routes = append(routes, httpserver.Route{Pattern: p, Handler: idp})
	}
	warnSessionsNeedHTTPS(ctx, cfg.PublicURL)
	warnSharedClientKey(ctx, cfg.TrustedProxies, githttp.DefaultMaxPerClient)
	site, err := newWeb(cfg, f, limiter)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	routes = append(routes, webRoutes(site)...)
	f.server = httpserver.New(httpserver.Config{
		Addr:         cfg.Addr,
		PublicURL:    cfg.PublicURL,
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
	if f.CI != nil {
		f.CI.Close(ctx)
	}
	if f.EventLog != nil {
		f.EventLog.Close(ctx)
	}
	if f.reader != nil {
		f.reader.Close()
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

// artifacts gives the CI the stores and limits of the artifacts (CI-4 D3):
// records in the kv store, contents under ci/artifacts. ERMITE_CI_ARTIFACTS=0
// gives none, and the API then serves no artifact service.
func artifacts(ci *usecase.CIConfig, cfg config.Config, store *kvstore.Store) {
	if !cfg.CIArtifacts {
		return
	}
	ci.Artifacts, ci.ArtifactFiles = store.Artifacts(), ciartifacts.New(ciartifacts.Dir(cfg.DataDir))
	ci.ArtifactLimits = ports.ArtifactLimits{Retention: cfg.CIArtifactRetention, Quota: cfg.CIArtifactQuota, MaxSize: cfg.CIArtifactMax}
}

// pullRequests gives the CI the merge requests, the refs/pull refs and the
// identity of the test merge commits (CI-3, D5).
func (f *forge) pullRequests(cfg *usecase.CIConfig, store *kvstore.Store, git *gitcli.Git, committer domain.Identity) {
	cfg.MRs, cfg.Pull, cfg.Committer = store.MergeRequests(), git, committer
	if f.reader != nil {
		cfg.Fresh = f.reader // a nil *ermitegit.Adapter must not become a non-nil interface
	}
}

// signatures wires the commit signature checks (ADR 0030): the stored trusted
// keys, the instance key as a builtin one for the committer email, the SSH and
// OpenPGP verifiers, and the raw commit objects of the selected reader.
func (f *forge) signatures(store *kvstore.Store, reader ports.RepoReader, key domain.SigningPublicKey, committer domain.Identity, clock ports.Clock) error {
	objects, ok := reader.(ports.CommitObjectReader)
	if !ok {
		return errors.New("the git reader cannot read raw commit objects")
	}
	builtin := domain.TrustedKey{
		ID: key.Fingerprint, Format: domain.FormatSSH, Title: "ermite instance key",
		PublicKey: key.AuthorizedKey, Emails: []string{strings.ToLower(committer.Email)}, Builtin: true,
	}
	verifiers := map[domain.SignatureFormat]ports.SignatureVerifier{
		domain.FormatSSH: sigverify.SSH{}, domain.FormatGPG: sigverify.GPG{},
	}
	sigs, err := usecase.NewSignatures(store.TrustedKeys(), sigverify.Inspector{}, verifiers, objects, builtin, clock)
	if err != nil {
		return err
	}
	f.Signatures = sigs
	f.Browse.WithVerifier(sigs)
	return nil
}
