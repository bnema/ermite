// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

type releaseFixture struct {
	srv               *Server
	svc               *mocks.ReleaseService
	published, draft  domain.Release
	asset             domain.ReleaseAsset
	data              string
	downloadErr       error
	failAfterMetadata bool
	downloads         int
	tokens            []string
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	repo := repoName(t, "proj")
	f := &releaseFixture{svc: mocks.NewReleaseService(t), data: "0123456789"}
	f.published = domain.Release{ID: 1, Repo: repo, Tag: "stable/v1", Title: "First release", Notes: "# Notes\n\n**Ready** [bad](javascript:alert(1)) <script>alert(1)</script>\n\n[guide](guide.md)\n\n```sh\ncurl https://example.com\n```", TargetCommitish: strings.Repeat("a", 40), PublishedAt: fixedNow, Prerelease: true}
	f.draft = domain.Release{ID: 2, Repo: repo, Tag: "unpublished", Title: "Secret draft", Draft: true, MakeLatest: true}
	f.asset = domain.ReleaseAsset{ID: 7, ReleaseID: 1, Repo: repo, Name: "build.zip", Size: 10, ContentType: "application/zip", Digest: "sha256:" + strings.Repeat("b", 64)}
	f.svc.EXPECT().GetRelease(mock.Anything, mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Principal, _ domain.RepoName, ref ports.ReleaseRef) (domain.Release, error) {
		if ref.Latest || ref.Tag == f.published.Tag {
			return f.published, nil
		}
		if ref.Tag == f.draft.Tag && p.IsOwner() {
			return f.draft, nil
		}
		return domain.Release{}, domain.ErrNotFound
	}).Maybe()
	f.svc.EXPECT().ListReleases(mock.Anything, mock.Anything, repo, 30, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Principal, _ domain.RepoName, _ int, token string) (ports.ReleasePage, error) {
		f.tokens = append(f.tokens, token)
		rows := []domain.Release{f.published}
		if p.IsOwner() {
			rows = append([]domain.Release{f.draft}, rows...)
		}
		return ports.ReleasePage{Releases: rows, NextPageToken: "next+/="}, nil
	}).Maybe()
	f.svc.EXPECT().ListAssets(mock.Anything, mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, p domain.Principal, _ domain.RepoName, id int64) ([]domain.ReleaseAsset, error) {
		if id == f.published.ID {
			return []domain.ReleaseAsset{f.asset}, nil
		}
		if id == f.draft.ID && p.IsOwner() {
			return nil, nil
		}
		return nil, domain.ErrNotFound
	}).Maybe()
	f.svc.EXPECT().GetAsset(mock.Anything, mock.Anything, repo, mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, ref ports.AssetRef) (domain.ReleaseAsset, error) {
		if ref.ID == f.asset.ID || (ref.Name == f.asset.Name && (ref.Latest || ref.Tag == f.published.Tag)) {
			return f.asset, nil
		}
		return domain.ReleaseAsset{}, domain.ErrNotFound
	}).Maybe()
	f.svc.EXPECT().DownloadAsset(mock.Anything, mock.Anything, repo, ports.AssetRef{ID: 7}, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, offset, length int64, send func(ports.AssetChunk) error) error {
		f.downloads++
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.InDelta(t, 3600, time.Until(deadline).Seconds(), 2)
		if f.downloadErr != nil && !f.failAfterMetadata {
			return f.downloadErr
		}
		if err := send(ports.AssetChunk{Asset: &f.asset}); err != nil {
			return err
		}
		if f.downloadErr != nil {
			return f.downloadErr
		}
		return send(ports.AssetChunk{Data: []byte(f.data[offset : offset+length])})
	}).Maybe()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repo, mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	f.srv = newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: browse, Releases: f.svc, Sessions: newSessions(t, "tok"), Limiter: &blockingLimiter{}, Now: func() time.Time { return fixedNow }})
	return f
}
func (f *releaseFixture) request(method, path, cookie, byteRange string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	if byteRange != "" {
		r.Header.Set("Range", byteRange)
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	return w
}
func TestReleasePages(t *testing.T) {
	f := newReleaseFixture(t)
	for _, path := range []string{"/proj/releases", "/proj/releases/"} {
		w := f.request("GET", path, "", "")
		require.Equal(t, 200, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, "First release")
		assert.NotContains(t, body, "Secret draft")
		assert.Contains(t, body, ">Latest</span>")
		assert.Contains(t, body, ">Pre-release</span>")
		assert.Contains(t, body, `href="/proj/releases/tag/stable/v1"`)
		assert.Contains(t, body, "2026-10-12")
		assert.Contains(t, body, `<td>1</td>`)
		assert.Contains(t, body, `?page=next%2B%2F%3D`)
		assert.Contains(t, body, `data-key="g r"`)
		assert.NotContains(t, body, `aria-keyshortcuts="g r"`)
		assert.Contains(t, body, "view=releases")
	}
	owner := f.request("GET", "/proj/releases?page=cursor%2B%2F%3D", "tok", "")
	require.Equal(t, 200, owner.Code)
	assert.Contains(t, owner.Body.String(), "Secret draft")
	assert.Contains(t, owner.Body.String(), ">Draft</span>")
	assert.Contains(t, owner.Header().Get("Cache-Control"), "no-store")
	assert.Equal(t, []string{"", "", "cursor+/="}, f.tokens)
	// The pin on the draft must not become a computed latest badge.
	assert.Equal(t, 1, strings.Count(owner.Body.String(), ">Latest</span>"))
	w := f.request("GET", "/proj/releases/tag/stable/v1", "", "")
	require.Equal(t, 200, w.Code)
	body := w.Body.String()
	for _, text := range []string{"<strong>Ready</strong>", "build.zip", humanSize(10), "application/zip", `href="/proj/releases/assets/7"`, `href="/proj/tree/stable/v1"`, `href="/proj/commit/` + strings.Repeat("a", 40) + `"`, "view=release"} {
		assert.Contains(t, body, text)
	}
	notes := strings.SplitN(body, `<article class="mr-desc prose">`, 2)
	require.Len(t, notes, 2)
	notesBody := strings.SplitN(notes[1], "</article>", 2)[0]
	assert.Contains(t, notesBody, "<pre")
	assert.Contains(t, notesBody, "<code")
	assert.Contains(t, notesBody, "curl")
	assert.Contains(t, notesBody, "</code></pre>")
	assert.Contains(t, notesBody, `<h1 id="md-notes">Notes</h1>`)
	assert.Contains(t, body, `<section class="mr-card">`)
	assert.NotContains(t, body, "<script>alert")
	assert.NotContains(t, body, `href="javascript:`)
	assert.Contains(t, body, `href="/proj/blob/stable/v1/guide.md"`)
	for _, path := range []string{"/proj/releases/tag/unknown", "/proj/releases/tag/unpublished"} {
		assert.Equal(t, 404, f.request("GET", path, "", "").Code)
	}
	assert.Equal(t, 200, f.request("GET", "/proj/releases/tag/unpublished", "tok", "").Code)
	latest := f.request("GET", "/proj/releases/latest", "", "")
	assert.Equal(t, 303, latest.Code)
	assert.Equal(t, "/proj/releases/tag/stable/v1", latest.Header().Get("Location"))
}
func TestReleaseDownloads(t *testing.T) {
	f := newReleaseFixture(t)
	// A simple tag uses the stable URL; slash tags use the page's asset-id link.
	f.published.Tag = "v1"
	for _, path := range []string{"/proj/releases/download/v1/build.zip", "/proj/releases/latest/download/build.zip", "/proj/releases/assets/7"} {
		for _, tc := range []struct {
			rangeValue, body, cr string
			status               int
		}{
			{"", "0123456789", "", 200}, {"bytes=2-5", "2345", "bytes 2-5/10", 206}, {"bytes=7-", "789", "bytes 7-9/10", 206}, {"bytes=-3", "789", "bytes 7-9/10", 206}, {"bytes=8-99", "89", "bytes 8-9/10", 206}, {"bytes=0-0,2-3", "0123456789", "", 200}, {"bytes=10-", "", "bytes */10", 416},
		} {
			w := f.request("GET", path, "", tc.rangeValue)
			require.Equal(t, tc.status, w.Code, path+" "+tc.rangeValue)
			assert.Equal(t, tc.body, w.Body.String())
			assert.Equal(t, tc.cr, w.Header().Get("Content-Range"))
			assert.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
			assert.Equal(t, `"sha256-`+strings.Repeat("b", 64)+`"`, w.Header().Get("ETag"))
			assert.Equal(t, "application/zip", w.Header().Get("Content-Type"))
			assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, rawCSP, w.Header().Get("Content-Security-Policy"))
			_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			require.NoError(t, err)
			assert.Equal(t, "build.zip", params["filename"])
			assert.Equal(t, itoa(len(tc.body)), w.Header().Get("Content-Length"))
		}
		before := f.downloads
		w := f.request("HEAD", path, "", "bytes=1-2")
		assert.Equal(t, 200, w.Code)
		assert.Empty(t, w.Header().Get("Content-Range"))
		assert.Empty(t, w.Body.String())
		assert.Equal(t, "10", w.Header().Get("Content-Length"))
		assert.Equal(t, before, f.downloads)
	}
	for _, path := range []string{"/proj/releases/assets/0", "/proj/releases/assets/nope", "/proj/releases/assets/99", "/proj/releases/download/missing/build.zip", "/proj/releases/latest/download/nope"} {
		assert.Equal(t, 404, f.request("GET", path, "", "").Code)
	}
	r := httptest.NewRequest("GET", "/proj/releases/assets/7", nil)
	r.Header.Set("Range", "bytes=1-2")
	r.Header.Set("If-Range", `"old"`)
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, f.data, w.Body.String())
}
func TestReleaseDownloadIfRange(t *testing.T) {
	f := newReleaseFixture(t)
	etag := `"sha256-` + strings.Repeat("b", 64) + `"`
	for _, tc := range []struct {
		name, ifRange, body, cr string
		status                  int
	}{
		{"matching", etag, "2345", "bytes 2-5/10", 206},
		{"stale", `"old"`, "0123456789", "", 200},
		{"date", "Wed, 21 Oct 2026 12:00:00 GMT", "0123456789", "", 200},
	} {
		r := httptest.NewRequest("GET", "/proj/releases/assets/7", nil)
		r.Header.Set("Range", "bytes=2-5")
		r.Header.Set("If-Range", tc.ifRange)
		w := httptest.NewRecorder()
		f.srv.ServeHTTP(w, r)
		assert.Equal(t, tc.status, w.Code, tc.name)
		assert.Equal(t, tc.body, w.Body.String(), tc.name)
		assert.Equal(t, tc.cr, w.Header().Get("Content-Range"), tc.name)
	}
}

// A tag with a slash is one path segment once escaped: the download route must
// match it and resolve the tag with the slash.
func TestReleaseDownloadSlashTag(t *testing.T) {
	f := newReleaseFixture(t) // the published tag is "stable/v1"
	w := f.request("GET", "/proj/releases/download/"+url.PathEscape(f.published.Tag)+"/build.zip", "", "")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, f.data, w.Body.String())
}

func TestReleaseDownloadFailures(t *testing.T) {
	f := newReleaseFixture(t)
	f.downloadErr = errors.New("blob unavailable")
	w := f.request("GET", "/proj/releases/assets/7", "", "bytes=1-2")
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	for _, key := range []string{"Content-Disposition", "Content-Range", "Accept-Ranges"} {
		assert.Empty(t, w.Header().Get(key))
	}
	f.failAfterMetadata = true
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { f.request("GET", "/proj/releases/assets/7", "", "") })
}
func TestReleaseAttachmentHeaders(t *testing.T) {
	for _, name := range []string{"résumé.zip", "build.zip", "evil\r\nX-Injected: true.html", `<img src=x onerror=alert(1)>.html`} {
		h := http.Header{}
		releaseHeaders(h, domain.ReleaseAsset{Name: name, ContentType: "text/html", Digest: "sha256:abcd", Size: 12})
		assert.Equal(t, "application/octet-stream", h.Get("Content-Type"))
		assert.NotContains(t, h.Get("Content-Disposition"), "\r")
		assert.NotContains(t, h.Get("Content-Disposition"), "\n")
		media, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
		require.NoError(t, err)
		assert.Equal(t, "attachment", media)
		assert.Equal(t, attachmentName(name), params["filename"])
	}
}
func TestReleasesDisabled(t *testing.T) {
	s := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t)})
	for _, path := range []string{"/proj/releases", "/proj/releases/latest", "/proj/releases/assets/7"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		assert.Equal(t, 404, w.Code)
	}
}

func TestReleaseEmptyDownloads(t *testing.T) {
	f := newReleaseFixture(t)
	f.asset.Size = 0
	f.data = ""
	w := f.request("GET", "/proj/releases/assets/7", "", "")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "0", w.Header().Get("Content-Length"))
	assert.Empty(t, w.Body.String())
	w = f.request("GET", "/proj/releases/assets/7", "", "bytes=0-")
	assert.Equal(t, 416, w.Code)
	assert.Equal(t, "bytes */0", w.Header().Get("Content-Range"))
}

func TestReleaseLatestMissing(t *testing.T) {
	svc := mocks.NewReleaseService(t)
	repo := repoName(t, "proj")
	svc.EXPECT().GetRelease(mock.Anything, mock.Anything, repo, ports.ReleaseRef{Latest: true}).Return(domain.Release{}, domain.ErrNotFound)
	srv := newTestServer(t, Config{Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Releases: svc})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/proj/releases/latest", nil))
	assert.Equal(t, 404, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
}

func TestReleaseAssetNameEscapedInPage(t *testing.T) {
	f := newReleaseFixture(t)
	f.asset.Name = `<img src=x onerror=alert(1)>`
	w := f.request("GET", "/proj/releases/tag/stable/v1", "", "")
	require.Equal(t, 200, w.Code)
	assert.NotContains(t, w.Body.String(), f.asset.Name)
	assert.Contains(t, w.Body.String(), "&lt;img src=x onerror=alert(1)&gt;")
}

func TestReleaseRowFallbacks(t *testing.T) {
	row := releaseRow("proj", domain.Release{ID: 1, Tag: "v1", Draft: true, PublishedAt: fixedNow}, 0)
	assert.Equal(t, "v1", row.Title)
	assert.Equal(t, "draft", row.Published)
	assert.False(t, row.Latest)
}

func TestReleaseNotesReferences(t *testing.T) {
	f := newReleaseFixture(t)
	full := "f25742a" + strings.Repeat("b", 33)
	id, err := domain.ParseObjectID(full)
	require.NoError(t, err)
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ListRefs(mock.Anything, mock.Anything, repoName(t, "proj"), mock.Anything).Return(ports.RefPage{}, nil).Maybe()
	browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "proj"), mock.Anything).Return(domain.ResolvedRevision{CommitID: id}, nil).Once()
	nums := mocks.NewIssueNumberResolver(t)
	nums.EXPECT().Kind(mock.Anything, domain.Anonymous(), repoName(t, "proj"), int64(37)).Return(domain.NumberMergeRequest, nil).Once()
	f.srv.cfg.Browse = browse
	f.srv.cfg.IssueNumbers = nums
	f.published.Notes = full + " (!37) #37"
	w := f.request("GET", "/proj/releases/tag/stable/v1", "", "")
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `href="/proj/commit/`+full+`" class="ref">f25742a</a>`)
	assert.Contains(t, w.Body.String(), `href="/proj/merge-requests/37"`)
	assert.Contains(t, w.Body.String(), `>!37</a>`)
	assert.Contains(t, w.Body.String(), `>#37</a>`)
}

// Private-repository authorization is represented by the release port: the
// adapter must pass the authenticated principal on both metadata and bytes.
func TestReleaseDownloadCredentials(t *testing.T) {
	repo := repoName(t, "proj")
	reader := domain.TokenPrincipal(domain.Token{ID: "reader", Scopes: []domain.Scope{domain.ScopeRepoRead}, Repos: []domain.RepoName{repo}})
	job := domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "job", Repo: repo, Permissions: map[string]string{domain.PermContents: domain.PermissionRead}})
	for _, tc := range []struct {
		name, header, cookie string
		p                    domain.Principal
		authErr              error
		status               int
	}{
		{name: "bearer", header: "Bearer stored", p: reader, status: 200},
		{name: "basic", header: "Basic dXNlcjpzdG9yZWQ=", p: reader, status: 200},
		{name: "job", header: "Bearer job", p: job, status: 200},
		{name: "empty overrides cookie", cookie: "tok", authErr: domain.ErrUnauthenticated, status: 401},
		{name: "malformed", header: "Unsupported credentials", authErr: domain.ErrUnauthenticated, status: 401},
		{name: "invalid", header: "Bearer revoked", authErr: domain.ErrUnauthenticated, status: 401},
		{name: "invalid overrides cookie", header: "Bearer revoked", cookie: "tok", authErr: domain.ErrUnauthenticated, status: 401},
		{name: "header overrides cookie", header: "Bearer stored", cookie: "tok", p: reader, status: 200},
		{name: "anonymous", p: domain.Anonymous(), status: 404},
		{name: "cookie", cookie: "tok", p: domain.Owner(), status: 200},
		{name: "auth unavailable", header: "Bearer stored", authErr: errors.New("unavailable"), status: 500},
	} {
		for _, path := range []string{"/proj/releases/download/v1/build.zip", "/proj/releases/latest/download/build.zip", "/proj/releases/assets/7"} {
			t.Run(tc.name+path, func(t *testing.T) {
				auth := mocks.NewAuthService(t)
				svc := mocks.NewReleaseService(t)
				if tc.header != "" || tc.name == "empty overrides cookie" {
					secret := "stored"
					if tc.name == "job" {
						secret = "job"
					}
					if tc.authErr == domain.ErrUnauthenticated {
						secret = "revoked"
					}
					if tc.name == "empty overrides cookie" || tc.name == "malformed" {
						secret = ""
					}
					auth.EXPECT().Authenticate(mock.Anything, secret).Return(tc.p, tc.authErr).Once()
				}
				if tc.authErr == nil {
					ref := ports.AssetRef{Tag: "v1", Name: "build.zip"}
					if strings.Contains(path, "latest") {
						ref = ports.AssetRef{Latest: true, Name: "build.zip"}
					}
					if strings.Contains(path, "assets/7") {
						ref = ports.AssetRef{ID: 7}
					}
					a := domain.ReleaseAsset{ID: 7, Name: "build.zip", Size: 4, Digest: "sha256:abcd"}
					var getErr error
					if tc.status == 404 {
						getErr = domain.ErrNotFound
					}
					svc.EXPECT().GetAsset(mock.Anything, tc.p, repo, ref).Return(a, getErr).Once()
					if tc.status == 200 {
						svc.EXPECT().DownloadAsset(mock.Anything, tc.p, repo, ports.AssetRef{ID: 7}, int64(0), int64(4), mock.Anything).RunAndReturn(func(_ context.Context, _ domain.Principal, _ domain.RepoName, _ ports.AssetRef, _, _ int64, send func(ports.AssetChunk) error) error {
							if err := send(ports.AssetChunk{Asset: &a}); err != nil {
								return err
							}
							return send(ports.AssetChunk{Data: []byte("data")})
						}).Once()
					}
				}
				srv := newTestServer(t, Config{Auth: auth, Releases: svc, Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Sessions: newSessions(t, "tok"), Limiter: &blockingLimiter{}})
				r := httptest.NewRequest("GET", path, nil)
				if tc.header != "" || tc.name == "empty overrides cookie" {
					r.Header.Set("Authorization", tc.header)
				}
				if tc.cookie != "" {
					r.AddCookie(&http.Cookie{Name: CookieName, Value: tc.cookie})
				}
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, r)
				require.Equal(t, tc.status, w.Code, w.Body.String())
				if tc.status == 401 {
					assert.Equal(t, `Basic realm="ermite"`, w.Header().Get("WWW-Authenticate"))
				} else {
					assert.Empty(t, w.Header().Get("WWW-Authenticate"))
				}
				if tc.status == 200 {
					assert.Equal(t, "data", w.Body.String())
					assert.Empty(t, w.Header().Get("Location"))
				}
				if tc.header != "" || tc.name == "empty overrides cookie" {
					assert.Contains(t, w.Header().Get("Cache-Control"), "no-store")
				}
			})
		}
	}
}

func TestReleaseDownloadCredentialLimiter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocked bool
		status  int
		fails   int
	}{
		{name: "blocked client skips authentication", blocked: true, status: http.StatusTooManyRequests},
		{name: "bad credentials count as a failure", status: http.StatusUnauthorized, fails: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := mocks.NewAuthService(t)
			if !tc.blocked {
				auth.EXPECT().Authenticate(mock.Anything, "revoked").Return(domain.Principal{}, domain.ErrUnauthenticated).Once()
			}
			limiter := &blockingLimiter{blocked: tc.blocked}
			srv := newTestServer(t, Config{Auth: auth, Releases: mocks.NewReleaseService(t), Repos: reposServing(t, publicRepo(t, "proj")), Browse: mocks.NewBrowseService(t), Limiter: limiter})
			r := httptest.NewRequest("GET", "/proj/releases/download/v1/build.zip", nil)
			r.Header.Set("Authorization", "Bearer revoked")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.fails, limiter.fails)
		})
	}
}

func TestReleaseTokenDoesNotAuthorizeWebPage(t *testing.T) {
	repo := repoName(t, "proj")
	repos := mocks.NewRepoService(t)
	repos.EXPECT().Get(mock.Anything, domain.Anonymous(), repo).Return(domain.Repo{}, domain.ErrNotFound).Once()
	srv := newTestServer(t, Config{Auth: mocks.NewAuthService(t), Repos: repos, Browse: mocks.NewBrowseService(t), Releases: mocks.NewReleaseService(t)})
	r := httptest.NewRequest("GET", "/proj/releases", nil)
	r.Header.Set("Authorization", "Bearer stored")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
}
