// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/deps"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	textOld = "v0.20.0"
	textNew = "v0.21.0"
)

func goModText(goLine, extra, textVersion string) string {
	return "module example.com/m\n\n" + goLine + "\n" + extra + "\nrequire (\n\tgolang.org/x/text " + textVersion + "\n\tgolang.org/x/net v0.30.0\n)\n"
}

func sumLine(mod, version string) string {
	return mod + " " + version + " h1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n"
}

// runnerRig is Open and the report of a Go job, over the real Go ecosystem and
// a mocked actions ecosystem applied in the server.
type runnerRig struct {
	*openRig
	files   map[string]string // the files at the tip
	started ports.DepsRunParams
	starts  int

	// A small forge: branches, commits and merge requests.
	branches map[string]domain.ObjectID
	trees    []map[domain.TreePath][]byte // the changes of each written tree
	commits  []string                     // the commit objects, in order
	mrs      map[int64]domain.MergeRequest
	nextMR   int64
	readied  []int64
	edits    []ports.EditMergeRequestParams
	onPush   func(domain.RefUpdate) // called as CI hears of each Bernard push
}

// commitIDs are the ids WriteCommit hands out, in order.
var commitIDs = []string{"d", "e", "f", "1", "2", "3"}

// fakeForge backs git and the merge requests of rr with a stateful fake.
func (rr *runnerRig) fakeForge() {
	t := rr.t
	rr.branches, rr.mrs, rr.nextMR = map[string]domain.ObjectID{}, map[int64]domain.MergeRequest{}, 7
	rr.git.EXPECT().BranchHead(mock.Anything, rr.repo, mock.MatchedBy(func(b string) bool { return b != "main" })).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, b string) (domain.ObjectID, error) {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			if h, ok := rr.branches[b]; ok {
				return h, nil
			}
			return domain.ObjectID{}, domain.ErrNotFound
		}).Maybe()
	rr.git.EXPECT().WriteTree(mock.Anything, rr.repo, oid("a"), mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, _ domain.ObjectID, ch map[domain.TreePath][]byte) (domain.ObjectID, error) {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			rr.trees = append(rr.trees, ch)
			return oid("b"), nil
		}).Maybe()
	rr.signer.EXPECT().Sign(mock.Anything).Return("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n", nil).Maybe()
	rr.git.EXPECT().WriteCommit(mock.Anything, rr.repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, obj []byte) (domain.ObjectID, error) {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		rr.commits = append(rr.commits, string(obj))
		return oid(commitIDs[len(rr.commits)-1]), nil
	}).Maybe()
	rr.tips.EXPECT().PutRefTip(mock.Anything, mock.Anything).Return(nil).Maybe()
	rr.git.EXPECT().CommitTree(mock.Anything, rr.repo, mock.Anything).Return(oid("9"), nil).Maybe()
	rr.git.EXPECT().UpdateBranch(mock.Anything, rr.repo, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, b string, old, next domain.ObjectID) error {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			if rr.branches[b] != old {
				return domain.ErrConflict
			}
			rr.branches[b] = next
			return nil
		}).Maybe()
	rr.ci.EXPECT().OnPush(mock.Anything, rr.repo, mock.Anything, mock.Anything).Run(func(_ context.Context, _ domain.RepoName, ups []domain.RefUpdate, _ domain.Principal) {
		if rr.onPush != nil {
			for _, u := range ups {
				rr.onPush(u)
			}
		}
	}).Maybe()
	rr.mrst.EXPECT().OpenBySource(mock.Anything, rr.repo, mock.Anything, mock.Anything).Return(nil, false, nil).Maybe()
	rr.mrst.EXPECT().Get(mock.Anything, rr.repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, n int64) (domain.MergeRequest, error) {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		mr, ok := rr.mrs[n]
		if !ok {
			return domain.MergeRequest{}, domain.ErrNotFound
		}
		return mr, nil
	}).Maybe()
	rr.openRig.mrs.EXPECT().Create(mock.Anything, mock.Anything, rr.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, params ports.CreateMergeRequestParams) (domain.MergeRequest, error) {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			rr.creator, rr.created = p, params
			title, err := domain.ParseMRTitle(params.Title)
			require.NoError(t, err)
			desc, err := domain.ParsePlainText(params.Description)
			require.NoError(t, err)
			mr := domain.MergeRequest{Repo: rr.repo, Number: rr.nextMR, State: domain.MRStateOpen, Title: title, Description: desc, Source: params.Source, Target: params.Target}
			rr.mrs[mr.Number] = mr
			rr.nextMR++
			return mr, nil
		}).Maybe()
	rr.openRig.mrs.EXPECT().Edit(mock.Anything, mock.Anything, rr.repo, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, n int64, params ports.EditMergeRequestParams) (domain.MergeRequest, error) {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			mr := rr.mrs[n]
			if params.Title != nil {
				mr.Title, _ = domain.ParseMRTitle(*params.Title)
			}
			if params.Description != nil {
				mr.Description, _ = domain.ParsePlainText(*params.Description)
			}
			rr.mrs[n], rr.edits = mr, append(rr.edits, params)
			return mr, nil
		}).Maybe()
	rr.openRig.mrs.EXPECT().MarkReady(mock.Anything, mock.Anything, rr.repo, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _ domain.RepoName, n int64) (domain.MergeRequest, error) {
			rr.mu.Lock()
			defer rr.mu.Unlock()
			mr := rr.mrs[n]
			title, ok := domain.ReadyTitle(mr.Title)
			if !ok {
				return domain.MergeRequest{}, domain.ErrPreconditionFailed
			}
			mr.Title = title
			rr.mrs[n], rr.readied = mr, append(rr.readied, n)
			return mr, nil
		}).Maybe()
}

// mr returns the merge request number n of the fake forge.
func (rr *runnerRig) mr(n int64) domain.MergeRequest {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.mrs[n]
}

const runnerRunID = 77

func newRunnerRig(t *testing.T, goMod string) *runnerRig {
	t.Helper()
	rr := &runnerRig{files: map[string]string{
		"go.mod":    goMod,
		"go.sum":    sumLine("golang.org/x/text", "v0.20.0"),
		botWorkflow: "old",
	}}
	rr.openRig = newOpenRigWith(t, func(r *openRig) []ports.DepsEcosystem {
		r.eco.EXPECT().Name().Return(domain.EcosystemActions).Maybe()
		r.eco.EXPECT().ApplyInServer().Return(true).Maybe()
		wf, err := domain.ParseTreePath(botWorkflow)
		require.NoError(t, err)
		r.eco.EXPECT().Apply(mock.Anything, mock.Anything).Return(map[domain.TreePath][]byte{wf: []byte("new")}, nil).Maybe()
		r.eco.EXPECT().Validate(mock.Anything, mock.Anything, mock.Anything).Return(nil, nil).Maybe()
		return []ports.DepsEcosystem{deps.GoMod{}, r.eco}
	})
	rr.fakeForge()
	rr.reader.EXPECT().ListPaths(mock.Anything, rr.repo, mock.Anything, domain.MaxPathEntries, domain.MaxPathBytes).RunAndReturn(func(context.Context, domain.RepoName, domain.Revision, int, int) (domain.PathList, error) {
		var paths []string
		for p := range rr.files {
			paths = append(paths, p)
		}
		return domain.PathList{Paths: paths}, nil
	}).Maybe()
	rr.reader.EXPECT().OpenBlob(mock.Anything, rr.repo, domain.RevisionOf(oid("a")), mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.RepoName, _ domain.Revision, p domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
			c, ok := rr.files[p.String()]
			if !ok {
				return domain.BlobInfo{}, nil, domain.ErrNotFound
			}
			return domain.BlobInfo{Size: int64(len(c))}, io.NopCloser(strings.NewReader(c)), nil
		}).Maybe()
	rr.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, p ports.DepsRunParams) (domain.CIRun, error) {
			rr.started, rr.starts = p, rr.starts+1
			return domain.CIRun{Repo: p.Repo, ID: runnerRunID, Sha: p.Sha, Ref: p.Ref, Event: domain.CIEventDependencies}, nil
		}).Maybe()
	rr.meta.EXPECT().Get(mock.Anything, rr.repo).Return(domain.Repo{Name: rr.repo}, nil).Maybe()
	rr.repos.EXPECT().DefaultBranch(mock.Anything, rr.repo).Return("main", true, nil).Maybe()
	rr.git.EXPECT().BranchHead(mock.Anything, rr.repo, "main").Return(oid("a"), nil).Maybe()
	return rr
}

func goUpdate() domain.DepUpdate {
	return domain.DepUpdate{
		DepRef:  domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "golang.org/x/text", Version: "v0.21.0"},
		Current: "v0.20.0", Kind: domain.DepMinor,
	}
}

func (rr *runnerRig) open(ups ...domain.DepUpdate) domain.RepoDeps {
	rr.t.Helper()
	rr.rec.Base().Updates = ups
	refs := make([]domain.DepRef, len(ups))
	for i, u := range ups {
		refs[i] = u.DepRef
	}
	got, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, domain.DepBatchKey(""), refs)
	require.NoError(rr.t, err)
	return got
}

// end tells Bernard the job of the run Open started ended with the report and
// the conclusion, as CI does.
func (rr *runnerRig) end(report *ports.DepsReport, conclusion domain.CIConclusion) {
	rr.t.Helper()
	require.Equal(rr.t, 1, rr.starts, "Open started a run")
	run, job := rr.endedRun(conclusion)
	rr.b.DepsJobEnded(context.Background(), run, job, report)
}

func (rr *runnerRig) endedRun(conclusion domain.CIConclusion) (domain.CIRun, domain.CIJob) {
	job := rr.started.Job
	run := domain.CIRun{
		Repo: rr.repo, ID: runnerRunID, Sha: rr.started.Sha, Ref: rr.started.Ref, Event: domain.CIEventDependencies,
		Status: domain.CICompleted, Conclusion: conclusion,
	}
	return run, domain.CIJob{
		ID: domain.NewJobID(runnerRunID, 0, 0), RunID: runnerRunID, Repo: rr.repo, Deps: &job,
		Phase: domain.CIPhaseEnded, Conclusion: conclusion,
	}
}

func goReport(goMod, goSum string) *ports.DepsReport {
	return &ports.DepsReport{Files: []ports.DepsFile{
		{Path: "go.mod", Content: []byte(goMod)}, {Path: "go.sum", Content: []byte(goSum)},
	}}
}

func TestBernardOpenGoStartsARunAtTheTip(t *testing.T) {
	for _, c := range []struct{ name, goMod, want string }{
		{"go only", goModText("go 1.24", "", textOld), "1.24"},
		{"toolchain higher", goModText("go 1.24", "toolchain go1.25.1\n", textOld), "1.25.1"},
		{"go higher", goModText("go 1.26.0", "toolchain go1.24.3\n", textOld), "1.26.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, c.goMod)
			got := rr.open(goUpdate(), actionUpdate())
			assert.Equal(t, domain.DepsApplying, got.Base().State)
			assert.EqualValues(t, runnerRunID, got.Base().RunID)
			assert.Equal(t, []domain.DepUpdate{goUpdate(), actionUpdate()}, got.Base().RunUpdates, "every pending update is kept for the commit")

			assert.Equal(t, ports.DepsRunParams{
				Repo: rr.repo, Ref: "refs/heads/main", Sha: oid("a").String(), Title: "build(deps): update 2 dependencies",
				Job:   domain.CIDepsJob{GoVersion: c.want, Details: []domain.CIDepsUpdate{{Ref: goUpdate().DepRef}}, Requires: []string{"deps"}}, // only the runner-side updates
				Label: "ubuntu-latest", Timeout: 10 * time.Minute,
			}, rr.started)
		})
	}
}

func TestBernardOpenWhileApplyingStartsNoSecondRun(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, domain.DepBatchKey(""), []domain.DepRef{goUpdate().DepRef})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	assert.Equal(t, 1, rr.starts)
	assert.Equal(t, domain.DepsApplying, rr.snapshot().Batches[0].State, "the first run is still the record's")
	assert.EqualValues(t, runnerRunID, rr.snapshot().Batches[0].RunID)
}

func TestBernardOpenStartFailureFailsTheRecord(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.runs.ExpectedCalls = nil
	rr.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).Return(domain.CIRun{}, errors.New("store down\nsecond line"))
	rr.rec.Base().Updates = []domain.DepUpdate{goUpdate()}
	_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, domain.DepBatchKey(""), []domain.DepRef{goUpdate().DepRef})
	require.Error(t, err)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsFailed, got.Base().State)
	assert.Equal(t, "start the dependency run: store down\nsecond line", got.Base().Error)
	assert.Zero(t, got.Base().RunID)
}

// A record that moved while the run was being created does not keep it: the
// run is cancelled and the record stays as the other transition left it.
func TestBernardOpenCancelsTheRunWhenTheRecordMoved(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.runs.ExpectedCalls = nil
	rr.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, ports.DepsRunParams) (domain.CIRun, error) {
			rr.mu.Lock()
			rr.rec.Base().Updates = nil // the daily check ran meanwhile
			rr.mu.Unlock()
			return domain.CIRun{Repo: rr.repo, ID: 5}, nil
		})
	rr.runs.EXPECT().CancelDepsRun(mock.Anything, rr.repo, int64(5)).Return(nil).Once()
	rr.rec.Base().Updates = []domain.DepUpdate{goUpdate()}
	_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, domain.DepBatchKey(""), []domain.DepRef{goUpdate().DepRef})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsDrafted, got.Base().State, "the draft is open, waiting for a run")
	assert.Zero(t, got.Base().RunID)
}

func TestBernardOpenGoRefusesABadGoVersion(t *testing.T) {
	for _, line := range []string{"go 1.25rc1", "go 2.0"} {
		rr := newRunnerRig(t, goModText(line, "", textOld))
		rr.rec.Base().Updates = []domain.DepUpdate{goUpdate()}
		_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, domain.DepBatchKey(""), []domain.DepRef{goUpdate().DepRef})
		require.Error(t, err, line)
		assert.Equal(t, domain.DepsFailed, rr.rec.Base().State)
		assert.Contains(t, rr.rec.Base().Error, "Go version")
		assert.Zero(t, rr.starts, "no run was started")
	}
}

func TestBernardRunnerReportIsRejected(t *testing.T) {
	good := goModText("go 1.24", "", textNew)
	goodSum := sumLine("golang.org/x/text", "v0.21.0")
	mk := func(files ...ports.DepsFile) *ports.DepsReport { return &ports.DepsReport{Files: files} }
	file := func(p, c string) ports.DepsFile { return ports.DepsFile{Path: p, Content: []byte(c)} }
	many := make([]ports.DepsFile, 65)
	for i := range many {
		many[i] = file("go.sum", goodSum)
	}
	for _, c := range []struct {
		name   string
		report *ports.DepsReport
		want   string
	}{
		{"extra path", mk(file("go.mod", good), file("evil.sh", "x")), "unexpected report path"},
		{"other directory", mk(file("sub/go.mod", good)), "unexpected report path"},
		{"duplicate path", mk(file("go.mod", good), file("go.mod", good)), "duplicate report path"},
		{"too many files", &ports.DepsReport{Files: many}, "at most 64"},
		{"oversize file", mk(file("go.sum", strings.Repeat("a", 2<<20+1))), "larger than"},
		{"total size", mk(ports.DepsFile{Path: "go.mod", Content: make([]byte, 2<<20)}, ports.DepsFile{Path: "go.sum", Content: make([]byte, 2<<20)}), "malformed"},
		{"new replace", mk(file("go.mod", goModText("go 1.24", "replace golang.org/x/net => ../net\n", textNew)), file("go.sum", goodSum)), "module directives changed"},
		{"changed go directive", mk(file("go.mod", goModText("go 1.25", "", textNew))), "module directives changed"},
		{"go 1.24 to 1.24.1 refused", mk(file("go.mod", goModText("go 1.24.1", "", textNew))), "module directives changed"},
		{"go 1.24 to 1.25.0 refused", mk(file("go.mod", goModText("go 1.25.0", "", textNew))), "module directives changed"},
		{"unrequested direct downgrade", mk(file("go.mod", strings.Replace(good, "x/net v0.30.0", "x/net v0.29.0", 1))), "unrequested direct requirement"},
		{"requested version missing", mk(file("go.mod", goModText("go 1.24", "", "v0.20.1"))), "requested version missing"},
		{"malformed go.sum", mk(file("go.mod", good), file("go.sum", "nonsense\n")), "malformed go.sum"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
			rr.open(goUpdate())
			rr.end(c.report, domain.CISuccess)
			assert.Equal(t, domain.DepsFailed, rr.rec.Base().State)
			assert.Contains(t, rr.rec.Base().Error, c.want)
			assert.NotContains(t, rr.rec.Base().Error, "\n", "one line")
			assert.EqualValues(t, runnerRunID, rr.rec.Base().RunID, "the record keeps the link to the run")
			assert.EqualValues(t, 7, rr.rec.Base().MR, "the draft stays")
			assert.True(t, rr.mr(7).IsDraft())
		})
	}
}

func TestBernardRunnerReportOversizeTotal(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	// Four 2 MiB files cannot all be allowed paths: use the limit through two
	// directories' worth of files.
	rr.end(&ports.DepsReport{Files: []ports.DepsFile{
		{Path: "go.mod", Content: make([]byte, 2<<20)}, {Path: "go.sum", Content: make([]byte, 2<<20)},
	}}, domain.CISuccess)
	assert.Equal(t, domain.DepsFailed, rr.rec.Base().State)
}

func TestBernardRunnerReportSuccessPublishesFromTheJobCommit(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.20.0")+sumLine("golang.org/x/text", "v0.21.0")
	wf := mustPath(t, botWorkflow)
	mod, sum := mustPath(t, "go.mod"), mustPath(t, "go.sum")

	got := rr.open(goUpdate(), actionUpdate())
	require.Equal(t, domain.DepsApplying, got.Base().State)
	rr.end(goReport(goMod, goSum), domain.CISuccess)

	assert.Equal(t, domain.DepsIdle, rr.rec.Base().State)
	assert.Empty(t, rr.rec.Base().Error)
	assert.EqualValues(t, runnerRunID, rr.rec.Base().RunID, "the record keeps the link to the last run")
	assert.Empty(t, rr.rec.Base().RunUpdates)
	assert.EqualValues(t, 7, rr.rec.Base().MR)
	assert.ElementsMatch(t, []domain.DepRef{goUpdate().DepRef, actionUpdate().DepRef}, rr.rec.Base().MRVersions)
	require.Len(t, rr.trees, 2)
	assert.Equal(t, map[domain.TreePath][]byte{mod: []byte(goMod), sum: []byte(goSum), wf: []byte("new")}, rr.trees[1], "the final tree is the run's commit with the report and the server changes")
	assert.Equal(t, oid("e"), rr.branches["bernard/deps"])
	assert.Equal(t, oid("e"), rr.rec.Base().Head)
	assert.Contains(t, rr.commits[1], "\nparent "+oid("d").String()+"\n", "the final commit follows the draft")
	mr := rr.mr(7)
	assert.Equal(t, "build(deps): update 2 dependencies", mr.Title.String())
	assert.Contains(t, mr.Description.String(), "golang.org/x/text v0.20.0 \u2192 v0.21.0")
	assert.Contains(t, mr.Description.String(), "actions/checkout")
}

func TestBernardRunnerReportGoOnlyWithNoActions(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	goMod := goModText("go 1.24", "", textNew)
	mod := mustPath(t, "go.mod")
	rr.open(goUpdate())
	// An unchanged go.sum is not part of the commit.
	rr.end(goReport(goMod, rr.files["go.sum"]), domain.CISuccess)
	assert.Equal(t, domain.DepsIdle, rr.rec.Base().State)
	assert.EqualValues(t, 7, rr.rec.Base().MR)
	assert.Equal(t, map[domain.TreePath][]byte{mod: []byte(goMod)}, rr.trees[1])
}

func TestBernardRunnerFailureKeepsOutput(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	rr.end(&ports.DepsReport{Error: "go get failed\ngo: example.com/x@v9: not found"}, domain.CIFailure)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsFailed, got.Base().State)
	assert.Equal(t, "go get failed\ngo: example.com/x@v9: not found", got.Base().Error)
	assert.EqualValues(t, runnerRunID, got.Base().RunID)
	assert.Empty(t, got.Base().RunUpdates)
}

func TestBernardClosingTheMRCancelsTheRun(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	other := actionUpdate()
	got := rr.open(goUpdate(), other)
	require.Equal(t, domain.DepsApplying, got.Base().State)
	require.EqualValues(t, 7, got.Base().MR, "the draft is open")
	rr.mu.Lock()
	rr.rec.Base().MRVersions = []domain.DepRef{goUpdate().DepRef} // the draft holds only the Go update
	rr.mu.Unlock()
	rr.runs.EXPECT().CancelDepsRun(mock.Anything, rr.repo, int64(runnerRunID)).Return(nil).Once()

	rr.startWatch()
	rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 7}}
	require.Eventually(t, func() bool { return rr.snapshot().Batches[0].State == domain.DepsIdle }, 5*time.Second, 5*time.Millisecond)

	snap := rr.snapshot()
	assert.EqualValues(t, runnerRunID, snap.Base().RunID, "the link to the cancelled run stays")
	assert.Empty(t, snap.Base().RunUpdates)
	assert.Equal(t, []domain.DepUpdate{other}, snap.Base().Updates, "the closed versions are gone, the rest remains")
	assert.Zero(t, snap.Base().MR)

	// The cancelled job ends: the record left the run, nothing is published
	// (no git mock is called) and nothing is written.
	rr.end(nil, domain.CICancelled)
	assert.Equal(t, snap, rr.snapshot())
}

func TestBernardClosingTheMRWithoutARunCancelsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.rec = domain.RepoDeps{Repo: rr.repo, Batches: []domain.DepBatch{{MR: 4, RunID: 9, MRVersions: []domain.DepRef{goUpdate().DepRef}, Updates: []domain.DepUpdate{goUpdate()}}}}
		rr.startWatch()
		rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 4}}
		synctest.Wait()
		assert.Zero(t, rr.snapshot().Batches[0].MR)
		assert.EqualValues(t, 9, rr.snapshot().Batches[0].RunID) // CancelDepsRun is not expected: the mock would fail
	})
}

func TestBernardReportDroppedWhenRecordLeftTheRun(t *testing.T) {
	for _, c := range []struct {
		name string
		move func(*domain.RepoDeps)
	}{
		{"idle", func(r *domain.RepoDeps) { r.Base().State = domain.DepsIdle }},
		{"another run", func(r *domain.RepoDeps) { r.Base().RunID = runnerRunID + 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
			rr.open(goUpdate())
			rr.mu.Lock()
			c.move(&rr.rec)
			moved := rr.rec
			rr.mu.Unlock()
			goMod := goModText("go 1.24", "", textNew)
			rr.end(goReport(goMod, rr.files["go.sum"]), domain.CISuccess)
			assert.Equal(t, moved, rr.snapshot(), "no write, nothing published")
			rr.end(&ports.DepsReport{Error: "late"}, domain.CIFailure)
			assert.Equal(t, moved, rr.snapshot())
		})
	}
}

// The daily check rewrites Updates while the run works; the run still commits
// what it started with.
func TestBernardCommitsTheUpdatesTheRunStartedWith(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.20.0")+sumLine("golang.org/x/text", "v0.21.0")
	rr.open(goUpdate())
	rr.mu.Lock()
	rr.rec.Base().Updates = []domain.DepUpdate{{DepRef: depRef("other/mod", "v1.0.0"), Current: "v0.9.0", Kind: domain.DepMajor, New: true}}
	rr.mu.Unlock()

	rr.end(goReport(goMod, goSum), domain.CISuccess)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsIdle, got.Base().State)
	assert.Equal(t, []domain.DepRef{goUpdate().DepRef}, got.Base().MRVersions)
	mr := rr.mr(7)
	assert.Contains(t, mr.Description.String(), "golang.org/x/text")
	assert.NotContains(t, mr.Description.String(), "other/mod")
	assert.Equal(t, "build(deps): update 1 dependency", mr.Title.String())
}

func TestBernardGoVersionComesFromTheUpdatedModules(t *testing.T) {
	sub := func(dir string) domain.DepUpdate {
		u := goUpdate()
		u.Directory = dir
		return u
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		ups   []domain.DepUpdate
		want  string
	}{
		{"no root go.mod", map[string]string{"sub/go.mod": goModText("go 1.23", "", textOld)}, []domain.DepUpdate{sub("sub")}, "1.23"},
		{"nested higher than root", map[string]string{
			"go.mod": goModText("go 1.22", "", textOld), "sub/go.mod": goModText("go 1.25", "toolchain go1.25.2\n", textOld),
		}, []domain.DepUpdate{sub("sub")}, "1.25.2"},
		{"highest of the modules", map[string]string{
			"a/go.mod": goModText("go 1.23", "", textOld), "b/go.mod": goModText("go 1.24.1", "", textOld),
		}, []domain.DepUpdate{sub("a"), sub("b")}, "1.24.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, "")
			rr.files = c.files
			rr.open(c.ups...)
			assert.Equal(t, c.want, rr.started.Job.GoVersion)
		})
	}
}

func TestBernardRunnerReportListsGoSideEffects(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	mod := strings.Replace(goModText("go 1.24", "", textNew), "x/net v0.30.0", "x/net v0.31.0", 1)
	sum := sumLine("golang.org/x/text", textNew)
	rr.open(goUpdate())
	rr.end(goReport(mod, sum), domain.CISuccess)
	assert.Equal(t, domain.DepsIdle, rr.rec.Base().State)
	assert.Contains(t, rr.mr(7).Description.String(), "## Also changed by Go")
	assert.Contains(t, rr.mr(7).Description.String(), "- `golang.org/x/net` v0.30.0 → v0.31.0")
}

func TestBernardCheckDepsReportDoesNotPublishAndDropsSupersededRuns(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	run, job := rr.endedRun(domain.CISuccess)
	report := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	require.NoError(t, rr.b.CheckDepsReport(context.Background(), run, job, *report))
	assert.Equal(t, domain.DepsApplying, rr.rec.Base().State)
	assert.True(t, rr.mr(rr.rec.Base().MR).IsDraft(), "the merge request is still the draft")
	assert.Len(t, rr.commits, 1, "nothing published")
	rr.rec.Base().RunID++
	require.NoError(t, rr.b.CheckDepsReport(context.Background(), run, job, ports.DepsReport{Files: []ports.DepsFile{{Path: "evil.sh"}}}))
}

// The check reads the record without the repository lock that publication
// holds during its git writes. A regression deadlocks here, which go test
// -timeout reports.
func TestBernardCheckDepsReportDoesNotWaitForTheRepositoryLock(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	run, job := rr.endedRun(domain.CISuccess)
	report := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	rr.b.UnderRepoLock(rr.repo, func() {
		require.NoError(t, rr.b.CheckDepsReport(context.Background(), run, job, *report))
	})
}

func TestBernardCheckDepsReportClassifiesReadFailures(t *testing.T) {
	good := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	t.Run("go.mod absent at the commit is a permanent refusal", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		delete(rr.files, "go.mod")
		run, job := rr.endedRun(domain.CISuccess)
		err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
	t.Run("a git failure stays transient", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		boom := errors.New("git is down")
		rr.reader.ExpectedCalls = nil
		rr.reader.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.BlobInfo{}, nil, boom)
		run, job := rr.endedRun(domain.CISuccess)
		err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrInvalid)
		assert.NotErrorIs(t, err, domain.ErrPreconditionFailed)
	})
	t.Run("an oversize go.mod is a permanent refusal", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		rr.files["go.mod"] = strings.Repeat("x", 3<<20)
		run, job := rr.endedRun(domain.CISuccess)
		err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
}

func TestBernardOpenMajorBatchByKey(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	up := goUpdate()
	key := domain.FamilyBatchKey(up, nil)
	base := domain.DepBatch{State: domain.DepsApplying, RunID: 900, MR: 17}
	rr.rec.Batches = []domain.DepBatch{base, {Key: key, Updates: []domain.DepUpdate{up}}}
	rr.b.SetConcurrencyForTest(2) // the base batch holds one slot
	_, err := rr.b.OpenNow(t.Context(), domain.Owner(), rr.repo, "missing", []domain.DepRef{up.DepRef})
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.Zero(t, rr.starts)
	_, err = rr.b.OpenNow(t.Context(), domain.Owner(), rr.repo, key, nil)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	got, err := rr.b.OpenNow(t.Context(), domain.Owner(), rr.repo, key, []domain.DepRef{up.DepRef})
	require.NoError(t, err)
	require.Equal(t, base, got.Batches[0])
	require.Equal(t, domain.DepsApplying, got.Batches[1].State)
	require.Equal(t, int64(runnerRunID), got.Batches[1].RunID)
	require.Equal(t, []domain.DepUpdate{up}, got.Batches[1].RunUpdates)
	require.Equal(t, domain.BotSubject(got.Batches[1]), rr.started.Title)
}

func TestBernardPathMajorReport(t *testing.T) {
	for _, c := range []struct {
		name, file, content string
		valid               bool
	}{
		{"valid", "main.go", "package main\nimport _ \"example.com/lib/v2/sub\"\nvar X = 1\n", true},
		{"non import", "main.go", "package main\nimport _ \"example.com/lib/v2/sub\"\nvar X = 2\n", false},
		{"disallowed", "README.md", "oops", false},
		{"nested module", "nested/main.go", "package main\nimport _ \"example.com/lib/v2/sub\"\nvar X = 1\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, "module example.com/app\ngo 1.24\nrequire example.com/lib v1.0.0\n")
			rr.files["main.go"] = "package main\nimport _ \"example.com/lib/sub\"\nvar X = 1\n"
			rr.files["nested/go.mod"] = "module example.com/nested\n"
			rr.files["nested/main.go"] = rr.files["main.go"]
			u := domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/lib", Version: "v2.0.0"}, Current: "v1.0.0", NewName: "example.com/lib/v2"}
			rr.open(u)
			run, job := rr.endedRun(domain.CISuccess)
			report := ports.DepsReport{Files: []ports.DepsFile{{Path: "go.mod", Content: []byte("module example.com/app\ngo 1.24\nrequire example.com/lib/v2 v2.0.0\n")}, {Path: c.file, Content: []byte(c.content)}}}
			err := rr.b.CheckDepsReport(context.Background(), run, job, report)
			if c.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, domain.ErrInvalid)
			}
		})
	}
}

func TestBernardOpenRequiresCapableRunner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kinds, labels []string
		newName, want string
	}{
		{"no runners", nil, nil, "", "deps"},
		{"wrong label", []string{"deps"}, []string{"other"}, "", "deps"},
		{"old runner", []string{"deps"}, []string{"ubuntu-latest"}, "golang.org/x/text/v2", "deps.gomod-path-majors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
			rr.runners = nil
			if tc.labels != nil {
				rr.runners = []domain.CIRunner{{Labels: tc.labels, Kinds: tc.kinds}}
			}
			u := goUpdate()
			u.NewName = tc.newName
			rr.rec.Base().Updates = []domain.DepUpdate{u}
			_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, "", []domain.DepRef{u.DepRef})
			require.ErrorContains(t, err, "no runner supports "+tc.want+" updates")
			require.Equal(t, domain.DepsFailed, rr.rec.Base().State)
			require.Contains(t, rr.rec.Base().Error, "no runner supports "+tc.want+" updates")
			require.Zero(t, rr.starts)
		})
	}
}

// The report check reads only the files of the updates: no listing (which may
// be truncated in a large repository) and no go.sum needed at the commit.
func TestBernardCheckDepsReportReadsOnlyTheUpdateInputs(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	delete(rr.files, "go.sum") // a module without dependencies has none
	rr.reader.ExpectedCalls = slices.DeleteFunc(rr.reader.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "ListPaths" })
	run, job := rr.endedRun(domain.CISuccess)
	report := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	require.NoError(t, rr.b.CheckDepsReport(context.Background(), run, job, *report))
	rr.reader.AssertNotCalled(t, "ListPaths", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// With two required kinds, the error names the first one no labelled runner
// advertises, not the last one required.
func TestBernardOpenNamesTheMissingRunnerKind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []string
		want  string
	}{
		{"first kind missing", []string{"deps.gomod-path-majors"}, "deps"},
		{"second kind missing", []string{"deps"}, "deps.gomod-path-majors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
			rr.runners = []domain.CIRunner{
				{Labels: []string{"ubuntu-latest"}, Kinds: tc.kinds},
				{Labels: []string{"other"}, Kinds: []string{"deps", "deps.gomod-path-majors"}},
			}
			u := goUpdate()
			u.NewName = "golang.org/x/text/v2"
			rr.rec.Base().Updates = []domain.DepUpdate{u}
			_, err := rr.b.OpenNow(context.Background(), domain.Owner(), rr.repo, "", []domain.DepRef{u.DepRef})
			require.EqualError(t, err, "failed precondition: no runner supports "+tc.want+" updates")
		})
	}
}

// A changed Go file larger than the report cap is never compared by its
// header: a runner could otherwise replace it with its first kilobytes.
func TestBernardPathMajorReportRefusesTruncatedLargeGoFile(t *testing.T) {
	rr := newRunnerRig(t, "module example.com/app\ngo 1.24\nrequire example.com/lib v1.0.0\n")
	head := "package main\nimport _ \"example.com/lib/sub\"\n"
	rr.files["main.go"] = head + "var X = `" + strings.Repeat("x", 3<<20) + "`\n"
	u := domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/lib", Version: "v2.0.0"}, Current: "v1.0.0", NewName: "example.com/lib/v2"}
	rr.open(u)
	run, job := rr.endedRun(domain.CISuccess)
	truncated := strings.Replace(rr.files["main.go"][:4096], "example.com/lib/sub", "example.com/lib/v2/sub", 1)
	report := ports.DepsReport{Files: []ports.DepsFile{{Path: "go.mod", Content: []byte("module example.com/app\ngo 1.24\nrequire example.com/lib/v2 v2.0.0\n")}, {Path: "main.go", Content: []byte(truncated)}}}
	err := rr.b.CheckDepsReport(context.Background(), run, job, report)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	require.ErrorContains(t, err, "main.go")
	require.ErrorContains(t, err, "larger than")
}

// An unchanged go.sum is not read at all, whatever its size.
func TestBernardCheckDepsReportIgnoresUnchangedGoSum(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	rr.files["go.sum"] = strings.Repeat("x", 3<<19) // 1.5 MiB, over the manifest limit
	run, job := rr.endedRun(domain.CISuccess)
	report := ports.DepsReport{Files: []ports.DepsFile{{Path: "go.mod", Content: []byte(goModText("go 1.24", "", textNew))}}}
	require.NoError(t, rr.b.CheckDepsReport(context.Background(), run, job, report))
}

// Open pushes the manifest bump (and the server-side updates) on the batch
// branch at the tip and opens a draft merge request before the runner job.
func TestBernardOpenPushesDraftBump(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	got := rr.open(goUpdate(), actionUpdate())

	b := got.Base()
	assert.Equal(t, domain.DepsApplying, b.State)
	assert.Equal(t, "bernard/deps", b.Branch)
	assert.Equal(t, oid("d"), b.Head)
	assert.Equal(t, oid("d"), rr.branches["bernard/deps"])
	require.Len(t, rr.trees, 1)
	assert.Equal(t, map[domain.TreePath][]byte{
		mustPath(t, "go.mod"): []byte(goModText("go 1.24", "", textNew)), mustPath(t, botWorkflow): []byte("new"),
	}, rr.trees[0], "the go.mod bump and the action update, no go.sum")
	assert.Contains(t, rr.commits[0], "\nparent "+oid("a").String()+"\n")

	assert.EqualValues(t, 7, b.MR)
	mr := rr.mr(7)
	assert.Equal(t, "WIP: build(deps): update 2 dependencies", mr.Title.String())
	assert.True(t, mr.IsDraft())
	assert.Equal(t, "bernard/deps", mr.Source)
	assert.Equal(t, "bernard", domain.AuthorName(rr.creator))
	assert.Equal(t, oid("a").String(), rr.started.Sha, "the job runs on the tip, not the draft")
}

func TestBernardRunSuccessMarksReady(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.20.0")+sumLine("golang.org/x/text", "v0.21.0")
	rr.end(goReport(goMod, goSum), domain.CISuccess)

	snap := rr.snapshot()
	b := snap.Base()
	assert.Equal(t, domain.DepsIdle, b.State)
	assert.Equal(t, oid("e"), b.Head)
	assert.Equal(t, oid("e"), rr.branches["bernard/deps"], "moved from the draft head")
	assert.Contains(t, rr.commits[1], "\nparent "+oid("d").String()+"\n")
	assert.Equal(t, []int64{7}, rr.readied)
	mr := rr.mr(7)
	assert.False(t, mr.IsDraft())
	assert.Equal(t, "build(deps): update 1 dependency", mr.Title.String())
	assert.Contains(t, mr.Description.String(), "golang.org/x/text v0.20.0 \u2192 v0.21.0")
}

func TestBernardRunFailureKeepsDraft(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	rr.end(&ports.DepsReport{Error: "go mod tidy failed\ngo: missing go.sum entry"}, domain.CIFailure)

	snap := rr.snapshot()
	b := snap.Base()
	assert.Equal(t, domain.DepsFailed, b.State)
	assert.Equal(t, "go mod tidy failed\ngo: missing go.sum entry", b.Error)
	assert.Equal(t, []string{"go get golang.org/x/text@v0.21.0", "go mod tidy"}, b.Repro)
	assert.Equal(t, oid("d"), rr.branches["bernard/deps"], "the draft commit stays")
	assert.Empty(t, rr.readied)
	mr := rr.mr(7)
	assert.True(t, mr.IsDraft())
	assert.Contains(t, mr.Description.String(), "Bernard: update failed")
	assert.Contains(t, mr.Description.String(), "go: missing go.sum entry")
	assert.Contains(t, mr.Description.String(), "go get golang.org/x/text@v0.21.0")
}

// CI holds the runs of the draft head while the dependency job runs: the
// record already names it when CI hears of the push.
func TestBernardHoldsDraftHead(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	var heldAtPush []bool
	rr.onPush = func(u domain.RefUpdate) {
		sha, err := domain.ParseObjectID(u.New)
		require.NoError(t, err)
		heldAtPush = append(heldAtPush, rr.b.Held(t.Context(), rr.repo, "bernard/deps", sha))
	}
	rr.open(goUpdate())
	assert.Equal(t, []bool{true}, heldAtPush, "the draft push is held")
	assert.True(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("d")))
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("a")), "another head")
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "main", oid("d")), "another branch")

	rr.end(&ports.DepsReport{Error: "tidy failed"}, domain.CIFailure)
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("d")), "a failed batch holds nothing (and starts nothing)")
}

func TestBernardFinalCommitNotHeld(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	var heldAtPush []bool
	rr.onPush = func(u domain.RefUpdate) {
		sha, err := domain.ParseObjectID(u.New)
		require.NoError(t, err)
		heldAtPush = append(heldAtPush, rr.b.Held(t.Context(), rr.repo, "bernard/deps", sha))
	}
	goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.20.0")+sumLine("golang.org/x/text", "v0.21.0")
	rr.end(goReport(goMod, goSum), domain.CISuccess)
	assert.Equal(t, []bool{false}, heldAtPush, "CI runs the final commit normally")
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("e")))
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("d")))
}

// The final tree equals the draft: no new commit, the held draft head is
// released instead.
func TestBernardFinalWithoutNewCommitReleasesTheHold(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	hold := &releaseRecorder{}
	rr.b.SetHoldForTest(hold)
	rr.open(goUpdate())
	rr.git.ExpectedCalls = slices.DeleteFunc(rr.git.ExpectedCalls, func(c *mock.Call) bool { return c.Method == "CommitTree" })
	rr.git.EXPECT().CommitTree(mock.Anything, rr.repo, oid("d")).Return(oid("b"), nil) // the draft tree is the final tree
	rr.end(goReport(goModText("go 1.24", "", textNew), rr.files["go.sum"]), domain.CISuccess)
	assert.Len(t, rr.commits, 1, "nothing new")
	snap := rr.snapshot()
	assert.Equal(t, domain.DepsIdle, snap.Base().State)
	assert.Equal(t, []domain.ObjectID{oid("d")}, hold.released)
	assert.False(t, rr.mr(7).IsDraft())
}

type releaseRecorder struct{ released []domain.ObjectID }

func (h *releaseRecorder) ReleaseHold(_ context.Context, _ domain.RepoName, _ string, sha domain.ObjectID) error {
	h.released = append(h.released, sha)
	return nil
}

func TestBernardHumanPushAdopts(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	rr.startWatch()
	// Another branch, and a push of Bernard's own head, change nothing.
	rr.events <- domain.PushEvent{Repo: rr.repo, Updates: []domain.RefUpdate{{Ref: "refs/heads/other", Old: domain.ZeroOID, New: oid("5").String()}}}
	rr.events <- domain.PushEvent{Repo: rr.repo, Updates: []domain.RefUpdate{{Ref: "refs/heads/bernard/deps", Old: oid("a").String(), New: oid("d").String()}}}
	rr.mu.Lock()
	rr.branches["bernard/deps"] = oid("5")
	rr.mu.Unlock()
	rr.events <- domain.PushEvent{Repo: rr.repo, Updates: []domain.RefUpdate{{Ref: "refs/heads/bernard/deps", Old: oid("d").String(), New: oid("5").String()}}}
	require.Eventually(t, func() bool { return rr.snapshot().Batches[0].Adopted }, 5*time.Second, 5*time.Millisecond)
	assert.Contains(t, rr.mr(7).Description.String(), "changed by hand")
	assert.False(t, rr.b.Held(t.Context(), rr.repo, "bernard/deps", oid("d")))

	// The MR ends: the branch is Bernard's again.
	rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 7}}
	rr.runs.EXPECT().CancelDepsRun(mock.Anything, rr.repo, int64(runnerRunID)).Return(nil).Maybe()
	require.Eventually(t, func() bool { return !rr.snapshot().Batches[0].Adopted }, 5*time.Second, 5*time.Millisecond)
}

func TestBernardAdoptedBranchNotRewritten(t *testing.T) {
	t.Run("job success only refreshes the body", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		rr.mu.Lock()
		rr.branches["bernard/deps"] = oid("5") // a human push Bernard has not heard of: the CAS tells
		rr.mu.Unlock()
		goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.21.0")
		rr.end(goReport(goMod, goSum), domain.CISuccess)
		snap := rr.snapshot()
		assert.True(t, snap.Base().Adopted)
		assert.Equal(t, domain.DepsIdle, snap.Base().State)
		assert.Equal(t, oid("5"), rr.branches["bernard/deps"], "not rewritten")
		assert.Len(t, rr.commits, 1)
		assert.Empty(t, rr.readied, "the title is the human's")
		assert.Contains(t, rr.mr(7).Description.String(), "changed by hand")

		_, err := rr.b.OpenNow(t.Context(), domain.Owner(), rr.repo, "", snap.Base().Refs())
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
		require.ErrorContains(t, err, "the branch was changed by hand")
		assert.Equal(t, 1, rr.starts)
	})
	t.Run("Open finds the branch moved", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.rec.Base().Head, rr.rec.Base().MR = oid("d"), 7
		rr.branches["bernard/deps"] = oid("5")
		rr.rec.Base().Updates = []domain.DepUpdate{goUpdate()}
		_, err := rr.b.OpenNow(t.Context(), domain.Owner(), rr.repo, "", []domain.DepRef{goUpdate().DepRef})
		require.ErrorContains(t, err, "the branch was changed by hand")
		assert.True(t, rr.snapshot().Batches[0].Adopted)
		assert.Equal(t, domain.DepsIdle, rr.snapshot().Batches[0].State)
		assert.Zero(t, rr.starts)
		assert.Empty(t, rr.commits)
	})
}
