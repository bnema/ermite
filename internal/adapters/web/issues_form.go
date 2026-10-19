// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The public issue form (ADR 0031). A visitor of a public repository whose
// issues mode is public writes an issue that awaits the owner's moderation.
//
//	GET  /<repo>/issues/new         the form, with a proof of work solved by script
//	POST /<repo>/issues/new         the submission (the one write a visitor may make)
//	GET  /<repo>/issues/submitted   the thanks page
//
// Every answer is "Cache-Control: no-store". The challenge token is never
// echoed back, and neither the address of the client nor the text of the
// submission is logged. A refusal for a bot-related reason is one generic page.

// maxIssueFormBytes bounds the body of the submission: 10,000 characters are
// at most 40,000 bytes, plus the title, the name and the tokens.
const maxIssueFormBytes = 64 << 10

// The fields of the form, and so the only ones read.
const (
	fieldTitle     = "title"
	fieldBody      = "body"
	fieldAuthor    = "author"
	fieldToken     = "token"
	fieldFormToken = "form_token"
	fieldSolution  = "solution"
	fieldHoneypot  = "website"
)

var issueFormFields = []string{fieldTitle, fieldBody, fieldAuthor, fieldToken, fieldFormToken, fieldSolution, fieldHoneypot}

// IssueFormPage is the public form.
type IssueFormPage struct {
	Action, Cancel string
	Token          string
	FormToken      string
	Nonce          string
	Difficulty     int
	MinSubmitAt    int64 // unix milliseconds
	Worker         string
	// Title, Body and Author are what the visitor already wrote.
	Title, Body, Author string
	// ErrorField is "title", "body" or "author" and Error the message to show
	// next to it; both empty without an error.
	ErrorField, Error string
	// Max lengths, in characters.
	MaxTitle, MaxBody, MaxAuthor int
}

// issueNewRoute is GET /<repo>/issues/new: the owner's form when signed in,
// else the public form, else the page that is not there.
func (s *Server) issueNewRoute(w http.ResponseWriter, r *http.Request) {
	if SignedIn(r.Context()) || s.cfg.IssueForm == nil {
		s.page(s.issueNew)(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	page := IssueFormPage{}
	status, err := s.fillIssueForm(ctx, r, name, &page)
	if err != nil {
		s.formFailed(ctx, w, r, err, page.Cancel)
		return
	}
	s.writeIssueForm(ctx, w, r, status, name, page)
}

// fillIssueForm asks a challenge for the client and fills what the form needs
// from it, leaving the text of the visitor as it is.
func (s *Server) fillIssueForm(ctx context.Context, r *http.Request, name domain.RepoName, p *IssueFormPage) (int, error) {
	base := issuesBase(name.String())
	p.Cancel = base
	ch, err := s.cfg.IssueForm.Challenge(ctx, name, s.formClient(r))
	if err != nil {
		return 0, err
	}
	p.Action = base + "/new"
	p.Token, p.FormToken, p.Nonce, p.Difficulty = ch.Token, ch.FormToken, ch.Nonce, ch.Difficulty
	p.MinSubmitAt = ch.MinSubmitAt.UnixMilli()
	p.Worker = s.assets.urls.PowWorker
	p.MaxTitle, p.MaxBody, p.MaxAuthor = domain.MaxIssueTitleLen, domain.MaxIssueBodyLen, domain.MaxAuthorLabelLen
	return http.StatusOK, nil
}

// formClient is the key of the client for the limits of the submissions.
func (s *Server) formClient(r *http.Request) string {
	return clientlimit.Key(clientlimit.ClientAddr(r, s.cfg.TrustedProxies))
}

// writeIssueForm renders the form in the layout of the issues of the repository.
func (s *Server) writeIssueForm(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, name domain.RepoName, p IssueFormPage) {
	d, err := s.issueRepo(ctx, name.String())
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	l := s.repoLayout(ctx, d, "issues")
	l.Title = "new issue · " + d.name() + " · " + s.instance
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "issues", Href: issuesBase(d.name())},
		Crumb{Label: "new", Current: true},
	)
	l.HasRail = true
	l.IssueForm = true
	w.Header().Set("Cache-Control", noStore)
	s.write(ctx, w, r, status, IssueForm(l, p))
}

// formFailed answers an error of the challenge or of the submission.
func (s *Server) formFailed(ctx context.Context, w http.ResponseWriter, r *http.Request, err error, back string) {
	var retry *domain.SubmissionRetryError
	switch {
	case errors.Is(err, context.Canceled):
	case errors.Is(err, domain.ErrNotFound), domain.IsInvalid(err) && !errors.Is(err, domain.ErrInvalidText):
		// The same page, with the same headers, as every other page that is
		// not there (a POST has set no-store already).
		s.fail(ctx, w, r, domain.ErrNotFound)
	case errors.As(err, &retry):
		secs := int(retry.After.Round(time.Second).Seconds())
		if retry.After > time.Duration(secs)*time.Second {
			secs++
		}
		secs = max(secs, 1)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		mins := (secs + 59) / 60
		unit := " minutes."
		if mins == 1 {
			unit = " minute."
		}
		s.refusal(ctx, w, r, http.StatusTooManyRequests, "too many submissions",
			"Too many submissions from your network, try again in "+strconv.Itoa(mins)+unit, back)
	case errors.Is(err, domain.ErrSubmissionsPaused):
		s.refusal(ctx, w, r, http.StatusServiceUnavailable, "submissions paused", "Submissions are paused, try later.", back)
	case errors.Is(err, domain.ErrSubmissionRefused):
		s.refusal(ctx, w, r, http.StatusBadRequest, "not verified",
			"The submission could not be verified. Reload the page and try again.", back)
	default:
		s.internalFailure(ctx, w, r, err)
	}
}

// refusal writes a state page for a visitor, never cached.
func (s *Server) refusal(ctx context.Context, w http.ResponseWriter, r *http.Request, status int, title, line, back string) {
	if back == "" {
		back = "/"
	}
	l := s.stateLayout(title, []Crumb{{Label: title, Current: true}})
	w.Header().Set("Cache-Control", noStore)
	s.write(ctx, w, r, status, State(l, StatePage{
		Code: itoa(status), Title: title, Lines: []string{line},
		Back: []Link{{Label: "back", Href: back}, {Label: "home", Href: "/"}},
	}))
}

// issueFormPost is POST /<repo>/issues/new.
func (s *Server) issueFormPost(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	w.Header().Set("Cache-Control", noStore)
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		s.formFailed(ctx, w, r, err, "")
		return
	}
	base := issuesBase(name.String())
	r.Body = http.MaxBytesReader(w, r.Body, maxIssueFormBytes)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.refusal(ctx, w, r, http.StatusRequestEntityTooLarge, "form too large", "The form is larger than an issue can be.", base+"/new")
			return
		}
		s.formFailed(ctx, w, r, domain.ErrSubmissionInvalid, base+"/new")
		return
	}
	for _, f := range issueFormFields {
		if len(r.PostForm[f]) > 1 {
			s.formFailed(ctx, w, r, domain.ErrSubmissionInvalid, base+"/new")
			return
		}
	}
	in := ports.IssueSubmission{
		Token: r.PostForm.Get(fieldToken), Solution: r.PostForm.Get(fieldSolution),
		FormToken: r.PostForm.Get(fieldFormToken), Honeypot: r.PostForm.Get(fieldHoneypot),
		Title: r.PostForm.Get(fieldTitle), Body: normalizeNewlines(r.PostForm.Get(fieldBody)),
		Author: r.PostForm.Get(fieldAuthor),
	}
	err = s.cfg.IssueForm.Submit(ctx, name, s.formClient(r), in)
	if err == nil {
		seeOther(w, r, base+"/submitted")
		return
	}
	var te *domain.TextError
	if errors.As(err, &te) {
		s.formAgain(ctx, w, r, name, in, err, te)
		return
	}
	s.formFailed(ctx, w, r, err, base+"/new")
}

// formAgain shows the form again after a text the visitor can fix: a new
// challenge (the first one is spent), the text kept, the message at its field.
func (s *Server) formAgain(ctx context.Context, w http.ResponseWriter, r *http.Request, name domain.RepoName, in ports.IssueSubmission, err error, te *domain.TextError) {
	p := IssueFormPage{Title: in.Title, Body: in.Body, Author: in.Author}
	field, _, _ := strings.Cut(err.Error(), ": ")
	switch field {
	case fieldTitle, fieldBody, fieldAuthor:
	default:
		field = fieldBody
	}
	label := field
	if field == fieldAuthor {
		label = "name"
	}
	p.ErrorField = field
	p.Error = "The " + label + " " + visibleText(te.Reason) + "."
	if _, cerr := s.fillIssueForm(ctx, r, name, &p); cerr != nil {
		s.formFailed(ctx, w, r, cerr, p.Cancel)
		return
	}
	s.writeIssueForm(ctx, w, r, http.StatusUnprocessableEntity, name, p)
}

// issueSubmittedPage is GET /<repo>/issues/submitted.
//
//hexcheck:ignore suspicious-business-logic-in-adapter maps domain RepoSettings.ShowsIssueSubmissionReceipt to a 404; decides nothing itself
func (s *Server) issueSubmittedPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	d, err := s.issueRepo(ctx, r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	if !d.repo.Settings.ShowsIssueSubmissionReceipt() {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	w.Header().Set("Cache-Control", noStore)
	s.write(ctx, w, r, http.StatusOK, s.submittedPage(d.name()))
}

func (s *Server) submittedPage(repo string) templ.Component {
	l := s.stateLayout("thanks", []Crumb{{Label: repo, Href: repoHref(repo)}, {Label: "thanks", Current: true}})
	return State(l, StatePage{
		Code: "ok", Title: "thanks",
		Lines: []string{"Thanks, your issue is waiting for moderation."},
		Back:  []Link{{Label: "issues", Href: issuesBase(repo)}, {Label: "home", Href: "/"}},
	})
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// runeCount is the length of s in characters, like the limits of the form.
func runeCount(s string) int { return utf8.RuneCountInString(s) }
