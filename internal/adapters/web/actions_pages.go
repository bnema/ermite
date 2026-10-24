// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

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
//	GET  /<repo>/actions/runs/<id>/artifacts/<artifact id>   (actions_artifacts.go)

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
	case domain.CIPending:
		return "pending"
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
	case status == domain.CIQueued, status == domain.CIPending:
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
	// Event is the event that triggered the run (push, pull_request,
	// workflow_dispatch, schedule, workflow_run).
	Event    string
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
	// Disabled are the workflow files that start no run (`ermite workflow
	// disable`), as paths.
	Disabled []string
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
	// Annotations are those of the jobs of the run, AnnotationsMore says that
	// the server holds more than it returned.
	Annotations     []AnnotationView
	AnnotationsMore bool
	// Summaries are the job summaries, rendered; only the full page has them.
	Summaries []JobSummaries
	// SummariesMore says that some summaries are not shown (the page budget).
	SummariesMore bool
	// Artifacts are those of the run; only the full page has them, nil when
	// the visitor cannot list them or there is none.
	Artifacts *ArtifactsView
	Active    bool
	// detail is what the page was built from.
	detail ports.RunDetail
	// WorkflowDisabled: the workflow file of the run is disabled now, so a new
	// event starts no run of it.
	WorkflowDisabled bool
	// Forms are the owner's actions; nil/empty when the visitor cannot.
	CancelAction, RerunAction, RerunFailedAction string
	CanRerunFailed                               bool
}

// AttemptLink switches between the attempts of a run.
type AttemptLink struct {
	Label, Href string
	Current     bool
}

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
	p.Disabled = s.disabledWorkflows(ctx, d)
	l := s.repoLayout(ctx, d, "actions")
	l.Title = "actions · " + d.name() + " · " + s.instance
	l.Live, l.LiveURL = true, liveURL("repo:"+d.name(), "view=actions")
	l.Crumbs[0].Current = false
	l.Crumbs = append(l.Crumbs, Crumb{Label: "actions", Current: true})
	return Actions(l, p), http.StatusOK, nil
}

// disabledWorkflows are the disabled workflow paths of the repository; the
// pages show them as a note, so a failure to read them is not one of the page.
func (s *Server) disabledWorkflows(ctx context.Context, d *repoData) []string {
	off, err := s.cfg.Runs.DisabledWorkflows(ctx, principal(ctx), d.repo.Name)
	if err != nil {
		return nil
	}
	return off
}

func (s *Server) runRow(repo string, run domain.CIRun, now time.Time) RunRow {
	row := RunRow{
		ID: strconv.FormatInt(run.ID, 10), Href: runHref(repo, run.ID),
		Kind: ciKind(run.Status, run.Conclusion), State: ciState(run.Status, run.Conclusion),
		Workflow: visibleText(run.WorkflowName), Event: visibleText(run.Event), Title: visibleText(run.Title),
		Ref:   visibleText(strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/")),
		Short: short(run.Sha), CHref: repoHref(repo, "commit", run.Sha), Actor: s.who(run.Actor),
		Duration: duration(run.StartedAt, run.CompletedAt, now), Number: "#" + strconv.FormatInt(run.Number, 10), Attempt: run.Attempt,
	}
	row.RefHref = repoHref(repo, "tree", strings.TrimPrefix(strings.TrimPrefix(run.Ref, "refs/heads/"), "refs/tags/"))
	n, ok := run.PullNumber, run.PullNumber > 0
	if !ok {
		n, ok = pullNumber(run.Ref) // a run stored before PullNumber existed
	}
	if ok {
		// A pull_request run is for refs/pull/<n>/merge, a pull_request_target
		// run for the default branch: both show the merge request.
		row.Ref, row.RefHref = "#"+strconv.FormatInt(n, 10), mrHref(repo, n)
	}
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
	// The summaries are part of the page, not of the live body: the page
	// fetches itself again when the run completes (runBody), and they show then.
	if p.Summaries, p.SummariesMore, err = s.jobSummaries(ctx, d, p.detail.Run, p.detail.Jobs, p.Attempt == p.Latest, maxSummaryPageBytes); err != nil {
		return nil, 0, err
	}
	p.Artifacts = s.runArtifacts(ctx, d.repo.Name, p)
	p.WorkflowDisabled = slices.Contains(s.disabledWorkflows(ctx, d), p.detail.Run.WorkflowPath)
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
		Attempt: run.Attempt, Latest: detail.LatestAttempt, Active: run.Status != domain.CICompleted, detail: detail,
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
	// The annotations of the attempt shown (the jobs kept from an earlier attempt
	// by a re-run bring their own).
	if p.Annotations, p.AnnotationsMore, err = s.runAnnotations(ctx, d, run, run.Attempt, detail.Jobs, run.Attempt == detail.LatestAttempt); err != nil {
		return RunPage{}, err
	}
	if SignedIn(ctx) && run.Attempt == detail.LatestAttempt {
		base := runHref(d.name(), id)
		if p.Active {
			p.CancelAction = base + "/cancel"
		} else if len(detail.Jobs) > 0 && !d.repo.Archived {
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
	if p.Annotations, p.AnnotationsMore, err = s.jobAnnotations(ctx, d, id, run.Run.Sha, view, attempt); err != nil {
		return nil, 0, err
	}
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

// pullNumber reads the merge request number of refs/pull/<n>/merge or /head.
func pullNumber(ref string) (int64, bool) {
	if !domain.IsPullRef(ref) {
		return 0, false
	}
	num, _, _ := strings.Cut(strings.TrimPrefix(ref, domain.PullRefPrefix), "/")
	n, err := strconv.ParseInt(num, 10, 64)
	return n, err == nil
}
