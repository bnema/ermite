// SPDX-License-Identifier: AGPL-3.0-or-later
package usecase_test

import (
	"bnema.dev/ermite/internal/adapters/deps"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"context"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBernardPNPMWorkspaceCheckOpenReport(t *testing.T) {
	testBernardPNPM(t, "lockfileVersion: '9.0'\n")
}

// Lockfiles count only by presence and version header: one larger than the
// manifest and report caps neither hides the root nor blocks Open or the report.
func TestBernardPNPMLargeLockfile(t *testing.T) {
	testBernardPNPM(t, "lockfileVersion: '9.0'\n"+strings.Repeat("# padding\n", (3<<20)/10))
}

func testBernardPNPM(t *testing.T, lock string) {
	r := newOpenRigWith(t, func(*openRig) []ports.DepsEcosystem { return []ports.DepsEcosystem{deps.NPM{}} })
	r.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e domain.Event) bool { _, ok := e.(domain.DependencyUpdatesFound); return ok })).Return()
	reg := mocks.NewDepsRegistry(t)
	r.registry = reg
	files := map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0","engines":{"node":">=20"}}`, "pnpm-workspace.yaml": "packages:\n - 'packages/*'\n", "pnpm-lock.yaml": lock, "packages/app/package.json": `{ "dependencies": {"react": "^18.0.0"}, "scripts":{"postinstall":"do not run"}}`}
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("a")}, nil)
	r.reader.EXPECT().ListPaths(mock.Anything, r.repo, mock.Anything, domain.MaxPathEntries, domain.MaxPathBytes).RunAndReturn(func(context.Context, domain.RepoName, domain.Revision, int, int) (domain.PathList, error) {
		var paths []string
		for p := range files {
			paths = append(paths, p)
		}
		return domain.PathList{Paths: paths}, nil
	}).Maybe()
	r.reader.EXPECT().OpenBlob(mock.Anything, r.repo, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, _ domain.Revision, p domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
		s, ok := files[p.String()]
		if !ok {
			return domain.BlobInfo{}, nil, domain.ErrNotFound
		}
		return domain.BlobInfo{Size: int64(len(s))}, io.NopCloser(strings.NewReader(s)), nil
	}).Maybe()
	reg.EXPECT().NPMVersions(mock.Anything, "react", "v18.0.0").Return([]domain.DepVersion{{Version: "v18.1.0", Published: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}, nil)
	reg.EXPECT().Malicious(mock.Anything, domain.EcosystemNPM, "react", "v18.1.0").Return(false, nil)
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil).Maybe()
	checked, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, checked.Base().Updates, 1)
	r.repos.EXPECT().DefaultBranch(mock.Anything, r.repo).Return("main", true, nil)
	r.git.EXPECT().BranchHead(mock.Anything, r.repo, "main").Return(oid("a"), nil)
	var job domain.CIDepsJob
	r.runs.EXPECT().StartDepsRun(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p ports.DepsRunParams) (domain.CIRun, error) {
		job = p.Job
		return domain.CIRun{ID: 77, Sha: oid("a").String()}, nil
	})
	_, err = r.b.Open(context.Background(), domain.Owner(), r.repo, "", checked.Base().Refs())
	require.NoError(t, err)
	require.Equal(t, []string{"deps", "deps.npm"}, job.Requires)
	require.Equal(t, "20", job.NodeVersion)
	require.Empty(t, job.GoVersion)
	require.Equal(t, "pnpm", job.Details[0].Manager)
	require.Equal(t, "9.15.0", job.Details[0].ManagerVersion)
	require.Equal(t, "^18.1.0", job.Details[0].Range)
	require.Empty(t, job.Details[0].Root)
	run := domain.CIRun{Repo: r.repo, ID: 77, Sha: oid("a").String()}
	ciJob := domain.CIJob{Deps: &job}
	updated := strings.Replace(files["packages/app/package.json"], "^18.0.0", "^18.1.0", 1)
	report := ports.DepsReport{Files: []ports.DepsFile{{Path: "packages/app/package.json", Content: []byte(updated)}, {Path: "pnpm-lock.yaml", Content: []byte("opaque updated lock")}}}
	require.NoError(t, r.b.CheckDepsReport(context.Background(), run, ciJob, report))
	report.Files[0].Content = []byte(strings.Replace(updated, "do not run", "evil", 1))
	require.ErrorIs(t, r.b.CheckDepsReport(context.Background(), run, ciJob, report), domain.ErrInvalid)
}

// A broken package.json is skipped with a warning; Go updates are still proposed.
func TestBernardBrokenPackageJSONKeepsGoUpdates(t *testing.T) {
	r := newOpenRigWith(t, func(*openRig) []ports.DepsEcosystem { return []ports.DepsEcosystem{deps.GoMod{}, deps.NPM{}} })
	r.bus.EXPECT().Publish(mock.Anything, mock.Anything).Return().Maybe()
	reg := mocks.NewDepsRegistry(t)
	r.registry = reg
	files := map[string]string{"go.mod": "module example.com/app\ngo 1.24\nrequire example.com/lib v1.0.0\n", "web/package.json": `{"dependencies":`}
	r.meta.EXPECT().Get(mock.Anything, r.repo).Return(domain.Repo{Name: r.repo}, nil).Maybe()
	r.reader.EXPECT().ResolveRevision(mock.Anything, r.repo, domain.Revision{}).Return(domain.ResolvedRevision{CommitID: oid("a")}, nil)
	r.reader.EXPECT().ListPaths(mock.Anything, r.repo, mock.Anything, domain.MaxPathEntries, domain.MaxPathBytes).Return(domain.PathList{Paths: []string{"go.mod", "web/package.json"}}, nil)
	r.reader.EXPECT().OpenBlob(mock.Anything, r.repo, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.RepoName, _ domain.Revision, p domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
		s := files[p.String()]
		return domain.BlobInfo{Size: int64(len(s))}, io.NopCloser(strings.NewReader(s)), nil
	})
	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg.EXPECT().GoVersions(mock.Anything, "example.com/lib", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.1.0", Published: published}}, nil)
	reg.EXPECT().GoMajorPaths(mock.Anything, "example.com/lib").Return(nil, nil)
	reg.EXPECT().GoDirective(mock.Anything, "example.com/lib", "v1.1.0").Return("1.22", nil)
	reg.EXPECT().Malicious(mock.Anything, domain.EcosystemGoMod, "example.com/lib", "v1.1.0").Return(false, nil)
	r.store.EXPECT().IsIgnored(mock.Anything, r.repo, mock.Anything).Return(false, nil).Maybe()
	checked, err := r.b.Check(context.Background(), domain.Owner(), r.repo)
	require.NoError(t, err)
	require.Len(t, checked.Base().Updates, 1)
	require.Equal(t, "example.com/lib", checked.Base().Updates[0].Name)
}
