// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"errors"
	"io"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// The interfaces below are the use cases as the inbound adapters (the API,
// Git over HTTP, the web) and the composition root see them. The types of package
// usecase implement them, each assertion being made in usecase; the mocks in
// ports/mocks stand in for them in the tests of the adapters.

// AuthService turns the secret of a request into a principal. It returns
// domain.ErrUnauthenticated for a secret that is not valid. *usecase.Auth
// implements it.
type AuthService interface {
	Authenticate(ctx context.Context, secret string) (domain.Principal, error)
}

// Revalidator confirms that a principal obtained earlier is still
// authorized. *usecase.Auth implements it.
type Revalidator interface {
	Revalidate(ctx context.Context, p domain.Principal) error
}

// RepoLookup is the read side of a single repository. A repository the
// principal may not read is domain.ErrNotFound. *usecase.Repos implements it, as
// part of RepoService.
type RepoLookup interface {
	Get(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error)
}

// RepoService is the repository use cases the API serves. *usecase.Repos
// implements it.
type RepoService interface {
	RepoLookup
	ListPage(ctx context.Context, p domain.Principal, size int, token string) (RepoPage, error)
	View(ctx context.Context, repo domain.Repo) (RepoView, error)
	Create(ctx context.Context, p domain.Principal, params CreateRepoParams) (domain.Repo, error)
	Update(ctx context.Context, p domain.Principal, name domain.RepoName, params UpdateRepoParams) (domain.Repo, error)
	Delete(ctx context.Context, p domain.Principal, name domain.RepoName) error
}

// TokenService is the access token use cases the API serves. *usecase.Tokens
// implements it.
type TokenService interface {
	Create(ctx context.Context, p domain.Principal, params CreateTokenParams) (CreatedToken, error)
	ListPage(ctx context.Context, p domain.Principal, size int, token string) (TokenPage, error)
	Revoke(ctx context.Context, p domain.Principal, id string) error
}

// GitHubAPIService is the subset of GitHub's REST API that ermite serves under
// /-/github/api, for the actions that run on it (ADR 0025, CI-2). owner is the
// owner of the URL: only ERMITE_OWNER names repositories, any other owner is
// domain.ErrNotFound, as is a repository the principal may not read.
// *usecase.CI implements it.
type GitHubAPIService interface {
	// GetRepo returns the JSON of GET /repos/{owner}/{repo}.
	GetRepo(ctx context.Context, p domain.Principal, owner, repo string) ([]byte, error)
	// HashAlgorithm returns the object format of the repository, "sha1".
	HashAlgorithm(ctx context.Context, p domain.Principal, owner, repo string) (string, error)
	// Tarball resolves ref (a branch, a tag or a commit id; empty means the
	// default branch) and writes the gzip-compressed tar of the commit to the
	// writer that begin returns; the archive token of an action (see
	// domain.RepoTokenPrincipal) gets the commit it was issued for and no other.
	// begin is called once, with the file name of the
	// archive, before the first byte and not at all when the ref does not
	// resolve (domain.ErrNotFound).
	Tarball(ctx context.Context, p domain.Principal, owner, repo, ref string, begin func(filename string) io.Writer) error
	// TarballName is Tarball for a HEAD request: the same access and the same
	// resolution of ref, then the file name, without producing the archive.
	TarballName(ctx context.Context, p domain.Principal, owner, repo, ref string) (string, error)
	// ListPullFiles answers GET /repos/{owner}/{repo}/pulls/{number}/files: the
	// JSON array of the files of page (1-based) of perPage files (1 to 100) of
	// the merge request, and the Link header value ("" for a single page). A
	// public repository is read by anyone; a private one by a principal that
	// reads it. The job token of the repository needs pull-requests: read; one
	// that reads the repository without it gets domain.ErrForbidden (GitHub: 403
	// "Resource not accessible by integration").
	ListPullFiles(ctx context.Context, p domain.Principal, owner, repo string, number int64, page, perPage int) (body []byte, link string, err error)
}

// BrowseService is the read side of a repository's content. *usecase.Browse
// implements it.
type BrowseService interface {
	ListRefs(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListRefsParams) (RefPage, error)
	ResolveRevision(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error)
	GetTree(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error)
	ListPaths(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.PathList, error)
	OpenBlob(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error)
	ListCommits(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListCommitsParams) (HistoryPage, error)
	GetCommit(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, opts GetCommitOptions) (domain.CommitDetail, error)
	// RepoStats counts the commits of the branch whose head is given, and
	// names the latest tag.
	RepoStats(ctx context.Context, p domain.Principal, repo domain.RepoName, head domain.ObjectID, now time.Time) (RepoStats, error)
}

// EventService streams the events of the forge. *usecase.Events implements it.
type EventService interface {
	Watch(ctx context.Context, p domain.Principal, params WatchParams, send func(WatchEvent) error) error
}

// MergeRequestService is the merge request use cases. *usecase.MergeRequests
// implements it.
type MergeRequestService interface {
	// Compare queries repository history; it stays here because this service owns the merge-git port.
	Compare(ctx context.Context, p domain.Principal, repo domain.RepoName, target, source domain.ObjectID) (CompareResult, error)
	// PushSummary queries repository history; it stays here because this service owns the merge-git port.
	PushSummary(ctx context.Context, p domain.Principal, repo domain.RepoName, oldHead, newTip domain.ObjectID) (PushSummary, error)
	Create(ctx context.Context, p domain.Principal, name domain.RepoName, params CreateMergeRequestParams) (domain.MergeRequest, error)
	List(ctx context.Context, p domain.Principal, name domain.RepoName, params ListMergeRequestsParams) (MergeRequestPage, error)
	Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, opts GetMergeRequestOptions) (MergeRequestDetail, error)
	AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params AddCommentParams) (domain.Comment, error)
	EditComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64, text string) (domain.Comment, error)
	DeleteComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64) (domain.Comment, error)
	ResolveComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64, resolved bool) (domain.Comment, error)
	Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error)
	MarkRead(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, head domain.ObjectID) (domain.ObjectID, error)
	Merge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params MergeParams) (MergeResult, error)
	// MarkReady takes a draft out of draft: its title loses its "WIP:" prefix.
	MarkReady(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error)
	// EnableAutoMerge sets an open merge request to merge itself once its
	// checks pass; enabling again replaces the mode and the head.
	EnableAutoMerge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params EnableAutoMergeParams) (domain.MergeRequest, error)
	// CancelAutoMerge turns auto-merge off on an open merge request.
	CancelAutoMerge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error)
	// CountOpen is the number of open merge requests of a repository the
	// principal can read. It reads no Git: the tab of every page shows it.
	CountOpen(ctx context.Context, p domain.Principal, name domain.RepoName) (int, error)
	// Mergeability is the mergeability of an open merge request, "checking"
	// until the server has a result for the current tips; zero for a request
	// that is not open. It reads no diff: the live pages call it on each change.
	Mergeability(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Mergeability, error)
}

// IssueService is the issue use cases. *usecase.Issues implements it.
type IssueService interface {
	Create(ctx context.Context, p domain.Principal, name domain.RepoName, params CreateIssueParams) (domain.Issue, error)
	List(ctx context.Context, p domain.Principal, name domain.RepoName, params ListIssuesParams) (IssuePage, error)
	// Get returns the issue with its comments, oldest first.
	Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (IssueDetail, error)
	AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, text string) (domain.IssueComment, error)
	Resolve(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error)
	Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error)
	Reopen(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Issue, error)
}

// IssueModerationService is the owner's moderation of the pending issues.
// Every call needs owner rights and answers domain.ErrNotFound otherwise.
// *usecase.IssueModeration implements it.
type IssueModerationService interface {
	// ListPending lists the pending issues of a repository, or of every
	// repository when repo is zero, oldest first.
	ListPending(ctx context.Context, p domain.Principal, repo domain.RepoName, page ListPendingParams) (PendingIssuePage, error)
	// Approve opens the pending issue under a new number.
	Approve(ctx context.Context, p domain.Principal, repo domain.RepoName, id domain.PendingIssueID) (domain.Issue, error)
	// Reject deletes the pending issue and, with ban, bans its submitter.
	Reject(ctx context.Context, p domain.Principal, repo domain.RepoName, id domain.PendingIssueID, ban bool) error
}

// IssueChallenge is what the public form needs to submit: a signed token, the
// proof-of-work to solve and the timing.
type IssueChallenge struct {
	// Token is the signed challenge, sent back untouched.
	Token string
	// Nonce is the base64url of the 16 random bytes the browser hashes:
	// SHA-256(nonce bytes || ":" || decimal counter) must have Difficulty
	// leading zero bits.
	Nonce string
	// Difficulty is the number of leading zero bits required.
	Difficulty int
	// FormToken is a hidden field of the form, sent back untouched.
	FormToken string
	// IssuedAt, MinSubmitAt and ExpiresAt bound when the form may be sent.
	IssuedAt, MinSubmitAt, ExpiresAt time.Time
}

// IssueSubmission is what the browser sends back.
type IssueSubmission struct {
	Token string
	// Solution is the decimal counter that solves the proof of work.
	Solution  string
	FormToken string
	// Honeypot must be empty: a field only a bot fills.
	Honeypot            string
	Title, Body, Author string
}

// IssueSubmissionService is the anonymous issue submission. Both calls answer
// domain.ErrNotFound for a repository that is unknown, not public or without
// public issues. Submit refuses with an error matching
// domain.ErrSubmissionRefused (ErrSubmissionRateLimited, ErrSubmissionsPaused,
// ErrSubmissionInvalid) or with a *domain.TextError the user can fix.
// addr is the client key (clientlimit.Key of the address).
// *usecase.IssueSubmissions implements it.
type IssueSubmissionService interface {
	Challenge(ctx context.Context, repo domain.RepoName, addr string) (IssueChallenge, error)
	Submit(ctx context.Context, repo domain.RepoName, addr string, f IssueSubmission) error
}

// IssueNumberResolver tells whether a number of a repository names an issue or
// a merge request, for the callers that follow a "#n" reference. *usecase.Issues
// implements it.
type IssueNumberResolver interface {
	// Kind needs the access of reading the repository and answers
	// domain.ErrNotFound otherwise. Issues are hidden when disabled; merge
	// requests are resolved even when issues are off.
	Kind(ctx context.Context, p domain.Principal, name domain.RepoName, n int64) (domain.NumberKind, error)
}

// InboxService is the owner's inbox: what is open across every repository.
// *usecase.Inbox implements it.
type InboxService interface {
	// Overview needs owner rights and answers domain.ErrNotFound to anyone
	// else. The filter, when not zero, restricts the lists to that repository.
	Overview(ctx context.Context, p domain.Principal, filter domain.RepoName) (InboxOverview, error)
}

// SigningKeyService tells which key signs the commits of the forge.
// *usecase.SigningKey implements it.
type SigningKeyService interface {
	Info() domain.SigningKeyInfo
}

// SessionService is the owner's web session use cases the API serves.
// *usecase.Sessions implements it.
type SessionService interface {
	CreateLoginLink(ctx context.Context, p domain.Principal) (LoginLink, error)
	SignOutEverywhere(ctx context.Context, p domain.Principal) error
}

// ErrLinkRejected is what WebSessionService.Login returns for a link that is
// unknown, expired or already used.
var ErrLinkRejected = errors.New("login link rejected")

// WebSessionService is the session side of the use cases, as the web needs
// it. The composition root implements it over *usecase.Sessions.
type WebSessionService interface {
	// LoginLinkValid reports whether the link is pending, without using it.
	LoginLinkValid(nonce string) bool
	// Login uses the link and returns the session token and how long it
	// lasts, or ErrLinkRejected.
	Login(ctx context.Context, nonce string) (token string, lifetime time.Duration, err error)
	// Verify checks a session token. When it is valid it returns the owner,
	// the principal the web passes to the use cases for that request; when it
	// is not, ok is false and err nil. An error means the check could not be
	// made (the request then runs as anonymous).
	Verify(ctx context.Context, token string) (owner domain.Principal, ok bool, err error)
}

// PasskeyService is the owner's passkey use cases the API serves.
// *usecase.Passkeys implements it. Registering a passkey and signing in with
// one are browser ceremonies and are not part of it.
type PasskeyService interface {
	// List returns the passkeys, oldest first.
	List(ctx context.Context, p domain.Principal) ([]domain.Passkey, error)
	// Remove deletes the passkey with that credential id, or returns
	// domain.ErrNotFound, and signs every browser out.
	Remove(ctx context.Context, p domain.Principal, id []byte) error
}

// WebPasskeyService is the passkey side of the use cases, as the web needs
// it: the API's List and Remove, and the browser ceremonies. The composition
// root implements it over *usecase.Passkeys. A ceremony is the opaque
// identifier BeginLogin and BeginRegistration return; options is the JSON the
// browser passes to the WebAuthn API.
type WebPasskeyService interface {
	PasskeyService
	// BeginLogin starts a sign-in. Anonymous callers may. client is the client
	// key of the request (clientlimit.Key of the address the failure limiter
	// uses); it bounds how many sign-ins a client keeps waiting.
	BeginLogin(ctx context.Context, client string) (ceremony string, options []byte, err error)
	// FinishLogin checks the browser's response and returns the session token
	// and how long it lasts, or ErrLinkRejected for any refusal.
	FinishLogin(ctx context.Context, ceremony string, response []byte) (token string, lifetime time.Duration, err error)
	// BeginRegistration starts adding a passkey. Only the owner may. name is
	// the name the passkey will get (empty means the default): a name that
	// breaks the rules is refused here, before any ceremony starts, and is the
	// one FinishRegistration stores.
	BeginRegistration(ctx context.Context, p domain.Principal, name string) (ceremony string, options []byte, err error)
	// FinishRegistration checks the browser's response and stores the passkey
	// under the name given to BeginRegistration.
	FinishRegistration(ctx context.Context, p domain.Principal, ceremony string, response []byte) error
}

// PushService opens a repository for a push: it is domain.ErrUnauthenticated
// for an anonymous caller, domain.ErrNotFound for a repository the caller may
// not write and cannot see (or that is missing and may not be created),
// domain.ErrForbidden for one it can see but not write. With create, a
// missing repository the principal may write is created. *usecase.Pushes
// implements it.
type PushService interface {
	Open(ctx context.Context, p domain.Principal, name domain.RepoName, create bool) error
}

// PushHookService is what the hook socket asks of the use cases.
// *usecase.Pushes implements it.
type PushHookService interface {
	// Check decides a push before Git applies it; an error matching
	// domain.ErrRejected refuses it, any other error refuses it as a failure.
	// objects are the objects of the push in progress (the quarantine of git).
	Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate, objects PushObjects) error
	// Record handles an accepted push.
	Record(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error
}

// MaintenanceScheduler queues repository maintenance. *usecase.Maintainer
// implements it.
type MaintenanceScheduler interface {
	Schedule(ctx context.Context, name domain.RepoName)
}

// HookTokens hands out the per-request tokens the hooks of a push present
// back to ermite, and the socket they present them on. The Git HTTP adapter
// implements it.
type HookTokens interface {
	// Register binds a new token to the repository and principal of a push.
	// release revokes it and must be called when the request ends.
	Register(ctx context.Context, repo domain.RepoName, p domain.Principal) (token string, release func())
	// Socket is the path of the hook socket.
	Socket() string
}

// FailureLimiter counts the failed authentications per client. The adapters
// that authenticate share one, so that a client has one budget of failures.
// *clientlimit.Limiter implements it.
type FailureLimiter interface {
	Blocked(ctx context.Context, addr string) (bool, time.Duration)
	Fail(ctx context.Context, addr string)
}

// SubmissionLimiter limits the submissions of the public issue form per
// client. *clientlimit.SubmissionLimiter implements it.
type SubmissionLimiter interface {
	// Reserve atomically checks the limits for addr and, if it may submit,
	// counts a submission. When it may not, retryAfter says when to retry.
	// release refunds the reservation (only when the submission was not
	// stored); it is never nil and safe to call once.
	Reserve(ctx context.Context, addr string) (ok bool, retryAfter time.Duration, release func())
	// Recent returns the submissions of the last hour from addr and from all
	// clients.
	Recent(ctx context.Context, addr string) (perAddr, global int)
}

// SecretPage is one page of secrets.
type SecretPage struct {
	Secrets       []domain.Secret
	NextPageToken string
}

// VariablePage is one page of variables.
type VariablePage struct {
	Variables     []domain.Variable
	NextPageToken string
}

// EnvironmentPage is one page of environments.
type EnvironmentPage struct {
	Environments  []domain.Environment
	NextPageToken string
}

// SecretService is the secret use cases the API serves. They need the
// secrets:write scope, never on a token limited to branches. Values are never
// returned. *usecase.CISettings implements it.
type SecretService interface {
	SetSecret(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name, value string) (domain.Secret, error)
	ListSecrets(ctx context.Context, p domain.Principal, scope domain.SettingsScope, size int, token string) (SecretPage, error)
	DeleteSecret(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) error
}

// VariableService is the variable use cases the API serves, with the access
// rules of SecretService. *usecase.CISettings implements it.
type VariableService interface {
	SetVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name, value string) (domain.Variable, error)
	GetVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) (domain.Variable, error)
	ListVariables(ctx context.Context, p domain.Principal, scope domain.SettingsScope, size int, token string) (VariablePage, error)
	DeleteVariable(ctx context.Context, p domain.Principal, scope domain.SettingsScope, name string) error
}

// EnvironmentService is the environment use cases the API serves, with the
// access rules of SecretService. *usecase.CISettings implements it.
type EnvironmentService interface {
	CreateEnvironment(ctx context.Context, p domain.Principal, repo domain.RepoName, name string) (domain.Environment, error)
	ListEnvironments(ctx context.Context, p domain.Principal, repo domain.RepoName, size int, token string) (EnvironmentPage, error)
	DeleteEnvironment(ctx context.Context, p domain.Principal, repo domain.RepoName, name string) error
}

// GitHubReleaseAPI is the Releases part of the REST subset. owner/repo follow
// GitHubAPIService; bodies are GitHub JSON. *usecase.ReleasesGitHub implements it.
type GitHubReleaseAPI interface {
	CreateRelease(ctx context.Context, p domain.Principal, owner, repo string, body []byte) ([]byte, error)
	GetRelease(ctx context.Context, p domain.Principal, owner, repo string, ref ReleaseRef) ([]byte, error)
	ListReleases(ctx context.Context, p domain.Principal, owner, repo string, page, perPage int) (body []byte, link string, err error)
	UpdateRelease(ctx context.Context, p domain.Principal, owner, repo string, id int64, body []byte) ([]byte, error)
	DeleteRelease(ctx context.Context, p domain.Principal, owner, repo string, id int64) error
	ListAssets(ctx context.Context, p domain.Principal, owner, repo string, id int64, page, perPage int) (body []byte, link string, err error)
	UploadAsset(ctx context.Context, p domain.Principal, owner, repo string, id int64, name, label, contentType string, size int64, r io.Reader) ([]byte, error)
	GetAsset(ctx context.Context, p domain.Principal, owner, repo string, assetID int64) ([]byte, error)
	UpdateAsset(ctx context.Context, p domain.Principal, owner, repo string, assetID int64, body []byte) ([]byte, error)
	DeleteAsset(ctx context.Context, p domain.Principal, owner, repo string, assetID int64) error
	// AssetByID resolves metadata for a download through ReleaseService.
	AssetByID(ctx context.Context, p domain.Principal, owner, repo string, assetID int64) (domain.ReleaseAsset, error)
}
