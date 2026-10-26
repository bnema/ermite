// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// jsonRun is the stable JSON shape of a run.
type jsonRun struct {
	ID           int64       `json:"id"`
	Repo         string      `json:"repo"`
	Number       int64       `json:"number"`
	Attempt      int32       `json:"attempt"`
	Workflow     string      `json:"workflow"`
	Name         string      `json:"name" help:"the run-name, else the workflow"`
	WorkflowPath string      `json:"workflow_path"`
	Title        string      `json:"title"`
	Event        string      `json:"event"`
	Ref          string      `json:"ref"`
	Sha          string      `json:"sha"`
	Actor        string      `json:"actor"`
	Status       string      `json:"status" help:"queued, in_progress or completed"`
	Conclusion   string      `json:"conclusion" help:"success, failure, cancelled, skipped, timed_out, neutral or action_required; empty until completed"`
	Error        string      `json:"error"`
	CreatedAt    jsonInstant `json:"created_at"`
	StartedAt    jsonInstant `json:"started_at"`
	CompletedAt  jsonInstant `json:"completed_at"`
}

type jsonStep struct {
	Number     int32  `json:"number"`
	Name       string `json:"name"`
	State      string `json:"state" help:"pending, in_progress or completed"`
	Conclusion string `json:"conclusion"`
	// Outcome is the result before continue-on-error; for a step stored without
	// one it is the conclusion.
	Outcome     string      `json:"outcome" help:"the result before continue-on-error"`
	StartedAt   jsonInstant `json:"started_at"`
	CompletedAt jsonInstant `json:"completed_at"`
	LogStart    int64       `json:"log_start"`
	LogEnd      int64       `json:"log_end"`
}

type jsonRunJob struct {
	ID          string      `json:"id"`
	Attempt     int32       `json:"attempt"`
	Key         string      `json:"key"`
	Name        string      `json:"name"`
	Status      string      `json:"status" help:"queued, pending, in_progress or completed"`
	Conclusion  string      `json:"conclusion"`
	Error       string      `json:"error"`
	Labels      []string    `json:"labels"`
	Needs       []string    `json:"needs"`
	Runner      string      `json:"runner"`
	Isolation   string      `json:"isolation" help:"vm, container-sysbox or container-privileged"`
	Steps       []jsonStep  `json:"steps"`
	StartedAt   jsonInstant `json:"started_at"`
	CompletedAt jsonInstant `json:"completed_at"`
}

// jsonAnnotation is an annotation of a run, with the job and the step it belongs to.
type jsonAnnotation struct {
	Job         string `json:"job"`
	JobID       string `json:"job_id"`
	Step        int32  `json:"step"`
	Level       string `json:"level" help:"notice, warning or failure"`
	Title       string `json:"title"`
	Message     string `json:"message"`
	File        string `json:"file"`
	StartLine   int32  `json:"start_line"`
	EndLine     int32  `json:"end_line"`
	StartColumn int32  `json:"start_column"`
	EndColumn   int32  `json:"end_column"`
}

type jsonRunDetail struct {
	jsonRun       `help:"the run fields" see:"ermite run"`
	LatestAttempt int32            `json:"latest_attempt"`
	Jobs          []jsonRunJob     `json:"jobs"`
	Annotations   []jsonAnnotation `json:"annotations"`
	// AnnotationsTruncated is true when the server holds more annotations than it returned.
	AnnotationsTruncated bool `json:"annotations_truncated"`
}

func statusText(s ermite.CiStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "CI_STATUS_"))
}

func conclusionText(c ermite.CiConclusion) string {
	if c == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "CI_CONCLUSION_"))
}

func isolationText(i ermite.RunnerIsolation) string {
	if i == ermite.RunnerIsolation_RUNNER_ISOLATION_UNSPECIFIED {
		return ""
	}
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(i.String(), "RUNNER_ISOLATION_")), "_", "-")
}

func stepStateText(s ermite.StepState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STEP_STATE_"))
}

// runResult is "status" while the run is going, the conclusion once over.
func runResult(status ermite.CiStatus, c ermite.CiConclusion) string {
	if status == ermite.CiStatus_CI_STATUS_COMPLETED {
		return conclusionText(c)
	}
	return statusText(status)
}

// stepOutcomeText is the outcome of a step; a completed step stored without one
// has the outcome of its conclusion (Step.outcome).
func stepOutcomeText(s *ermite.Step) string {
	if s.GetOutcome() == ermite.CiConclusion_CI_CONCLUSION_UNSPECIFIED && s.GetState() == ermite.StepState_STEP_STATE_COMPLETED {
		return conclusionText(s.GetConclusion())
	}
	return conclusionText(s.GetOutcome())
}

func toJSONRun(r *ermite.Run) jsonRun {
	return jsonRun{
		ID: r.GetId(), Repo: r.GetRepo(), Number: r.GetNumber(), Attempt: r.GetAttempt(), Workflow: r.GetWorkflowName(),
		Name: r.GetName(), WorkflowPath: r.GetWorkflowPath(), Title: r.GetTitle(), Event: r.GetEvent(), Ref: r.GetRef(), Sha: r.GetSha(),
		Actor: r.GetActor(), Status: statusText(r.GetStatus()), Conclusion: conclusionText(r.GetConclusion()), Error: r.GetError(),
		CreatedAt: jsonTime(r.GetCreatedAt()), StartedAt: jsonTime(r.GetStartedAt()), CompletedAt: jsonTime(r.GetCompletedAt()),
	}
}

func toJSONRunJob(j *ermite.RunJob) jsonRunJob {
	steps := make([]jsonStep, 0, len(j.GetSteps()))
	for _, s := range j.GetSteps() {
		steps = append(steps, jsonStep{
			Number: s.GetNumber(), Name: s.GetName(), State: stepStateText(s.GetState()), Conclusion: conclusionText(s.GetConclusion()),
			Outcome: stepOutcomeText(s), StartedAt: jsonTime(s.GetStartedAt()), CompletedAt: jsonTime(s.GetCompletedAt()), LogStart: s.GetLogStart(), LogEnd: s.GetLogEnd(),
		})
	}
	labels, needs := j.GetLabels(), j.GetNeeds()
	if labels == nil {
		labels = []string{}
	}
	if needs == nil {
		needs = []string{}
	}
	return jsonRunJob{
		ID: j.GetId(), Attempt: j.GetAttempt(), Key: j.GetKey(), Name: j.GetName(), Status: statusText(j.GetStatus()),
		Conclusion: conclusionText(j.GetConclusion()), Error: j.GetError(), Labels: labels, Needs: needs,
		Runner: j.GetRunnerName(), Isolation: isolationText(j.GetIsolation()), Steps: steps,
		StartedAt: jsonTime(j.GetStartedAt()), CompletedAt: jsonTime(j.GetCompletedAt()),
	}
}

func newRunCmd(env *cliEnv) *cobra.Command {
	run := &cobra.Command{
		Use:   "run",
		Short: "Manage CI runs and download artifacts",
		Long: "Manage repository CI runs (workflows in .github/workflows run on push).\n" +
			"Reads need repository read access; cancel and rerun need ci:write.\n" +
			"Artifacts need a token, even for public repositories.\n" +
			jsonFieldsHelp("a run", jsonRun{}) + "\n" + jsonFieldsHelp("a run with its jobs", jsonRunDetail{}),
	}
	run.AddCommand(newRunListCmd(env), newRunViewCmd(env), newRunWatchCmd(env), newRunCancelCmd(env), newRunRerunCmd(env),
		newRunArtifactsCmd(env), newRunDownloadCmd(env))
	return run
}

// parseRunID reads a run id argument.
func parseRunID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%q is not a run id: use the number shown by `ermite run list`", s)
	}
	return id, nil
}

func newRunListCmd(env *cliEnv) *cobra.Command {
	var workflow, branch, status string
	var limit int
	cmd := &cobra.Command{
		Use:               "list <repo>",
		Short:             "List the runs of a repository",
		Long:              "List repository runs, newest first. Filters: --workflow path or filename (bernard/deps for Bernard dependency runs), --branch branch, --status queued|in_progress|completed.\nJSON: an array of runs; see `ermite run --help`.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&workflow, "workflow", "", "only the runs of this workflow file")
	cmd.Flags().StringVar(&branch, "branch", "", "only the runs of this branch")
	cmd.Flags().StringVar(&status, "status", "", "only the runs with this status: queued, in_progress, completed")
	cmd.Flags().IntVar(&limit, "limit", 20, "show at most this many runs")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		st, err := parseStatusFlag(status)
		if err != nil {
			return err
		}
		if limit < 1 {
			return fmt.Errorf("--limit must be at least 1")
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		var runs []*ermite.Run
		token := ""
		for len(runs) < limit {
			res, err := c.Runs().ListRuns(ctx, connect.NewRequest(&ermite.ListRunsRequest{
				Repo: args[0], PageSize: int32(min(limit-len(runs), 100)), PageToken: token, //nolint:gosec // bounded
				Workflow: workflow, Branch: branch, Status: st,
			}))
			if err != nil {
				return err
			}
			runs = append(runs, res.Msg.GetRuns()...)
			if token = res.Msg.GetNextPageToken(); token == "" {
				break
			}
		}
		if *asJSON {
			out := make([]jsonRun, 0, len(runs))
			for _, r := range runs {
				out = append(out, toJSONRun(r))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(runs) == 0 {
			noticef(cmd, "no runs")
			return nil
		}
		rows := make([][]string, 0, len(runs))
		for _, r := range runs {
			rows = append(rows, []string{
				strconv.FormatInt(r.GetId(), 10), runResult(r.GetStatus(), r.GetConclusion()), r.GetWorkflowName(), r.GetEvent(),
				refText(r.GetRef()), shortSha(r.GetSha()),
				r.GetActor(), shortTime(r.GetCreatedAt(), "-"), r.GetTitle(),
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "RESULT", "WORKFLOW", "EVENT", "REF", "COMMIT", "ACTOR", "CREATED", "TITLE"}, rows)
	}
	return cmd
}

func shortSha(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func parseStatusFlag(s string) (ermite.CiStatus, error) {
	switch strings.ToLower(s) {
	case "":
		return ermite.CiStatus_CI_STATUS_UNSPECIFIED, nil
	case "queued":
		return ermite.CiStatus_CI_STATUS_QUEUED, nil
	case "in_progress", "in-progress":
		return ermite.CiStatus_CI_STATUS_IN_PROGRESS, nil
	case "completed":
		return ermite.CiStatus_CI_STATUS_COMPLETED, nil
	}
	return 0, fmt.Errorf("unknown status %q: use queued, in_progress or completed", s)
}

func newRunViewCmd(env *cliEnv) *cobra.Command {
	var attempt int32
	var logFlag, summaryFlag bool
	var jobFlag string
	cmd := &cobra.Command{
		Use:               "view <repo> <run id>",
		Short:             "Show a run, jobs, steps, annotations, logs or summaries",
		Long:              "Show run state, jobs, steps and ANNOTATIONS (step notices, warnings and errors with job and file:line). --attempt reads an earlier attempt.\n--log prints the job logs instead, verbatim and unprefixed. --summary prints the job summaries instead\n(GITHUB_STEP_SUMMARY Markdown, unrendered). Both cover every job unless --job names one (id or name).\nJSON: a run with its jobs; see `ermite run --help`.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().Int32Var(&attempt, "attempt", 0, "the attempt to read (default: the latest)")
	cmd.Flags().BoolVar(&logFlag, "log", false, "print the log of the jobs instead of the summary")
	cmd.Flags().BoolVar(&summaryFlag, "summary", false, "print the job summaries (Markdown) instead of the summary of the run")
	cmd.Flags().StringVar(&jobFlag, "job", "", "with --log or --summary, only this job (its id or its name)")
	asJSON := addJSONFlag(cmd)
	cmd.MarkFlagsMutuallyExclusive("log", "summary")
	cmd.MarkFlagsMutuallyExclusive("json", "summary")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		res, err := c.Runs().GetRun(ctx, connect.NewRequest(&ermite.GetRunRequest{Repo: args[0], RunId: id, Attempt: attempt}))
		if err != nil {
			return err
		}
		if logFlag {
			return printRunLog(ctx, cmd.OutOrStdout(), c, args[0], res.Msg, jobFlag, false)
		}
		if summaryFlag {
			return printRunSummaries(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), c, args[0], res.Msg, jobFlag)
		}
		anns, err := runAnnotations(ctx, c, args[0], res.Msg)
		if err != nil {
			return err
		}
		if *asJSON {
			d := jsonRunDetail{
				jsonRun: toJSONRun(res.Msg.GetRun()), LatestAttempt: res.Msg.GetLatestAttempt(), Jobs: []jsonRunJob{},
				Annotations: []jsonAnnotation{}, AnnotationsTruncated: anns.truncated,
			}
			for _, j := range res.Msg.GetJobs() {
				d.Jobs = append(d.Jobs, toJSONRunJob(j))
			}
			for _, a := range anns.items {
				d.Annotations = append(d.Annotations, toJSONAnnotation(a, anns.jobs))
			}
			return writeJSON(cmd.OutOrStdout(), d)
		}
		if err := printRun(cmd.OutOrStdout(), res.Msg); err != nil {
			return err
		}
		return printAnnotations(cmd.OutOrStdout(), anns)
	}
	return cmd
}

func printRun(w io.Writer, r *ermite.GetRunResponse) error {
	run := r.GetRun()
	pairs := [][2]string{
		{"run", fmt.Sprintf("%d (#%d of %s)", run.GetId(), run.GetNumber(), run.GetWorkflowName())},
		{"result", runResult(run.GetStatus(), run.GetConclusion())},
		{"event", run.GetEvent()},
		{"ref", run.GetRef()},
		{"commit", run.GetSha()},
		{"title", run.GetTitle()},
		{"actor", run.GetActor()},
		{"attempt", fmt.Sprintf("%d of %d", run.GetAttempt(), r.GetLatestAttempt())},
		{"created", shortTime(run.GetCreatedAt(), "-")},
	}
	if run.GetError() != "" {
		pairs = append(pairs, [2]string{"error", run.GetError()})
	}
	if err := fields(w, pairs); err != nil {
		return err
	}
	if len(r.GetJobs()) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(w)
	for _, j := range r.GetJobs() {
		line := fmt.Sprintf("%s  %s", runResult(j.GetStatus(), j.GetConclusion()), j.GetName())
		if j.GetRunnerName() != "" {
			line += "  [" + j.GetRunnerName() + "]"
		}
		_, _ = fmt.Fprintln(w, clean(line))
		if j.GetError() != "" {
			_, _ = fmt.Fprintln(w, "    "+clean(j.GetError()))
		}
		for _, s := range j.GetSteps() {
			res := stepStateText(s.GetState())
			if s.GetState() == ermite.StepState_STEP_STATE_COMPLETED {
				res = conclusionText(s.GetConclusion())
			}
			_, _ = fmt.Fprintln(w, clean(fmt.Sprintf("    %-9s %2d  %s", res, s.GetNumber(), s.GetName())))
		}
	}
	return nil
}

// runNotes are the annotations of a run as read from the server.
type runNotes struct {
	items     []*ermite.JobAnnotation
	truncated bool
	// jobs maps a job id to the job.
	jobs map[string]*ermite.RunJob
}

// runAnnotations reads the annotations of the attempt of a run that was read.
// A server without the feature has none: it answers unimplemented, which is
// not an error here.
func runAnnotations(ctx context.Context, c *client.Client, repo string, r *ermite.GetRunResponse) (runNotes, error) {
	out := runNotes{jobs: map[string]*ermite.RunJob{}}
	for _, j := range r.GetJobs() {
		out.jobs[j.GetId()] = j
	}
	res, err := c.Runs().ListAnnotations(ctx, connect.NewRequest(&ermite.ListAnnotationsRequest{
		Repo: repo, RunId: r.GetRun().GetId(), Attempt: r.GetRun().GetAttempt(),
	}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return out, nil
	}
	if err != nil {
		return runNotes{}, err
	}
	out.items, out.truncated = res.Msg.GetAnnotations(), res.Msg.GetTruncated()
	return out, nil
}

func annotationLevelText(l ermite.AnnotationLevel) string {
	switch l {
	case ermite.AnnotationLevel_ANNOTATION_LEVEL_NOTICE:
		return "notice"
	case ermite.AnnotationLevel_ANNOTATION_LEVEL_WARNING:
		return "warning"
	case ermite.AnnotationLevel_ANNOTATION_LEVEL_FAILURE:
		return "error"
	}
	return ""
}

// annotationPlace is "file:line" (or "file:line-endline"), "" without a file.
func annotationPlace(a *ermite.Annotation) string {
	if a.GetFile() == "" {
		return ""
	}
	p := a.GetFile()
	if a.GetStartLine() > 0 {
		p += ":" + strconv.Itoa(int(a.GetStartLine()))
		if a.GetEndLine() > a.GetStartLine() {
			p += "-" + strconv.Itoa(int(a.GetEndLine()))
		}
	}
	return p
}

func toJSONAnnotation(ja *ermite.JobAnnotation, jobs map[string]*ermite.RunJob) jsonAnnotation {
	a := ja.GetAnnotation()
	level := annotationLevelText(a.GetLevel())
	if level == "error" {
		level = "failure" // the API's name, as GitHub's annotation_level
	}
	return jsonAnnotation{
		Job: jobs[ja.GetJobId()].GetName(), JobID: ja.GetJobId(), Step: a.GetStepNumber(), Level: level, Title: a.GetTitle(),
		Message: a.GetMessage(), File: a.GetFile(), StartLine: a.GetStartLine(), EndLine: a.GetEndLine(),
		StartColumn: a.GetStartColumn(), EndColumn: a.GetEndColumn(),
	}
}

// printAnnotations prints the ANNOTATIONS section of `run view`, like `gh run
// view` does: one line per annotation with its level, its job, its place and
// its message. Nothing is printed for a run without annotation.
func printAnnotations(w io.Writer, n runNotes) error {
	if len(n.items) == 0 {
		return nil
	}
	_, _ = fmt.Fprint(w, "\nANNOTATIONS\n")
	rows := make([][]string, 0, len(n.items))
	for _, ja := range n.items {
		a := ja.GetAnnotation()
		msg := a.GetMessage()
		if a.GetTitle() != "" {
			msg = a.GetTitle() + ": " + msg
		}
		rows = append(rows, []string{annotationLevelText(a.GetLevel()), n.jobs[ja.GetJobId()].GetName(), annotationPlace(a), msg})
	}
	if err := table(w, []string{"LEVEL", "JOB", "FILE", "MESSAGE"}, rows); err != nil {
		return err
	}
	if n.truncated {
		_, _ = fmt.Fprintf(w, "(more annotations exist than the %d shown)\n", len(n.items))
	}
	return nil
}

// printRunSummaries prints the job summaries of the chosen jobs as the Markdown
// the steps wrote to w; notes go to errw. With several of them, each is preceded
// by a line naming its job and step. Control characters are replaced, as everywhere the CLI prints
// text it did not write (a tab becomes a space); line breaks are kept.
func printRunSummaries(ctx context.Context, w, errw io.Writer, c *client.Client, repo string, r *ermite.GetRunResponse, sel string) error {
	jobs, err := pickJobs(r.GetJobs(), sel)
	if err != nil {
		return err
	}
	type found struct {
		job *ermite.RunJob
		sum *ermite.StepSummary
	}
	var all []found
	truncated := false
	for _, j := range jobs {
		if j.GetStatus() == ermite.CiStatus_CI_STATUS_QUEUED || j.GetStatus() == ermite.CiStatus_CI_STATUS_PENDING {
			continue
		}
		res, err := c.Runs().GetStepSummaries(ctx, connect.NewRequest(&ermite.GetStepSummariesRequest{Repo: repo, JobId: j.GetId(), Attempt: j.GetAttempt()}))
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return fmt.Errorf("the server does not keep job summaries (it lacks the ci.summaries feature)")
		}
		if err != nil {
			return err
		}
		for _, s := range res.Msg.GetSummaries() {
			all = append(all, found{j, s})
		}
		truncated = truncated || res.Msg.GetTruncated()
	}
	if len(all) == 0 {
		noticef2(errw, "the run has no job summary")
		return nil
	}
	for i, f := range all {
		if len(all) > 1 {
			if i > 0 {
				_, _ = fmt.Fprintln(w)
			}
			_, _ = fmt.Fprintf(w, "== %s: %s ==\n", clean(f.job.GetName()), clean(stepName(f.job, f.sum.GetStepNumber())))
		}
		text := rawText(f.sum.GetMarkdown())
		_, _ = fmt.Fprint(w, text)
		if !strings.HasSuffix(text, "\n") {
			_, _ = fmt.Fprintln(w)
		}
	}
	if truncated {
		noticef2(errw, "some jobs have more summaries than the 20 the server returns")
	}
	return nil
}

// stepName is the name of a step of a job, or "step N" when it is not known.
func stepName(j *ermite.RunJob, n int32) string {
	for _, s := range j.GetSteps() {
		if s.GetNumber() == n && s.GetName() != "" {
			return s.GetName()
		}
	}
	return "step " + strconv.Itoa(int(n))
}

// rawText replaces the control characters of a text that may come from the
// workflow (clean), line by line, so that its line breaks stay.
func rawText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = clean(l)
	}
	return strings.Join(lines, "\n")
}

// pickJobs selects the jobs --job names (an id or a name), or all of them.
func pickJobs(jobs []*ermite.RunJob, sel string) ([]*ermite.RunJob, error) {
	if sel == "" {
		return jobs, nil
	}
	var out []*ermite.RunJob
	for _, j := range jobs {
		if j.GetId() == sel || j.GetName() == sel || j.GetKey() == sel {
			out = append(out, j)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the run has no job %q", sel)
	}
	return out, nil
}

// printRunLog prints the log of the chosen jobs; with follow it waits for the
// lines of a job that is running.
func printRunLog(ctx context.Context, w io.Writer, c *client.Client, repo string, r *ermite.GetRunResponse, sel string, follow bool) error {
	jobs, err := pickJobs(r.GetJobs(), sel)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if len(jobs) > 1 {
			_, _ = fmt.Fprintf(w, "== %s ==\n", clean(j.GetName()))
		}
		if err := streamJobLog(ctx, w, c, repo, j, follow); err != nil {
			return err
		}
	}
	return nil
}

// streamJobLog writes the lines of one job. Continued pieces are joined to the
// line before them, as the raw log has them.
func streamJobLog(ctx context.Context, w io.Writer, c *client.Client, repo string, j *ermite.RunJob, follow bool) error {
	if (j.GetStatus() == ermite.CiStatus_CI_STATUS_QUEUED || j.GetStatus() == ermite.CiStatus_CI_STATUS_PENDING) && !follow {
		noticef2(w, "job %q has not started: no log yet", j.GetName())
		return nil
	}
	stream, err := c.Runs().GetJobLog(ctx, connect.NewRequest(&ermite.GetJobLogRequest{
		Repo: repo, JobId: j.GetId(), Attempt: j.GetAttempt(), Follow: follow,
	}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	open := false
	for stream.Receive() {
		for _, l := range stream.Msg().GetLines() {
			if l.GetContinued() && open {
				_, _ = fmt.Fprint(w, clean(l.GetText()))
				continue
			}
			if open {
				_, _ = fmt.Fprintln(w)
			}
			_, _ = fmt.Fprint(w, clean(l.GetText()))
			open = true
		}
	}
	if open {
		_, _ = fmt.Fprintln(w)
	}
	if err := stream.Err(); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return err
	}
	return nil
}

func noticef2(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "ermite: "+format+"\n", args...)
}

func newRunWatchCmd(env *cliEnv) *cobra.Command {
	var interval time.Duration
	var logFlag bool
	cmd := &cobra.Command{
		Use:   "watch <repo> <run id>",
		Short: "Wait for a run to complete and follow its jobs",
		Long: "Follow job state changes until the run completes. --log also streams job logs.\n" +
			"Exit status: 0 for success, 1 for any other conclusion; use this to gate scripts.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "how often to read the run")
	cmd.Flags().BoolVar(&logFlag, "log", false, "stream the log of the jobs while they run")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		w := cmd.OutOrStdout()
		last := ""
		for {
			res, err := c.Runs().GetRun(ctx, connect.NewRequest(&ermite.GetRunRequest{Repo: args[0], RunId: id}))
			if err != nil {
				return err
			}
			if line := watchLine(res.Msg); line != last {
				_, _ = fmt.Fprintln(w, line)
				last = line
			}
			if res.Msg.GetRun().GetStatus() == ermite.CiStatus_CI_STATUS_COMPLETED {
				if logFlag {
					if err := printRunLog(ctx, w, c, args[0], res.Msg, "", false); err != nil {
						return err
					}
				}
				if res.Msg.GetRun().GetConclusion() != ermite.CiConclusion_CI_CONCLUSION_SUCCESS {
					return fmt.Errorf("run %d: %s", id, runResult(res.Msg.GetRun().GetStatus(), res.Msg.GetRun().GetConclusion()))
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return cmd
}

// watchLine is a one-line state of a run for `run watch`.
func watchLine(r *ermite.GetRunResponse) string {
	var parts []string
	for _, j := range r.GetJobs() {
		parts = append(parts, j.GetName()+" "+runResult(j.GetStatus(), j.GetConclusion()))
	}
	return clean(fmt.Sprintf("run %d  %s  %s", r.GetRun().GetId(), runResult(r.GetRun().GetStatus(), r.GetRun().GetConclusion()), strings.Join(parts, ", ")))
}

func newRunCancelCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "cancel <repo> <run id>",
		Short:             "Cancel a run",
		Long:              "Ask runners to stop an unfinished run. Unassigned jobs are cancelled immediately. Access needs ci:write.\nJSON: a run; see `ermite run --help`.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().CancelRun(env.ctx(cmd), connect.NewRequest(&ermite.CancelRunRequest{Repo: args[0], RunId: id}))
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONRun(res.Msg.GetRun()))
		}
		noticef(cmd, "run %d: cancellation requested (%s)", id, runResult(res.Msg.GetRun().GetStatus(), res.Msg.GetRun().GetConclusion()))
		return nil
	}
	return cmd
}

func newRunRerunCmd(env *cliEnv) *cobra.Command {
	var failed bool
	cmd := &cobra.Command{
		Use:               "rerun <repo> <run id>",
		Short:             "Run a completed run again",
		Long:              "Start another attempt of a completed run with the same commit and workflow file. --failed reruns only unsuccessful jobs and their dependents. Access needs ci:write.\nJSON: a run; see `ermite run --help`.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().BoolVar(&failed, "failed", false, "run again only the jobs that failed, and those that need them")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().RerunRun(env.ctx(cmd), connect.NewRequest(&ermite.RerunRunRequest{Repo: args[0], RunId: id, FailedOnly: failed}))
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONRun(res.Msg.GetRun()))
		}
		noticef(cmd, "run %d: attempt %d started", id, res.Msg.GetRun().GetAttempt())
		return nil
	}
	return cmd
}

// refText is a ref as the run list shows it: the short name of a branch or a
// tag, "!<n>" for the merge ref of a merge request (a pull_request run).
func refText(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		if n, _, ok := strings.Cut(rest, "/"); ok {
			return "!" + n
		}
	}
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
}
