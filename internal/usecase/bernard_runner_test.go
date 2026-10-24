// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
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
	files map[string]string // the files at the tip
}

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

// run takes the job as runner r1 and completes it with the report.
func (rr *runnerRig) run(report *ports.DepsReport, conclusion domain.CIConclusion) ports.DepsAssignment {
	rr.t.Helper()
	ctx := context.Background()
	a, ok, err := rr.b.Next(ctx, "r1")
	require.NoError(rr.t, err)
	require.True(rr.t, ok)
	require.NoError(rr.t, rr.b.Complete(ctx, "r1", a.JobID, report, conclusion))
	return a
}

func goReport(goMod, goSum string) *ports.DepsReport {
	return &ports.DepsReport{Files: []ports.DepsFile{
		{Path: "go.mod", Content: []byte(goMod)}, {Path: "go.sum", Content: []byte(goSum)},
	}}
}

func TestBernardOpenGoQueuesAJobAtTheTip(t *testing.T) {
	for _, c := range []struct{ name, goMod, want string }{
		{"go only", goModText("go 1.24", "", textOld), "1.24"},
		{"toolchain higher", goModText("go 1.24", "toolchain go1.25.1\n", textOld), "1.25.1"},
		{"go higher", goModText("go 1.26.0", "toolchain go1.24.3\n", textOld), "1.26.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, c.goMod)
			got := rr.open(goUpdate(), actionUpdate())
			assert.Equal(t, domain.DepsApplying, got.State)
			assert.Regexp(t, `^deps-[0-9a-f]{16}$`, got.JobID)

			a, ok, err := rr.b.Next(context.Background(), "r1")
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, got.JobID, a.JobID)
			assert.Equal(t, oid("a").String(), a.Sha)
			assert.Equal(t, c.want, a.Job.GoVersion)
			assert.Equal(t, []domain.DepRef{goUpdate().DepRef}, a.Job.Updates, "only the runner-side updates")
		})
	}
}

func TestBernardOpenGoRefusesABadGoVersion(t *testing.T) {
	for _, line := range []string{"go 1.25rc1", "go 2.0"} {
		rr := newRunnerRig(t, goModText(line, "", textOld))
		rr.rec.Updates = []domain.DepUpdate{goUpdate()}
		_, err := rr.b.Open(context.Background(), domain.Owner(), rr.repo, []domain.DepRef{goUpdate().DepRef})
		require.Error(t, err, line)
		assert.Equal(t, domain.DepsFailed, rr.rec.State)
		assert.Contains(t, rr.rec.Error, "Go version")
		_, ok, _ := rr.b.Next(context.Background(), "r1")
		assert.False(t, ok, "no job was queued")
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
		{"unrequested direct bump", mk(file("go.mod", strings.Replace(good, "x/net v0.30.0", "x/net v0.31.0", 1))), "unrequested direct requirement"},
		{"requested version missing", mk(file("go.mod", goModText("go 1.24", "", "v0.20.1"))), "requested version missing"},
		{"malformed go.sum", mk(file("go.mod", good), file("go.sum", "nonsense\n")), "malformed go.sum"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
			rr.open(goUpdate())
			rr.run(c.report, domain.CISuccess)
			assert.Equal(t, domain.DepsFailed, rr.rec.State)
			assert.Contains(t, rr.rec.Error, c.want)
			assert.Zero(t, rr.rec.MR)
		})
	}
}

func TestBernardRunnerReportOversizeTotal(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	// Four 2 MiB files cannot all be allowed paths: use the limit through two
	// directories' worth of files.
	rr.run(&ports.DepsReport{Files: []ports.DepsFile{
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
	rr.eco.EXPECT().Validate(mock.Anything, mock.Anything, mock.Anything).Return(nil)
	mod, sum := mustPath(t, "go.mod"), mustPath(t, "go.sum")
	rr.happyGit(domain.ObjectID{}, map[domain.TreePath][]byte{mod: []byte(goMod), sum: []byte(goSum), wf: []byte("new")})
	rr.expectCreate(7)

	got := rr.open(goUpdate(), actionUpdate())
	require.Equal(t, domain.DepsApplying, got.State)
	// The target moves while the runner works: the commit is still built on the job's.
	rr.run(goReport(goMod, goSum), domain.CISuccess)

	assert.Equal(t, domain.DepsIdle, rr.rec.State)
	assert.Empty(t, rr.rec.Error)
	assert.Empty(t, rr.rec.JobID)
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
	rr.run(goReport(goMod, rr.files["go.sum"]), domain.CISuccess)
	assert.Equal(t, domain.DepsIdle, rr.rec.State)
	assert.EqualValues(t, 3, rr.rec.MR)
}

func TestBernardRunnerFailureShowsTheLogTail(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	ctx := context.Background()
	a, _, err := rr.b.Next(ctx, "r1")
	require.NoError(t, err)
	_, err = rr.b.AppendLog(ctx, "r1", a.JobID, 0, []domain.CILogLine{{Text: "go: example.com/x@v9: not found"}})
	require.NoError(t, err)
	require.NoError(t, rr.b.Complete(ctx, "r1", a.JobID, &ports.DepsReport{Error: "go get failed"}, domain.CIFailure))
	assert.Equal(t, domain.DepsFailed, rr.rec.State)
	assert.Contains(t, rr.rec.Error, "go get failed")
	assert.Contains(t, rr.rec.Error, "not found")
}

func TestBernardClosingTheMRCancelsTheJob(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	var revoked []string
	rr.b.SetRevokeJob(func(_ context.Context, id string) { revoked = append(revoked, id) })
	other := actionUpdate()
	rr.rec.MR = 4
	rr.rec.MRVersions = []domain.DepRef{goUpdate().DepRef}
	got := rr.open(goUpdate(), other)
	require.Equal(t, domain.DepsApplying, got.State)
	rr.mu.Lock()
	rr.rec.Updates = []domain.DepUpdate{goUpdate(), other}
	rr.mu.Unlock()
	a, ok, err := rr.b.Next(context.Background(), "r1")
	require.NoError(t, err)
	require.True(t, ok)

	rr.startWatch()
	rr.events <- domain.MergeRequestClosed{MergeRequest: domain.MergeRequest{Repo: rr.repo, Number: 4}}
	require.Eventually(t, func() bool { return rr.snapshot().State == domain.DepsIdle }, 5*time.Second, 5*time.Millisecond)

	snap := rr.snapshot()
	assert.Empty(t, snap.JobID)
	assert.Equal(t, []domain.DepUpdate{other}, snap.Updates, "the closed versions are gone, the rest remains")
	assert.Equal(t, []string{a.JobID}, revoked)
	// The late report is refused: the job left the queue.
	assert.ErrorIs(t, rr.b.Complete(context.Background(), "r1", a.JobID, goReport("x", "y"), domain.CISuccess), domain.ErrNotFound)
	assert.Zero(t, snap.MR)
}

func TestBernardReportDroppedWhenRecordLeftTheJob(t *testing.T) {
	rr := newRunnerRig(t, goModText("go 1.24", "", textOld))
	rr.open(goUpdate())
	a, ok, err := rr.b.Next(context.Background(), "r1")
	require.NoError(t, err)
	require.True(t, ok)
	// The record moved on (job replaced) while the report was in flight.
	rr.mu.Lock()
	rr.rec.State, rr.rec.JobID = domain.DepsIdle, ""
	rr.mu.Unlock()
	goMod := goModText("go 1.24", "", textNew)
	require.NoError(t, rr.b.Complete(context.Background(), "r1", a.JobID, goReport(goMod, rr.files["go.sum"]), domain.CISuccess))
	assert.Equal(t, domain.DepsIdle, rr.snapshot().State)
	assert.Zero(t, rr.snapshot().MR) // no git mock was called: nothing was published
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
			a, ok, err := rr.b.Next(context.Background(), "r1")
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, c.want, a.Job.GoVersion)
		})
	}
}
