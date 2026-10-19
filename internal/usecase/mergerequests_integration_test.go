// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/eventbus"
	"bnema.dev/ermite/internal/adapters/gitcli"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/adapters/sshsig"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// queuedRepos records the repositories whose maintenance was queued.
type queuedRepos struct {
	mu sync.Mutex
	rs []domain.RepoName
}

func (q *queuedRepos) add(_ context.Context, n domain.RepoName) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rs = append(q.rs, n)
}

func (q *queuedRepos) names() []domain.RepoName {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]domain.RepoName(nil), q.rs...)
}

// mrRig runs the merge request use cases over the real adapters: a data
// directory with ermite-kv and bare repositories, and a work tree to push
// branches from.
type mrRig struct {
	t      *testing.T
	ctx    context.Context
	data   string
	repos  *usecase.Repos
	mrs    *usecase.MergeRequests
	key    *usecase.SigningKey
	store  *kvstore.Store
	git    *gitcli.Git
	bus    *eventbus.Bus
	sched  *mocks.MaintenanceScheduler
	queued *queuedRepos
	events <-chan domain.Event
	work   string
	repo   domain.RepoName
}

var (
	root   = domain.Root()
	author = domain.Identity{Name: "Ada Lovelace", Email: "ada@example.org"}
)

func newMRRig(t *testing.T) *mrRig {
	t.Helper()
	ctx := context.Background()
	bin, err := exec.LookPath("git")
	if err != nil {
		unusable(t, "git not found")
	}
	data := t.TempDir()
	for _, d := range []string{"repos/.staging", "trash", "run/git-home", "kv"} {
		require.NoError(t, os.MkdirAll(filepath.Join(data, d), 0o700))
	}
	git := gitcli.New(gitcli.Config{Bin: bin, DataDir: data, Path: os.Getenv("PATH")})
	if _, err := git.CheckVersion(ctx); err != nil {
		unusable(t, "git is not usable by ermite: %v", err)
	}
	t.Cleanup(func() { _ = git.Close(context.Background()) })
	store, err := kvstore.Open(ctx, filepath.Join(data, "kv"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	signer, err := sshsig.New(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	require.NoError(t, err)
	bus := eventbus.NewWithBuffer(1024)
	committer := domain.Identity{Name: "ermite", Email: "ermite@git.example.org"}
	sched := mocks.NewMaintenanceScheduler(t)
	queued := &queuedRepos{}
	sched.EXPECT().Schedule(mock.Anything, mock.Anything).Run(queued.add).Return().Maybe()
	r := &mrRig{
		sched: sched, queued: queued,
		t: t, ctx: ctx, data: data, store: store, git: git, bus: bus, events: bus.Subscribe(ctx),
		repos: usecase.NewRepos(store, git, bus, fixedClock{}),
		key:   usecase.NewSigningKey(signer, committer),
		work:  t.TempDir(),
		repo:  name(t, "proj"),
	}
	r.mrs = usecase.NewMergeRequests(usecase.MergeRequestsConfig{
		Meta: store, Store: store.MergeRequests(), Repos: git, Git: git, Signer: signer,
		Committer: committer, Bus: bus, Clock: fixedClock{}, Maint: r.sched,
	})
	_, err = r.repos.Create(ctx, root, ports.CreateRepoParams{Name: r.repo, Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	r.sh("init", "-q", "--initial-branch=main")
	return r
}

func (r *mrRig) bare() string { return filepath.Join(r.data, "repos", r.repo.String()+".git") }

func (r *mrRig) env(who domain.Identity, extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.work, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=" + who.Name, "GIT_AUTHOR_EMAIL=" + who.Email,
		"GIT_COMMITTER_NAME=" + who.Name, "GIT_COMMITTER_EMAIL=" + who.Email,
		"LC_ALL=C",
	}, extra...)
}

func (r *mrRig) run(dir string, env []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %s\n%s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// sh runs git in the work tree as the default author.
func (r *mrRig) sh(args ...string) string { return r.run(r.work, r.env(author), args...) }

// commit writes a file and commits it as who; it returns the commit id.
func (r *mrRig) commit(who domain.Identity, path, content, msg string) domain.ObjectID {
	r.t.Helper()
	full := filepath.Join(r.work, path)
	require.NoError(r.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(r.t, os.WriteFile(full, []byte(content), 0o644))
	r.run(r.work, r.env(who), "add", path)
	r.run(r.work, r.env(who), "commit", "-q", "-m", msg)
	return r.head()
}

func (r *mrRig) head() domain.ObjectID {
	r.t.Helper()
	return oidFrom(r.t, r.sh("rev-parse", "HEAD"))
}

func oidFrom(t *testing.T, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

// push pushes the branch of the work tree to the repository, forcing it.
func (r *mrRig) push(branch string) {
	r.t.Helper()
	r.sh("push", "-q", "--force", r.bare(), branch+":refs/heads/"+branch)
}

// branchHead reads a branch of the bare repository.
func (r *mrRig) branchHead(branch string) domain.ObjectID {
	r.t.Helper()
	return oidFrom(r.t, r.run(r.bare(), r.env(author), "rev-parse", "refs/heads/"+branch))
}

// seed pushes main with two files and returns its id.
func (r *mrRig) seed() domain.ObjectID {
	r.t.Helper()
	r.commit(author, "a.txt", "one\ntwo\nthree\n", "add a")
	id := r.commit(author, "b.txt", "bee\n", "add b")
	r.push("main")
	return id
}

// feature creates the branch feat from main with two commits by two authors
// and pushes it; it returns its head.
func (r *mrRig) feature(branch string) domain.ObjectID {
	r.t.Helper()
	r.sh("checkout", "-q", "-B", branch, "main")
	r.commit(author, "a.txt", "one\n2\nthree\n", "change a")
	bob := domain.Identity{Name: "Bob", Email: "bob@example.org"}
	id := r.commit(bob, "c.txt", "see\n", "add c")
	r.push(branch)
	return id
}

func (r *mrRig) open(branch string) domain.MergeRequest {
	r.t.Helper()
	mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "Add " + branch, Source: branch})
	require.NoError(r.t, err)
	return mr
}

// drain returns the events published so far. The use cases publish on the bus
// from the calling goroutine, and the bus delivers into the buffer of the
// subscriber before Publish returns: whatever a returned call published is
// already in the channel.
func (r *mrRig) drain() []domain.Event {
	var out []domain.Event
	for {
		select {
		case ev := <-r.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func writer(scopes ...domain.Scope) domain.Principal {
	return domain.TokenPrincipal(domain.Token{ID: "t", Label: "agent-1", Scopes: scopes})
}

var (
	// agent is the agent preset: it pushes and writes merge requests, and has
	// no repo:read.
	agent  = writer(domain.ScopeRepoWrite, domain.ScopeMRWrite)
	reader = writer(domain.ScopeRepoRead)
	mrW    = writer(domain.ScopeRepoRead, domain.ScopeMRWrite)
	mrM    = writer(domain.ScopeRepoRead, domain.ScopeMRWrite, domain.ScopeMRMerge)
)

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var fe *domain.InvalidFieldsError
	require.ErrorAs(t, err, &fe, "want a rejection naming the fields")
	out := map[string]string{}
	for _, f := range fe.Fields {
		out[f.Field] = f.Reason
	}
	return out
}

// AC3: the input rules of ADR 0017.
func TestCreateMergeRequestInputRules(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	r.sh("checkout", "-q", "-B", "other", "main")
	r.commit(author, "o.txt", "o\n", "other")
	r.push("other")

	tests := []struct {
		name   string
		params ports.CreateMergeRequestParams
		field  string // "" for valid
	}{
		{"valid, default target", ports.CreateMergeRequestParams{Title: "t", Source: "feat"}, ""},
		{"valid, explicit target and description", ports.CreateMergeRequestParams{Title: "t", Description: "a\nb", Source: "other", Target: "main"}, ""},
		{"empty title", ports.CreateMergeRequestParams{Title: "", Source: "feat", Target: "other"}, "title"},
		{"two-line title", ports.CreateMergeRequestParams{Title: "a\nb", Source: "feat", Target: "other"}, "title"},
		{"title too long", ports.CreateMergeRequestParams{Title: strings.Repeat("x", 121), Source: "feat", Target: "other"}, "title"},
		{"title with a bidi override", ports.CreateMergeRequestParams{Title: "a\u202eb", Source: "feat", Target: "other"}, "title"},
		{"title with an escape", ports.CreateMergeRequestParams{Title: "\x1b[31m", Source: "feat", Target: "other"}, "title"},
		{"description with a control character", ports.CreateMergeRequestParams{Title: "t", Description: "a\x07", Source: "feat", Target: "other"}, "description"},
		{"description with a bidi isolate", ports.CreateMergeRequestParams{Title: "t", Description: "a\u2066b", Source: "feat", Target: "other"}, "description"},
		{"description too long", ports.CreateMergeRequestParams{Title: "t", Description: strings.Repeat("x", 8001), Source: "feat", Target: "other"}, "description"},
		{"description not utf-8", ports.CreateMergeRequestParams{Title: "t", Description: "\xff", Source: "feat", Target: "other"}, "description"},
		{"source missing", ports.CreateMergeRequestParams{Title: "t", Source: ""}, "source_branch"},
		{"source does not exist", ports.CreateMergeRequestParams{Title: "t", Source: "nope", Target: "main"}, "source_branch"},
		{"source is not a branch name", ports.CreateMergeRequestParams{Title: "t", Source: "a..b", Target: "main"}, "source_branch"},
		{"source is a full ref", ports.CreateMergeRequestParams{Title: "t", Source: "refs/heads/feat", Target: "main"}, "source_branch"},
		{"target does not exist", ports.CreateMergeRequestParams{Title: "t", Source: "feat", Target: "nope"}, "target_branch"},
		{"same branch", ports.CreateMergeRequestParams{Title: "t", Source: "feat", Target: "feat"}, "target_branch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr, err := r.mrs.Create(r.ctx, root, r.repo, tc.params)
			if tc.field == "" {
				require.NoError(t, err)
				assert.Equal(t, domain.MRStateOpen, mr.State)
				assert.Equal(t, "root", mr.Author)
				assert.Equal(t, "main", mr.Target)
				assert.NotZero(t, mr.HeadID)
				return
			}
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}

	t.Run("a request is rejected whole with every field named", func(t *testing.T) {
		_, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "a\nb", Description: "\x07", Source: "nope", Target: "feat"})
		got := fieldsOf(t, err)
		assert.Contains(t, got, "title")
		assert.Contains(t, got, "description")
		assert.Contains(t, got, "source_branch")
	})
}

func TestCreateMergeRequestNumbersAndDuplicates(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("one")
	r.feature("two")

	first := r.open("one")
	assert.Equal(t, int64(1), first.Number)
	second := r.open("two")
	assert.Equal(t, int64(2), second.Number)

	_, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "again", Source: "one", Target: "main"})
	require.ErrorIs(t, err, domain.ErrAlreadyExists, "same open source and target")
	_, err = r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "again", Source: "one"})
	require.ErrorIs(t, err, domain.ErrAlreadyExists, "the default target is the same target")

	// Another target is another request.
	r.sh("checkout", "-q", "-B", "stage", "main")
	r.push("stage")
	third, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "to stage", Source: "one", Target: "stage"})
	require.NoError(t, err)
	assert.Equal(t, int64(3), third.Number)

	// Once the first is closed, the pair is free again, with a new number.
	_, err = r.mrs.Close(r.ctx, root, r.repo, first.Number)
	require.NoError(t, err)
	fourth, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "retry", Source: "one"})
	require.NoError(t, err)
	assert.Equal(t, int64(4), fourth.Number, "numbers are never reused")
}

func TestMergeRequestAccess(t *testing.T) {
	r := newMRRig(t) // proj is private
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	n := mr.Number
	noScope := writer(domain.ScopeRepoWrite)
	otherRepo := domain.TokenPrincipal(domain.Token{ID: "o", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeMRWrite, domain.ScopeMRMerge}, Repos: []domain.RepoName{name(t, "else")}})

	reads := map[string]func(p domain.Principal) error{
		"List": func(p domain.Principal) error {
			_, err := r.mrs.List(r.ctx, p, r.repo, ports.ListMergeRequestsParams{})
			return err
		},
		"Get": func(p domain.Principal) error {
			_, err := r.mrs.Get(r.ctx, p, r.repo, n, ports.GetMergeRequestOptions{})
			return err
		},
	}
	for what, call := range reads {
		for who, p := range map[string]domain.Principal{"anonymous": domain.Anonymous(), "no repo:read": noScope, "agent, no repo:read": agent, "token of another repo": otherRepo} {
			assert.ErrorIs(t, call(p), domain.ErrNotFound, "%s by %s on a private repository", what, who)
		}
		assert.NoError(t, call(reader), what+" with repo:read")
		assert.NoError(t, call(root), what+" by root")
	}
	_, err := r.mrs.Get(r.ctx, reader, r.repo, n+1, ports.GetMergeRequestOptions{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "unknown number")
	_, err = r.mrs.List(r.ctx, root, name(t, "ghost"), ports.ListMergeRequestsParams{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "unknown repository")

	writes := map[string]func(p domain.Principal) error{
		"Create": func(p domain.Principal) error {
			_, err := r.mrs.Create(r.ctx, p, r.repo, ports.CreateMergeRequestParams{Title: "t", Source: "feat", Target: "feat"})
			return err
		},
		"AddComment": func(p domain.Principal) error {
			_, err := r.mrs.AddComment(r.ctx, p, r.repo, n, ports.AddCommentParams{Text: "x"})
			return err
		},
		"ResolveComment": func(p domain.Principal) error {
			_, err := r.mrs.ResolveComment(r.ctx, p, r.repo, n, 1, true)
			return err
		},
		"Close": func(p domain.Principal) error {
			_, err := r.mrs.Close(r.ctx, p, r.repo, n+100)
			return err
		},
		"MarkRead": func(p domain.Principal) error {
			_, err := r.mrs.MarkRead(r.ctx, p, r.repo, n, domain.ObjectID{})
			return err
		},
		"Merge": func(p domain.Principal) error {
			_, err := r.mrs.Merge(r.ctx, p, r.repo, n+100, ports.MergeParams{HeadID: mr.HeadID})
			return err
		},
	}
	for what, call := range writes {
		scope := domain.ScopeMRWrite
		if what == "Merge" {
			scope = domain.ScopeMRMerge
		}
		assert.ErrorIs(t, call(domain.Anonymous()), domain.ErrNotFound, "%s: anonymous cannot see the private repository", what)
		assert.ErrorIs(t, call(noScope), domain.ErrNotFound, "%s: no repo:read and not the scope", what)
		assert.ErrorIs(t, call(otherRepo), domain.ErrNotFound, "%s: another repository's token", what)
		assert.ErrorIs(t, call(reader), domain.ErrForbidden, "%s: reading is not writing", what)
		if scope == domain.ScopeMRMerge {
			assert.ErrorIs(t, call(mrW), domain.ErrForbidden, "mr:write does not merge")
			assert.ErrorIs(t, call(agent), domain.ErrNotFound, "no repo:read and not mr:merge: the repository stays hidden")
			continue
		}
		// With the scope the call gets past access (and fails on its subject),
		// with or without repo:read (GO-001; TestAgentPresetWorksOnItsPrivateRepository
		// shows the calls that succeed, as some of these fail with not found on
		// their subject).
		assert.NotErrorIs(t, call(agent), domain.ErrForbidden, what+": the agent preset")
		assert.NotErrorIs(t, call(agent), domain.ErrUnauthenticated, what+": the agent preset")
		assert.NotErrorIs(t, call(mrW), domain.ErrForbidden, what)
		assert.NotErrorIs(t, call(mrW), domain.ErrUnauthenticated, what)
	}

	// On a public repository an anonymous caller reads, but writing is
	// unauthenticated rather than not found.
	pubName := name(t, "pub")
	_, err = r.repos.Create(r.ctx, root, ports.CreateRepoParams{Name: pubName, Visibility: domain.VisibilityPublic})
	require.NoError(t, err)
	_, err = r.mrs.List(r.ctx, domain.Anonymous(), pubName, ports.ListMergeRequestsParams{})
	assert.NoError(t, err)
	_, err = r.mrs.Create(r.ctx, domain.Anonymous(), pubName, ports.CreateMergeRequestParams{Title: "t", Source: "a", Target: "b"})
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

func TestListMergeRequests(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	var nums []int64
	for i := range 5 {
		b := fmt.Sprintf("f%d", i)
		r.feature(b)
		nums = append(nums, r.open(b).Number)
	}
	_, err := r.mrs.Close(r.ctx, root, r.repo, 2)
	require.NoError(t, err)
	r.sh("checkout", "-q", "main")
	merged := r.feature("f0")
	_, err = r.mrs.Merge(r.ctx, root, r.repo, 1, ports.MergeParams{HeadID: merged})
	require.NoError(t, err)
	r.sh("branch", "-D", "f4")
	r.run(r.bare(), r.env(author), "branch", "-D", "f4")

	numbers := func(p ports.MergeRequestPage) []int64 {
		var out []int64
		for _, m := range p.MergeRequests {
			out = append(out, m.Number)
		}
		return out
	}
	all, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{})
	require.NoError(t, err)
	assert.Equal(t, []int64{5, 4, 3, 2, 1}, numbers(all), "highest number first")
	assert.Empty(t, all.NextPageToken)

	open, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{State: domain.MRStateOpen})
	require.NoError(t, err)
	assert.Equal(t, []int64{5, 4, 3}, numbers(open))
	for _, m := range open.MergeRequests {
		if m.Number == 5 {
			assert.True(t, m.HeadID.IsZero(), "the branch of !5 was deleted")
		} else {
			assert.False(t, m.HeadID.IsZero(), "an open request carries its head")
		}
	}
	closed, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{State: domain.MRStateClosed})
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, numbers(closed))
	mergedPage, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{State: domain.MRStateMerged})
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, numbers(mergedPage))
	assert.Equal(t, merged, mergedPage.MergeRequests[0].HeadID, "a merged request keeps the head it had")

	// Paging: the token names the last number.
	p1, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{PageSize: 2})
	require.NoError(t, err)
	assert.Equal(t, []int64{5, 4}, numbers(p1))
	require.NotEmpty(t, p1.NextPageToken)
	// A request created between the pages does not shift them.
	r.feature("late")
	r.open("late")
	p2, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{PageSize: 2, PageToken: p1.NextPageToken})
	require.NoError(t, err)
	assert.Equal(t, []int64{3, 2}, numbers(p2))
	p3, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{PageSize: 2, PageToken: p2.NextPageToken})
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, numbers(p3))
	assert.Empty(t, p3.NextPageToken)

	for _, tok := range []string{"garbage", "bW9yZQ"} {
		_, err = r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{PageToken: tok})
		assert.ErrorIs(t, err, domain.ErrInvalidPageToken)
	}
	_, err = r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{PageSize: -1})
	assert.ErrorIs(t, err, domain.ErrInvalidPageSize)
	_ = nums
}

func TestGetMergeRequest(t *testing.T) {
	r := newMRRig(t)
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	_, err := r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Text: "first"})
	require.NoError(t, err)
	_, err = r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Text: "second"})
	require.NoError(t, err)

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, head, d.MergeRequest.HeadID)
	assert.Equal(t, main, d.DiffBaseID, "the base is the merge base of the target and the head")
	require.Len(t, d.Commits, 2)
	assert.Equal(t, "add c", d.Commits[0].Summary, "newest first")
	assert.False(t, d.CommitsTruncated)
	assert.Equal(t, domain.DiffStats{FilesChanged: 2, Additions: 2, Deletions: 1}, d.Stats)
	require.Len(t, d.Diff.Files, 2)
	require.Len(t, d.Comments, 2)
	assert.Equal(t, "first", d.Comments[0].Text.String(), "oldest first")
	assert.True(t, d.LastReadID.IsZero())
	assert.False(t, d.LastReadLost)

	// The target moved on: the base stays the merge base, not the new tip.
	r.sh("checkout", "-q", "main")
	r.commit(author, "later.txt", "x\n", "later on main")
	r.push("main")
	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, main, d.DiffBaseID)
	assert.Len(t, d.Commits, 2, "the commits that main lacks")

	// A deleted source branch: no commits, no diff, an empty head.
	r.run(r.bare(), r.env(author), "branch", "-D", "feat")
	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.True(t, d.MergeRequest.HeadID.IsZero())
	assert.Empty(t, d.Commits)
	assert.True(t, d.DiffBaseID.IsZero())
	assert.Empty(t, d.Diff.Files)
	assert.Len(t, d.Comments, 2)
}

func TestGetMergeRequestTruncatesCommits(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.sh("checkout", "-q", "-B", "long", "main")
	for i := range usecase.MaxMergeRequestCommits + 5 {
		r.sh("commit", "-q", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
	}
	r.push("long")
	mr := r.open("long")
	d, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Len(t, d.Commits, usecase.MaxMergeRequestCommits)
	assert.True(t, d.CommitsTruncated)
	assert.Equal(t, "c104", d.Commits[0].Summary)
}

// AC3: a comment on a path:line must be a line of the current diff.
func TestAddComment(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	n := mr.Number
	// The diff: a.txt line 2 changed (old "two", new "2"); c.txt added, line 1.

	tests := []struct {
		name   string
		params ports.AddCommentParams
		field  string // "" for valid
	}{
		{"general", ports.AddCommentParams{Text: "looks good"}, ""},
		{"multi-line general", ports.AddCommentParams{Text: "a\n\nb"}, ""},
		{"new side of a changed line", ports.AddCommentParams{Path: "a.txt", Line: 2, Side: domain.SideNew, Text: "x"}, ""},
		{"old side of a changed line", ports.AddCommentParams{Path: "a.txt", Line: 2, Side: domain.SideOld, Text: "x"}, ""},
		{"context line", ports.AddCommentParams{Path: "a.txt", Line: 3, Side: domain.SideNew, Text: "x"}, ""},
		{"added file", ports.AddCommentParams{Path: "c.txt", Line: 1, Side: domain.SideNew, Text: "x"}, ""},
		{"empty text", ports.AddCommentParams{Text: ""}, "text"},
		{"blank text", ports.AddCommentParams{Text: " \n "}, "text"},
		{"text too long", ports.AddCommentParams{Text: strings.Repeat("x", 8001)}, "text"},
		{"text with a control character", ports.AddCommentParams{Text: "a\x1b[0m"}, "text"},
		{"text with a tab", ports.AddCommentParams{Text: "a\tb"}, "text"},
		{"text with a bidi override", ports.AddCommentParams{Text: "a\u202eb"}, "text"},
		{"line outside the diff", ports.AddCommentParams{Path: "a.txt", Line: 40, Side: domain.SideNew, Text: "x"}, "path"},
		{"unchanged file", ports.AddCommentParams{Path: "b.txt", Line: 1, Side: domain.SideNew, Text: "x"}, "path"},
		{"unknown path", ports.AddCommentParams{Path: "nope.txt", Line: 1, Side: domain.SideNew, Text: "x"}, "path"},
		{"deleted line on the wrong side", ports.AddCommentParams{Path: "c.txt", Line: 1, Side: domain.SideOld, Text: "x"}, "path"},
		{"path without a line", ports.AddCommentParams{Path: "a.txt", Side: domain.SideNew, Text: "x"}, "line"},
		{"path without a side", ports.AddCommentParams{Path: "a.txt", Line: 2, Text: "x"}, "side"},
		{"line without a path", ports.AddCommentParams{Line: 2, Side: domain.SideNew, Text: "x"}, "path"},
		{"side without a path", ports.AddCommentParams{Side: domain.SideOld, Text: "x"}, "path"},
		{"negative line", ports.AddCommentParams{Path: "a.txt", Line: -2, Side: domain.SideNew, Text: "x"}, "line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := r.mrs.AddComment(r.ctx, mrW, r.repo, n, tc.params)
			if tc.field == "" {
				require.NoError(t, err)
				assert.Equal(t, "agent-1", c.Author)
				assert.Equal(t, tc.params.Path, c.Path)
				assert.Equal(t, tc.params.Line, c.Line)
				assert.Equal(t, tc.params.Side, c.Side)
				assert.Equal(t, mr.HeadID, c.CommitID, "the head it was written against")
				assert.False(t, c.Resolved)
				return
			}
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.Contains(t, fieldsOf(t, err), tc.field)
		})
	}

	// Ids count 1, 2, ... in order of creation and only the valid ones are kept.
	d, err := r.mrs.Get(r.ctx, root, r.repo, n, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	var kept int
	for _, tc := range tests {
		if tc.field == "" {
			kept++
		}
	}
	require.Len(t, d.Comments, kept)
	for i, c := range d.Comments {
		assert.Equal(t, int64(i+1), c.ID)
	}

	t.Run("not open", func(t *testing.T) {
		_, err := r.mrs.Close(r.ctx, mrW, r.repo, n)
		require.NoError(t, err)
		_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, n, ports.AddCommentParams{Text: "late"})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = r.mrs.ResolveComment(r.ctx, mrW, r.repo, n, 1, true)
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
}

func TestConcurrentComments(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	const workers = 10
	var wg sync.WaitGroup
	ids := make(chan int64, workers)
	for i := range workers {
		wg.Go(func() {
			c, err := r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: fmt.Sprintf("c%d", i)})
			if assert.NoError(t, err) {
				ids <- c.ID
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		assert.False(t, seen[id], "id %d handed out twice", id)
		seen[id] = true
	}
	assert.Len(t, seen, workers)
	d, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Len(t, d.Comments, workers)
}

func TestResolveComment(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	c, err := r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: "fix this"})
	require.NoError(t, err)
	r.drain()

	got, err := r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, c.ID, true)
	require.NoError(t, err)
	assert.True(t, got.Resolved)
	again, err := r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, c.ID, true)
	require.NoError(t, err, "resolving twice is not an error")
	assert.True(t, again.Resolved)
	undone, err := r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, c.ID, false)
	require.NoError(t, err)
	assert.False(t, undone.Resolved)
	undone, err = r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, c.ID, false)
	require.NoError(t, err)
	assert.False(t, undone.Resolved)

	_, err = r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, 99, true)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, 0, true)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	events := r.drain()
	require.Len(t, events, 2, "only state changes publish resolution events")
	assert.True(t, events[0].(domain.CommentResolved).Comment.Resolved)
	assert.False(t, events[1].(domain.CommentResolved).Comment.Resolved)
}

func TestEventsOfAMergeRequest(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	r.drain()

	mr := r.open("feat")
	c, err := r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: "note"})
	require.NoError(t, err)
	_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)

	r.feature("other")
	other := r.open("other")
	_, err = r.mrs.Close(r.ctx, mrW, r.repo, other.Number)
	require.NoError(t, err)

	evs := r.drain()
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.EventKind())
	}
	assert.Equal(t, []string{"mr.opened", "mr.comment_added", "mr.merged", "repo.pushed", "mr.opened", "mr.closed"}, kinds)

	opened := evs[0].(domain.MergeRequestOpened)
	assert.Equal(t, mr.Number, opened.MergeRequest.Number)
	assert.Equal(t, domain.VisibilityPrivate, opened.Visibility, "the event carries the visibility, so that it is filtered")
	assert.False(t, opened.CreatedAt.IsZero())
	added := evs[1].(domain.CommentAdded)
	assert.Equal(t, c, added.Comment)
	assert.Equal(t, mr.Number, added.Number)
	merged := evs[2].(domain.MergeRequestMerged)
	assert.Equal(t, domain.MRStateMerged, merged.MergeRequest.State)
	push := evs[3].(domain.PushEvent)
	assert.Equal(t, []domain.RefUpdate{{Old: r.seedID(), New: head.String(), Ref: "refs/heads/main"}}, push.Updates)
	assert.Equal(t, domain.VisibilityPrivate, push.Visibility)
	assert.Equal(t, domain.MRStateClosed, evs[5].(domain.MergeRequestClosed).MergeRequest.State)

	// Failed calls publish nothing.
	_, err = r.mrs.Close(r.ctx, mrW, r.repo, other.Number)
	require.Error(t, err)
	_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, other.Number, ports.AddCommentParams{Text: "x"})
	require.Error(t, err)
	assert.Empty(t, r.drain())
}

// seedID is the id main had before the merges of the test: the parent of its
// first feature commit.
func (r *mrRig) seedID() string {
	return r.run(r.bare(), r.env(author), "rev-parse", "refs/heads/feat~2")
}

func TestMarkReadAndSinceLastRead(t *testing.T) {
	r := newMRRig(t)
	base := r.seed()
	head1 := r.feature("feat")
	mr := r.open("feat")
	n := mr.Number

	// Nothing recorded: the flag changes nothing.
	d, err := r.mrs.Get(r.ctx, mrW, r.repo, n, ports.GetMergeRequestOptions{SinceLastRead: true})
	require.NoError(t, err)
	assert.True(t, d.LastReadID.IsZero())
	assert.False(t, d.LastReadLost)
	assert.Equal(t, base, d.DiffBaseID)

	// Without a head: the current head.
	got, err := r.mrs.MarkRead(r.ctx, mrW, r.repo, n, domain.ObjectID{})
	require.NoError(t, err)
	assert.Equal(t, head1, got)
	d, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, head1, d.LastReadID, "reported whether or not since_last_read is set")
	assert.Equal(t, base, d.DiffBaseID)

	// New work on the branch: the diff since the last read has only that.
	r.commit(author, "d.txt", "dee\n", "add d")
	head2 := r.head()
	r.push("feat")
	d, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{SinceLastRead: true})
	require.NoError(t, err)
	assert.Equal(t, head1, d.DiffBaseID)
	assert.Equal(t, head2, d.MergeRequest.HeadID)
	require.Len(t, d.Diff.Files, 1)
	assert.Equal(t, "d.txt", d.Diff.Files[0].NewPath)
	assert.Equal(t, domain.DiffStats{FilesChanged: 1, Additions: 1}, d.Stats)
	assert.Len(t, d.Commits, 3, "the commits are still those of the target..head range")

	// Marking a commit that is not in the repository is refused.
	_, err = r.mrs.MarkRead(r.ctx, mrW, r.repo, n, oidFrom(t, strings.Repeat("1", 40)))
	require.ErrorIs(t, err, domain.ErrInvalid)
	assert.Contains(t, fieldsOf(t, err), "head_id")
	// Neither is a tree.
	tree := oidFrom(t, r.run(r.bare(), r.env(author), "rev-parse", head1.String()+"^{tree}"))
	_, err = r.mrs.MarkRead(r.ctx, mrW, r.repo, n, tree)
	require.ErrorIs(t, err, domain.ErrInvalid)
	// An explicit head is recorded as given.
	_, err = r.mrs.MarkRead(r.ctx, mrW, r.repo, n, base)
	require.NoError(t, err)
	d, err = r.mrs.Get(r.ctx, reader, r.repo, n, ports.GetMergeRequestOptions{SinceLastRead: true})
	require.NoError(t, err)
	assert.Equal(t, base, d.DiffBaseID)
}

// AC6: the diff since the last read is right after a force-push; when the
// recorded commit is gone the whole change is shown and the response says so.
func TestSinceLastReadAfterForcePush(t *testing.T) {
	r := newMRRig(t)
	base := r.seed()
	head1 := r.feature("feat")
	mr := r.open("feat")
	_, err := r.mrs.MarkRead(r.ctx, mrW, r.repo, mr.Number, domain.ObjectID{})
	require.NoError(t, err)

	// Force-push: the last commit is amended (the old one is unreachable but
	// still stored), and one more file is added.
	r.sh("checkout", "-q", "feat")
	require.NoError(t, os.WriteFile(filepath.Join(r.work, "c.txt"), []byte("see, amended\n"), 0o644))
	r.sh("add", "c.txt")
	r.sh("commit", "-q", "--amend", "-m", "add c, amended")
	head2 := r.head()
	require.NotEqual(t, head1, head2)
	r.push("feat")
	assert.Equal(t, head2, r.branchHead("feat"))
	// head1 is not an ancestor of head2 any more.
	r.run(r.bare(), r.env(author), "cat-file", "-e", head1.String())

	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{SinceLastRead: true})
	require.NoError(t, err)
	assert.False(t, d.LastReadLost)
	assert.Equal(t, head1, d.DiffBaseID, "the diff starts at the head that was read")
	require.Len(t, d.Diff.Files, 1, "only what the force-push changed")
	assert.Equal(t, "c.txt", d.Diff.Files[0].NewPath)
	var added, removed []string
	for _, l := range d.Diff.Files[0].Hunks[0].Lines {
		switch l.Kind {
		case domain.LineAddition:
			added = append(added, l.Text)
		case domain.LineDeletion:
			removed = append(removed, l.Text)
		}
	}
	assert.Equal(t, []string{"see, amended"}, added)
	assert.Equal(t, []string{"see"}, removed)

	// The whole change is still available without the flag.
	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, base, d.DiffBaseID)
	assert.Len(t, d.Diff.Files, 2)

	// Once the recorded commit is pruned, the response says so.
	r.run(r.bare(), r.env(author), "gc", "-q", "--prune=now")
	d, err = r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{SinceLastRead: true})
	require.NoError(t, err)
	assert.True(t, d.LastReadLost, "the recorded head is gone")
	assert.Equal(t, head1, d.LastReadID, "still reported")
	assert.Equal(t, base, d.DiffBaseID, "the diff is against the target")
	assert.Len(t, d.Diff.Files, 2)
}

func TestMergeFastForward(t *testing.T) {
	r := newMRRig(t)
	main := r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	r.drain()

	merged, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateMerged, merged.State)
	assert.Equal(t, domain.MergeModeFastForward, merged.MergeMode, "the default mode")
	assert.Equal(t, head, merged.MergedCommitID, "no commit is created")
	assert.Equal(t, head, merged.HeadID)
	assert.Equal(t, head, r.branchHead("main"))
	assert.Equal(t, "root", merged.Author)
	_ = main

	// The stored request is the merged one, and reads keep a diff.
	d, err := r.mrs.Get(r.ctx, reader, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateMerged, d.MergeRequest.State)
	assert.Equal(t, head, d.MergeRequest.HeadID)
	assert.Equal(t, head, d.MergeRequest.MergedCommitID)
	assert.Equal(t, main, d.DiffBaseID, "the base of the merge stays readable")
	assert.Len(t, d.Commits, 2, "the commits that were merged")
	assert.Len(t, d.Diff.Files, 2)

	// Repeating it, or touching a final request, is a failed precondition.
	_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = r.mrs.Close(r.ctx, mrW, r.repo, mr.Number)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: "x"})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
}

// A draft is not merged until it is marked ready; ready drops the prefix of
// the title, and only for an open draft. CountOpen counts drafts as open.
func TestDraftMergeRequest(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "WIP: add feat", Source: "feat"})
	require.NoError(t, err)

	_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	assert.Contains(t, err.Error(), "draft")

	n, err := r.mrs.CountOpen(r.ctx, reader, r.repo)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = r.mrs.MarkReady(r.ctx, reader, r.repo, mr.Number)
	require.Error(t, err, "mr:write is needed")
	r.drain()
	ready, err := r.mrs.MarkReady(r.ctx, mrW, r.repo, mr.Number)
	require.NoError(t, err)
	assert.Equal(t, "add feat", ready.Title.String())
	assert.False(t, ready.IsDraft())
	evs := r.drain()
	require.Len(t, evs, 1, "live pages learn the request is ready")
	assert.Equal(t, "add feat", evs[0].(domain.MergeRequestReady).MergeRequest.Title.String())
	_, err = r.mrs.MarkReady(r.ctx, mrW, r.repo, mr.Number)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed, "not a draft any more")

	_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
	require.NoError(t, err)
	n, err = r.mrs.CountOpen(r.ctx, reader, r.repo)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// AC4: the checks of a merge.
func TestMergeRefusals(t *testing.T) {
	t.Run("a branch that moved since the review", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		r.commit(author, "more.txt", "m\n", "pushed after the review")
		r.push("feat")
		_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "branch moved, read again")
		assert.NotEqual(t, head, r.branchHead("main"), "main did not move")
		// Reading again gives the new head, which merges.
		d, err := r.mrs.Get(r.ctx, mrM, r.repo, mr.Number, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: d.MergeRequest.HeadID})
		require.NoError(t, err)
	})

	t.Run("a target that is not an ancestor of the head", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		r.sh("checkout", "-q", "main")
		r.commit(author, "news.txt", "n\n", "main moved")
		r.push("main")
		tip := r.branchHead("main")

		for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash} {
			_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: mode})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed, mode)
			assert.Contains(t, err.Error(), "rebase", mode)
		}
		assert.Equal(t, tip, r.branchHead("main"))
		d, err := r.mrs.Get(r.ctx, mrM, r.repo, mr.Number, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		assert.Equal(t, domain.MRStateOpen, d.MergeRequest.State)

		// After a rebase and a push it merges.
		r.sh("checkout", "-q", "feat")
		r.sh("rebase", "-q", "main")
		r.push("feat")
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: r.head()})
		require.NoError(t, err)
	})

	t.Run("a mode the repository does not allow", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		modes := []domain.MergeMode{domain.MergeModeFastForward}
		_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: &modes})
		require.NoError(t, err)

		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeSquash})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "squash")
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.NoError(t, err, "fast-forward is allowed")
	})

	t.Run("only squash is allowed, so the default mode is refused", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		modes := []domain.MergeMode{domain.MergeModeSquash}
		_, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: &modes})
		require.NoError(t, err)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeSquash})
		require.NoError(t, err)
	})

	t.Run("bad arguments", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{})
		require.ErrorIs(t, err, domain.ErrInvalid)
		assert.Contains(t, fieldsOf(t, err), "head_id")
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: "rebase"})
		require.ErrorIs(t, err, domain.ErrInvalid)
		assert.Contains(t, fieldsOf(t, err), "mode")
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, 99, ports.MergeParams{HeadID: head})
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a deleted source or target, or nothing to merge", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		head := r.feature("feat")
		mr := r.open("feat")
		r.run(r.bare(), r.env(author), "branch", "-D", "feat")
		_, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), "no longer exists")

		r.push("feat")
		r.run(r.bare(), r.env(author), "update-ref", "refs/heads/main", head.String())
		_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head})
		require.ErrorIs(t, err, domain.ErrPreconditionFailed, "the target already is at the head")
		assert.Contains(t, err.Error(), "nothing to merge")
	})
}

// AC4: merges of one repository run one at a time.
func TestConcurrentMergesSerialize(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	const n = 6
	heads := map[int64]domain.ObjectID{}
	for i := range n {
		b := fmt.Sprintf("f%d", i)
		heads[r.openFeature(b).Number] = r.branchHead(b)
	}

	type outcome struct {
		number int64
		err    error
	}
	results := make(chan outcome, n*2)
	var wg sync.WaitGroup
	for number, head := range heads {
		for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash} {
			if mode == domain.MergeModeSquash && number%2 == 0 {
				continue
			}
			wg.Go(func() {
				_, err := r.mrs.Merge(r.ctx, mrM, r.repo, number, ports.MergeParams{HeadID: head, Mode: mode})
				results <- outcome{number, err}
			})
		}
	}
	wg.Wait()
	close(results)

	won := map[int64]int{}
	for o := range results {
		if o.err == nil {
			won[o.number]++
			continue
		}
		require.ErrorIs(t, o.err, domain.ErrPreconditionFailed, "a loser fails cleanly: %v", o.err)
	}
	// All the branches start from the same main: exactly one request can win,
	// once, whatever the mode, and main is where it left it.
	require.Len(t, won, 1, "exactly one merge request was merged: %v", won)
	for number, times := range won {
		assert.Equal(t, 1, times, "!%d merged twice", number)
		mr, err := r.mrs.Get(r.ctx, root, r.repo, number, ports.GetMergeRequestOptions{})
		require.NoError(t, err)
		assert.Equal(t, domain.MRStateMerged, mr.MergeRequest.State)
		assert.Equal(t, mr.MergeRequest.MergedCommitID, r.branchHead("main"))
	}
	for number := range heads {
		if won[number] == 0 {
			d, err := r.mrs.Get(r.ctx, root, r.repo, number, ports.GetMergeRequestOptions{})
			require.NoError(t, err)
			assert.Equal(t, domain.MRStateOpen, d.MergeRequest.State, "!%d lost and stays open", number)
		}
	}
}

// openFeature pushes a branch from main with one commit and opens a request.
func (r *mrRig) openFeature(branch string) domain.MergeRequest {
	r.t.Helper()
	r.sh("checkout", "-q", "-B", branch, "main")
	r.commit(author, branch+".txt", branch+"\n", "add "+branch)
	r.push(branch)
	return r.open(branch)
}

func TestSquashMerge(t *testing.T) {
	r := newMRRig(t)
	main := r.seed()
	// A branch of three commits by two authors; Bob wrote the first one.
	bob := domain.Identity{Name: "Bob", Email: "bob@example.org"}
	cy := domain.Identity{Name: "Cy", Email: "cy@example.org"}
	r.sh("checkout", "-q", "-B", "feat", "main")
	r.commit(bob, "x.txt", "x\n", "bob starts")
	r.commit(author, "y.txt", "y\n", "ada continues")
	r.commit(cy, "z.txt", "z\n", "cy finishes")
	r.commit(bob, "w.txt", "w\n", "bob again")
	r.push("feat")
	head := r.branchHead("feat")
	mr, err := r.mrs.Create(r.ctx, root, r.repo, ports.CreateMergeRequestParams{Title: "Add things", Source: "feat"})
	require.NoError(t, err)
	r.drain()

	merged, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: head, Mode: domain.MergeModeSquash})
	require.NoError(t, err)
	assert.Equal(t, domain.MergeModeSquash, merged.MergeMode)
	require.False(t, merged.MergedCommitID.IsZero())
	assert.NotEqual(t, head, merged.MergedCommitID, "a squash creates a commit")
	assert.Equal(t, merged.MergedCommitID, r.branchHead("main"))

	sq := merged.MergedCommitID.String()
	g := func(args ...string) string { return r.run(r.bare(), r.env(author), args...) }
	assert.Equal(t, main.String(), g("rev-parse", sq+"^"), "the target is the only parent")
	assert.Equal(t, g("rev-parse", head.String()+"^{tree}"), g("rev-parse", sq+"^{tree}"), "the tree of the head")
	assert.Equal(t, "Bob <bob@example.org>", g("log", "-1", "--format=%an <%ae>", sq), "author of the first commit")
	assert.Equal(t, "ermite <ermite@git.example.org>", g("log", "-1", "--format=%cn <%ce>", sq))
	assert.Equal(t, "Add things (!1)\n\n- bob starts\n- ada continues\n- cy finishes\n- bob again\n\n"+
		"Co-authored-by: Ada Lovelace <ada@example.org>\nCo-authored-by: Cy <cy@example.org>",
		g("log", "-1", "--format=%B", sq))
	assert.Contains(t, g("cat-file", "commit", sq), "\ngpgsig -----BEGIN SSH SIGNATURE-----\n")

	// Events: merged, then pushed for the target branch.
	evs := r.drain()
	require.Len(t, evs, 2)
	assert.Equal(t, merged.MergedCommitID, evs[0].(domain.MergeRequestMerged).MergeRequest.MergedCommitID)
	assert.Equal(t, []domain.RefUpdate{{Old: main.String(), New: sq, Ref: "refs/heads/main"}}, evs[1].(domain.PushEvent).Updates)

	// The signature verifies with an allowed_signers file built from the
	// public key the server hands out.
	r.verifyCommit(sq)
}

// verifyCommit checks the signature of a commit of the repository with
// `git verify-commit` and an allowed signers file made from the signing key
// info, and that another key does not verify.
func (r *mrRig) verifyCommit(id string) {
	r.t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		unusable(r.t, "ssh-keygen not found: install openssh to verify the signed commits (CI must have it)")
	}
	dir := r.t.TempDir()
	signers := filepath.Join(dir, "allowed_signers")
	require.NoError(r.t, os.WriteFile(signers, []byte(r.key.Info().AllowedSigners()+"\n"), 0o600))
	out := r.run(r.bare(), r.env(author), "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", "--raw", id)
	assert.Contains(r.t, out, "Good \"git\" signature for ermite@git.example.org")

	other, err := sshsig.New(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	require.NoError(r.t, err)
	wrong := filepath.Join(dir, "wrong_signers")
	require.NoError(r.t, os.WriteFile(wrong, []byte("ermite@git.example.org "+other.PublicKey().AuthorizedKey+"\n"), 0o600))
	cmd := exec.Command("git", "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+wrong, "verify-commit", id)
	cmd.Dir, cmd.Env = r.bare(), r.env(author)
	assert.Error(r.t, cmd.Run(), "another key must not verify the commit")
}

func TestSquashSingleCommitHasNoTrailer(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	head := r.feature("solo") // two authors; make a one-author branch instead
	_ = head
	r.sh("checkout", "-q", "-B", "mine", "main")
	r.commit(author, "m.txt", "m\n", "only")
	r.push("mine")
	mr := r.open("mine")
	merged, err := r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{HeadID: r.branchHead("mine"), Mode: domain.MergeModeSquash})
	require.NoError(t, err)
	msg := r.run(r.bare(), r.env(author), "log", "-1", "--format=%B", merged.MergedCommitID.String())
	assert.Equal(t, "Add mine (!1)\n\n- only", msg)
	r.verifyCommit(merged.MergedCommitID.String())
}

func TestUpdateRepoSettings(t *testing.T) {
	r := newMRRig(t)
	repo, err := r.repos.Get(r.ctx, root, r.repo)
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultRepoSettings(), repo.Settings, "stored explicitly at creation")

	modes := []domain.MergeMode{domain.MergeModeSquash, domain.MergeModeSquash}
	off := false
	got, err := r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: &modes, FailingChecksBlock: &off})
	require.NoError(t, err)
	assert.Equal(t, domain.RepoSettings{AllowedMergeModes: []domain.MergeMode{domain.MergeModeSquash}, FailingChecksBlock: false, Issues: domain.IssuesOff}, got.Settings)
	again, err := r.repos.Get(r.ctx, root, r.repo)
	require.NoError(t, err)
	assert.Equal(t, got.Settings, again.Settings)

	// Only the named setting changes.
	on := true
	got, err = r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{FailingChecksBlock: &on})
	require.NoError(t, err)
	assert.Equal(t, []domain.MergeMode{domain.MergeModeSquash}, got.Settings.AllowedMergeModes)
	assert.True(t, got.Settings.FailingChecksBlock)

	for _, bad := range [][]domain.MergeMode{{}, nil, {"rebase"}} {
		bad := bad
		_, err = r.repos.Update(r.ctx, root, r.repo, ports.UpdateRepoParams{AllowedMergeModes: &bad})
		assert.ErrorIs(t, err, domain.ErrInvalid, "%v", bad)
	}
	again, err = r.repos.Get(r.ctx, root, r.repo)
	require.NoError(t, err)
	assert.Equal(t, got.Settings, again.Settings, "a refused change stores nothing")

	// repo:admin is needed, like for any other field.
	_, err = r.repos.Update(r.ctx, writer(domain.ScopeRepoRead, domain.ScopeMRMerge), r.repo, ports.UpdateRepoParams{FailingChecksBlock: &off})
	assert.ErrorIs(t, err, domain.ErrForbidden)
}

func TestDeletingARepositoryDropsItsMergeRequests(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	r.open("feat")
	require.NoError(t, r.repos.Delete(r.ctx, root, r.repo))

	_, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Created again under the same name, it starts empty.
	_, err = r.repos.Create(r.ctx, root, ports.CreateRepoParams{Name: r.repo})
	require.NoError(t, err)
	page, err := r.mrs.List(r.ctx, root, r.repo, ports.ListMergeRequestsParams{})
	require.NoError(t, err)
	assert.Empty(t, page.MergeRequests)
}

func TestSigningKeyInfo(t *testing.T) {
	r := newMRRig(t)
	info := r.key.Info()
	assert.True(t, strings.HasPrefix(info.Key.AuthorizedKey, "ssh-ed25519 "))
	assert.True(t, strings.HasPrefix(info.Key.Fingerprint, "SHA256:"))
	assert.Equal(t, "ermite@git.example.org "+info.Key.AuthorizedKey, info.AllowedSigners())
	assert.Equal(t, domain.Identity{Name: "ermite", Email: "ermite@git.example.org"}, info.Committer)
}

// GO-001: the agent preset (repo:write, mr:write) has no repo:read, yet it
// opens, comments on, resolves, marks read and closes merge requests on the
// private repository it pushed to. Reading them still needs repo:read.
func TestAgentPresetWorksOnItsPrivateRepository(t *testing.T) {
	r := newMRRig(t) // proj is private
	r.seed()
	head := r.feature("feat")

	mr, err := r.mrs.Create(r.ctx, agent, r.repo, ports.CreateMergeRequestParams{Title: "From the agent", Source: "feat"})
	require.NoError(t, err)
	assert.Equal(t, head, mr.HeadID)
	assert.Equal(t, "agent-1", mr.Author)

	c, err := r.mrs.AddComment(r.ctx, agent, r.repo, mr.Number, ports.AddCommentParams{Text: "note"})
	require.NoError(t, err)
	_, err = r.mrs.ResolveComment(r.ctx, agent, r.repo, mr.Number, c.ID, true)
	require.NoError(t, err)
	_, err = r.mrs.MarkRead(r.ctx, agent, r.repo, mr.Number, domain.ObjectID{})
	require.NoError(t, err)

	// repo:write alone does not read merge requests.
	_, err = r.mrs.List(r.ctx, agent, r.repo, ports.ListMergeRequestsParams{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = r.mrs.Get(r.ctx, agent, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	closed, err := r.mrs.Close(r.ctx, agent, r.repo, mr.Number)
	require.NoError(t, err)
	assert.Equal(t, domain.MRStateClosed, closed.State)

	// An unknown repository is not found whatever the scopes.
	_, err = r.mrs.Create(r.ctx, agent, name(t, "ghost"), ports.CreateMergeRequestParams{Title: "t", Source: "feat"})
	assert.ErrorIs(t, err, domain.ErrNotFound)
	// A token limited to other repositories does not get the scope here.
	elsewhere := domain.TokenPrincipal(domain.Token{ID: "e", Scopes: []domain.Scope{domain.ScopeRepoWrite, domain.ScopeMRWrite}, Repos: []domain.RepoName{name(t, "else")}})
	_, err = r.mrs.Create(r.ctx, elsewhere, r.repo, ports.CreateMergeRequestParams{Title: "t", Source: "feat"})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// GO-006: a merge moves the target branch, so it records pushed_at and queues
// the maintenance of the repository, as a push does.
func TestMergeRecordsAPush(t *testing.T) {
	for _, mode := range []domain.MergeMode{domain.MergeModeFastForward, domain.MergeModeSquash} {
		t.Run(string(mode), func(t *testing.T) {
			r := newMRRig(t)
			r.seed()
			head := r.feature("feat")
			mr := r.open("feat")
			before, err := r.repos.Get(r.ctx, root, r.repo)
			require.NoError(t, err)
			require.True(t, before.PushedAt.IsZero(), "no push recorded yet")
			assert.Empty(t, r.queued.names())

			_, err = r.mrs.Merge(r.ctx, mrM, r.repo, mr.Number, ports.MergeParams{Mode: mode, HeadID: head})
			require.NoError(t, err)

			after, err := r.repos.Get(r.ctx, root, r.repo)
			require.NoError(t, err)
			assert.Equal(t, now, after.PushedAt)
			assert.Equal(t, []domain.RepoName{r.repo}, r.queued.names(), "maintenance queued")
		})
	}
}

// GO-008: a merge request holds at most MaxCommentsPerMergeRequest comments.
func TestCommentsPerMergeRequestAreBounded(t *testing.T) {
	r := newMRRig(t)
	r.seed()
	r.feature("feat")
	mr := r.open("feat")
	for i := range usecase.MaxCommentsPerMergeRequest {
		_, err := r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: fmt.Sprintf("c%d", i)})
		require.NoError(t, err, "comment %d", i+1)
	}
	_, err := r.mrs.AddComment(r.ctx, mrW, r.repo, mr.Number, ports.AddCommentParams{Text: "one too many"})
	assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	d, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.Len(t, d.Comments, usecase.MaxCommentsPerMergeRequest)

	// Resolving still works, and another request has its own budget.
	_, err = r.mrs.ResolveComment(r.ctx, mrW, r.repo, mr.Number, 1, true)
	require.NoError(t, err)
	r.sh("checkout", "-q", "-B", "other", "main")
	r.commit(author, "o.txt", "o\n", "other")
	r.push("other")
	other := r.open("other")
	_, err = r.mrs.AddComment(r.ctx, mrW, r.repo, other.Number, ports.AddCommentParams{Text: "fine"})
	require.NoError(t, err)
}

func TestNoteThreadsRangesAndVerdicts(t *testing.T) {
	t.Run("start outside diff", func(t *testing.T) {
		r := newMRRig(t)
		r.seed()
		before := strings.Repeat("unchanged\n", 30)
		r.commit(author, "long.txt", before, "long base")
		r.push("main")
		r.sh("checkout", "-q", "-B", "range", "main")
		r.commit(author, "long.txt", before+"changed\n", "late hunk")
		r.push("range")
		mr := r.open("range")
		_, err := r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Path: "long.txt", Line: 31, Side: domain.SideNew, Text: "valid"})
		require.NoError(t, err)
		_, err = r.mrs.AddComment(r.ctx, root, r.repo, mr.Number, ports.AddCommentParams{Path: "long.txt", StartLine: 1, Line: 31, Side: domain.SideNew, Text: "invalid range"})
		require.ErrorIs(t, err, domain.ErrInvalid)
		assert.Contains(t, fieldsOf(t, err), "start_line")
	})
	r := newMRRig(t)
	r.seed()
	head := r.feature("feat")
	mr := r.open("feat")
	add := func(p domain.Principal, params ports.AddCommentParams) (domain.Comment, error) {
		return r.mrs.AddComment(r.ctx, p, r.repo, mr.Number, params)
	}
	rootNote, err := add(root, ports.AddCommentParams{Path: "a.txt", StartLine: 1, Line: 3, Side: domain.SideNew, Text: "range"})
	require.NoError(t, err)
	reply, err := add(mrW, ports.AddCommentParams{ReplyTo: rootNote.ID, Text: "reply"})
	require.NoError(t, err)
	assert.Equal(t, rootNote.Path, reply.Path)
	assert.Equal(t, rootNote.StartLine, reply.StartLine)
	assert.Equal(t, rootNote.Line, reply.Line)
	assert.Equal(t, rootNote.Side, reply.Side)
	assert.Equal(t, head, reply.CommitID)
	assert.False(t, reply.Resolved)
	for _, params := range []ports.AddCommentParams{
		{ReplyTo: reply.ID, Text: "x"}, {ReplyTo: 999, Text: "x"}, {ReplyTo: rootNote.ID, Path: "a.txt", Text: "x"}, {ReplyTo: rootNote.ID, Line: 1, Text: "x"}, {ReplyTo: rootNote.ID, Side: domain.SideNew, Text: "x"}, {ReplyTo: rootNote.ID, StartLine: 1, Text: "x"}, {ReplyTo: rootNote.ID, Verdict: domain.VerdictApprove},
		{StartLine: 1, Text: "x"}, {Path: "a.txt", StartLine: 3, Line: 3, Side: domain.SideNew, Text: "x"}, {Path: "a.txt", StartLine: -1, Line: 3, Side: domain.SideNew, Text: "x"}, {Path: "a.txt", StartLine: 1, Line: 99, Side: domain.SideNew, Text: "x"},
	} {
		_, err := add(root, params)
		assert.ErrorIs(t, err, domain.ErrInvalid, "%+v", params)
	}
	_, err = r.mrs.ResolveComment(r.ctx, root, r.repo, mr.Number, reply.ID, true)
	assert.ErrorIs(t, err, domain.ErrInvalid)
	assert.ErrorContains(t, err, fmt.Sprintf("root ^%d", rootNote.ID))
	_, err = add(mrW, ports.AddCommentParams{Verdict: domain.VerdictApprove})
	assert.ErrorIs(t, err, domain.ErrForbidden)
	assert.ErrorContains(t, err, "this needs the mr:review scope")
	reviewer := domain.TokenPrincipal(domain.Token{Label: "reviewer", Scopes: []domain.Scope{domain.ScopeMRWrite, domain.ScopeMRReview}})
	approve, err := add(reviewer, ports.AddCommentParams{Verdict: domain.VerdictApprove})
	require.NoError(t, err)
	assert.True(t, approve.Resolved)
	changes, err := add(root, ports.AddCommentParams{Verdict: domain.VerdictChangesRequested})
	require.NoError(t, err)
	assert.False(t, changes.Resolved)
	_, err = r.mrs.EditComment(r.ctx, root, r.repo, mr.Number, approve.ID, "no")
	assert.ErrorIs(t, err, domain.ErrForbidden)
	_, err = r.mrs.DeleteComment(r.ctx, root, r.repo, mr.Number, approve.ID)
	assert.ErrorIs(t, err, domain.ErrForbidden)
	r.drain()
	edited, err := r.mrs.EditComment(r.ctx, reviewer, r.repo, mr.Number, approve.ID, "updated")
	require.NoError(t, err)
	assert.False(t, edited.EditedAt.IsZero())
	assert.Equal(t, approve.Verdict, edited.Verdict)
	_, err = r.mrs.EditComment(r.ctx, reviewer, r.repo, mr.Number, approve.ID, "updated")
	require.NoError(t, err)
	events := r.drain()
	require.Len(t, events, 1)
	assert.IsType(t, domain.CommentEdited{}, events[0])
	deleted, err := r.mrs.DeleteComment(r.ctx, reviewer, r.repo, mr.Number, approve.ID)
	require.NoError(t, err)
	assert.True(t, deleted.Deleted)
	assert.Equal(t, "", deleted.Text.String())
	assert.Equal(t, approve.Verdict, deleted.Verdict)
	again, err := r.mrs.DeleteComment(r.ctx, reviewer, r.repo, mr.Number, approve.ID)
	require.NoError(t, err)
	deleted.Version = again.Version // SaveComment assigns the new version on the next read.
	assert.Equal(t, deleted, again)
	events = r.drain()
	require.Len(t, events, 1)
	assert.IsType(t, domain.CommentDeleted{}, events[0])
	_, err = r.mrs.EditComment(r.ctx, reviewer, r.repo, mr.Number, approve.ID, "x")
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = r.mrs.EditComment(r.ctx, root, r.repo, mr.Number, rootNote.ID, "")
	assert.ErrorIs(t, err, domain.ErrInvalid)
	_, err = r.mrs.DeleteComment(r.ctx, root, r.repo, mr.Number, rootNote.ID)
	require.NoError(t, err)
	_, err = add(root, ports.AddCommentParams{ReplyTo: rootNote.ID, Text: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalid)
	r.commit(author, "a.txt", "changed\n2\nthree\n", "advance")
	r.push("feat")
	detail, err := r.mrs.Get(r.ctx, root, r.repo, mr.Number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	require.Len(t, detail.Verdicts, 2)
	assert.True(t, detail.Verdicts[0].Stale)
	assert.False(t, detail.Verdicts[1].Stale)
	_, err = r.mrs.Close(r.ctx, root, r.repo, mr.Number)
	require.NoError(t, err)
	_, err = r.mrs.EditComment(r.ctx, root, r.repo, mr.Number, changes.ID, "x")
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = r.mrs.DeleteComment(r.ctx, root, r.repo, mr.Number, changes.ID)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
}
