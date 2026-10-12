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

// BrowseService is the read side of a repository's content. *usecase.Browse
// implements it.
type BrowseService interface {
	ListRefs(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListRefsParams) (RefPage, error)
	ResolveRevision(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error)
	GetTree(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error)
	ListPaths(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.PathList, error)
	OpenBlob(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error)
	ListCommits(ctx context.Context, p domain.Principal, repo domain.RepoName, params ListCommitsParams) (HistoryPage, error)
	GetCommit(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.CommitDetail, error)
}

// EventService streams the events of the forge. *usecase.Events implements it.
type EventService interface {
	Watch(ctx context.Context, p domain.Principal, params WatchParams, send func(WatchEvent) error) error
}

// MergeRequestService is the merge request use cases. *usecase.MergeRequests
// implements it.
type MergeRequestService interface {
	Create(ctx context.Context, p domain.Principal, name domain.RepoName, params CreateMergeRequestParams) (domain.MergeRequest, error)
	List(ctx context.Context, p domain.Principal, name domain.RepoName, params ListMergeRequestsParams) (MergeRequestPage, error)
	Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, sinceLastRead bool) (MergeRequestDetail, error)
	AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params AddCommentParams) (domain.Comment, error)
	ResolveComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64, resolved bool) (domain.Comment, error)
	Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error)
	MarkRead(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, head domain.ObjectID) (domain.ObjectID, error)
	Merge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params MergeParams) (MergeResult, error)
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
	Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error
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
