// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The CI pages (ADR 0025): the runs of a repository, one run with its jobs,
// one job with its steps and its log. They read through ports.RunService and
// follow the events RunUpdated and JobUpdated of the repository topic.
//
//	GET  /<repo>/actions                          the runs, newest first
//	GET  /<repo>/actions/runs/<id>                one run: jobs and steps
//	GET  /<repo>/actions/runs/<id>/job/<job id>   one job: steps and live log
//	GET  /<repo>/actions/runs/<id>/job/<job id>/raw   the log as text
//	POST /<repo>/actions/runs/<id>/cancel | rerun | rerun-failed   (the owner)

const (
	// actionsPageSize is the number of runs one page lists.
	actionsPageSize = 30
	// jobLogPageLines is the number of log lines a job page shows at once; the
	// rest is reached with ?from=.
	jobLogPageLines = 2000
	// rawLogMaxBytes bounds the raw log the server writes to one response.
	rawLogMaxBytes = 64 << 20
)

func actionsBase(repo string) string { return repoHref(repo, "actions") }
func runHref(repo string, id int64) string {
	return repoHref(repo, "actions", "runs", strconv.FormatInt(id, 10))
}
func jobHref(repo string, runID int64, jobID string) string {
	return runHref(repo, runID) + "/job/" + jobID
}

// ciState is the one word shown for a status and a conclusion: what is going
// on, or how it ended.
func ciState(status domain.CIStatus, c domain.CIConclusion) string {
	switch status {
	case domain.CIQueued:
		return "queued"
	case domain.CIInProgress:
		return "running"
	}
	switch c {
	case domain.CISuccess:
		return "success"
	case domain.CIFailure:
		return "failure"
	case domain.CICancelled:
		return "cancelled"
	case domain.CISkipped:
		return "skipped"
	case domain.CITimedOut:
		return "timed out"
	case domain.CINeutral:
		return "neutral"
	case domain.CIActionRequired:
		return "action required"
	}
	return "done"
}

// ciKind groups the states into the few that have an icon and a colour.
func ciKind(status domain.CIStatus, c domain.CIConclusion) string {
	switch {
	case status == domain.CIQueued:
		return "queued"
	case status == domain.CIInProgress:
		return "running"
	case c == domain.CISuccess, c == domain.CINeutral:
		return "ok"
	case c == domain.CISkipped:
		return "skipped"
	case c == domain.CICancelled:
		return "cancelled"
	}
	return "bad"
}

// duration is a short elapsed time between two instants; empty when the
// start is unknown, running up to now when the end is not.
func duration(start, end, now time.Time) string {
	if start.IsZero() {
		return ""
	}
	if end.IsZero() {
		end = now
	}
	d := end.Sub(start).Round(time.Second)
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// RunRow is a run of the list.
type RunRow struct {
	ID       string
	Href     string
	Kind     string // ciKind
	State    string // ciState
	Workflow string
	Title    string
	Ref      string
	RefHref  string
	Short    string
	CHref    string
	Actor    string
	When     string
	WhenAt   string
	Duration string
	Number   string
	Attempt  int32
}

// ActionsPage lists the runs of a repository.
type ActionsPage struct {
	Runs []RunRow
	// More is the link to the next page, "" on the last.
	More string
	// NoRunner says that CI has no runner to run on yet.
	NoRunner bool
}

// StepView is a step of a job.
type StepView struct {
	Number      int32
	Name        string
	Kind, State string
	Duration    string
	// LogHref jumps to the lines of the step in the job log.
	LogStart, LogEnd int64
}

// JobRow is a job of a run.
type JobRow struct {
	ID, Name, Href string
	Kind, State    string
	Runner         string
	Isolation      string
	Duration       string
	Error          string
	Needs          []string
	Steps          []StepView
	Attempt        int32
	// Waiting says that the job is queued for a runner that no registered
	// runner can be.
	Waiting bool
}

// RunPage is one run with its jobs.
type RunPage struct {
	RunRow
	Sha, Error string
	Attempt    int32
	Latest     int32
	Attempts   []AttemptLink
	Jobs       []JobRow
	Active     bool
	// Forms are the owner's actions; nil/empty when the visitor cannot.
	CancelAction, RerunAction, RerunFailedAction string
	CanRerunFailed                               bool
}

// AttemptLink switches between the attempts of a run.
type AttemptLink struct {
	Label, Href string
	Current     bool
}

// LogLineView is a line of a job log, as shown.
type LogLineView struct {
	N    int64
	Step int32
	Text string
	Time string
	// Group marks "##[group]" and "##[endgroup]" lines and Error the
	// "##[error]" ones; the text is shown without the marker.
	Group, End, Error bool
}

// JobPage is one job with its steps and its log.
type JobPage struct {
	JobRow
	RunHref string
	RunName string
	Run     string
	Steps   []StepView
	Lines   []LogLineView
	// From is the first line shown; Next is the link to the lines after the
	// last shown, "" when it is the end; Follow says the log is still being
	// written, so the page follows it live.
	From    int64
	Next    string
	Prev    string
	Total   int64
	Follow  bool
	RawHref string
	// StreamHref is the live feed of the lines (logStream).
	StreamHref string
	Note       string
}

// --- handlers ---

// actions is GET /<repo>/actions.
func (s *Server) actions(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	token := r.URL.Query().Get("page")
	page, err := s.cfg.Runs.ListRuns(ctx, principal(ctx), d.repo.Name, ports.ListRunsParams{PageSize: actionsPageSize, PageToken: token})
	if err != nil {
		return nil, 0, err
	}
	now := s.cfg.Now()
	p := ActionsPage{}
	for _, run := range page.Runs {
		p.Runs = append(p.Runs, s.runRow(d.name(), run, now))
	}
	if page.NextPageToken != "" {
		p.More = actionsBase(d.name()) + "?page=" + page.NextPageToken
	}
	l := s.repoLayout(ctx, d, "actions")
	l.Title = "actions · " + d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=actions")
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "actions", Current: true})
	return Actions(l, p), http.StatusOK, nil
}

func (s *Server) runRow(repo string, run domain.CIRun, now time.Time) RunRow {
	row := RunRow{
		ID: strconv.FormatInt(run.ID, 10), Href: runHref(repo, run.ID),
		Kind: ciKind(run.Status, run.Conclusion), State: ciState(run.Status, run.Conclusion),
		Workflow: visibleText(run.WorkflowName), Title: visibleText(run.Title),
		Ref:   visibleText(strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/")),
		Short: short(run.Sha), CHref: repoHref(repo, "commit", run.Sha), Actor: visibleText(run.Actor),
		Duration: duration(run.StartedAt, run.CompletedAt, now), Number: "#" + strconv.FormatInt(run.Number, 10), Attempt: run.Attempt,
	}
	row.RefHref = repoHref(repo, "tree", strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/"))
	row.When, row.WhenAt = whenOf(now, run.CreatedAt)
	return row
}

func stepViews(j ports.JobView, now time.Time) []StepView {
	ranges := j.StepRanges()
	out := make([]StepView, 0, len(j.Steps))
	for i, st := range j.Steps {
		status := domain.CIQueued
		switch st.State {
		case domain.CIStepInProgress:
			status = domain.CIInProgress
		case domain.CIStepCompleted:
			status = domain.CICompleted
		}
		out = append(out, StepView{
			Number: st.Number, Name: visibleText(st.Name), Kind: ciKind(status, st.Conclusion), State: ciState(status, st.Conclusion),
			Duration: duration(st.StartedAt, st.CompletedAt, now), LogStart: ranges[i].Start, LogEnd: ranges[i].End,
		})
	}
	return out
}

func (s *Server) jobRow(repo string, j ports.JobView, now time.Time) JobRow {
	return JobRow{
		ID: j.ID, Name: visibleText(j.Name), Href: jobHref(repo, j.RunID, j.ID), Kind: ciKind(j.Status(), j.Conclusion),
		State: ciState(j.Status(), j.Conclusion), Runner: visibleText(j.RunnerName), Isolation: string(j.Isolation),
		Duration: duration(j.StartedAt, j.CompletedAt, now), Error: visibleText(j.Error), Needs: j.Needs,
		Steps: stepViews(j, now), Attempt: j.Attempt,
	}
}

func parseRunPath(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errNotFound
	}
	return id, nil
}

// run is GET /<repo>/actions/runs/<id>.
func (s *Server) run(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	id, err := parseRunPath(r)
	if err != nil {
		return nil, 0, err
	}
	var attempt int32
	if a := r.URL.Query().Get("attempt"); a != "" {
		n, perr := strconv.ParseInt(a, 10, 32)
		if perr != nil || n < 1 {
			return nil, 0, errNotFound
		}
		attempt = int32(n)
	}
	p, err := s.runPage(ctx, d, id, attempt)
	if err != nil {
		return nil, 0, err
	}
	l := s.repoLayout(ctx, d, "actions")
	l.Title = "run " + p.ID + " · " + d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=run&n="+p.ID)
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "actions", Href: actionsBase(d.name())}, Crumb{Label: p.Workflow + " " + p.Number, Current: true})
	l.HasRail = true
	return RunDetail(l, p), http.StatusOK, nil
}

//hexcheck:ignore suspicious-business-logic-in-adapter builds the run page from the use case results; no rule is decided here
func (s *Server) runPage(ctx context.Context, d *repoData, id int64, attempt int32) (RunPage, error) {
	detail, err := s.cfg.Runs.GetRun(ctx, principal(ctx), d.repo.Name, id, attempt)
	if err != nil {
		return RunPage{}, err
	}
	now := s.cfg.Now()
	run := detail.Run
	p := RunPage{
		RunRow: s.runRow(d.name(), run, now), Sha: run.Sha, Error: visibleText(run.Error),
		Attempt: run.Attempt, Latest: detail.LatestAttempt, Active: run.Status != domain.CICompleted,
	}
	for a := detail.LatestAttempt; a >= 1 && detail.LatestAttempt > 1; a-- {
		p.Attempts = append(p.Attempts, AttemptLink{
			Label: "attempt " + strconv.Itoa(int(a)), Href: runHref(d.name(), id) + "?attempt=" + strconv.Itoa(int(a)), Current: a == run.Attempt,
		})
	}
	for _, j := range detail.Jobs {
		row := s.jobRow(d.name(), j, now)
		if run.Attempt == detail.LatestAttempt {
			// An earlier attempt keeps the job as it ran then.
			row.Href = jobHref(d.name(), id, j.ID)
		}
		p.Jobs = append(p.Jobs, row)
	}
	if SignedIn(ctx) && run.Attempt == detail.LatestAttempt {
		base := runHref(d.name(), id)
		if p.Active {
			p.CancelAction = base + "/cancel"
		} else if len(detail.Jobs) > 0 {
			p.RerunAction = base + "/rerun"
			for _, j := range detail.Jobs {
				if j.Conclusion.IsFailure() || j.Conclusion == domain.CICancelled {
					p.RerunFailedAction, p.CanRerunFailed = base+"/rerun-failed", true
					break
				}
			}
		}
	}
	return p, nil
}

// job is GET /<repo>/actions/runs/<id>/job/<job id>.
func (s *Server) job(ctx context.Context, r *http.Request) (templ.Component, int, error) {
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		return nil, 0, err
	}
	id, err := parseRunPath(r)
	if err != nil {
		return nil, 0, err
	}
	jobID := r.PathValue("job")
	if runOf, _, _, ok := domain.ParseJobID(jobID); !ok || runOf != id {
		return nil, 0, errNotFound
	}
	var attempt int32
	if a := r.URL.Query().Get("attempt"); a != "" {
		n, perr := strconv.ParseInt(a, 10, 32)
		if perr != nil || n < 1 {
			return nil, 0, errNotFound
		}
		attempt = int32(n)
	}
	view, err := s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, jobID, attempt)
	if err != nil {
		return nil, 0, err
	}
	run, err := s.cfg.Runs.GetRun(ctx, principal(ctx), d.repo.Name, id, 0)
	if err != nil {
		return nil, 0, err
	}
	now := s.cfg.Now()
	var from int64
	if f := r.URL.Query().Get("from"); f != "" {
		from, err = strconv.ParseInt(f, 10, 64)
		if err != nil || from < 0 {
			return nil, 0, errNotFound
		}
	}
	p := JobPage{
		JobRow: s.jobRow(d.name(), view, now), RunHref: runHref(d.name(), id), RunName: visibleText(run.Run.WorkflowName),
		Run: strconv.FormatInt(id, 10), Steps: stepViews(view, now), From: from, Total: view.Lines,
		RawHref: jobHref(d.name(), id, jobID) + "/raw",
	}
	if attempt > 0 {
		p.RawHref += "?attempt=" + strconv.Itoa(int(attempt))
	}
	p.Follow = !view.Ended()
	p.StreamHref = jobHref(d.name(), id, jobID) + "/stream"
	lines, next, err := s.readLog(ctx, s.logClient(r), d.repo.Name, view, from, jobLogPageLines)
	if err != nil {
		return nil, 0, err
	}
	p.Lines = lines
	if next < view.Lines {
		p.Next = jobHref(d.name(), id, jobID) + "?from=" + strconv.FormatInt(next, 10)
		p.Follow = false // more is waiting before the live end
	}
	if from > 0 {
		p.Prev = jobHref(d.name(), id, jobID) + "?from=" + strconv.FormatInt(max(from-jobLogPageLines, 0), 10)
	}
	if view.Phase == domain.CIPhaseWaiting || view.Phase == domain.CIPhaseReady {
		p.Note = "the job has not started: no log yet."
	} else if view.Lines == 0 && view.Ended() {
		p.Note = "the job wrote no log."
	}
	l := s.repoLayout(ctx, d, "actions")
	l.Title = view.Name + " · run " + p.Run + " · " + d.name() + " · " + s.instance
	l.Live = true
	l.LiveURL = liveURL("repo:"+d.name(), "view=job&job="+jobID)
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs,
		Crumb{Label: "actions", Href: actionsBase(d.name())},
		Crumb{Label: p.RunName + " #" + strconv.FormatInt(run.Run.Number, 10), Href: p.RunHref},
		Crumb{Label: p.Name, Current: true})
	l.HasRail = true
	return Job(l, p), http.StatusOK, nil
}

// logClient is the key of the visitor for the limit of log streams per client.
func (s *Server) logClient(r *http.Request) string {
	return clientlimit.Key(clientlimit.ClientAddr(r, s.cfg.TrustedProxies))
}

// readLog reads up to max lines from a line number, through the log stream of
// the use case, and returns the line after the last read.
func (s *Server) readLog(ctx context.Context, client string, repo domain.RepoName, j ports.JobView, from int64, max int) ([]LogLineView, int64, error) {
	if from > j.Lines || j.Lines == 0 {
		return nil, from, nil
	}
	var out []LogLineView
	next := from
	ranges := j.StepRanges()
	stepOf := func(n int64) int32 {
		for i, r := range ranges {
			if n >= r.Start && n < r.End {
				return j.Steps[i].Number
			}
		}
		return 0
	}
	stop := errors.New("enough")
	err := s.cfg.Runs.GetJobLog(ctx, principal(ctx), repo, ports.GetJobLogParams{JobID: j.ID, Attempt: j.Attempt, Offset: from, Client: client}, func(b ports.LogBatch) error {
		for i, l := range b.Lines {
			if len(out) >= max {
				return stop
			}
			n := b.Offset + int64(i)
			text := l.Text
			v := LogLineView{N: n, Step: stepOf(n), Time: l.Time.UTC().Format("15:04:05")}
			switch {
			case strings.HasPrefix(text, "##[group]"):
				v.Group, text = true, strings.TrimPrefix(text, "##[group]")
			case strings.HasPrefix(text, "##[endgroup]"):
				v.End, text = true, strings.TrimPrefix(text, "##[endgroup]")
			case strings.HasPrefix(text, "##[error]"):
				v.Error, text = true, strings.TrimPrefix(text, "##[error]")
			}
			if l.Continued && len(out) > 0 {
				out[len(out)-1].Text += visibleText(text)
				next = n + 1
				continue
			}
			v.Text = visibleText(text)
			out = append(out, v)
			next = n + 1
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) && !errors.Is(err, domain.ErrNotFound) {
		return nil, from, err
	}
	return out, next, nil
}

// rawLog is GET /<repo>/actions/runs/<id>/job/<job id>/raw: the log as text,
// one line per line with its time, joined where the runner cut a long line.
func (s *Server) rawLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	id, err := parseRunPath(r)
	jobID := r.PathValue("job")
	if runOf, _, _, ok := domain.ParseJobID(jobID); err != nil || !ok || runOf != id {
		s.fail(ctx, w, r, errNotFound)
		return
	}
	var attempt int32
	if a := r.URL.Query().Get("attempt"); a != "" {
		n, perr := strconv.ParseInt(a, 10, 32)
		if perr != nil || n < 1 {
			s.fail(ctx, w, r, errNotFound)
			return
		}
		attempt = int32(n)
	}
	view, err := s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, jobID, attempt)
	if err != nil {
		s.fail(ctx, w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Disposition", fmt.Sprintf(`inline; filename="job-%s.log"`, jobID))
	cacheHeaders(ctx, h, "no-cache", sessionCache)
	written := 0
	open := false
	err = s.cfg.Runs.GetJobLog(ctx, principal(ctx), d.repo.Name, ports.GetJobLogParams{JobID: jobID, Attempt: attempt, Client: s.logClient(r)}, func(b ports.LogBatch) error {
		for _, l := range b.Lines {
			if written > rawLogMaxBytes {
				return errors.New("log too large")
			}
			var line string
			switch {
			case l.Continued && open:
				line = l.Text
			case open:
				line = "\n" + l.Time.UTC().Format(time.RFC3339Nano) + " " + l.Text
			default:
				line = l.Time.UTC().Format(time.RFC3339Nano) + " " + l.Text
			}
			open = true
			n, werr := fmt.Fprint(w, line)
			written += n
			if werr != nil {
				return werr
			}
		}
		return nil
	})
	if open {
		_, _ = fmt.Fprintln(w)
	}
	if err != nil && !open && errors.Is(err, domain.ErrNotFound) {
		_ = view
		w.WriteHeader(http.StatusNotFound)
	}
}

// --- the owner's actions ---

// ciAction wraps a POST of the actions pages: the form gate, the bounded
// context and the mapping of a refusal to a page.
func (s *Server) ciAction(h func(ctx context.Context, r *http.Request) (to string, err error)) http.HandlerFunc {
	return s.mrAction(func(ctx context.Context, r *http.Request) (string, string, error) {
		to, err := h(ctx, r)
		return to, actionsBase(r.PathValue("repo")), err
	})
}

func (s *Server) runCancel(ctx context.Context, r *http.Request) (string, error) {
	name, id, err := s.runTarget(r)
	if err != nil {
		return "", err
	}
	if _, err := s.cfg.Runs.CancelRun(ctx, principal(ctx), name, id); err != nil {
		return "", err
	}
	return runHref(name.String(), id), nil
}

func (s *Server) runRerun(failedOnly bool) func(ctx context.Context, r *http.Request) (string, error) {
	return func(ctx context.Context, r *http.Request) (string, error) {
		name, id, err := s.runTarget(r)
		if err != nil {
			return "", err
		}
		if _, err := s.cfg.Runs.RerunRun(ctx, principal(ctx), name, id, failedOnly); err != nil {
			return "", err
		}
		return runHref(name.String(), id), nil
	}
}

func (s *Server) runTarget(r *http.Request) (domain.RepoName, int64, error) {
	name, err := domain.ParseRepoName(r.PathValue("repo"))
	if err != nil {
		return domain.RepoName{}, 0, errNotFound
	}
	id, err := parseRunPath(r)
	return name, id, err
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// runnerText names the runner of a job and how it isolated it.
func runnerText(j JobRow) string {
	if j.Isolation == "" {
		return j.Runner
	}
	return j.Runner + " (" + j.Isolation + ")"
}

func joinWords(ws []string) string { return strings.Join(ws, ", ") }

// logClass is the class of a line of the log.
func logClass(ln LogLineView) string {
	switch {
	case ln.Group:
		return "lg group"
	case ln.End:
		return "lg endgroup"
	case ln.Error:
		return "lg error"
	}
	return "lg"
}

// ChecksSummary is the combined state of the checks of a commit.
type ChecksSummary struct {
	Total        int
	Kind, State  string
	Label, Title string
	Href         string
}

// CheckRow is a check of a commit or a merge request.
type CheckRow struct {
	Name, Kind, State, Href string
	Waiting                 bool
}

// ChecksSection is the checks of the head of a merge request.
type ChecksSection struct {
	Rows []CheckRow
	// Note explains the rule that applies to them.
	Note string
}

// checksFor reads the checks of a commit as the caller of the request. A page
// shows them when it can: a failure of the read is logged and the page goes on
// without, since the code and the review do not depend on CI.
func (s *Server) checksFor(ctx context.Context, repo domain.RepoName, sha string) []ports.CheckView {
	if s.cfg.Runs == nil || sha == "" {
		return nil
	}
	res, err := s.cfg.Runs.ListChecks(ctx, principal(ctx), repo, sha)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			log := zerowrap.FromCtx(ctx)
			log.Warn().Err(err).Str("repo", repo.String()).Msg("read the checks of a commit")
		}
		return nil
	}
	return res.Checks
}

// checksSummary reduces checks to the mark a list shows next to a commit.
func checksSummary(repo domain.RepoName, sha string, checks []ports.CheckView) ChecksSummary {
	if len(checks) == 0 {
		return ChecksSummary{}
	}
	plain := make([]domain.CICheck, 0, len(checks))
	passed := 0
	for _, c := range checks {
		plain = append(plain, c.CICheck)
		if c.Status == domain.CICompleted && c.Conclusion.PassesMerge() {
			passed++
		}
	}
	sum := ChecksSummary{Total: len(checks), Href: repoHref(repo.String(), "actions"), Label: fmt.Sprintf("%d/%d", passed, len(checks))}
	switch domain.CombineChecks(plain) {
	case domain.CheckStateSuccess:
		sum.Kind, sum.State = "ok", "checks passed"
	case domain.CheckStateFailure:
		sum.Kind, sum.State = "bad", "checks failed"
	default:
		sum.Kind, sum.State = "running", "checks pending"
	}
	sum.Title = sum.State + ": " + sum.Label
	return sum
}

// checksSection is the checks of the rail of a merge request, with the rule of
// the repository that applies to them.
func checksSection(repo domain.Repo, checks []ports.CheckView, now time.Time) ChecksSection {
	sec := ChecksSection{}
	for _, c := range checks {
		row := CheckRow{
			Name: visibleText(c.Name), Kind: ciKind(c.Status, c.Conclusion), State: ciState(c.Status, c.Conclusion),
			Waiting: c.WaitingForRunner,
		}
		if c.JobID != "" {
			row.Href = jobHref(repo.Name.String(), c.RunID, c.JobID)
		} else {
			row.Href = runHref(repo.Name.String(), c.RunID)
		}
		sec.Rows = append(sec.Rows, row)
	}
	switch {
	case len(checks) == 0:
		sec.Note = "no check on this commit: nothing blocks the merge."
	case repo.Settings.FailingChecksBlock:
		sec.Note = "a failing or unfinished check blocks the merge."
	default:
		sec.Note = "checks do not block the merge in this repository."
	}
	_ = now
	return sec
}

// logStream is GET /<repo>/actions/runs/<id>/job/<job id>/stream?from=N: the
// lines of a job log from line N as Server-Sent Events, following the job while
// it runs. Each "ci-log" event holds the <li> of the lines of one batch and has
// the number of the next line as its id, so that a reconnection (Last-Event-ID)
// goes on where it stopped. "ci-end" tells the job is over and every line was
// sent. The job events of the repository stream carry no line (see events.go).
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	d, err := s.loadRepo(ctx, r.PathValue("repo"))
	if err != nil {
		s.denyEvents(ctx, w, err)
		return
	}
	id, err := parseRunPath(r)
	jobID := r.PathValue("job")
	if runOf, _, _, ok := domain.ParseJobID(jobID); err != nil || !ok || runOf != id {
		s.denyEvents(ctx, w, errNotFound)
		return
	}
	from := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		from, err = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("from"); v != "" {
		from, err = strconv.ParseInt(v, 10, 64)
	}
	if err != nil || from < 0 {
		s.denyEvents(ctx, w, errNotFound)
		return
	}
	view, err := s.cfg.Runs.GetJob(ctx, principal(ctx), d.repo.Name, jobID, 0)
	if err != nil {
		s.denyEvents(ctx, w, err)
		return
	}

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	cacheHeaders(ctx, hdr, "no-cache, no-transform", sessionCache+", no-transform")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &sseWriter{w: w, rc: rc}
	if out.raw(": connected\n\n") != nil {
		return
	}
	ranges := view.StepRanges()
	_ = ranges
	n := from
	err = s.cfg.Runs.GetJobLog(ctx, principal(ctx), d.repo.Name, ports.GetJobLogParams{
		JobID: jobID, Offset: from, Follow: true,
		Client: s.logClient(r),
	}, func(b ports.LogBatch) error {
		var html strings.Builder
		for i, l := range b.Lines {
			v := LogLineView{N: b.Offset + int64(i), Time: l.Time.UTC().Format("15:04:05"), Step: l.Step}
			text := l.Text
			switch {
			case strings.HasPrefix(text, "##[group]"):
				v.Group, text = true, strings.TrimPrefix(text, "##[group]")
			case strings.HasPrefix(text, "##[endgroup]"):
				v.End, text = true, strings.TrimPrefix(text, "##[endgroup]")
			case strings.HasPrefix(text, "##[error]"):
				v.Error, text = true, strings.TrimPrefix(text, "##[error]")
			}
			v.Text = visibleText(text)
			if l.Continued {
				continue // joined to its line by the page reload; rare and harmless
			}
			var b2 strings.Builder
			if err := LogLine(v).Render(ctx, &b2); err != nil {
				return err
			}
			html.WriteString(strings.ReplaceAll(b2.String(), "\n", ""))
		}
		n = b.Offset + int64(len(b.Lines))
		_ = rc.SetWriteDeadline(time.Now().Add(writeWindow))
		_, werr := io.WriteString(w, "event: ci-log\nid: "+strconv.FormatInt(n, 10)+"\ndata: "+html.String()+"\n\n")
		if werr != nil {
			return werr
		}
		return rc.Flush()
	})
	if err == nil || errors.Is(err, domain.ErrNotFound) {
		_ = out.raw("event: ci-end\nid: " + strconv.FormatInt(n, 10) + "\ndata: end\n\n")
	}
}
