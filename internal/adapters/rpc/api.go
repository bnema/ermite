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

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaxRequestBytes bounds the size of a request message. Responses are not
// bounded: a diff has its own limits and blobs are streamed.
const MaxRequestBytes = 1 << 20

// RunnerMaxRequestBytes bounds the size of a request message of RunnerService
// and JobArtifactService: a job summary of exactly 1 MiB (UploadSummary) and a
// block message with 1 MiB of data must be accepted, so the limit leaves room
// for the envelope (job id, step number, block id, framing).
const RunnerMaxRequestBytes = MaxRequestBytes + 64<<10

// Features are the optional capabilities of the contract that this server
// always implements, as GetVersion lists them. FeaturePasskeys comes on top
// when Config.Passkeys is set, and FeatureCISecrets when the settings services
// are, and FeatureCommitSignatures when Config.TrustedKeys is set.
var Features = []string{"repo.update", "events.resume", "merge_requests", "merge_requests.delete_branch", "issues", "issues.links", "merge_requests.mergeability", "signing_key", "session", "git.list_paths", "token.branch_prefixes", "ci.runs", "ci.runs.filters", "ci.runners", "ci.actions", "ci.annotations", "ci.summaries", "ci.concurrency", "ci.dispatch", "ci.cache", "ci.workflows"}

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
	opts := []connect.HandlerOption{
		connect.WithInterceptors(newInterceptor(cfg)),
		connect.WithReadMaxBytes(MaxRequestBytes),
		connect.WithRecover(func(ctx context.Context, spec connect.Spec, _ http.Header, panicValue any) error {
			log := zerowrap.FromCtx(ctx)
			log.Error().Interface("panic", panicValue).Str("procedure", spec.Procedure).Msg("api handler panicked")
			return connect.NewError(connect.CodeInternal, errInternal)
		}),
	}
	mux := http.NewServeMux()
	mux.Handle(ermiteconnect.NewRepoServiceHandler(&repoService{cfg.Repos}, opts...))
	mux.Handle(ermiteconnect.NewGitServiceHandler(&gitService{cfg.Browse}, opts...))
	mux.Handle(ermiteconnect.NewTokenServiceHandler(&tokenService{cfg.Tokens}, opts...))
	mux.Handle(ermiteconnect.NewEventServiceHandler(&eventService{cfg.Events}, opts...))
	features := slices.Clone(Features)
	if cfg.Mirrors != nil {
		mux.Handle(ermiteconnect.NewMirrorServiceHandler(&mirrorService{cfg.Mirrors}, opts...))
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
	if cfg.OIDCKeys != nil && cfg.OIDCKeys.Enabled() {
		features = append(features, FeatureCIOIDC)
	}
	if cfg.TrustedKeys != nil {
		features = append(features, FeatureCommitSignatures)
	}
	mux.Handle(ermiteconnect.NewServerServiceHandler(&serverService{version: cfg.Version, features: features, key: cfg.SigningKey, oidc: cfg.OIDCKeys}, opts...))
	mux.Handle(ermiteconnect.NewMergeRequestServiceHandler(&mergeRequestService{mrs: cfg.MergeRequests}, opts...))
	mux.Handle(ermiteconnect.NewIssueServiceHandler(&issueService{issues: cfg.Issues, moderation: cfg.IssueModeration}, opts...))
	if cfg.Sessions != nil {
		mux.Handle(ermiteconnect.NewSessionServiceHandler(&sessionService{sessions: cfg.Sessions, passkeys: cfg.Passkeys}, opts...))
	}
	if cfg.TrustedKeys != nil {
		mux.Handle(ermiteconnect.NewTrustedKeyServiceHandler(&trustedKeyService{cfg.TrustedKeys}, opts...))
	}
	if settings {
		mux.Handle(ermiteconnect.NewSecretServiceHandler(&secretService{cfg.Secrets}, opts...))
		mux.Handle(ermiteconnect.NewVariableServiceHandler(&variableService{cfg.Variables}, opts...))
		mux.Handle(ermiteconnect.NewEnvironmentServiceHandler(&environmentService{cfg.Environments}, opts...))
	}
	if cfg.Runner != nil {
		runnerOpts := append(slices.Clone(opts), connect.WithReadMaxBytes(RunnerMaxRequestBytes))
		runners := &runnerService{runners: cfg.Runner, limiter: cfg.Limiter}
		mux.Handle(ermiteconnect.NewRunnerServiceHandler(runners, runnerOpts...))
		if artifacts {
			// A block message carries up to 1 MiB of data, like a summary.
			mux.Handle(ermiteconnect.NewJobArtifactServiceHandler(&jobArtifactService{artifacts: cfg.JobArtifacts, runner: runners}, runnerOpts...))
			mux.Handle(ermiteconnect.NewArtifactServiceHandler(&artifactService{artifacts: cfg.Artifacts}, opts...))
		}
	}
	if cfg.Runs != nil {
		mux.Handle(ermiteconnect.NewRunServiceHandler(&runService{runs: cfg.Runs}, opts...))
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
