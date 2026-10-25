// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Inbox is the owner's inbox (ADR 0031): the open merge requests and issues of
// every repository in one place. Only the owner may read it; anyone else
// gets domain.ErrNotFound, as for a page that does not exist. The issues of a
// repository whose issues mode is off are left out.
type Inbox struct {
	meta   ports.RepoMetaStore
	mrs    ports.MergeRequestStore
	issues ports.IssueStore
	// pending is nil when the public form is not wired.
	pending ports.PendingIssueStore
	ci      ports.CIStore
	// git gives the head of the source branch; nil leaves HeadID as stored.
	git ports.MergeGit
	// deps is nil when Bernard is off.
	deps  ports.DepsStore
	clock ports.Clock
	// journal and reader give the activity journal; nil leaves it empty.
	journal ports.JournalStore
	repos   ports.GitRepos
	reader  ports.RepoReader

	// warnings caches the tag warning of each repository; see unreleased.
	warnings warningCache
}

var (
	_ ports.InboxService   = (*Inbox)(nil)
	_ ports.JournalService = (*Inbox)(nil)
	_ ports.RefsObserver   = (*Inbox)(nil)
)

// warningCache holds the last tag warning of each repository, valid while
// the repository, its default branch, the head of that branch and its
// generation are the same. A push bumps the generation (RefsChanged): a tag
// created or deleted changes the warning without moving the head. Safe for
// concurrent use.
type warningCache struct {
	mu      sync.Mutex
	gen     map[domain.RepoName]uint64
	entries map[domain.RepoName]warningEntry
}

// warningKey is what a cached warning was computed from.
type warningKey struct {
	created time.Time
	branch  string
	head    domain.ObjectID
	gen     uint64
}

type warningEntry struct {
	key warningKey
	u   ports.UnreleasedRepo
	ok  bool
}

func (c *warningCache) generation(repo domain.RepoName) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen[repo]
}

func (c *warningCache) bump(repo domain.RepoName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == nil {
		c.gen = make(map[domain.RepoName]uint64)
	}
	c.gen[repo]++
	delete(c.entries, repo)
}

func (c *warningCache) get(repo domain.RepoName, key warningKey) (warningEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[repo]
	return e, ok && e.key == key
}

// put stores the warning unless a push came since key was read.
func (c *warningCache) put(repo domain.RepoName, e warningEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen[repo] != e.key.gen {
		return
	}
	if c.entries == nil {
		c.entries = make(map[domain.RepoName]warningEntry)
	}
	c.entries[repo] = e
}

// keep drops what is cached of the repositories that are gone.
func (c *warningCache) keep(exists map[domain.RepoName]time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := range c.entries {
		if _, ok := exists[n]; !ok {
			delete(c.entries, n)
		}
	}
	for n := range c.gen {
		if _, ok := exists[n]; !ok {
			delete(c.gen, n)
		}
	}
}

// RefsChanged forgets the tag warning of the repository: a push moved its
// refs, tags included.
func (in *Inbox) RefsChanged(repo domain.RepoName) { in.warnings.bump(repo) }

// InboxConfig holds what the inbox needs; Pending and CI are optional.
type InboxConfig struct {
	Meta          ports.RepoMetaStore
	MergeRequests ports.MergeRequestStore
	Issues        ports.IssueStore
	// Pending lists the issues awaiting moderation. Optional: without it the
	// inbox has no pending section.
	Pending ports.PendingIssueStore
	CI      ports.CIStore
	// Git reads the head of each open request's source branch. Optional.
	Git ports.MergeGit
	// Deps and Clock add the Bernard section; optional.
	Deps  ports.DepsStore
	Clock ports.Clock
	// Journal, Repos and Reader give the activity journal and the commits
	// since the nearest tag. Optional; they need Clock.
	Journal ports.JournalStore
	Repos   ports.GitRepos
	Reader  ports.RepoReader
}

// NewInbox builds the inbox use case.
func NewInbox(cfg InboxConfig) *Inbox {
	return &Inbox{
		meta: cfg.Meta, mrs: cfg.MergeRequests, issues: cfg.Issues, pending: cfg.Pending, ci: cfg.CI, git: cfg.Git, deps: cfg.Deps, clock: cfg.Clock,
		journal: cfg.Journal, repos: cfg.Repos, reader: cfg.Reader,
	}
}

// Journal reads the entries of the window from the journal, leaving out those
// of repositories that are gone or were created again since, and the issues of
// a repository whose issues are off (as Overview does). The store applies
// that filter during its one scan and keeps the newest MaxJournalEntries:
// entries left out do not hide older ones. It
// also measures, for each repository with a tag, the commits of its default
// branch after the nearest tag: one walk per repository, bounded by
// domain.MaxTagDistance and served from the commit-graph, and cached until
// the refs of the repository move. A repository whose walk fails is logged
// and skipped.
func (in *Inbox) Journal(ctx context.Context, p domain.Principal, filter domain.RepoName, window time.Duration) (ports.InboxJournal, error) {
	var out ports.InboxJournal
	if !p.HasOwnerRights() {
		return out, domain.ErrNotFound
	}
	if in.journal == nil || in.clock == nil {
		return out, nil
	}
	repos, err := in.journalRepos(ctx)
	if err != nil {
		return out, err
	}
	listed, err := in.journal.ListSince(ctx, in.clock.Now().Add(-window), "", filter, repos.eligible(p, nil), ports.MaxJournalEntries)
	if err != nil {
		return out, fmt.Errorf("read the journal: %w", err)
	}
	out.Entries, out.Truncated = listed.Entries, listed.Truncated
	out.Unreleased, err = in.unreleasedOf(ctx, repos, func(r domain.Repo) bool { return filter.IsZero() || r.Name == filter })
	return out, err
}

// journalRepos is what the journal is filtered with: the repositories, by
// name, as they are now.
type journalRepos struct {
	all     []domain.Repo
	created map[domain.RepoName]time.Time
	issues  map[domain.RepoName]bool
}

func (in *Inbox) journalRepos(ctx context.Context) (journalRepos, error) {
	all, err := in.meta.List(ctx)
	if err != nil {
		return journalRepos{}, fmt.Errorf("list repositories: %w", err)
	}
	r := journalRepos{all: all, created: make(map[domain.RepoName]time.Time, len(all)), issues: make(map[domain.RepoName]bool, len(all))}
	for _, repo := range all {
		r.created[repo.Name] = repo.CreatedAt
		r.issues[repo.Name] = repo.Settings.IssuesEnabled()
	}
	return r, nil
}

// eligible is the predicate of the journal scan: the entries of the
// repositories p reads the journal of that still exist (not created again
// since), without the issues of a repository whose issues are off, and of
// the kinds when there are some.
func (r journalRepos) eligible(p domain.Principal, kinds []domain.JournalKind) func(domain.JournalEntry) bool {
	owner := p.HasOwnerRights()
	return func(e domain.JournalEntry) bool {
		at, ok := r.created[e.Repo]
		if !ok || at.After(e.At) {
			return false // a repository deleted, or created again, since
		}
		if !owner && !p.ReadsJournalOf(e.Repo) {
			return false
		}
		if len(kinds) > 0 && !slices.Contains(kinds, e.Kind) {
			return false
		}
		return (e.Kind != domain.JournalIssueOpened && e.Kind != domain.JournalIssueClosed) || r.issues[e.Repo]
	}
}

// unreleasedOf measures the non-archived repositories that want accepts. It
// measures, for each repository with a tag, the commits of its default
// branch after the nearest tag; a repository whose walk fails is logged and
// skipped.
func (in *Inbox) unreleasedOf(ctx context.Context, repos journalRepos, want func(domain.Repo) bool) ([]ports.UnreleasedRepo, error) {
	if in.repos == nil || in.reader == nil {
		return nil, nil
	}
	in.warnings.keep(repos.created)
	log := zerowrap.FromCtx(ctx)
	var out []ports.UnreleasedRepo
	for _, r := range repos.all {
		if r.Archived || !want(r) {
			continue
		}
		u, ok, err := in.unreleased(ctx, r)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			log.Warn().Err(err).Str("repo", r.Name.String()).Msg("inbox: count the commits since the last tag")
			continue
		}
		if ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// journalAccess checks that p may read the journal at all, and repo (when
// not zero) in particular; an unknown repository is not found.
func journalAccess(p domain.Principal, repos journalRepos, repo domain.RepoName) error {
	switch {
	case p.IsAnonymous():
		return domain.ErrUnauthenticated
	case !p.ReadsJournal():
		return fmt.Errorf("%w: reading the journal needs owner rights or the journal:read scope", domain.ErrForbidden)
	}
	if repo.IsZero() {
		return nil
	}
	if _, ok := repos.created[repo]; !ok || !p.ReadsJournalOf(repo) {
		return domain.ErrNotFound
	}
	return nil
}

// journalTokenPrefix starts the page tokens of ListActivity.
const journalTokenPrefix = "jr1:"

// ListActivity reads one page with one bounded scan of the journal: the
// page token is the ID of the last entry of the previous page, and the scan
// stops before it.
func (in *Inbox) ListActivity(ctx context.Context, p domain.Principal, q ports.ActivityQuery) (ports.ActivityPage, error) {
	var out ports.ActivityPage
	if err := journalAccess(p, journalRepos{}, domain.RepoName{}); err != nil {
		return out, err
	}
	var v domain.Violations
	switch {
	case q.Since.IsZero():
		v.Add("since", "is required")
	case in.clock != nil && q.Since.After(in.clock.Now()):
		v.Add("since", "is in the future")
	}
	for _, k := range q.Kinds {
		if !slices.Contains(domain.JournalKinds(), k) {
			v.Add("kinds", fmt.Sprintf("unknown kind %q", k))
			break
		}
	}
	if err := v.Err(); err != nil {
		return out, err
	}
	if q.PageSize == 0 {
		q.PageSize = ports.DefaultActivityPageSize
	}
	size, err := pageSizeUpTo(q.PageSize, ports.MaxActivityPageSize)
	if err != nil {
		return out, err
	}
	before := ""
	if q.PageToken != "" {
		raw, ok := decodeToken(q.PageToken, journalTokenPrefix)
		if !ok || !domain.ValidJournalID(raw) {
			return out, fmt.Errorf("%w: journal", domain.ErrInvalidPageToken)
		}
		before = raw
	}
	if in.journal == nil {
		return out, nil
	}
	repos, err := in.journalRepos(ctx)
	if err != nil {
		return out, err
	}
	if err := journalAccess(p, repos, q.Repo); err != nil {
		return out, err
	}
	listed, err := in.journal.ListSince(ctx, q.Since, before, q.Repo, repos.eligible(p, q.Kinds), size)
	if err != nil {
		return out, fmt.Errorf("read the journal: %w", err)
	}
	out.Entries = listed.Entries
	if listed.Truncated && len(listed.Entries) > 0 {
		last := listed.Entries[len(listed.Entries)-1]
		out.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(journalTokenPrefix + last.ID))
	}
	return out, nil
}

// ListUnreleased serves the tag warnings of the inbox, from the same cache.
func (in *Inbox) ListUnreleased(ctx context.Context, p domain.Principal, repo domain.RepoName) ([]ports.UnreleasedRepo, error) {
	if err := journalAccess(p, journalRepos{}, domain.RepoName{}); err != nil {
		return nil, err
	}
	repos, err := in.journalRepos(ctx)
	if err != nil {
		return nil, err
	}
	if err := journalAccess(p, repos, repo); err != nil {
		return nil, err
	}
	return in.unreleasedOf(ctx, repos, func(r domain.Repo) bool {
		return (repo.IsZero() || r.Name == repo) && p.ReadsJournalOf(r.Name)
	})
}

// unreleased is the distance of the default branch to its nearest tag; ok is
// false when there is nothing to show: no branch, no tag, or a tag on the head.
// HEAD is the default branch: HeadBranch reads its name without the refs, and
// the reader resolves it (an unborn branch is domain.ErrNotFound). The walk is
// cached: it is done again only when the repository, its default branch, its
// head or its generation (a push) changed.
func (in *Inbox) unreleased(ctx context.Context, repo domain.Repo) (ports.UnreleasedRepo, bool, error) {
	name := repo.Name
	gen := in.warnings.generation(name)
	branch, err := in.repos.HeadBranch(ctx, name)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && branch == "") {
		return ports.UnreleasedRepo{}, false, nil // gone, or a detached HEAD
	}
	if err != nil {
		return ports.UnreleasedRepo{}, false, err
	}
	head, err := in.reader.ResolveRevision(ctx, name, domain.Revision{})
	if errors.Is(err, domain.ErrNotFound) {
		return ports.UnreleasedRepo{}, false, nil
	}
	if err != nil {
		return ports.UnreleasedRepo{}, false, err
	}
	key := warningKey{created: repo.CreatedAt, branch: branch, head: head.CommitID, gen: gen}
	if e, ok := in.warnings.get(name, key); ok {
		return e.u, e.ok, nil
	}
	d, ok, err := in.reader.TagDistance(ctx, name, head.CommitID, domain.MaxTagDistance)
	if err != nil {
		return ports.UnreleasedRepo{}, false, err // not cached: tried again
	}
	e := warningEntry{key: key}
	if ok && d.Ahead() {
		e.u, e.ok = ports.UnreleasedRepo{Repo: name, Branch: branch, Distance: d}, true
	}
	in.warnings.put(name, e)
	return e.u, e.ok, nil
}

// Overview lists what is open, through the indexes of open items. The counts
// and the list of repositories do not depend on the filter, the lists do.
func (in *Inbox) Overview(ctx context.Context, p domain.Principal, filter domain.RepoName) (ports.InboxOverview, error) {
	var out ports.InboxOverview
	if !p.HasOwnerRights() {
		return out, domain.ErrNotFound
	}
	mrs, err := in.mrs.ListOpenAll(ctx)
	if err != nil {
		return out, fmt.Errorf("list open merge requests: %w", err)
	}
	issues, err := in.issues.ListOpenAll(ctx)
	if err != nil {
		return out, fmt.Errorf("list open issues: %w", err)
	}

	// exists and issuesOn cache what is read of each repository.
	issuesOn := make(map[domain.RepoName]bool)
	exists := make(map[domain.RepoName]bool)
	look := func(name domain.RepoName) error {
		if _, ok := exists[name]; ok {
			return nil
		}
		repo, err := lookupRepo(ctx, in.meta, name)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			exists[name] = false
		case err != nil:
			return fmt.Errorf("read repository %q: %w", name, err)
		default:
			exists[name] = true
			issuesOn[name] = repo.Settings.IssuesEnabled()
		}
		return nil
	}

	repos := make(map[domain.RepoName]bool)
	for _, m := range mrs {
		if err := look(m.Repo); err != nil {
			return out, err
		}
		if !exists[m.Repo] {
			continue
		}
		repos[m.Repo] = true
		out.Counts.MergeRequests++
		if filter.IsZero() || m.Repo == filter {
			out.MergeRequests = append(out.MergeRequests, m)
		}
	}
	for _, i := range issues {
		if err := look(i.Repo); err != nil {
			return out, err
		}
		if !exists[i.Repo] || !issuesOn[i.Repo] {
			continue
		}
		repos[i.Repo] = true
		out.Counts.Issues++
		if filter.IsZero() || i.Repo == filter {
			out.Issues = append(out.Issues, i)
		}
	}

	if in.pending != nil {
		page, err := in.pending.List(ctx, domain.RepoName{}, ports.ListPendingParams{PageSize: domain.MaxPendingIssues})
		if err != nil {
			return out, fmt.Errorf("list pending issues: %w", err)
		}
		for _, pi := range page.Issues {
			if err := look(pi.Repo); err != nil {
				return out, err
			}
			if !exists[pi.Repo] || !issuesOn[pi.Repo] {
				continue
			}
			pi.IPHash = nil // the hash is for bans, the page never needs it
			out.Pending = append(out.Pending, pi)
			repos[pi.Repo] = true
		}
		out.Counts.Pending = len(out.Pending)
	}

	if in.ci != nil {
		out.RunsEnabled = true
		runs, err := in.ci.ListOpenRuns(ctx)
		if err != nil {
			return out, fmt.Errorf("list open runs: %w", err)
		}
		for _, run := range runs {
			if run.Status != domain.CIQueued && run.Status != domain.CIInProgress {
				continue
			}
			if err := look(run.Repo); err != nil {
				return out, err
			}
			if !exists[run.Repo] {
				continue
			}
			repos[run.Repo] = true
			out.Counts.Running++
			if filter.IsZero() || run.Repo == filter {
				out.Runs = append(out.Runs, run)
			}
		}
		slices.SortStableFunc(out.Runs, func(a, b domain.CIRun) int {
			return compareRecent(a.CreatedAt, a.CreatedAt, a.Repo, a.ID, b.CreatedAt, b.CreatedAt, b.Repo, b.ID)
		})
	}

	if in.deps != nil && in.clock != nil {
		all, err := in.deps.List(ctx)
		if err != nil {
			return out, fmt.Errorf("list dependency updates: %w", err)
		}
		now := in.clock.Now()
		for _, d := range all {
			if len(d.Updates) == 0 || d.SnoozedUntil.After(now) {
				continue
			}
			repo, err := lookupRepo(ctx, in.meta, d.Repo)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				continue
			case err != nil:
				return out, fmt.Errorf("read repository %q: %w", d.Repo, err)
			}
			if repo.Archived {
				continue
			}
			repos[d.Repo] = true
			out.Counts.Deps++
			out.Counts.DepsUpdates += len(d.Updates)
			if d.CheckedAt.After(out.DepsCheckedAt) {
				out.DepsCheckedAt = d.CheckedAt
			}
			if filter.IsZero() || d.Repo == filter {
				item := ports.InboxDeps{Rec: d}
				if d.RunID != 0 && in.ci != nil {
					run, err := in.ci.GetRun(ctx, d.Repo, d.RunID)
					switch {
					case err == nil:
						item.Run = &run
					case !errors.Is(err, domain.ErrNotFound):
						return out, fmt.Errorf("read run %d of %q: %w", d.RunID, d.Repo, err)
					}
				}
				out.Deps = append(out.Deps, item)
			}
		}
	}

	if in.git != nil {
		for i, m := range out.MergeRequests {
			// Like MergeRequests.withHead: the head comes from the source
			// branch, zero when the branch is gone.
			head, err := in.git.BranchHead(ctx, m.Repo, m.Source)
			switch {
			case errors.Is(err, domain.ErrNotFound):
				out.MergeRequests[i].HeadID = domain.ObjectID{}
			case err != nil:
				return out, fmt.Errorf("read the head of %q in %q: %w", m.Source, m.Repo, err)
			default:
				out.MergeRequests[i].HeadID = head
			}
		}
	}

	slices.SortStableFunc(out.MergeRequests, func(a, b domain.MergeRequest) int {
		return compareRecent(a.UpdatedAt, a.CreatedAt, a.Repo, a.Number, b.UpdatedAt, b.CreatedAt, b.Repo, b.Number)
	})
	slices.SortStableFunc(out.Issues, func(a, b domain.Issue) int {
		return compareRecent(a.UpdatedAt, a.CreatedAt, a.Repo, a.Number, b.UpdatedAt, b.CreatedAt, b.Repo, b.Number)
	})
	for r := range repos {
		out.Repos = append(out.Repos, r)
	}
	slices.SortFunc(out.Repos, func(a, b domain.RepoName) int { return strings.Compare(a.String(), b.String()) })
	return out, nil
}

// compareRecent orders by update time, most recent first (the creation time
// stands in for a missing one), then by repository and number.
func compareRecent(aUp, aCr time.Time, aRepo domain.RepoName, aN int64, bUp, bCr time.Time, bRepo domain.RepoName, bN int64) int {
	if aUp.IsZero() {
		aUp = aCr
	}
	if bUp.IsZero() {
		bUp = bCr
	}
	if c := bUp.Compare(aUp); c != 0 {
		return c
	}
	if c := strings.Compare(aRepo.String(), bRepo.String()); c != 0 {
		return c
	}
	switch {
	case aN > bN:
		return -1
	case aN < bN:
		return 1
	}
	return 0
}
