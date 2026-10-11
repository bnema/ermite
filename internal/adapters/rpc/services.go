// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"
	"io"
	"time"

	"bnema.dev/ermite/internal/app"
	"bnema.dev/ermite/internal/domain"
)

// The interfaces below are what the handlers need of the use cases, declared
// where they are used. The types of app fit them; the mocks in rpc/mocks
// stand in for them in tests.

// Repos is the repository use cases the API serves. *app.Repos fits.
type Repos interface {
	ListPage(ctx context.Context, p domain.Principal, size int, token string) (app.RepoPage, error)
	View(ctx context.Context, repo domain.Repo) (app.RepoView, error)
	Get(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error)
	Create(ctx context.Context, p domain.Principal, params app.CreateParams) (domain.Repo, error)
	Update(ctx context.Context, p domain.Principal, name domain.RepoName, params app.UpdateParams) (domain.Repo, error)
	Delete(ctx context.Context, p domain.Principal, name domain.RepoName) error
}

// Tokens is the access token use cases the API serves. *app.Tokens fits.
type Tokens interface {
	Create(ctx context.Context, p domain.Principal, params app.CreateTokenParams) (app.CreatedToken, error)
	ListPage(ctx context.Context, p domain.Principal, size int, token string) (app.TokenPage, error)
	Revoke(ctx context.Context, p domain.Principal, id string) error
}

// Browse is the read side of a repository's content. *app.Browse fits.
type Browse interface {
	ListRefs(ctx context.Context, p domain.Principal, repo domain.RepoName, params app.ListRefsParams) (app.RefPage, error)
	ResolveRevision(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.ResolvedRevision, error)
	GetTree(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath, includeLastCommit bool) (domain.Tree, error)
	OpenBlob(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision, path domain.TreePath) (domain.BlobInfo, io.ReadCloser, error)
	ListCommits(ctx context.Context, p domain.Principal, repo domain.RepoName, params app.ListCommitsParams) (app.CommitPage, error)
	GetCommit(ctx context.Context, p domain.Principal, repo domain.RepoName, rev domain.Revision) (domain.CommitDetail, error)
}

// Events streams the events of the forge. *app.Events fits.
type Events interface {
	Watch(ctx context.Context, p domain.Principal, params app.WatchParams, send func(app.WatchEvent) error) error
}

// MergeRequests is the merge request use cases. *app.MergeRequests fits.
type MergeRequests interface {
	Create(ctx context.Context, p domain.Principal, name domain.RepoName, params app.CreateMergeRequestParams) (domain.MergeRequest, error)
	List(ctx context.Context, p domain.Principal, name domain.RepoName, params app.ListMergeRequestsParams) (app.MergeRequestPage, error)
	Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, sinceLastRead bool) (app.MergeRequestDetail, error)
	AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params app.AddCommentParams) (domain.Comment, error)
	ResolveComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64, resolved bool) (domain.Comment, error)
	Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error)
	MarkRead(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, head domain.ObjectID) (domain.ObjectID, error)
	Merge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params app.MergeParams) (app.MergeResult, error)
}

// SigningKey tells which key signs the commits of the forge. *app.SigningKey
// fits.
type SigningKey interface {
	Info() domain.SigningKeyInfo
}

// Sessions is the owner's web session use cases the API serves.
// *app.Sessions fits.
type Sessions interface {
	CreateLoginLink(ctx context.Context, p domain.Principal) (app.LoginLink, error)
	SignOutEverywhere(ctx context.Context, p domain.Principal) error
}

// FailureLimiter counts the failed authentications per client. It is shared
// with the other adapters that authenticate, so that a client has one budget
// of failures. *clientlimit.Limiter fits.
type FailureLimiter interface {
	Blocked(ctx context.Context, addr string) (bool, time.Duration)
	Fail(ctx context.Context, addr string)
}
