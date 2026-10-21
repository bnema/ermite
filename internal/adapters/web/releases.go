// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/httprange"
	"bnema.dev/ermite/internal/ports"
)

func releasesBase(repo string) string     { return repoHref(repo, "releases") }
func releaseHref(repo, tag string) string { return repoHref(repo, "releases", "tag", tag) }
func releaseAssetHref(repo string, id int64) string {
	return releasesBase(repo) + "/assets/" + strconv.FormatInt(id, 10)
}

// ReleaseRow describes a release without mistaking MakeLatest (a pin) for
// the latest release selected by the service.
type ReleaseRow struct {
	Tag, Title, Href, Published string
	Draft, Prerelease, Latest   bool
	AssetCount                  int
}
type ReleaseListPage struct {
	Rows  []ReleaseRow
	Older string
}
type ReleaseAssetRow struct{ Name, Href, Size, ContentType string }
type ReleaseDetailPage struct {
	ReleaseRow
	Notes                       template.HTML
	SHA, CommitHref, SourceHref string
	Assets                      []ReleaseAssetRow
}

func (s *Server) latestReleaseID(ctx context.Context, repo domain.RepoName) (int64, error) {
	rel, err := s.cfg.Releases.GetRelease(ctx, principal(ctx), repo, ports.ReleaseRef{Latest: true})
	if errors.Is(err, domain.ErrNotFound) {
		return 0, nil
	}
	return rel.ID, err
}
func releaseRow(repo string, rel domain.Release, latest int64) ReleaseRow {
	published := "draft"
	if !rel.Draft && !rel.PublishedAt.IsZero() {
		published = rel.PublishedAt.UTC().Format("2006-01-02")
	}
	title := rel.Title
	if title == "" {
		title = rel.Tag
	}
	return ReleaseRow{Tag: rel.Tag, Title: title, Href: releaseHref(repo, rel.Tag), Published: published, Draft: rel.Draft, Prerelease: rel.Prerelease, Latest: rel.ID == latest}
}
func (s *Server) releaseList(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	page, err := s.cfg.Releases.ListReleases(ctx, principal(ctx), d.repo.Name, 30, r.URL.Query().Get("page"))
	if err != nil {
		return nil, 0, err
	}
	latest, err := s.latestReleaseID(ctx, d.repo.Name)
	if err != nil {
		return nil, 0, err
	}
	p := ReleaseListPage{}
	for _, rel := range page.Releases {
		row := releaseRow(d.name(), rel, latest)
		assets, err := s.cfg.Releases.ListAssets(ctx, principal(ctx), d.repo.Name, rel.ID)
		if err != nil {
			return nil, 0, err
		}
		row.AssetCount = len(assets)
		p.Rows = append(p.Rows, row)
	}
	if page.NextPageToken != "" {
		p.Older = releasesBase(d.name()) + "?page=" + url.QueryEscape(page.NextPageToken)
	}
	l := s.repoLayout(ctx, d, "releases")
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=releases")
	l.Title = "Releases · " + d.name()
	l.Crumbs = append(l.Crumbs, Crumb{Label: "releases", Current: true})
	return Releases(l, p), http.StatusOK, nil
}
func (s *Server) releaseShow(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	rel, err := s.cfg.Releases.GetRelease(ctx, principal(ctx), d.repo.Name, ports.ReleaseRef{Tag: r.PathValue("tag")})
	if err != nil {
		return nil, 0, err
	}
	latest, err := s.latestReleaseID(ctx, d.repo.Name)
	if err != nil {
		return nil, 0, err
	}
	assets, err := s.cfg.Releases.ListAssets(ctx, principal(ctx), d.repo.Name, rel.ID)
	if err != nil {
		return nil, 0, err
	}
	notes, _, err := s.md.render([]byte(rel.Notes), d.name(), rel.Tag, "")
	if err != nil {
		return nil, 0, err
	}
	p := ReleaseDetailPage{ReleaseRow: releaseRow(d.name(), rel, latest), Notes: notes, SourceHref: repoHref(d.name(), "tree", rel.Tag)}
	if rel.TargetCommitish != "" {
		p.SHA = short(rel.TargetCommitish)
		p.CommitHref = repoHref(d.name(), "commit", rel.TargetCommitish)
	}
	for _, a := range assets {
		p.Assets = append(p.Assets, ReleaseAssetRow{Name: a.Name, Href: releaseAssetHref(d.name(), a.ID), Size: humanSize(a.Size), ContentType: a.ContentType})
	}
	l := s.repoLayout(ctx, d, "releases")
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=release")
	l.Title = rel.Tag + " · " + d.name()
	l.Crumbs = append(l.Crumbs, Crumb{Label: "releases", Href: releasesBase(d.name())}, Crumb{Label: rel.Tag, Current: true})
	return ReleaseDetail(l, p), http.StatusOK, nil
}
func (s *Server) releaseLatest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	repo, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	rel, err := s.cfg.Releases.GetRelease(ctx, principal(ctx), repo, ports.ReleaseRef{Latest: true})
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	cacheHeaders(ctx, w.Header(), "no-cache", sessionCache)
	http.Redirect(w, r, releaseHref(repo.String(), rel.Tag), http.StatusSeeOther)
}

func releaseHeaders(h http.Header, a domain.ReleaseAsset) {
	// Reuse the artifact sandbox and attachment policy; never trust an
	// uploader's media type to execute content in the forge's origin.
	typ := "application/octet-stream"
	if a.ContentType == domain.CIArtifactZip {
		typ = domain.CIArtifactZip
	}
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", contentDisposition(attachmentName(a.Name)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", rawCSP)
	h.Set("Content-Length", strconv.FormatInt(a.Size, 10))
	h.Set("ETag", `"`+strings.Replace(a.Digest, "sha256:", "sha256-", 1)+`"`)
	h.Set("Accept-Ranges", "bytes")
}
func (s *Server) releaseDownload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Minute)
	defer cancel()
	repo, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	ref := ports.AssetRef{Tag: r.PathValue("tag"), Name: r.PathValue("asset")}
	if id := r.PathValue("id"); id != "" {
		ref.ID, err = strconv.ParseInt(id, 10, 64)
		if err != nil || ref.ID < 1 {
			s.fail(ctx, w, r, errNotFound)
			return
		}
	} else if ref.Tag == "" {
		ref.Latest = true
	}
	a, err := s.cfg.Releases.GetAsset(ctx, principal(ctx), repo, ref)
	if err != nil {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	// Resolve latest/tag only once: a moving latest cannot select another
	// asset between reading the headers and streaming the bytes.
	ref = ports.AssetRef{ID: a.ID}
	h := w.Header()
	releaseHeaders(h, a)
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	offset, length, partial, err := httprange.Select(r, a.Size, h.Get("ETag"))
	if err != nil {
		h.Set("Content-Range", httprange.Unsatisfied(a.Size))
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", httprange.ContentRange(offset, length, a.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	started := false
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	err = s.cfg.Releases.DownloadAsset(ctx, principal(ctx), repo, ref, offset, length, func(c ports.AssetChunk) error {
		if !started {
			if c.Asset == nil || c.Asset.ID != a.ID || c.Asset.Digest != a.Digest || c.Asset.Size != a.Size {
				return errors.New("web: release asset changed before download")
			}
			_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
			w.WriteHeader(status)
			started = true
		}
		if len(c.Data) == 0 {
			return nil
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
		_, err := w.Write(c.Data)
		return err
	})
	if err == nil {
		return
	}
	if !started {
		// Do not leave binary headers or range metadata on the HTML error page.
		for _, key := range []string{"Content-Length", "Content-Disposition", "Content-Range", "Content-Security-Policy", "ETag", "Accept-Ranges"} {
			h.Del(key)
		}
		s.fail(ctx, w, r, errNotFound)
		return
	}
	log := zerowrap.FromCtx(ctx)
	log.Error().Err(err).Str("repo", repo.String()).Int64("asset", a.ID).Msg("release download failed")
	panic(http.ErrAbortHandler)
}
