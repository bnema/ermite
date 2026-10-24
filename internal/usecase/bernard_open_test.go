// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

const botWorkflow = ".github/workflows/ci.yml"

type openRig struct {
	t      *testing.T
	repo   domain.RepoName
	store  *mocks.DepsStore
	meta   *mocks.RepoMetaStore
	reader *mocks.RepoReader
	repos  *mocks.GitRepos
	git    *mocks.MergeGit
	signer *mocks.CommitSigner
	tips   *mocks.CIRefTipStore
	ci     *mocks.WorkflowTrigger
	mrs    *mocks.MergeRequestService
	mrst   *mocks.MergeRequestStore
	eco    *mocks.DepsEcosystem
	bus    *mocks.EventBus
	events chan domain.Event
	b      *usecase.Bernard

	mu        sync.Mutex
	rec       domain.RepoDeps
	ops       []string
	ignored   []domain.DepRef
	registers atomic.Int32
	commit    []byte
	created   ports.CreateMergeRequestParams
	creator   domain.Principal
	tip       domain.CIRefTip
	branchOld domain.ObjectID
	branchNew domain.ObjectID
	pushed    []domain.RefUpdate
	pusher    domain.Principal

	changedEvents []domain.DependencyUpdatesChanged
	changedStates []domain.DepsState // the stored state when each deps.changed was published
}

func (r *openRig) op(s string) { r.mu.Lock(); r.ops = append(r.ops, s); r.mu.Unlock() }

func newOpenRig(t *testing.T, eco domain.DepsEcosystem, applyInServer bool) *openRig {
	t.Helper()
	return newOpenRigWith(t, func(r *openRig) []ports.DepsEcosystem {
		r.eco.EXPECT().Name().Return(eco).Maybe()
		r.eco.EXPECT().ApplyInServer().Return(applyInServer).Maybe()
		return []ports.DepsEcosystem{r.eco}
	})
}

// newOpenRigWith is newOpenRig with the ecosystems chosen by the test.
func newOpenRigWith(t *testing.T, ecos func(*openRig) []ports.DepsEcosystem) *openRig {
	t.Helper()
	r := &openRig{
		t: t, repo: name(t, "proj"),
		store: mocks.NewDepsStore(t), meta: mocks.NewRepoMetaStore(t), reader: mocks.NewRepoReader(t),
		repos: mocks.NewGitRepos(t), git: mocks.NewMergeGit(t), signer: mocks.NewCommitSigner(t),
		tips: mocks.NewCIRefTipStore(t), ci: mocks.NewWorkflowTrigger(t), mrs: mocks.NewMergeRequestService(t),
		mrst: mocks.NewMergeRequestStore(t), eco: mocks.NewDepsEcosystem(t), bus: mocks.NewEventBus(t),
		events: make(chan domain.Event, 4),
	}
	r.rec = domain.RepoDeps{Repo: r.repo}
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)).Maybe()
	r.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(isDepsChanged)).Run(func(_ context.Context, ev domain.Event) {
		r.mu.Lock()
		r.changedEvents = append(r.changedEvents, ev.(domain.DependencyUpdatesChanged))
		r.changedStates = append(r.changedStates, r.rec.State)
		r.mu.Unlock()
	}).Maybe()
	r.bus.EXPECT().Subscribe(mock.Anything).Return((<-chan domain.Event)(r.events)).Maybe()
	r.store.EXPECT().List(mock.Anything).Return(nil, nil).Maybe() // Start looks for interrupted jobs
	r.store.EXPECT().Get(mock.Anything, r.repo).RunAndReturn(func(context.Context, domain.RepoName) (domain.RepoDeps, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.rec, nil
	}).Maybe()
	r.store.EXPECT().Mutate(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			cp := r.rec
			if _, err := fn(&cp); err != nil {
				return domain.RepoDeps{}, err
			}
			r.rec = cp
			return cp, nil
		}).Maybe()
	r.store.EXPECT().Ignore(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, refs []domain.DepRef) error {
			r.mu.Lock()
			r.ignored = append(r.ignored, refs...)
			r.mu.Unlock()
			return nil
		}).Maybe()
	ecoList := ecos(r)
	b, err := usecase.NewBernard(usecase.BernardConfig{
		Store: r.store, Meta: r.meta, Reader: r.reader, Ecosystems: ecoList,
		Registry: func() ports.DepsRegistry { r.registers.Add(1); return mocks.NewDepsRegistry(t) },
		Bus:      r.bus, Clock: clock, CheckAt: "0 6 * * *", Tick: time.Hour,
		Repos: r.repos, Git: r.git, Signer: r.signer, Committer: domain.Identity{Name: "ermite", Email: "ermite@forge.test"},
		Tips: r.tips, CI: r.ci, MRs: r.mrs, MRStore: r.mrst,
	})
	require.NoError(t, err)
	r.b = b
	return r
}

func actionUpdate() domain.DepUpdate {
	return domain.DepUpdate{
		DepRef:  domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: botWorkflow, Name: "actions/checkout", Version: "v4"},
		Current: "v3", Kind: domain.DepMajor,
	}
}

// happy sets the git side of a successful Open. oldBranch is the current head
// of bernard/deps, the zero id when it does not exist.
func (r *openRig) happy(oldBranch domain.ObjectID) {
	tp, err := domain.ParseTreePath(botWorkflow)
	require.NoError(r.t, err)
	r.happyGit(oldBranch, map[domain.TreePath][]byte{tp: []byte("new")})
	r.reader.EXPECT().OpenBlob(mock.Anything, r.repo, domain.RevisionOf(oid("a")), tp).Return(domain.BlobInfo{Size: 3}, io.NopCloser(strings.NewReader("old")), nil)
	r.eco.EXPECT().Apply(mock.Anything, mock.Anything).Return(map[domain.TreePath][]byte{tp: []byte("new")}, nil)
	r.eco.EXPECT().Validate(mock.Anything, mock.Anything, mock.Anything).Return(nil)
}

// happyGit sets the git side of the publication of changes on the tip "a".
func (r *openRig) happyGit(oldBranch domain.ObjectID, changes map[domain.TreePath][]byte) {
	t := r.t
	tip, tree, head := oid("a"), oid("b"), oid("c")
	r.branchOld, r.branchNew = oldBranch, head
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.repos.EXPECT().DefaultBranch(mock.Anything, r.repo).Return("main", true, nil)
	r.git.EXPECT().BranchHead(mock.Anything, r.repo, "main").Return(tip, nil)
	if oldBranch.IsZero() {
		r.git.EXPECT().BranchHead(mock.Anything, r.repo, "bernard/deps").Return(domain.ObjectID{}, domain.ErrNotFound)
	} else {
		r.git.EXPECT().BranchHead(mock.Anything, r.repo, "bernard/deps").Return(oldBranch, nil)
	}
	r.git.EXPECT().WriteTree(mock.Anything, r.repo, tip, changes).Return(tree, nil)
	r.signer.EXPECT().Sign(mock.Anything).RunAndReturn(func(p []byte) (string, error) {
		assert.Contains(t, string(p), "tree "+tree.String()+"\nparent "+tip.String()+"\nauthor Bernard <bernard@forge.test> ")
		assert.Contains(t, string(p), "committer ermite <ermite@forge.test> ")
		r.op("sign")
		return "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n", nil
	})
	r.git.EXPECT().WriteCommit(mock.Anything, r.repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, obj []byte) (domain.ObjectID, error) {
		r.commit = bytes.Clone(obj)
		r.op("commit")
		return head, nil
	})
	r.tips.EXPECT().PutRefTip(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, tp domain.CIRefTip) error {
		r.tip = tp
		r.op("tip")
		return nil
	})
	r.git.EXPECT().UpdateBranch(mock.Anything, r.repo, "bernard/deps", oldBranch, head).RunAndReturn(func(context.Context, domain.RepoName, string, domain.ObjectID, domain.ObjectID) error {
		r.op("branch")
		return nil
	})
	r.ci.EXPECT().OnPush(mock.Anything, r.repo, mock.Anything, mock.Anything).Run(func(_ context.Context, _ domain.RepoName, ups []domain.RefUpdate, p domain.Principal) {
		r.pushed, r.pusher = ups, p
		r.op("ci")
	})
}

func (r *openRig) expectCreate(number int64) {
	r.mrst.EXPECT().OpenBySource(mock.Anything, r.repo, []string{"bernard/deps"}, mock.Anything).Return(nil, false, nil).Maybe()
	r.mrs.EXPECT().Create(mock.Anything, mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, params ports.CreateMergeRequestParams) (domain.MergeRequest, error) {
			r.creator, r.created = p, params
			r.op("mr")
			return domain.MergeRequest{Repo: r.repo, Number: number}, nil
		})
}

func TestBernardOpenActions(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec.Updates = []domain.DepUpdate{up}
	r.happy(domain.ObjectID{})
	r.expectCreate(9)

	got, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.NoError(t, err)

	assert.Equal(t, []string{"sign", "commit", "tip", "branch", "ci", "mr"}, r.ops, "the tip is recorded before the branch moves")
	assert.EqualValues(t, 9, got.MR)
	assert.Equal(t, []domain.DepRef{up.DepRef}, got.MRVersions)
	assert.Equal(t, domain.DepsIdle, got.State)
	assert.Empty(t, got.Error)
	assert.Contains(t, string(r.commit), "gpgsig -----BEGIN SSH SIGNATURE-----")
	assert.Contains(t, string(r.commit), "build(deps): update 1 dependency\n\n- actions/checkout v3 \u2192 v4 ("+botWorkflow+")\n")

	assert.Equal(t, domain.CIRefTip{Repo: r.repo, Ref: "refs/heads/bernard/deps", Sha: r.branchNew.String(), Actor: "bernard", Pusher: domain.PusherOf(domain.Bot(r.repo)), Trusted: false}, r.tip)
	assert.Equal(t, domain.CIPusherBot, r.tip.Pusher.Kind)
	assert.Equal(t, []domain.RefUpdate{{Old: domain.ZeroOID, New: r.branchNew.String(), Ref: "refs/heads/bernard/deps"}}, r.pushed)
	assert.Equal(t, "bernard", domain.AuthorName(r.pusher))

	assert.Equal(t, "bernard", domain.AuthorName(r.creator))
	assert.Equal(t, "bernard/deps", r.created.Source)
	assert.Equal(t, "main", r.created.Target)
	assert.Equal(t, "build(deps): update 1 dependency", r.created.Title)
	assert.Contains(t, r.created.Description, "- actions/checkout v3 \u2192 v4")
}

func TestBernardOpenUpdateKeepsMR(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec = domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{up}, MR: 4, State: domain.DepsFailed, Error: "boom"}
	r.happy(oid("d")) // an earlier Bernard head, possibly someone else's
	r.mrst.EXPECT().Get(mock.Anything, r.repo, int64(4)).Return(domain.MergeRequest{Repo: r.repo, Number: 4, State: domain.MRStateOpen}, nil)
	var edited ports.EditMergeRequestParams
	var editor domain.Principal
	r.mrs.EXPECT().Edit(mock.Anything, mock.Anything, r.repo, int64(4), mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, _ int64, params ports.EditMergeRequestParams) (domain.MergeRequest, error) {
			editor, edited = p, params
			return domain.MergeRequest{}, nil
		})

	got, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.NoError(t, err)
	require.NotNil(t, edited.Title)
	require.NotNil(t, edited.Description)
	assert.Equal(t, "build(deps): update 1 dependency", *edited.Title)
	assert.Contains(t, *edited.Description, "- actions/checkout v3 \u2192 v4")
	assert.Equal(t, "bernard", domain.AuthorName(editor), "edited as the bot")
	assert.Equal(t, []string{"sign", "commit", "tip", "branch", "ci"}, r.ops, "no new merge request")
	assert.EqualValues(t, 4, got.MR)
	assert.Equal(t, domain.DepsIdle, got.State)
	assert.Empty(t, got.Error)
	assert.Equal(t, oid("d").String(), r.pushed[0].Old)
}

func TestBernardOpenRecreatesClosedMR(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec = domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{up}, MR: 4}
	r.happy(domain.ObjectID{})
	r.mrst.EXPECT().Get(mock.Anything, r.repo, int64(4)).Return(domain.MergeRequest{Repo: r.repo, Number: 4, State: domain.MRStateClosed}, nil)
	r.expectCreate(5)
	got, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.NoError(t, err)
	assert.EqualValues(t, 5, got.MR)
}

func TestBernardOpenPreconditions(t *testing.T) {
	up := actionUpdate()
	t.Run("stale expected", func(t *testing.T) {
		r := newOpenRig(t, domain.EcosystemActions, true)
		r.rec.Updates = []domain.DepUpdate{up}
		r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
		stale := up.DepRef
		stale.Version = "v3.9"
		_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{stale})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Equal(t, domain.DepsIdle, r.rec.State)
	})
	t.Run("applying", func(t *testing.T) {
		r := newOpenRig(t, domain.EcosystemActions, true)
		r.rec.Updates, r.rec.State = []domain.DepUpdate{up}, domain.DepsApplying
		r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
		_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
	t.Run("archived", func(t *testing.T) {
		r := newOpenRig(t, domain.EcosystemActions, true)
		r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo, Archived: true}, nil)
		_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, nil)
		assert.ErrorIs(t, err, domain.ErrArchived)
	})
}

func TestBernardOpenFailureSetsFailed(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec.Updates = []domain.DepUpdate{up}
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.repos.EXPECT().DefaultBranch(mock.Anything, r.repo).Return("main", true, nil)
	r.git.EXPECT().BranchHead(mock.Anything, r.repo, "main").Return(oid("a"), nil)
	r.reader.EXPECT().OpenBlob(mock.Anything, r.repo, mock.Anything, mock.Anything).Return(domain.BlobInfo{}, nil, errors.New(strings.Repeat("x", 5000)))

	_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.Error(t, err)
	assert.Equal(t, domain.DepsFailed, r.rec.State)
	assert.NotEmpty(t, r.rec.Error)
	assert.LessOrEqual(t, len(r.rec.Error), 2048)
	assert.Len(t, r.rec.Updates, 1)
}

func (r *openRig) startWatch() {
	r.b.Start()
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.b.Close(ctx)
	})
}

func (r *openRig) snapshot() domain.RepoDeps {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rec
}

func TestBernardClosedMRIgnoresItsVersions(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	other := domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: domain.EcosystemActions, Directory: botWorkflow, Name: "x/y", Version: "v2"}, Current: "v1"}
	r.rec = domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{up, other}, MR: 4, MRVersions: []domain.DepRef{up.DepRef}}
	r.startWatch()

	// Another merge request is none of Bernard's business.
	r.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: r.repo, Number: 5}}
	r.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: r.repo, Number: 4}}
	require.Eventually(t, func() bool { return r.snapshot().MR == 0 }, 5*time.Second, 5*time.Millisecond)

	got := r.snapshot()
	assert.Empty(t, got.MRVersions)
	assert.Equal(t, []domain.DepUpdate{other}, got.Updates)
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Equal(t, []domain.DepRef{up.DepRef}, r.ignored)
	assert.Zero(t, r.registers.Load(), "no check after a close")
}

func TestBernardMergedMRClearsAndChecks(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec = domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{up}, MR: 4, MRVersions: []domain.DepRef{up.DepRef}}
	// The check finds an archived repository and stops there.
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo, Archived: true}, nil)
	r.startWatch()

	r.events <- domain.MergeRequestMerged{MergeRequest: domain.MergeRequest{Repo: r.repo, Number: 4}}
	require.Eventually(t, func() bool { return r.registers.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	got := r.snapshot()
	assert.Zero(t, got.MR)
	assert.Empty(t, got.MRVersions)
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Empty(t, r.ignored, "a merge ignores nothing")
}

func TestBernardReconcilesOnStartAndOnGap(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec = domain.RepoDeps{Repo: r.repo, Updates: []domain.DepUpdate{up}, MR: 4, MRVersions: []domain.DepRef{up.DepRef}}
	other := name(t, "other")
	r.store.ExpectedCalls = nil
	r.store.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]domain.RepoDeps, error) {
		return []domain.RepoDeps{r.snapshot(), {Repo: other}}, nil
	})
	r.store.EXPECT().Get(mock.Anything, r.repo).RunAndReturn(func(context.Context, domain.RepoName) (domain.RepoDeps, error) { return r.snapshot(), nil }).Maybe()
	r.store.EXPECT().Mutate(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, fn func(*domain.RepoDeps) (bool, error)) (domain.RepoDeps, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			cp := r.rec
			if _, err := fn(&cp); err != nil {
				return domain.RepoDeps{}, err
			}
			r.rec = cp
			return cp, nil
		}).Maybe()
	r.store.EXPECT().Ignore(mock.Anything, r.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, refs []domain.DepRef) error {
			r.mu.Lock()
			r.ignored = append(r.ignored, refs...)
			r.mu.Unlock()
			return nil
		}).Maybe()
	states := make(chan domain.MRState, 4)
	r.mrst.EXPECT().Get(mock.Anything, r.repo, int64(4)).RunAndReturn(func(context.Context, domain.RepoName, int64) (domain.MergeRequest, error) {
		select {
		case s := <-states:
			return domain.MergeRequest{Repo: r.repo, Number: 4, State: s}, nil
		default:
			return domain.MergeRequest{Repo: r.repo, Number: 4, State: domain.MRStateOpen}, nil
		}
	})
	// Closed while Bernard was down: reconciled on Start as a close.
	states <- domain.MRStateClosed
	r.startWatch()
	require.Eventually(t, func() bool { return r.snapshot().MR == 0 }, 5*time.Second, 5*time.Millisecond)
	r.mu.Lock()
	assert.Equal(t, []domain.DepRef{up.DepRef}, r.ignored)
	r.mu.Unlock()

	// Still open after a gap: nothing happens.
	r.mu.Lock()
	r.rec.MR, r.rec.MRVersions = 4, []domain.DepRef{up.DepRef}
	r.mu.Unlock()
	r.events <- domain.EventGap{}
	// Merged unseen: a gap reconciles it as a merge (a check follows).
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo, Archived: true}, nil)
	states <- domain.MRStateMerged
	r.events <- domain.EventGap{}
	require.Eventually(t, func() bool { return r.registers.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Zero(t, r.snapshot().MR)
}

func TestBernardOpenAdoptsAnOpenDepsMR(t *testing.T) {
	up := actionUpdate()
	open := domain.MergeRequest{Repo: name(t, "proj"), Number: 8, State: domain.MRStateOpen, Source: "bernard/deps", Target: "main"}
	t.Run("before creating", func(t *testing.T) {
		r := newOpenRig(t, domain.EcosystemActions, true)
		r.rec.Updates = []domain.DepUpdate{up}
		r.happy(domain.ObjectID{})
		r.mrst.EXPECT().OpenBySource(mock.Anything, r.repo, []string{"bernard/deps"}, mock.Anything).Return([]int64{8}, false, nil)
		r.mrst.EXPECT().Get(mock.Anything, r.repo, int64(8)).Return(open, nil)
		r.mrs.EXPECT().Edit(mock.Anything, mock.Anything, r.repo, int64(8), mock.Anything).Return(open, nil) // the adopted request is refreshed
		got, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
		require.NoError(t, err)
		assert.EqualValues(t, 8, got.MR) // Create is not expected: the mock fails on a call
	})
	t.Run("after already exists", func(t *testing.T) {
		r := newOpenRig(t, domain.EcosystemActions, true)
		r.rec.Updates = []domain.DepUpdate{up}
		r.happy(domain.ObjectID{})
		calls := 0
		r.mrst.EXPECT().OpenBySource(mock.Anything, r.repo, []string{"bernard/deps"}, mock.Anything).RunAndReturn(
			func(context.Context, domain.RepoName, []string, int) ([]int64, bool, error) {
				calls++
				if calls == 1 {
					return nil, false, nil
				}
				return []int64{8}, false, nil
			})
		r.mrst.EXPECT().Get(mock.Anything, r.repo, int64(8)).Return(open, nil)
		r.mrs.EXPECT().Create(mock.Anything, mock.Anything, r.repo, mock.Anything).Return(domain.MergeRequest{}, domain.ErrAlreadyExists)
		got, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
		require.NoError(t, err)
		assert.EqualValues(t, 8, got.MR)
	})
}

func TestBernardOpenPublishesTheChangeAfterTheFinalMutation(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec.Updates = []domain.DepUpdate{up}
	r.happy(domain.ObjectID{})
	r.expectCreate(9)
	_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.NoError(t, err)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.changedEvents, 1)
	assert.Equal(t, r.repo, r.changedEvents[0].Repo)
	assert.Equal(t, []domain.DepsState{domain.DepsIdle}, r.changedStates, "published once the record is Idle")
}

func TestBernardOpenFailurePublishesTheChange(t *testing.T) {
	r := newOpenRig(t, domain.EcosystemActions, true)
	up := actionUpdate()
	r.rec.Updates = []domain.DepUpdate{up}
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil)
	r.repos.EXPECT().DefaultBranch(mock.Anything, r.repo).Return("", false, nil)
	_, err := r.b.Open(context.Background(), domain.Owner(), r.repo, []domain.DepRef{up.DepRef})
	require.Error(t, err)
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Equal(t, []domain.DepsState{domain.DepsFailed}, r.changedStates)
}
