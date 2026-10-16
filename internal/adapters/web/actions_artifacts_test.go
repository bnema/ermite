// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// fakeArtifacts drives a mock ArtifactService that refuses anonymous callers,
// like the use case, and serves one artifact. It counts the lists and records
// the deletions.
type fakeArtifacts struct {
	m       *mocks.ArtifactService
	mu      sync.Mutex
	list    ports.ArtifactList
	listErr error
	art     domain.CIArtifact
	data    []byte
	lists   int
	deleted []int64
}

func newFakeArtifacts(t *testing.T, name, mediaType string, data []byte) *fakeArtifacts {
	t.Helper()
	repo := repoName(t, "proj")
	a := domain.CIArtifact{
		ID: 7, Repo: repo, RunID: 1, Attempt: 1, JobID: "1-0-0", Name: name, MediaType: mediaType,
		Size: int64(len(data)), State: domain.CIArtifactFinal,
		CreatedAt: fixedNow, ExpiresAt: fixedNow.Add(90 * 24 * time.Hour),
	}
	f := &fakeArtifacts{
		m: mocks.NewArtifactService(t), art: a, data: data,
		list: ports.ArtifactList{Artifacts: []domain.CIArtifact{a}, Used: 3 << 20, Quota: 10 << 30},
	}
	f.m.EXPECT().ListArtifacts(mock.Anything, mock.Anything, repo, mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, runID int64) (ports.ArtifactList, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lists++
			switch {
			case !p.IsOwner():
				return ports.ArtifactList{}, domain.ErrUnauthenticated
			case f.listErr != nil:
				return ports.ArtifactList{}, f.listErr
			case runID != f.art.RunID:
				return ports.ArtifactList{}, nil
			}
			return f.list, nil
		}).Maybe()
	f.m.EXPECT().DownloadArtifact(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, id, offset int64, send func(ports.ArtifactChunk) error) error {
			if !p.IsOwner() {
				return domain.ErrUnauthenticated
			}
			if id != f.art.ID || offset != 0 {
				return domain.ErrNotFound
			}
			a := f.art
			if err := send(ports.ArtifactChunk{Artifact: &a}); err != nil {
				return err
			}
			half := len(f.data) / 2
			for _, d := range [][]byte{f.data[:half], f.data[half:]} {
				if err := send(ports.ArtifactChunk{Data: d}); err != nil {
					return err
				}
			}
			return nil
		}).Maybe()
	f.m.EXPECT().DeleteArtifact(mock.Anything, mock.Anything, repo, mock.Anything).RunAndReturn(
		func(_ context.Context, p domain.Principal, _ domain.RepoName, id int64) error {
			if !p.IsOwner() {
				return domain.ErrUnauthenticated
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deleted = append(f.deleted, id)
			return nil
		}).Maybe()
	return f
}

// artifactsServer serves the run 1 of proj, with its job 1-0-0, and the
// artifacts of arts; the session token "tok" signs the owner in.
func artifactsServer(t *testing.T, arts *fakeArtifacts) *passkeyFixture {
	t.Helper()
	repo := repoName(t, "proj")
	runs := mocks.NewRunService(t)
	job := ports.JobView{CIJob: domain.CIJob{
		ID: "1-0-0", RunID: 1, Attempt: 1, RunAttempt: 1, Repo: repo, Key: "build", Name: "build",
		Phase: domain.CIPhaseEnded, Conclusion: domain.CISuccess,
	}}
	runs.EXPECT().GetRun(mock.Anything, mock.Anything, repo, int64(1), int32(0)).Return(ports.RunDetail{
		Run:  domain.CIRun{Repo: repo, ID: 1, Attempt: 1, Number: 1, WorkflowName: "ci", Sha: strings.Repeat("a", 40), Status: domain.CICompleted, Conclusion: domain.CISuccess},
		Jobs: []ports.JobView{job}, LatestAttempt: 1,
	}, nil).Maybe()
	runs.EXPECT().ListAnnotations(mock.Anything, mock.Anything, repo, int64(1), int32(1), "").Return(ports.AnnotationList{}, nil).Maybe()
	runs.EXPECT().GetStepSummaries(mock.Anything, mock.Anything, repo, mock.Anything, mock.Anything, mock.Anything).Return(ports.SummaryList{}, nil).Maybe()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	cfg := Config{
		Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse, Runs: runs,
		PublicURL: forgeOrigin, Sessions: newSessions(t, "tok"), Limiter: &blockingLimiter{},
		Now: func() time.Time { return fixedNow },
	}
	if arts != nil {
		cfg.Artifacts = arts.m
	}
	return &passkeyFixture{srv: newTestServer(t, cfg)}
}

func TestRunPageArtifactsForTheOwner(t *testing.T) {
	t.Parallel()
	arts := newFakeArtifacts(t, "dist", domain.CIArtifactZip, []byte("PK"))
	f := artifactsServer(t, arts)
	rec := f.do(t, req{method: "GET", target: "/proj/actions/runs/1", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `id="run-artifacts"`)
	assert.Contains(t, body, `href="/proj/actions/runs/1/artifacts/7"`)
	assert.Contains(t, body, ">dist</a>")
	assert.Contains(t, body, "2 B", "the size")
	assert.Contains(t, body, `datetime="2027-01-10T12:00:00Z"`, "the expiry")
	assert.Contains(t, body, `href="/proj/actions/runs/1/job/1-0-0">build</a>`, "the job that uploaded it")
	assert.Contains(t, body, "3.0 MiB of 10.0 GiB used")
	assert.Contains(t, body, `action="/proj/actions/runs/1/artifacts/7/delete"`)
}

func TestRunPageArtifactsHidden(t *testing.T) {
	t.Parallel()
	// Anonymous: no section, and the use case is not even asked.
	arts := newFakeArtifacts(t, "dist", domain.CIArtifactZip, []byte("PK"))
	f := artifactsServer(t, arts)
	rec := f.do(t, req{method: "GET", target: "/proj/actions/runs/1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "run-artifacts")
	assert.Zero(t, arts.lists)

	// A refusal or a failure of the list leaves the page whole, without it.
	for _, err := range []error{domain.ErrForbidden, domain.ErrNotFound, assert.AnError} {
		arts := newFakeArtifacts(t, "dist", domain.CIArtifactZip, []byte("PK"))
		arts.listErr = err
		rec := artifactsServer(t, arts).do(t, req{method: "GET", target: "/proj/actions/runs/1", cookie: "tok"})
		require.Equal(t, http.StatusOK, rec.Code, err)
		assert.NotContains(t, rec.Body.String(), "run-artifacts", err)
	}

	// No artifacts on the server: no section and no route.
	f = artifactsServer(t, nil)
	rec = f.do(t, req{method: "GET", target: "/proj/actions/runs/1", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "run-artifacts")
	assert.Equal(t, http.StatusNotFound, f.do(t, req{method: "GET", target: "/proj/actions/runs/1/artifacts/7", cookie: "tok"}).Code)
}

func TestArtifactDownloadZip(t *testing.T) {
	t.Parallel()
	data := []byte("PK\x03\x04 the archive")
	f := artifactsServer(t, newFakeArtifacts(t, "dist", domain.CIArtifactZip, data))
	rec := f.do(t, req{method: "GET", target: "/proj/actions/runs/1/artifacts/7", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	h := rec.Header()
	assert.Equal(t, "application/zip", h.Get("Content-Type"))
	assert.Equal(t, `attachment; filename=dist.zip`, h.Get("Content-Disposition"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Contains(t, h.Get("Content-Security-Policy"), "sandbox")
	assert.Equal(t, itoa(len(data)), h.Get("Content-Length"))
	assert.Equal(t, sessionCache, h.Get("Cache-Control"))
	assert.Equal(t, data, rec.Body.Bytes())

	// HEAD: the headers only.
	rec = f.do(t, req{method: "HEAD", target: "/proj/actions/runs/1/artifacts/7", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, itoa(len(data)), rec.Header().Get("Content-Length"))
	assert.Empty(t, rec.Body.Bytes())
}

// The media type an uploader chose is never served: it could be text/html.
func TestArtifactDownloadOtherType(t *testing.T) {
	t.Parallel()
	data := []byte("<script>alert(1)</script>")
	f := artifactsServer(t, newFakeArtifacts(t, "report é.html", "text/html", data))
	rec := f.do(t, req{method: "GET", target: "/proj/actions/runs/1/artifacts/7", cookie: "tok"})
	require.Equal(t, http.StatusOK, rec.Code)
	h := rec.Header()
	assert.Equal(t, "application/octet-stream", h.Get("Content-Type"))
	assert.Equal(t, `attachment; filename="report _.html"; filename*=UTF-8''report%20%C3%A9.html`, h.Get("Content-Disposition"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Contains(t, h.Get("Content-Security-Policy"), "sandbox")
	assert.Equal(t, data, rec.Body.Bytes())
}

func TestArtifactDownloadRefused(t *testing.T) {
	t.Parallel()
	f := artifactsServer(t, newFakeArtifacts(t, "dist", domain.CIArtifactZip, []byte("PK")))
	// Anonymous, even on a public repository.
	rec := f.do(t, req{method: "GET", target: "/proj/actions/runs/1/artifacts/7"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("Content-Disposition"))
	// Not a number, unknown, or an artifact of another run.
	for _, target := range []string{
		"/proj/actions/runs/1/artifacts/x", "/proj/actions/runs/1/artifacts/0", "/proj/actions/runs/1/artifacts/-7",
		"/proj/actions/runs/1/artifacts/8", "/proj/actions/runs/2/artifacts/7", "/proj/actions/runs/x/artifacts/7",
	} {
		rec := f.do(t, req{method: "GET", target: target, cookie: "tok"})
		assert.Equal(t, http.StatusNotFound, rec.Code, target)
		assert.Empty(t, rec.Header().Get("Content-Disposition"), target)
	}
}

func TestArtifactDelete(t *testing.T) {
	t.Parallel()
	arts := newFakeArtifacts(t, "dist", domain.CIArtifactZip, []byte("PK"))
	f := artifactsServer(t, arts)
	target := "/proj/actions/runs/1/artifacts/7/delete"
	form := "application/x-www-form-urlencoded"
	// The gate: a session and the Origin of the forge.
	assert.Equal(t, http.StatusForbidden, f.do(t, req{method: "POST", target: target, origin: forgeOrigin, contentType: form}).Code)
	assert.Equal(t, http.StatusForbidden, f.do(t, req{method: "POST", target: target, origin: "https://evil.example", cookie: "tok", contentType: form}).Code)
	assert.Empty(t, arts.deleted)

	rec := f.do(t, req{method: "POST", target: target, origin: forgeOrigin, cookie: "tok", contentType: form})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/proj/actions/runs/1", rec.Header().Get("Location"))
	assert.Equal(t, []int64{7}, arts.deleted)
}

func TestContentDisposition(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"dist.zip":         `attachment; filename=dist.zip`,
		"a b.txt":          `attachment; filename="a b.txt"`,
		"日本.zip":           `attachment; filename=__.zip; filename*=UTF-8''%E6%97%A5%E6%9C%AC.zip`,
		attachmentName(""): `attachment; filename=artifact`,
	} {
		assert.Equal(t, want, contentDisposition(in), in)
	}
	assert.Equal(t, "ab", attachmentName("a\"\x00\r\n\\b"))
	assert.Equal(t, "artifact", attachmentName(".."))
}
