// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The artifacts of a run (CI-4 D3), read through ports.ArtifactService. Only a
// signed-in caller may list or download them, even on a public repository.
//
//	GET  /<repo>/actions/runs/<id>/artifacts/<artifact id>          the content
//	POST /<repo>/actions/runs/<id>/artifacts/<artifact id>/delete   (the owner)

// artifactTimeout bounds the download of one artifact.
const artifactTimeout = 30 * time.Minute

func artifactHref(repo string, runID, id int64) string {
	return runHref(repo, runID) + "/artifacts/" + strconv.FormatInt(id, 10)
}

// ArtifactRow is an artifact of the run page.
type ArtifactRow struct {
	Name, Href, Size string
	// Expires is the expiry for the eye, ExpiresAt as RFC 3339.
	Expires, ExpiresAt string
	// Job is the job that uploaded it, JobHref its page ("" when it is not a
	// job of the attempt shown).
	Job, JobHref string
	// DeleteAction is the form that deletes it; "" when the visitor cannot.
	DeleteAction string
}

// ArtifactsView is the artifacts section of the run page.
type ArtifactsView struct {
	Rows []ArtifactRow
	// Usage is the space the artifacts of the repository use.
	Usage string
}

// runArtifacts is the artifacts section of a run, nil when there is nothing to
// show: no artifacts on this server, a visitor who cannot list them, no
// artifact, or a failed read, which is logged; the run page never fails for it.
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds the artifacts section from the use case results; which errors hide it is presentation, no rule is decided here
func (s *Server) runArtifacts(ctx context.Context, repo domain.RepoName, p RunPage) *ArtifactsView {
	if s.cfg.Artifacts == nil || !SignedIn(ctx) {
		return nil
	}
	runID := p.detail.Run.ID
	list, err := s.cfg.Artifacts.ListArtifacts(ctx, principal(ctx), repo, runID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrUnauthenticated) &&
			!errors.Is(err, domain.ErrForbidden) && !errors.Is(err, context.Canceled) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", repo.String()).Int64("run", runID).Msg("list the artifacts of the run")
		}
		return nil
	}
	if len(list.Artifacts) == 0 {
		return nil
	}
	jobs := map[string]string{}
	for _, j := range p.detail.Jobs {
		jobs[j.ID] = j.Name
	}
	name := repo.String()
	v := &ArtifactsView{Usage: humanSize(list.Used) + " used"}
	if list.Quota > 0 {
		v.Usage = humanSize(list.Used) + " of " + humanSize(list.Quota) + " used"
	}
	for _, a := range list.Artifacts {
		row := ArtifactRow{
			Name: visibleText(a.Name), Href: artifactHref(name, runID, a.ID), Size: humanSize(a.Size),
			Expires: a.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), ExpiresAt: rfc3339(a.ExpiresAt),
			Job: visibleText(a.JobID),
		}
		if a.ExpiresAt.IsZero() {
			row.Expires = "never"
		}
		if n, ok := jobs[a.JobID]; ok {
			row.Job, row.JobHref = visibleText(n), jobHref(name, runID, a.JobID)
		}
		row.DeleteAction = artifactHref(name, runID, a.ID) + "/delete"
		v.Rows = append(v.Rows, row)
	}
	return v
}

func parseArtifactPath(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("artifact"), 10, 64)
	if err != nil || id < 1 {
		return 0, errNotFound
	}
	return id, nil
}

// artifactDownload is GET /<repo>/actions/runs/<id>/artifacts/<artifact id>:
// it streams the content as an attachment. The media type the uploader chose
// is never served: a zip is application/zip, anything else
// application/octet-stream, and the answer is sandboxed and not sniffed, so
// that nothing uploaded by a job can run in the origin of the forge.
//
//hexcheck:ignore suspicious-business-logic-in-adapter streams what the use case sends; the headers are about the response, no rule is decided here
func (s *Server) artifactDownload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), artifactTimeout)
	defer cancel()
	repo, runID, err := s.runTarget(r)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	id, err := parseArtifactPath(r)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	var started, headOnly bool
	var writeErr error
	err = s.cfg.Artifacts.DownloadArtifact(ctx, principal(ctx), repo, id, 0, func(c ports.ArtifactChunk) error {
		if !started {
			if c.Artifact == nil {
				return errors.New("web: the download did not start with the artifact")
			}
			if c.Artifact.RunID != runID {
				return errNotFound // an artifact of another run of the repository
			}
			h := w.Header()
			artifactHeaders(h, *c.Artifact)
			cacheHeaders(ctx, h, noStore, sessionCache)
			w.WriteHeader(http.StatusOK)
			started = true
			if r.Method == http.MethodHead {
				headOnly = true
				return errStopDownload
			}
		}
		if len(c.Data) == 0 {
			return nil
		}
		if _, werr := w.Write(c.Data); werr != nil {
			writeErr = werr
			return werr
		}
		return nil
	})
	switch {
	case err == nil, headOnly, writeErr != nil:
		// Done, or the visitor is gone.
	case !started:
		s.artifactFailed(ctx, w, r, err, runHref(repo.String(), runID))
	default:
		// The headers are gone: cut the answer short, so that the client
		// sees it is incomplete.
		log := zerowrap.FromCtx(ctx)
		log.Error().Err(err).Str("repo", repo.String()).Int64("artifact", id).Msg("artifact download failed")
		panic(http.ErrAbortHandler)
	}
}

// errStopDownload ends the download of a HEAD request after the headers.
var errStopDownload = errors.New("web: download stopped")

// artifactFailed answers a download refused before any byte was sent.
func (s *Server) artifactFailed(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, back string) {
	if errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrForbidden) {
		s.reject(ctx, w, r, http.StatusForbidden, "forbidden", "sign in to download the artifacts of a run.", back)
		return
	}
	s.fail(ctx, w, r, err)
}

// artifactHeaders are the headers of the content of a.
func artifactHeaders(h http.Header, a domain.CIArtifact) {
	name := attachmentName(a.Name)
	typ := "application/octet-stream"
	if a.MediaType == domain.CIArtifactZip {
		typ = domain.CIArtifactZip
		name += ".zip"
	}
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", contentDisposition(name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", rawCSP)
	h.Set("Content-Length", strconv.FormatInt(a.Size, 10))
}

// attachmentName is an artifact name fit for a file name: without control
// characters, quotes, backslashes or slashes (which the domain already
// refuses), "artifact" when nothing is left.
func attachmentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == '\\' || r == '/' || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "artifact"
	}
	return name
}

// contentDisposition is an attachment named name: a quoted ASCII filename,
// and for a name that is not ASCII its RFC 5987 form as well, with the ASCII
// one as the fallback (non-ASCII characters replaced by '_').
func contentDisposition(name string) string {
	ascii := true
	fallback := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			ascii = false
			return '_'
		}
		return r
	}, name)
	cd := mime.FormatMediaType("attachment", map[string]string{"filename": fallback})
	if cd == "" {
		cd = "attachment"
	}
	if !ascii {
		cd += "; filename*=UTF-8''" + rfc5987(name)
	}
	return cd
}

// rfc5987 percent-encodes s but for the attr-char of RFC 5987.
func rfc5987(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// artifactDelete is POST /<repo>/actions/runs/<id>/artifacts/<artifact id>/delete.
func (s *Server) artifactDelete(ctx context.Context, r *http.Request) (string, error) {
	repo, runID, err := s.runTarget(r)
	if err != nil {
		return "", err
	}
	id, err := parseArtifactPath(r)
	if err != nil {
		return "", err
	}
	if err := s.cfg.Artifacts.DeleteArtifact(ctx, principal(ctx), repo, id); err != nil {
		return "", err
	}
	return runHref(repo.String(), runID), nil
}
