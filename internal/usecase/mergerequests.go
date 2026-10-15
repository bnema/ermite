// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaxMergeRequestCommits is the most commits GetMergeRequest returns.
const MaxMergeRequestCommits = 100

// maxWriteRetries bounds the retries of a write that loses a race with another
// write of the same merge request. Each lost race means another writer won, so
// a writer loses at most as many times as there are other writers at once; the
// retry is a read and a write of ermite-kv, cheap enough for a bound this high.
const maxWriteRetries = 32

// MaxCommentsPerMergeRequest is the most comments a merge request holds. Past
// it AddComment fails with domain.ErrResourceExhausted (resource_exhausted in
// the API). Comments are never deleted, so the limit counts the ones ever added.
const MaxCommentsPerMergeRequest = 1000

const mrTokenPrefix = "mp1:"

// MergeRequests are the merge request use cases (ADR 0017): review, comment,
// close and merge a branch. Reading needs the access of reading the
// repository: a repository the principal may not read, a missing one and an
// unknown number all answer domain.ErrNotFound. Writing needs mr:write, and
// merging mr:merge, on the repository (see scopeAccess), and that scope also
// makes a private repository visible for the action (see writable).
type MergeRequests struct {
	meta      ports.RepoMetaStore
	store     ports.MergeRequestStore
	repos     ports.GitRepos
	git       ports.MergeGit
	signer    ports.CommitSigner
	committer domain.Identity
	bus       ports.EventBus
	clock     ports.Clock
	maint     ports.MaintenanceScheduler
	ci        ports.WorkflowTrigger    // nil: no CI
	checks    ports.CIChecksReader     // nil: no CI, so no check blocks a merge
	pulls     ports.PullRequestTrigger // nil: no pull_request workflows

	mu    sync.Mutex
	locks map[domain.RepoName]chan struct{}
}

var (
	_ ports.MergeRequestService = (*MergeRequests)(nil)
	_ ports.PullGate            = (*MergeRequests)(nil)
)

// MergeRequestsConfig holds what the merge request use cases need. Every field
// is required.
type MergeRequestsConfig struct {
	Meta  ports.RepoMetaStore
	Store ports.MergeRequestStore
	// Repos gives the default branch of a repository.
	Repos ports.GitRepos
	Git   ports.MergeGit
	// Signer signs the squash commits.
	Signer ports.CommitSigner
	// Committer is recorded as the committer of the squash commits.
	Committer domain.Identity
	Bus       ports.EventBus
	Clock     ports.Clock
	// Maint queues the maintenance that follows a merge, as it does a push.
	Maint ports.MaintenanceScheduler
	// CI starts the workflows a merge triggers (GitHub runs "push" workflows
	// on a merge) and Checks gives the checks the merge rule reads. Both are
	// optional: without them there is no CI.
	CI     ports.WorkflowTrigger
	Checks ports.CIChecksReader
	// PullRequests starts the pull_request workflows when a request is opened,
	// closed or merged (the push of its source branch reaches CI instead).
	// Optional.
	PullRequests ports.PullRequestTrigger
}

// NewMergeRequests builds the merge request use cases.
func NewMergeRequests(cfg MergeRequestsConfig) *MergeRequests {
	return &MergeRequests{
		meta: cfg.Meta, store: cfg.Store, repos: cfg.Repos, git: cfg.Git, signer: cfg.Signer,
		committer: cfg.Committer, bus: cfg.Bus, clock: cfg.Clock, maint: cfg.Maint, ci: cfg.CI, checks: cfg.Checks, pulls: cfg.PullRequests,
		locks: make(map[domain.RepoName]chan struct{}),
	}
}

// lockRepo takes the merge lock of a repository, waiting until ctx ends. The
// merges of one repository, and the creation of its merge requests, run one at
// a time. The returned function releases the lock.
func (m *MergeRequests) lockRepo(ctx context.Context, name domain.RepoName) (unlock func(), err error) {
	m.mu.Lock()
	ch, ok := m.locks[name]
	if !ok {
		ch = make(chan struct{}, 1)
		m.locks[name] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readable looks the repository up and applies the read rule: reading merge
// requests needs the access of reading the repository (canRead). repo:write
// alone does not read them, as it does not fetch a private repository either.
func (m *MergeRequests) readable(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, m.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) {
		return domain.Repo{}, domain.ErrNotFound
	}
	return repo, nil
}

// writable looks the repository up for an action that needs a merge request
// scope. A principal whose token holds that scope on the repository sees the
// repository for this action even without repo:read, so that the agent preset
// (repo:write, mr:write) can open a request on the private repository it just
// pushed. A principal with neither repo:read nor the scope, and an unknown
// repository, still get domain.ErrNotFound; one that can read the repository
// but lacks the scope gets the error of scopeAccess.
func (m *MergeRequests) writable(ctx context.Context, p domain.Principal, name domain.RepoName, scope domain.Scope) (domain.Repo, error) {
	repo, err := lookupRepo(ctx, m.meta, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if !canRead(p, repo) && !p.Allows(scope, name) {
		return domain.Repo{}, domain.ErrNotFound
	}
	if err := scopeAccess(p, repo, scope); err != nil {
		return domain.Repo{}, err
	}
	return repo, nil
}

// withHead fills HeadID of an open request from its source branch: zero when
// the branch is gone. Merged and closed requests keep the head they stored.
func (m *MergeRequests) withHead(ctx context.Context, mr domain.MergeRequest) (domain.MergeRequest, error) {
	if !mr.IsOpen() {
		return mr, nil
	}
	head, err := m.git.BranchHead(ctx, mr.Repo, mr.Source)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		mr.HeadID = domain.ObjectID{}
	case err != nil:
		return domain.MergeRequest{}, err
	default:
		mr.HeadID = head
	}
	return mr, nil
}

// branchOrZero returns the head of a branch, the zero id when it is gone.
func (m *MergeRequests) branchOrZero(ctx context.Context, repo domain.RepoName, branch string) (domain.ObjectID, error) {
	id, err := m.git.BranchHead(ctx, repo, branch)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ObjectID{}, nil
	}
	return id, err
}

// Create opens a merge request and publishes MergeRequestOpened. The principal
// needs mr:write. A request that breaks a rule of ADR 0017 (the texts, a
// branch that does not exist, source and target the same) is rejected whole
// with an *domain.InvalidFieldsError naming the fields. An open request with
// the same source and target gives domain.ErrAlreadyExists.
func (m *MergeRequests) Create(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.CreateMergeRequestParams) (domain.MergeRequest, error) {
	repo, err := m.writable(ctx, p, name, domain.ScopeMRWrite)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	var v domain.Violations
	title, err := domain.ParseMRTitle(params.Title)
	if err != nil {
		v.AddErr("title", err)
	}
	desc, err := domain.ParsePlainText(params.Description)
	if err != nil {
		v.AddErr("description", err)
	}
	// Under the lock, so that two requests for the same pair cannot both pass
	// the check for an existing one, and a merge that deletes its source
	// cannot remove a branch between its check here and the creation.
	unlock, err := m.lockRepo(ctx, name)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	defer unlock()
	source, target, err := m.checkBranches(ctx, name, params, &v)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	if err := v.Err(); err != nil {
		return domain.MergeRequest{}, err
	}
	existing, err := m.store.List(ctx, name)
	if err != nil {
		return domain.MergeRequest{}, fmt.Errorf("list merge requests of %q: %w", name, err)
	}
	for _, e := range existing {
		if e.IsOpen() && e.Source == source && e.Target == target {
			return domain.MergeRequest{}, fmt.Errorf("%w: !%d already merges %s into %s", domain.ErrAlreadyExists, e.Number, source, target)
		}
	}

	now := m.clock.Now().UTC()
	mr, err := m.store.Create(ctx, domain.MergeRequest{
		Repo: name, Title: title, Description: desc, Source: source, Target: target,
		State: domain.MRStateOpen, Author: authorOf(p), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.MergeRequest{}, fmt.Errorf("create merge request of %q: %w", name, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Int64("number", mr.Number).
		Str("source", source).Str("target", target).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("merge request opened")
	mr, err = m.withHead(ctx, mr)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	m.bus.Publish(ctx, domain.MergeRequestOpened{MergeRequest: mr, At: now, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	if m.pulls != nil {
		// The commits the opening is for are the ones of now.
		base, _ := m.branchOrZero(ctx, name, target)
		m.pulls.OnPullRequest(ctx, name, ports.PullRequestEvent{Number: mr.Number, Action: domain.PullOpened, Head: mr.HeadID, Base: base}, p)
	}
	return mr, nil
}

// checkBranches validates the branches of a new request and returns them, with
// the default branch filled in for the target.
//
// Only a branch that does not exist is a violation. Any other failure to read
// Git is returned as an error, so that it is logged and masked, not shown in
// the field violations.
func (m *MergeRequests) checkBranches(ctx context.Context, name domain.RepoName, params ports.CreateMergeRequestParams, v *domain.Violations) (source, target string, err error) {
	source, target = params.Source, params.Target
	if source == "" {
		v.Add("source_branch", "is required")
	} else if _, err := domain.ParseBranchName(source); err != nil {
		v.Add("source_branch", "is not a valid branch name")
		source = ""
	}
	if target == "" {
		def, exists, err := m.repos.DefaultBranch(ctx, name)
		switch {
		case err != nil: // includes domain.ErrNotFound: the repository is gone
			return "", "", fmt.Errorf("read the default branch of %q: %w", name, err)
		case !exists:
			v.Add("target_branch", fmt.Sprintf("is empty and the default branch %q does not exist", def))
		default:
			target = def
		}
	} else if _, err := domain.ParseBranchName(target); err != nil {
		v.Add("target_branch", "is not a valid branch name")
		target = ""
	}
	if source != "" && source == target {
		v.Add("target_branch", "must differ from source_branch")
		return source, target, nil
	}
	for _, b := range []struct{ field, name string }{{"source_branch", source}, {"target_branch", target}} {
		if b.name == "" {
			continue
		}
		_, err := m.git.BranchHead(ctx, name, b.name)
		if errors.Is(err, domain.ErrNotFound) {
			v.Add(b.field, fmt.Sprintf("branch %q does not exist", b.name))
		} else if err != nil {
			return "", "", fmt.Errorf("read branch %q of %q: %w", b.name, name, err)
		}
	}
	return source, target, nil
}

// List returns a page of the merge requests of a repository, highest number
// first. The open ones carry the head of their source branch. The page token
// names the last number of the previous page, so requests created between two
// pages never shift the others.
func (m *MergeRequests) List(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.ListMergeRequestsParams) (ports.MergeRequestPage, error) {
	size, err := pageSize(params.PageSize)
	if err != nil {
		return ports.MergeRequestPage{}, err
	}
	after, err := decodeMRToken(params.PageToken)
	if err != nil {
		return ports.MergeRequestPage{}, err
	}
	if _, err := m.readable(ctx, p, name); err != nil {
		return ports.MergeRequestPage{}, err
	}
	all, err := m.store.List(ctx, name)
	if err != nil {
		return ports.MergeRequestPage{}, fmt.Errorf("list merge requests of %q: %w", name, err)
	}
	var page ports.MergeRequestPage
	page.MergeRequests, page.NextPageToken = pageAfter(all, size,
		func(mr domain.MergeRequest) bool {
			return (params.State == "" || mr.State == params.State) && (after == 0 || mr.Number < after)
		},
		encodeMRToken)
	for i, mr := range page.MergeRequests {
		if page.MergeRequests[i], err = m.withHead(ctx, mr); err != nil {
			return ports.MergeRequestPage{}, err
		}
	}
	return page, nil
}

func encodeMRToken(last domain.MergeRequest) string {
	return base64.RawURLEncoding.EncodeToString([]byte(mrTokenPrefix + strconv.FormatInt(last.Number, 10)))
}

// decodeMRToken returns 0 for the empty token.
func decodeMRToken(tok string) (int64, error) {
	if tok == "" {
		return 0, nil
	}
	raw, ok := decodeToken(tok, mrTokenPrefix)
	if ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w: merge requests", domain.ErrInvalidPageToken)
}

// get reads a merge request of a repository the principal can read.
func (m *MergeRequests) get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.Repo, domain.MergeRequest, error) {
	repo, err := m.readable(ctx, p, name)
	if err != nil {
		return domain.Repo{}, domain.MergeRequest{}, err
	}
	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return domain.Repo{}, domain.MergeRequest{}, err
	}
	return repo, mr, nil
}

// Get returns a merge request with its commits, its diff, its comments and the
// read position. The diff compares the merge base of the target and the head
// with the head ("target...head"), within domain.DefaultDiffLimits. With
// sinceLastRead it compares the head recorded by MarkRead with the current
// head instead, which works after a force-push as long as the recorded commit
// still exists; when none was recorded the diff is the default one, and when
// the recorded commit is gone too, with LastReadLost set. A merged or closed
// request keeps the head and the base it had.
func (m *MergeRequests) Get(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, sinceLastRead bool) (ports.MergeRequestDetail, error) {
	_, mr, err := m.get(ctx, p, name, number)
	if err != nil {
		return ports.MergeRequestDetail{}, err
	}
	if mr, err = m.withHead(ctx, mr); err != nil {
		return ports.MergeRequestDetail{}, err
	}
	d := ports.MergeRequestDetail{MergeRequest: mr}
	if d.Comments, err = m.store.ListComments(ctx, name, number); err != nil {
		return ports.MergeRequestDetail{}, fmt.Errorf("list comments of !%d in %q: %w", number, name, err)
	}
	if d.LastReadID, err = m.store.GetRead(ctx, name, number); err != nil {
		return ports.MergeRequestDetail{}, err
	}

	exclude, base, err := m.rangeOf(ctx, mr)
	if err != nil {
		return ports.MergeRequestDetail{}, err
	}
	head := mr.HeadID
	if head.IsZero() {
		return d, nil // the source branch is gone: no commits, no diff
	}
	if !mr.IsOpen() {
		// After a squash the reviewed commits are only reachable from the
		// source branch; once it is deleted, gc may prune them.
		switch ok, err := m.git.HasCommit(ctx, name, head); {
		case err != nil:
			return ports.MergeRequestDetail{}, err
		case !ok:
			return d, nil // the reviewed commits are gone: no commits, no diff
		}
	}
	if d.Commits, d.CommitsTruncated, err = m.git.ListRange(ctx, name, exclude, head, MaxMergeRequestCommits); err != nil {
		return ports.MergeRequestDetail{}, err
	}
	if sinceLastRead && !d.LastReadID.IsZero() {
		switch ok, err := m.git.HasCommit(ctx, name, d.LastReadID); {
		case err != nil:
			return ports.MergeRequestDetail{}, err
		case ok:
			base = d.LastReadID
		default:
			d.LastReadLost = true
		}
	}
	if base.IsZero() {
		return d, nil
	}
	d.DiffBaseID = base
	if d.Stats, d.Diff, err = m.git.DiffCommits(ctx, name, base, head, domain.DefaultDiffLimits()); err != nil {
		return ports.MergeRequestDetail{}, err
	}
	return d, nil
}

// rangeOf returns what the review of a request compares: the commit whose
// history the request's commits exclude, and the base of its diff. For an open
// request they are the tip of the target and the merge base of the target and
// the head; both are zero when the target is gone or shares no history with
// the head. A merged or closed request keeps the base it stored for both.
func (m *MergeRequests) rangeOf(ctx context.Context, mr domain.MergeRequest) (exclude, base domain.ObjectID, err error) {
	if !mr.IsOpen() {
		return mr.BaseID, mr.BaseID, nil
	}
	if mr.HeadID.IsZero() {
		return domain.ObjectID{}, domain.ObjectID{}, nil
	}
	tip, err := m.branchOrZero(ctx, mr.Repo, mr.Target)
	if err != nil || tip.IsZero() {
		return domain.ObjectID{}, domain.ObjectID{}, err
	}
	base, err = m.git.MergeBase(ctx, mr.Repo, tip, mr.HeadID)
	if errors.Is(err, domain.ErrNotFound) {
		return tip, domain.ObjectID{}, nil
	}
	return tip, base, err
}

// AddComment adds a comment and publishes CommentAdded. The principal needs
// mr:write. The text must follow the rules of ADR 0017 and not be blank; a
// comment on a line must name a line of the current diff of the request (see
// domain.Diff.HasLine), else the call fails with domain.ErrInvalid. A request
// that is not open gives domain.ErrPreconditionFailed, and one that already
// holds MaxCommentsPerMergeRequest comments domain.ErrResourceExhausted.
func (m *MergeRequests) AddComment(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.AddCommentParams) (domain.Comment, error) {
	repo, err := m.writable(ctx, p, name, domain.ScopeMRWrite)
	if err != nil {
		return domain.Comment{}, err
	}
	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return domain.Comment{}, err
	}
	var v domain.Violations
	text, err := domain.ParsePlainText(params.Text)
	switch {
	case err != nil:
		v.AddErr("text", err)
	case text.IsBlank():
		v.Add("text", "is required")
	}
	general := params.Path == ""
	switch {
	case general && (params.Line != 0 || params.Side != domain.SideNone):
		v.Add("path", "is required with line and side")
	case !general && params.Line < 1:
		v.Add("line", "must be 1 or more with a path")
	case !general && params.Side != domain.SideOld && params.Side != domain.SideNew:
		v.Add("side", "is required with a path")
	}
	if err := v.Err(); err != nil {
		return domain.Comment{}, err
	}
	if !mr.IsOpen() {
		return domain.Comment{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
	}

	if mr, err = m.withHead(ctx, mr); err != nil {
		return domain.Comment{}, err
	}
	if !general {
		if err := m.checkLine(ctx, mr, params); err != nil {
			return domain.Comment{}, err
		}
	}

	var c domain.Comment
	for range maxWriteRetries {
		if mr.CommentSeq >= MaxCommentsPerMergeRequest {
			return domain.Comment{}, fmt.Errorf("%w: !%d already has %d comments", domain.ErrResourceExhausted, number, MaxCommentsPerMergeRequest)
		}
		c = domain.Comment{
			ID: mr.CommentSeq + 1, Author: authorOf(p), Path: params.Path, Line: params.Line, Side: params.Side,
			CommitID: mr.HeadID, Text: text, CreatedAt: m.clock.Now().UTC(),
		}
		mr.CommentSeq, mr.UpdatedAt = c.ID, c.CreatedAt
		err = m.store.SaveComment(ctx, mr, c)
		if !errors.Is(err, domain.ErrConflict) {
			break
		}
		// Another write got in between: read the request again.
		fresh, gerr := m.store.Get(ctx, name, number)
		if gerr != nil {
			return domain.Comment{}, gerr
		}
		if !fresh.IsOpen() {
			return domain.Comment{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, fresh.State)
		}
		fresh.HeadID = mr.HeadID
		mr = fresh
	}
	if err != nil {
		return domain.Comment{}, fmt.Errorf("save comment on !%d of %q: %w", number, name, err)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Int64("number", number).Int64("comment", c.ID).
		Bool("on_line", !general).Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("comment added")
	m.bus.Publish(ctx, domain.CommentAdded{
		Repo: name, Number: number, Comment: c, At: c.CreatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt,
	})
	return c, nil
}

// checkLine verifies that a comment designates a line of the current diff of
// the request.
func (m *MergeRequests) checkLine(ctx context.Context, mr domain.MergeRequest, params ports.AddCommentParams) error {
	var v domain.Violations
	_, base, err := m.rangeOf(ctx, mr)
	if err != nil {
		return err
	}
	if mr.HeadID.IsZero() || base.IsZero() {
		v.Add("path", "the merge request has no diff to comment on")
		return v.Err()
	}
	_, diff, err := m.git.DiffCommits(ctx, mr.Repo, base, mr.HeadID, domain.DefaultDiffLimits())
	if err != nil {
		return err
	}
	if !diff.HasLine(params.Path, params.Side, params.Line) {
		v.Add("path", fmt.Sprintf("%s:%d on the %s side is not a line of the diff", params.Path, params.Line, params.Side))
	}
	return v.Err()
}

// ResolveComment marks a comment resolved or unresolved. The principal needs
// mr:write. An unknown comment gives domain.ErrNotFound and a request that is
// not open domain.ErrPreconditionFailed. Setting the state the comment already
// has succeeds and writes nothing.
func (m *MergeRequests) ResolveComment(ctx context.Context, p domain.Principal, name domain.RepoName, number, commentID int64, resolved bool) (domain.Comment, error) {
	if _, err := m.writable(ctx, p, name, domain.ScopeMRWrite); err != nil {
		return domain.Comment{}, err
	}
	for range maxWriteRetries {
		mr, err := m.store.Get(ctx, name, number)
		if err != nil {
			return domain.Comment{}, err
		}
		c, err := m.store.GetComment(ctx, name, number, commentID)
		if err != nil {
			return domain.Comment{}, err
		}
		if !mr.IsOpen() {
			return domain.Comment{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
		}
		if c.Resolved == resolved {
			return c, nil
		}
		c.Resolved = resolved
		mr.UpdatedAt = m.clock.Now().UTC()
		err = m.store.SaveComment(ctx, mr, c)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.Comment{}, fmt.Errorf("resolve comment %d of !%d in %q: %w", commentID, number, name, err)
		}
		return c, nil
	}
	return domain.Comment{}, fmt.Errorf("resolve comment %d of !%d in %q: %w", commentID, number, name, domain.ErrConflict)
}

// Close closes an open merge request without merging it and publishes
// MergeRequestClosed. The principal needs mr:write. A request that is not open
// gives domain.ErrPreconditionFailed. The request keeps the head it had and
// its merge base, so that its diff stays readable.
func (m *MergeRequests) Close(ctx context.Context, p domain.Principal, name domain.RepoName, number int64) (domain.MergeRequest, error) {
	repo, err := m.writable(ctx, p, name, domain.ScopeMRWrite)
	if err != nil {
		return domain.MergeRequest{}, err
	}
	unlock, err := m.lockRepo(ctx, name) // a merge and a close of one request exclude each other
	if err != nil {
		return domain.MergeRequest{}, err
	}
	defer unlock()
	for range maxWriteRetries {
		mr, err := m.store.Get(ctx, name, number)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		if !mr.IsOpen() {
			return domain.MergeRequest{}, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
		}
		if mr, err = m.withHead(ctx, mr); err != nil {
			return domain.MergeRequest{}, err
		}
		_, base, err := m.rangeOf(ctx, mr)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		mr.State, mr.BaseID, mr.UpdatedAt = domain.MRStateClosed, base, m.clock.Now().UTC()
		err = m.store.Update(ctx, mr)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.MergeRequest{}, fmt.Errorf("close !%d of %q: %w", number, name, err)
		}
		log := zerowrap.FromCtx(ctx)
		log.Info().Str("repo", name.String()).Int64("number", number).
			Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("merge request closed")
		m.bus.Publish(ctx, domain.MergeRequestClosed{MergeRequest: mr, At: mr.UpdatedAt, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
		if m.pulls != nil {
			m.pulls.OnPullRequest(ctx, name, ports.PullRequestEvent{Number: number, Action: domain.PullClosed, Head: mr.HeadID, Base: mr.BaseID}, p)
		}
		return mr, nil
	}
	return domain.MergeRequest{}, fmt.Errorf("close !%d of %q: %w", number, name, domain.ErrConflict)
}

// MarkRead records the head the reviewer has read, for the since-last-read diff
// of Get. The principal needs mr:write. A zero head means the current head of
// the source branch (the stored one for a merged or closed request); one that
// is not a commit of the repository gives domain.ErrInvalid. It returns the
// head recorded.
func (m *MergeRequests) MarkRead(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, head domain.ObjectID) (domain.ObjectID, error) {
	if _, err := m.writable(ctx, p, name, domain.ScopeMRWrite); err != nil {
		return domain.ObjectID{}, err
	}
	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return domain.ObjectID{}, err
	}
	if head.IsZero() {
		if mr, err = m.withHead(ctx, mr); err != nil {
			return domain.ObjectID{}, err
		}
		if head = mr.HeadID; head.IsZero() {
			return domain.ObjectID{}, fmt.Errorf("%w: the source branch %q no longer exists, give a head", domain.ErrPreconditionFailed, mr.Source)
		}
	} else {
		var v domain.Violations
		switch ok, err := m.git.HasCommit(ctx, name, head); {
		case err != nil:
			return domain.ObjectID{}, err
		case !ok:
			v.Add("head_id", fmt.Sprintf("%s is not a commit of the repository", head))
			return domain.ObjectID{}, v.Err()
		}
	}
	if err := m.store.SetRead(ctx, name, number, head); err != nil {
		return domain.ObjectID{}, err
	}
	return head, nil
}

// Merge merges an open merge request, publishes MergeRequestMerged then a
// PushEvent for the target branch, and returns the merged request. The
// principal needs mr:merge. Merges of one repository run one at a time.
//
// It fails with domain.ErrInvalid for a zero head, and with
// domain.ErrPreconditionFailed when the request is not open, the mode is not
// allowed by the settings of the repository, the source branch no longer
// points to the head ("branch moved, read again") or is gone, the target is
// gone or is not an ancestor of the head (the author rebases and pushes
// again), or the branch brings nothing. Checks come with CI (CI-1): there are
// none yet, so the setting that lets failing checks block the merge has
// nothing to act on.
//
// A fast-forward moves the target to the head, with the old value as a
// precondition. A squash writes one commit with the tree of the head, the
// target as parent and the author and message of domain.NewSquashCommit,
// signed by the instance key, and moves the target to it the same way.
//
// With DeleteSource the merge is also refused, before anything changes, when
// the source is the default branch or the source or target of another open
// request. After the merge the source is deleted if it still points to the
// head; the PushEvent then carries the deletion too. A failed deletion is
// logged and leaves the branch: the merge stands and SourceDeleted is false.
func (m *MergeRequests) Merge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.MergeParams) (ports.MergeResult, error) {
	mr, deleted, err := m.merge(ctx, p, name, number, params)
	return ports.MergeResult{MergeRequest: mr, SourceDeleted: deleted}, err
}

func (m *MergeRequests) merge(ctx context.Context, p domain.Principal, name domain.RepoName, number int64, params ports.MergeParams) (domain.MergeRequest, bool, error) {
	repo, err := m.writable(ctx, p, name, domain.ScopeMRMerge)
	if err != nil {
		return domain.MergeRequest{}, false, err
	}
	mode := params.Mode
	if mode == "" {
		mode = domain.MergeModeFastForward
	}
	var v domain.Violations
	if _, err := domain.ParseMergeMode(mode.String()); err != nil {
		v.Add("mode", "is not a merge mode")
	}
	if params.HeadID.IsZero() {
		v.Add("head_id", "is required")
	}
	if err := v.Err(); err != nil {
		return domain.MergeRequest{}, false, err
	}

	unlock, err := m.lockRepo(ctx, name)
	if err != nil {
		return domain.MergeRequest{}, false, err
	}
	defer unlock()

	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return domain.MergeRequest{}, false, err
	}
	// A merge moves the target and may delete the source: a token limited to
	// branch prefixes may do so only within them.
	if !p.AllowsRef("refs/heads/" + mr.Target) {
		return domain.MergeRequest{}, false, fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, mr.Target)
	}
	if params.DeleteSource && !p.AllowsRef("refs/heads/"+mr.Source) {
		return domain.MergeRequest{}, false, fmt.Errorf("%w: this token may not write %s", domain.ErrForbidden, mr.Source)
	}
	if !mr.IsOpen() {
		return domain.MergeRequest{}, false, fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
	}
	// The settings are read again under the lock: a change applies at once.
	repo, err = m.meta.Get(ctx, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.MergeRequest{}, false, err // deleted meanwhile
		}
		return domain.MergeRequest{}, false, fmt.Errorf("get repository %q: %w", name, err)
	}
	settings := repo.Settings
	if settings.IsZero() {
		settings = domain.DefaultRepoSettings()
	}
	if !settings.Allows(mode) {
		return domain.MergeRequest{}, false, fmt.Errorf("%w: the %s merge mode is not allowed in %s", domain.ErrPreconditionFailed, mode, name)
	}

	head, err := m.branchOrZero(ctx, name, mr.Source)
	switch {
	case err != nil:
		return domain.MergeRequest{}, false, err
	case head.IsZero():
		return domain.MergeRequest{}, false, fmt.Errorf("%w: the source branch %q no longer exists", domain.ErrPreconditionFailed, mr.Source)
	case head != params.HeadID:
		return domain.MergeRequest{}, false, fmt.Errorf("%w: branch moved, read again: %q is at %s, not %s", domain.ErrPreconditionFailed, mr.Source, head, params.HeadID)
	}
	tip, err := m.branchOrZero(ctx, name, mr.Target)
	switch {
	case err != nil:
		return domain.MergeRequest{}, false, err
	case tip.IsZero():
		return domain.MergeRequest{}, false, fmt.Errorf("%w: the target branch %q no longer exists", domain.ErrPreconditionFailed, mr.Target)
	case tip == head:
		return domain.MergeRequest{}, false, fmt.Errorf("%w: nothing to merge, %q already is at %s", domain.ErrPreconditionFailed, mr.Target, head)
	}
	switch ok, err := m.git.IsAncestor(ctx, name, tip, head); {
	case err != nil:
		return domain.MergeRequest{}, false, err
	case !ok:
		return domain.MergeRequest{}, false, fmt.Errorf("%w: %q is not an ancestor of %q: rebase it on %q and push again", domain.ErrPreconditionFailed, mr.Target, mr.Source, mr.Target)
	}
	if settings.FailingChecksBlock {
		if err := m.checksAllow(ctx, name, head); err != nil {
			return domain.MergeRequest{}, false, err
		}
	}
	if params.DeleteSource {
		if err := m.checkDeletable(ctx, name, mr); err != nil {
			return domain.MergeRequest{}, false, err
		}
	}

	merged, err := m.moveTarget(ctx, mr, mode, tip, head)
	if err != nil {
		return domain.MergeRequest{}, false, err
	}
	m.recordPush(ctx, name) // the target moved, whatever happens next
	mr, err = m.markMerged(ctx, name, number, mode, tip, head, merged)
	if err != nil {
		return domain.MergeRequest{}, false, err
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("repo", name.String()).Int64("number", number).Str("mode", mode.String()).
		Str("target", mr.Target).Str("commit", merged.String()).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("merge request merged")
	updates := []domain.RefUpdate{{Old: tip.String(), New: merged.String(), Ref: "refs/heads/" + mr.Target}}
	deleted := false
	if params.DeleteSource {
		// The merge is done and recorded: a failure here keeps the branch.
		switch err := m.git.DeleteBranch(context.WithoutCancel(ctx), name, mr.Source, head); {
		case err != nil:
			log.Warn().Err(err).Str("repo", name.String()).Int64("number", number).Str("branch", mr.Source).
				Msg("source branch kept after the merge")
		default:
			deleted = true
			updates = append(updates, domain.RefUpdate{Old: head.String(), New: domain.ZeroOID, Ref: "refs/heads/" + mr.Source})
			log.Info().Str("repo", name.String()).Int64("number", number).Str("branch", mr.Source).
				Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("source branch deleted after the merge")
		}
	}
	at := mr.UpdatedAt
	m.bus.Publish(ctx, domain.MergeRequestMerged{MergeRequest: mr, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt})
	m.bus.Publish(ctx, domain.PushEvent{
		Repo: name, At: at, Visibility: repo.Visibility, CreatedAt: repo.CreatedAt, Updates: updates,
	})
	if m.pulls != nil {
		m.pulls.OnPullRequest(ctx, name, ports.PullRequestEvent{Number: number, Action: domain.PullClosed, Head: mr.HeadID, Base: mr.BaseID}, p) // closed, merged: true
	}
	if m.ci != nil {
		// GitHub runs the "push" workflows of a merge, with the owner as actor;
		// the run is trusted with what the merging principal may read.
		m.ci.OnMerge(ctx, name, updates[:1], p)
	}
	return mr, deleted, nil
}

// WhileOpen implements ports.PullGate: fn runs under the merge lock, so a
// close or a merge of the request cannot complete in the middle of it, and not
// at all when the request is no longer open.
func (m *MergeRequests) WhileOpen(ctx context.Context, name domain.RepoName, number int64, fn func() error) error {
	unlock, err := m.lockRepo(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	mr, err := m.store.Get(ctx, name, number)
	if err != nil {
		return err
	}
	if !mr.IsOpen() {
		return fmt.Errorf("%w: !%d is %s", domain.ErrPreconditionFailed, number, mr.State)
	}
	return fn()
}

// checksAllow is the merge rule of FailingChecksBlock: every check of the
// head must have ended as success, skipped or neutral. A check still queued or
// running blocks too (the merge could land code that then fails), and a head
// without any check merges.
func (m *MergeRequests) checksAllow(ctx context.Context, name domain.RepoName, head domain.ObjectID) error {
	if m.checks == nil {
		return nil
	}
	checks, err := m.checks.CommitChecks(ctx, name, head.String())
	if errors.Is(err, domain.ErrResourceExhausted) {
		return fmt.Errorf("%w: too many checks to tell whether they passed: %v", domain.ErrPreconditionFailed, err)
	}
	if err != nil {
		return fmt.Errorf("read the checks of %s: %w", head, err)
	}
	var failing, pending []string
	for _, c := range checks {
		switch {
		case c.Status != domain.CICompleted:
			pending = append(pending, c.Name)
		case !c.Conclusion.PassesMerge():
			failing = append(failing, c.Name)
		}
	}
	switch {
	case len(failing) > 0:
		return fmt.Errorf("%w: failing checks block the merge: %s (turn off failing_checks_block to merge anyway)", domain.ErrPreconditionFailed, strings.Join(failing, ", "))
	case len(pending) > 0:
		return fmt.Errorf("%w: checks have not finished: %s", domain.ErrPreconditionFailed, strings.Join(pending, ", "))
	}
	return nil
}

// checkDeletable refuses to delete the source branch of mr when it is the
// default branch or the source or target of another open request. It runs
// under the merge lock, which Create also holds while it checks its branches
// and stores the request.
func (m *MergeRequests) checkDeletable(ctx context.Context, name domain.RepoName, mr domain.MergeRequest) error {
	def, _, err := m.repos.DefaultBranch(ctx, name)
	if err != nil {
		return fmt.Errorf("default branch of %q: %w", name, err)
	}
	if mr.Source == def {
		return fmt.Errorf("%w: %q is the default branch of %s: it cannot be deleted", domain.ErrPreconditionFailed, mr.Source, name)
	}
	all, err := m.store.List(ctx, name)
	if err != nil {
		return fmt.Errorf("list merge requests of %q: %w", name, err)
	}
	for _, o := range all {
		if o.Number != mr.Number && o.IsOpen() && (o.Source == mr.Source || o.Target == mr.Source) {
			return fmt.Errorf("%w: !%d still uses %q: it cannot be deleted", domain.ErrPreconditionFailed, o.Number, mr.Source)
		}
	}
	return nil
}

// recordPush does for a merge what Pushes.Record does for a push, as the merge
// moved a branch: it sets pushed_at and queues the maintenance of the
// repository. The target already moved, so a failure is logged, not returned,
// and the caller giving up does not stop it.
func (m *MergeRequests) recordPush(ctx context.Context, name domain.RepoName) {
	ctx = context.WithoutCancel(ctx)
	if _, err := touchPushedAt(ctx, m.meta, name, m.clock.Now().UTC()); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("repo", name.String()).Msg("record pushed_at after a merge")
	}
	m.maint.Schedule(ctx, name)
}

// moveTarget moves the target branch from tip to the head (fast-forward) or to
// a new signed squash commit, and returns the new tip.
func (m *MergeRequests) moveTarget(ctx context.Context, mr domain.MergeRequest, mode domain.MergeMode, tip, head domain.ObjectID) (domain.ObjectID, error) {
	newTip := head
	if mode == domain.MergeModeSquash {
		commits, more, err := m.git.ListRange(ctx, mr.Repo, tip, head, domain.MaxSquashCommits)
		if err != nil {
			return domain.ObjectID{}, err
		}
		if more {
			return domain.ObjectID{}, fmt.Errorf("%w: the branch has more than %d commits to squash", domain.ErrPreconditionFailed, domain.MaxSquashCommits)
		}
		tree, err := m.git.CommitTree(ctx, mr.Repo, head)
		if err != nil {
			return domain.ObjectID{}, err
		}
		commit := domain.NewSquashCommit(mr, tree, tip, commits, m.committer, m.clock.Now())
		armored, err := m.signer.Sign(commit.Payload())
		if err != nil {
			return domain.ObjectID{}, fmt.Errorf("sign the squash commit of !%d in %q: %w", mr.Number, mr.Repo, err)
		}
		if newTip, err = m.git.WriteCommit(ctx, mr.Repo, commit.Signed(armored)); err != nil {
			return domain.ObjectID{}, err
		}
	}
	err := m.git.UpdateBranch(ctx, mr.Repo, mr.Target, tip, newTip)
	if errors.Is(err, domain.ErrConflict) {
		return domain.ObjectID{}, fmt.Errorf("%w: the target branch %q moved during the merge, try again", domain.ErrPreconditionFailed, mr.Target)
	}
	if err != nil {
		return domain.ObjectID{}, err
	}
	return newTip, nil
}

// markMerged records the merge. The target already moved, so a write that
// loses a race (a comment added meanwhile) is repeated on the fresh request.
func (m *MergeRequests) markMerged(ctx context.Context, name domain.RepoName, number int64, mode domain.MergeMode, base, head, merged domain.ObjectID) (domain.MergeRequest, error) {
	// The merge must be recorded even when the caller gives up.
	ctx = context.WithoutCancel(ctx)
	for range maxWriteRetries {
		mr, err := m.store.Get(ctx, name, number)
		if err != nil {
			return domain.MergeRequest{}, fmt.Errorf("record the merge of !%d in %q: %w", number, name, err)
		}
		mr.State, mr.HeadID, mr.BaseID, mr.MergedCommitID, mr.MergeMode = domain.MRStateMerged, head, base, merged, mode
		mr.UpdatedAt = m.clock.Now().UTC()
		err = m.store.Update(ctx, mr)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.MergeRequest{}, fmt.Errorf("record the merge of !%d in %q: %w", number, name, err)
		}
		return mr, nil
	}
	log := zerowrap.FromCtx(ctx)
	log.Error().Str("repo", name.String()).Int64("number", number).Str("commit", merged.String()).
		Msg("the target branch moved but the merge request could not be marked merged")
	return domain.MergeRequest{}, fmt.Errorf("record the merge of !%d in %q: %w", number, name, domain.ErrConflict)
}
