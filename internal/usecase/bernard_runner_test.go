// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
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
}

const runnerRunID = 77

func newRunnerRig(t *testing.T, goMod string) *runnerRig {
	t.Helper()
	rr := &runnerRig{files: map[string]string{
		"go.mod": goMod,
		"go.sum": sumLine("golang.org/x/text", "v0.20.0"),
	}}
	rr.openRig = newOpenRigWith(t, func(r *openRig) []ports.DepsEcosystem {
		r.eco.EXPECT().Name().Return(domain.EcosystemActions).Maybe()
		r.eco.EXPECT().ApplyInServer().Return(true).Maybe()
		return []ports.DepsEcosystem{deps.GoMod{}, r.eco}
	})
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
	rr.rec.Updates = ups
	refs := make([]domain.DepRef, len(ups))
	for i, u := range ups {
		refs[i] = u.DepRef
	}
	got, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, refs)
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
			assert.Equal(t, domain.DepsApplying, got.State)
			assert.EqualValues(t, runnerRunID, got.RunID)
			assert.Equal(t, []domain.DepUpdate{goUpdate(), actionUpdate()}, got.RunUpdates, "every pending update is kept for the commit")

			assert.Equal(t, ports.DepsRunParams{
				Repo: rr.repo, Ref: "refs/heads/main", Sha: oid("a").String(), Title: "build(deps): update 2 dependencies",
				Job:   domain.CIDepsJob{GoVersion: c.want, Updates: []domain.DepRef{goUpdate().DepRef}}, // only the runner-side updates
				Label: "ubuntu-latest", Timeout: 10 * time.Minute,
			}, rr.started)
		})
	}
}

func TestBernardOpenWhileApplyingStartsNoSecondRun(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	_, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, []domain.DepRef{goUpdate().DepRef})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	assert.Equal(t, 1, rr.starts)
	assert.Equal(t, domain.DepsApplying, rr.snapshot().State, "the first run is still the record's")
	assert.EqualValues(t, runnerRunID, rr.snapshot().RunID)
}

func TestBernardOpenStartFailureFailsTheRecord(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.runs.ExpectedCalls = nil
	rr.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).Return(domain.CIRun{}, errors.New("store down\nsecond line"))
	rr.rec.Updates = []domain.DepUpdate{goUpdate()}
	_, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, []domain.DepRef{goUpdate().DepRef})
	require.Error(t, err)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsFailed, got.State)
	assert.Equal(t, "start the dependency run: store down\nsecond line", got.Error)
	assert.Zero(t, got.RunID)
}

// A record that moved while the run was being created does not keep it: the
// run is cancelled and the record stays as the other transition left it.
func TestBernardOpenCancelsTheRunWhenTheRecordMoved(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.runs.ExpectedCalls = nil
	rr.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, ports.DepsRunParams) (domain.CIRun, error) {
			rr.mu.Lock()
			rr.rec.Updates = nil // the daily check ran meanwhile
			rr.mu.Unlock()
			return domain.CIRun{Repo: rr.repo, ID: 5}, nil
		})
	rr.runs.EXPECT().CancelDepsRun(mock.Anything, rr.repo, int64(5)).Return(nil).Once()
	rr.rec.Updates = []domain.DepUpdate{goUpdate()}
	_, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, []domain.DepRef{goUpdate().DepRef})
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsIdle, got.State)
	assert.Zero(t, got.RunID)
}

func TestBernardOpenGoRefusesABadGoVersion(t *testing.T) {
	for _, line := range []string{"go 1.25rc1", "go 2.0"} {
		rr := newRunnerRig(t, goModText(line, "", textOld))
		rr.rec.Updates = []domain.DepUpdate{goUpdate()}
		_, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, []domain.DepRef{goUpdate().DepRef})
		require.Error(t, err, line)
		assert.Equal(t, domain.DepsFailed, rr.rec.State)
		assert.Contains(t, rr.rec.Error, "Go version")
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
			assert.Equal(t, domain.DepsFailed, rr.rec.State)
			assert.Contains(t, rr.rec.Error, c.want)
			assert.NotContains(t, rr.rec.Error, "\n", "one line")
			assert.EqualValues(t, runnerRunID, rr.rec.RunID, "the record keeps the link to the run")
			assert.Zero(t, rr.rec.MR)
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
	assert.Equal(t, domain.DepsFailed, rr.rec.State)
}

func TestBernardRunnerReportSuccessPublishesFromTheJobCommit(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	goMod, goSum := goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", "v0.20.0")+sumLine("golang.org/x/text", "v0.21.0")
	wf, err := domain.ParseTreePath(botWorkflow)
	require.NoError(t, err)
	rr.files[botWorkflow] = "old"
	rr.eco.EXPECT().Apply(mock.Anything, mock.Anything).Return(map[domain.TreePath][]byte{wf: []byte("new")}, nil)
	rr.eco.EXPECT().Validate(mock.Anything, mock.Anything, mock.Anything).Return(nil, nil)
	mod, sum := mustPath(t, "go.mod"), mustPath(t, "go.sum")
	rr.happyGit(domain.ObjectID{}, map[domain.TreePath][]byte{mod: []byte(goMod), sum: []byte(goSum), wf: []byte("new")})
	rr.expectCreate(7)

	got := rr.open(goUpdate(), actionUpdate())
	require.Equal(t, domain.DepsApplying, got.State)
	// The target moves while the runner works: the commit is still built on the run's.
	rr.end(goReport(goMod, goSum), domain.CISuccess)

	assert.Equal(t, domain.DepsIdle, rr.rec.State)
	assert.Empty(t, rr.rec.Error)
	assert.EqualValues(t, runnerRunID, rr.rec.RunID, "the record keeps the link to the last run")
	assert.Empty(t, rr.rec.RunUpdates)
	assert.EqualValues(t, 7, rr.rec.MR)
	assert.ElementsMatch(t, []domain.DepRef{goUpdate().DepRef, actionUpdate().DepRef}, rr.rec.MRVersions)
	assert.Equal(t, []string{"sign", "commit", "tip", "branch", "ci", "mr"}, rr.ops)
	assert.Contains(t, rr.created.Description, "golang.org/x/text v0.20.0 \u2192 v0.21.0")
	assert.Contains(t, rr.created.Description, "actions/checkout")
	assert.Equal(t, "build(deps): update 2 dependencies", rr.created.Title)
}

func TestBernardRunnerReportGoOnlyWithNoActions(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	goMod := goModText("go 1.24", "", textNew)
	mod := mustPath(t, "go.mod")
	rr.happyGit(domain.ObjectID{}, map[domain.TreePath][]byte{mod: []byte(goMod)})
	rr.expectCreate(3)
	rr.open(goUpdate())
	// An unchanged go.sum is not part of the commit.
	rr.end(goReport(goMod, rr.files["go.sum"]), domain.CISuccess)
	assert.Equal(t, domain.DepsIdle, rr.rec.State)
	assert.EqualValues(t, 3, rr.rec.MR)
}

func TestBernardRunnerFailureKeepsOutput(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	rr.end(&ports.DepsReport{Error: "go get failed\ngo: example.com/x@v9: not found"}, domain.CIFailure)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsFailed, got.State)
	assert.Equal(t, "go get failed\ngo: example.com/x@v9: not found", got.Error)
	assert.EqualValues(t, runnerRunID, got.RunID)
	assert.Empty(t, got.RunUpdates)
}

func TestBernardClosingTheMRCancelsTheRun(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	other := actionUpdate()
	rr.rec.MR = 4
	rr.rec.MRVersions = []domain.DepRef{goUpdate().DepRef}
	got := rr.open(goUpdate(), other)
	require.Equal(t, domain.DepsApplying, got.State)
	rr.mu.Lock()
	rr.rec.Updates = []domain.DepUpdate{goUpdate(), other}
	rr.mu.Unlock()
	rr.runs.EXPECT().CancelDepsRun(mock.Anything, rr.repo, int64(runnerRunID)).Return(nil).Once()

	rr.startWatch()
	rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 4}}
	require.Eventually(t, func() bool { return rr.snapshot().State == domain.DepsIdle }, 5*time.Second, 5*time.Millisecond)

	snap := rr.snapshot()
	assert.EqualValues(t, runnerRunID, snap.RunID, "the link to the cancelled run stays")
	assert.Empty(t, snap.RunUpdates)
	assert.Equal(t, []domain.DepUpdate{other}, snap.Updates, "the closed versions are gone, the rest remains")
	assert.Zero(t, snap.MR)

	// The cancelled job ends: the record left the run, nothing is published
	// (no git mock is called) and nothing is written.
	rr.end(nil, domain.CICancelled)
	assert.Equal(t, snap, rr.snapshot())
}

func TestBernardClosingTheMRWithoutARunCancelsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.rec = domain.RepoDeps{Repo: rr.repo, MR: 4, RunID: 9, MRVersions: []domain.DepRef{goUpdate().DepRef}, Updates: []domain.DepUpdate{goUpdate()}}
		rr.startWatch()
		rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 4}}
		synctest.Wait()
		assert.Zero(t, rr.snapshot().MR)
		assert.EqualValues(t, 9, rr.snapshot().RunID) // CancelDepsRun is not expected: the mock would fail
	})
}

func TestBernardReportDroppedWhenRecordLeftTheRun(t *testing.T) {
	for _, c := range []struct {
		name string
		move func(*domain.RepoDeps)
	}{
		{"idle", func(r *domain.RepoDeps) { r.State = domain.DepsIdle }},
		{"another run", func(r *domain.RepoDeps) { r.RunID = runnerRunID + 1 }},
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
	mod, sum := mustPath(t, "go.mod"), mustPath(t, "go.sum")
	rr.happyGit(domain.ObjectID{}, map[domain.TreePath][]byte{mod: []byte(goMod), sum: []byte(goSum)})
	rr.expectCreate(8)
	rr.open(goUpdate())
	rr.mu.Lock()
	rr.rec.Updates = []domain.DepUpdate{{DepRef: depRef("other/mod", "v1.0.0"), Current: "v0.9.0", Kind: domain.DepMajor, New: true}}
	rr.mu.Unlock()

	rr.end(goReport(goMod, goSum), domain.CISuccess)
	got := rr.snapshot()
	assert.Equal(t, domain.DepsIdle, got.State)
	assert.Equal(t, []domain.DepRef{goUpdate().DepRef}, got.MRVersions)
	assert.Contains(t, rr.created.Description, "golang.org/x/text")
	assert.NotContains(t, rr.created.Description, "other/mod")
	assert.Equal(t, "build(deps): update 1 dependency", rr.created.Title)
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
	rr.happyGit(domain.ObjectID{}, map[domain.TreePath][]byte{mustPath(t, "go.mod"): []byte(mod), mustPath(t, "go.sum"): []byte(sum)})
	rr.expectCreate(7)
	rr.open(goUpdate())
	rr.end(goReport(mod, sum), domain.CISuccess)
	assert.Equal(t, domain.DepsIdle, rr.rec.State)
	assert.Contains(t, rr.created.Description, "## Also changed by Go")
	assert.Contains(t, rr.created.Description, "- `golang.org/x/net` v0.30.0 → v0.31.0")
}

func TestBernardCheckDepsReportDoesNotPublishAndDropsSupersededRuns(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	run, job := rr.endedRun(domain.CISuccess)
	report := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	effects, err := rr.b.CheckDepsReport(context.Background(), run, job, *report)
	require.NoError(t, err)
	assert.Empty(t, effects)
	assert.Equal(t, domain.DepsApplying, rr.rec.State)
	assert.Zero(t, rr.rec.MR)
	rr.rec.RunID++
	effects, err = rr.b.CheckDepsReport(context.Background(), run, job, ports.DepsReport{Files: []ports.DepsFile{{Path: "evil.sh"}}})
	require.NoError(t, err)
	assert.Empty(t, effects)
}

func TestBernardCheckDepsReportDoesNotWaitForTheRepositoryLock(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	run, job := rr.endedRun(domain.CISuccess)
	report := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	done := make(chan error, 1)
	rr.b.UnderRepoLock(rr.repo, func() {
		go func() {
			_, err := rr.b.CheckDepsReport(context.Background(), run, job, *report)
			done <- err
		}()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "CheckDepsReport waits for the repository lock")
		}
	})
}

func TestBernardCheckDepsReportClassifiesReadFailures(t *testing.T) {
	good := goReport(goModText("go 1.24", "", textNew), sumLine("golang.org/x/text", textNew))
	t.Run("go.mod absent at the commit is a permanent refusal", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		delete(rr.files, "go.mod")
		run, job := rr.endedRun(domain.CISuccess)
		_, err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
	t.Run("a git failure stays transient", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		boom := errors.New("git is down")
		rr.reader.ExpectedCalls = nil
		rr.reader.EXPECT().OpenBlob(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(domain.BlobInfo{}, nil, boom)
		run, job := rr.endedRun(domain.CISuccess)
		_, err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrInvalid)
		assert.NotErrorIs(t, err, domain.ErrPreconditionFailed)
	})
	t.Run("an oversize go.mod is a permanent refusal", func(t *testing.T) {
		rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
		rr.open(goUpdate())
		rr.files["go.mod"] = strings.Repeat("x", 3<<20)
		run, job := rr.endedRun(domain.CISuccess)
		_, err := rr.b.CheckDepsReport(context.Background(), run, job, *good)
		require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})
}
