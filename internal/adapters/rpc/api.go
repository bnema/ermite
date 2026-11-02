// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rpc serves the ermite API contract (bnema.dev/ermite-api) with
// ConnectRPC: handlers that map the messages of the contract to the use cases
// of usecase and back, the interceptor that authenticates callers, and the error
// mapping.
package rpc

import (
	"context"
	"net/http"
	"net/netip"
	"slices"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/zerowrap"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
)

// MaxRequestBytes bounds the size of a request message. Responses are not
// bounded: a diff has its own limits and blobs are streamed.
const MaxRequestBytes = 1 << 20

// RunnerMaxRequestBytes bounds the size of a request message of RunnerService
// and JobArtifactService: a job summary of exactly 1 MiB (UploadSummary) and a
// block message with 1 MiB of data must be accepted, so the limit leaves room
// for the envelope (job id, step number, block id, framing).
const RunnerMaxRequestBytes = MaxRequestBytes + 64<<10

// CompleteJobMaxRequestBytes bounds the size of a CompleteJob request message,
// which may carry a deps report of up to 8 MiB of file content (bytes fields
// are not inflated on the wire in protobuf binary, but JSON encodes them as
// base64, a 4/3 growth) plus a 1 MiB envelope.
const CompleteJobMaxRequestBytes = 8<<20*4/3 + 1<<20

// Features are the optional capabilities of the contract that this server
// always implements, as GetVersion lists them. FeaturePasskeys comes on top
// when Config.Passkeys is set, and FeatureCISecrets when the settings services
// are, and FeatureCommitSignatures when Config.TrustedKeys is set.
var Features = []string{"repos.archived", "repos.languages", "repo.update", "events.resume", "merge_requests", "merge_requests.delete_branch", "issues", "issues.links", "issues.resolve", "issues.edit", "merge_requests.edit", "merge_requests.mergeability", "signing_key", "session", "git.list_paths", "token.branch_prefixes", "ci.runs", "ci.runs.filters", "ci.runners", "ci.actions", "ci.annotations", "ci.summaries", "ci.concurrency", "ci.dispatch", "ci.cache", "ci.workflows", "merge_requests.auto_merge", "merge_requests.ignore_checks", "merge_requests.ready", "deps.runs", "deps.batches", "deps.npm"}

// FeaturePasskeys is listed by GetVersion when passkeys are available.
// FeatureCIArtifacts (artifact.go) when the artifact services are served.
const FeaturePasskeys = "passkeys"

// FeatureCIOIDC is listed by GetVersion when the server issues OIDC ID tokens
// to CI jobs (Job.id_token_url, ServerService.RotateOIDCKey).
const FeatureCIOIDC = "ci.oidc"

// Config configures the API.
type Config struct {
	// Mirrors serves MirrorService when set; leave unset rather than a typed nil.
	Mirrors ports.MirrorService
	Auth    ports.AuthService
	Repos   ports.RepoService
	Tokens  ports.TokenService
	Browse  ports.BrowseService
	Events  ports.EventService
	// MergeRequests serves MergeRequestService, SigningKey GetSigningKey.
	MergeRequests ports.MergeRequestService
	SigningKey    ports.SigningKeyService
	// Issues serves IssueService.
	Issues ports.IssueService
	// Releases serves ReleaseService when set; leave unset rather than a typed nil.
	Releases ports.ReleaseService
	// IssueModeration serves the moderation calls of IssueService.
	IssueModeration ports.IssueModerationService
	Version         string
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Limiter counts failed authentications; nil builds a default one. Share
	// it with the other adapters that authenticate, so that a client has one
	// budget of failures. Leave it unset rather than assigning a typed nil
	// pointer (a nil *T in the interface is not nil, and would be called).
	Limiter ports.FailureLimiter
	// Runner serves RunnerService and Runs serves RunService; without them the
	// CI services are not served. Leave them unset rather than assigning a
	// typed nil pointer.
	Runner ports.RunnerService
	Runs   ports.RunService
	// Sessions are the owner's web sessions; nil serves no SessionService.
	// Leave it unset rather than assigning a typed nil pointer (a nil *T in
	// the interface is not nil, and the service would be served and called).
	Sessions ports.SessionService
	// Passkeys serves ListPasskeys and RemovePasskey; nil means passkeys are
	// not available (the RPCs answer failed_precondition and GetVersion does
	// not list "passkeys"). Same rule as Sessions about typed nil pointers.
	Passkeys ports.PasskeyService
	// Secrets, Variables and Environments serve SecretService, VariableService
	// and EnvironmentService, and GetVersion lists "ci.secrets", when all three
	// are set (they are one use case). Same rule as Sessions about typed nil
	// pointers.
	Secrets      ports.SecretService
	Variables    ports.VariableService
	Environments ports.EnvironmentService
	// OIDCKeys manages ID-token signing keys, including when OIDC is disabled.
	OIDCKeys ports.OIDCKeyService
	// JobArtifacts and Artifacts serve JobArtifactService and ArtifactService,
	// and GetVersion lists "ci.artifacts", when both are set and Runner is.
	// Same rule as Sessions about typed nil pointers.
	JobArtifacts ports.JobArtifactService
	Artifacts    ports.ArtifactService
	// TrustedKeys serves TrustedKeyService, and GetVersion lists
	// "commit_signatures", when set (ADR 0030). Same rule as Sessions about
	// typed nil pointers.
	TrustedKeys ports.TrustedKeyService
	// Deps serves DepsService (Bernard); GetVersion lists "deps" when set.
	// Same rule as Sessions about typed nil pointers.
	Deps ports.DepsService
	// Journal serves JournalService and ReleaseService.ListUnreleased;
	// GetVersion lists "journal" when it is set together with Releases. Same
	// rule as Sessions about typed nil pointers.
	Journal ports.JournalService
}

// API is the set of Connect handlers of the contract.
type API struct {
	handler http.Handler
}

// New builds the API.
func New(cfg Config) *API {
	if cfg.Limiter == nil {
		cfg.Limiter = clientlimit.NewLimiter(nil)
	}
	opts := []connecthttp.Option{connecthttp.WithReadMaxBytes(MaxRequestBytes)}
	mux := http.NewServeMux()
	server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
	ermiteconnect.RegisterRepoServiceHandler(server, &repoService{cfg.Repos})
	ermiteconnect.RegisterGitServiceHandler(server, &gitService{cfg.Browse})
	ermiteconnect.RegisterTokenServiceHandler(server, &tokenService{cfg.Tokens})
	ermiteconnect.RegisterEventServiceHandler(server, &eventService{cfg.Events})
	connecthttp.Mount(mux, server, opts...)

	features := slices.Clone(Features)
	if cfg.Mirrors != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterMirrorServiceHandler(server, &mirrorService{cfg.Mirrors})
		connecthttp.Mount(mux, server, opts...)
		features = append(features, FeatureMirror)
	}
	if cfg.Passkeys != nil {
		features = append(features, FeaturePasskeys)
	}
	settings := cfg.Secrets != nil && cfg.Variables != nil && cfg.Environments != nil
	if settings {
		features = append(features, FeatureCISecrets)
	}
	artifacts := cfg.JobArtifacts != nil && cfg.Artifacts != nil && cfg.Runner != nil
	if artifacts {
		features = append(features, FeatureCIArtifacts)
	}
	if cfg.Releases != nil {
		features = append(features, FeatureReleases)
	}
	if cfg.OIDCKeys != nil && cfg.OIDCKeys.Enabled() {
		features = append(features, FeatureCIOIDC)
	}
	if cfg.TrustedKeys != nil {
		features = append(features, FeatureCommitSignatures)
	}
	if cfg.Deps != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterDepsServiceHandler(server, &depsService{cfg.Deps})
		connecthttp.Mount(mux, server, opts...)
		features = append(features, FeatureDeps)
	}
	if cfg.Journal != nil && cfg.Releases != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterJournalServiceHandler(server, &journalService{cfg.Journal})
		connecthttp.Mount(mux, server, opts...)
		features = append(features, FeatureJournal)
	} else {
		cfg.Journal = nil // ListUnreleased answers unimplemented without the feature
	}
	srv := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
	ermiteconnect.RegisterServerServiceHandler(srv, &serverService{version: cfg.Version, features: features, key: cfg.SigningKey, oidc: cfg.OIDCKeys})
	ermiteconnect.RegisterMergeRequestServiceHandler(srv, &mergeRequestService{mrs: cfg.MergeRequests})
	ermiteconnect.RegisterIssueServiceHandler(srv, &issueService{issues: cfg.Issues, moderation: cfg.IssueModeration})
	connecthttp.Mount(mux, srv, opts...)

	if cfg.Sessions != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterSessionServiceHandler(server, &sessionService{sessions: cfg.Sessions, passkeys: cfg.Passkeys})
		connecthttp.Mount(mux, server, opts...)
	}
	if cfg.TrustedKeys != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterTrustedKeyServiceHandler(server, &trustedKeyService{cfg.TrustedKeys})
		connecthttp.Mount(mux, server, opts...)
	}
	if settings {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterSecretServiceHandler(server, &secretService{cfg.Secrets})
		ermiteconnect.RegisterVariableServiceHandler(server, &variableService{cfg.Variables})
		ermiteconnect.RegisterEnvironmentServiceHandler(server, &environmentService{cfg.Environments})
		connecthttp.Mount(mux, server, opts...)

	}
	if cfg.Releases != nil {
		releaseOpts := append(slices.Clone(opts), connecthttp.WithReadMaxBytes(RunnerMaxRequestBytes))
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterReleaseServiceHandler(server, &releaseService{rel: cfg.Releases, journal: cfg.Journal})
		connecthttp.Mount(mux, server, releaseOpts...)
	}
	if cfg.Runner != nil {
		runnerOpts := append(slices.Clone(opts), connecthttp.WithReadMaxBytes(RunnerMaxRequestBytes))
		runners := &runnerService{runners: cfg.Runner, limiter: cfg.Limiter}
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterRunnerServiceHandler(server, runners)
		runnerOpts = append(runnerOpts, connecthttp.WithConditionalOptions(func(spec connect.Spec) []connecthttp.Option {
			if spec.Procedure == ermiteconnect.RunnerServiceCompleteJobProcedure {
				return []connecthttp.Option{connecthttp.WithReadMaxBytes(CompleteJobMaxRequestBytes)}
			}
			return nil
		}))
		connecthttp.Mount(mux, server, runnerOpts...)

		if artifacts {
			server :=
				// A block message carries up to 1 MiB of data, like a summary.
				connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)

			ermiteconnect.RegisterJobArtifactServiceHandler(server, &jobArtifactService{artifacts: cfg.JobArtifacts, runner: runners})
			connecthttp.Mount(mux, server, runnerOpts...)
			srv := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
			ermiteconnect.RegisterArtifactServiceHandler(srv, &artifactService{artifacts: cfg.Artifacts})
			connecthttp.Mount(mux, srv, opts...)
		}
	}
	if cfg.Runs != nil {
		server := connect.NewServer(newInterceptor(cfg).intercept, recoverPanic)
		ermiteconnect.RegisterRunServiceHandler(server, &runService{runs: cfg.Runs})
		connecthttp.Mount(mux, server, opts...)
	}
	return &API{handler: mux}
}

// Handler returns the handler of all services. Its paths are the procedure
// paths of the contract ("/ermite.RepoService/ListRepos"); httpserver mounts
// it under /-/api/ and strips the prefix.
func (a *API) Handler() http.Handler { return a.handler }

// caller returns the principal the interceptor put in ctx; anonymous when
// there is none.
func caller(ctx context.Context) domain.Principal {
	p, _ := domain.PrincipalFromContext(ctx)
	return p
}

func recoverPanic(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) (err error) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				log := zerowrap.FromCtx(ctx)
				log.Error().Interface("panic", p).Str("procedure", spec.Procedure).Msg("api handler panicked")
				err = connect.NewError(connect.CodeInternal, "internal error")
			}
		}()
		return next(ctx, spec, stream)
	}
}
