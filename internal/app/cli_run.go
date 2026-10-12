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
	ID           int64   `json:"id"`
	Repo         string  `json:"repo"`
	Number       int64   `json:"number"`
	Attempt      int32   `json:"attempt"`
	Workflow     string  `json:"workflow"`
	WorkflowPath string  `json:"workflow_path"`
	Title        string  `json:"title"`
	Event        string  `json:"event"`
	Ref          string  `json:"ref"`
	Sha          string  `json:"sha"`
	Actor        string  `json:"actor"`
	Status       string  `json:"status"`
	Conclusion   string  `json:"conclusion"`
	Error        string  `json:"error"`
	CreatedAt    *string `json:"created_at"`
	StartedAt    *string `json:"started_at"`
	CompletedAt  *string `json:"completed_at"`
}

type jsonStep struct {
	Number      int32   `json:"number"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Conclusion  string  `json:"conclusion"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
	LogStart    int64   `json:"log_start"`
	LogEnd      int64   `json:"log_end"`
}

type jsonRunJob struct {
	ID          string     `json:"id"`
	Attempt     int32      `json:"attempt"`
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	Error       string     `json:"error"`
	Labels      []string   `json:"labels"`
	Needs       []string   `json:"needs"`
	Runner      string     `json:"runner"`
	Isolation   string     `json:"isolation"`
	Steps       []jsonStep `json:"steps"`
	StartedAt   *string    `json:"started_at"`
	CompletedAt *string    `json:"completed_at"`
}

type jsonRunDetail struct {
	jsonRun
	LatestAttempt int32        `json:"latest_attempt"`
	Jobs          []jsonRunJob `json:"jobs"`
}

const jsonRunHelp = `
JSON fields of a run (--json): id, repo, number, attempt, workflow, workflow_path, title, event,
ref, sha, actor, status (queued, in_progress, completed), conclusion (success, failure, cancelled,
skipped, timed_out, neutral, action_required; empty until completed), error, created_at,
started_at and completed_at (RFC 3339 UTC, null when unset). "run view" adds latest_attempt and
jobs (id, attempt, key, name, status, conclusion, error, labels, needs, runner, isolation, steps
with number, name, state, conclusion, log_start and log_end, started_at, completed_at).`

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

func toJSONRun(r *ermite.Run) jsonRun {
	return jsonRun{
		ID: r.GetId(), Repo: r.GetRepo(), Number: r.GetNumber(), Attempt: r.GetAttempt(), Workflow: r.GetWorkflowName(),
		WorkflowPath: r.GetWorkflowPath(), Title: r.GetTitle(), Event: r.GetEvent(), Ref: r.GetRef(), Sha: r.GetSha(),
		Actor: r.GetActor(), Status: statusText(r.GetStatus()), Conclusion: conclusionText(r.GetConclusion()), Error: r.GetError(),
		CreatedAt: jsonTime(r.GetCreatedAt()), StartedAt: jsonTime(r.GetStartedAt()), CompletedAt: jsonTime(r.GetCompletedAt()),
	}
}

func toJSONRunJob(j *ermite.RunJob) jsonRunJob {
	steps := make([]jsonStep, 0, len(j.GetSteps()))
	for _, s := range j.GetSteps() {
		steps = append(steps, jsonStep{
			Number: s.GetNumber(), Name: s.GetName(), State: stepStateText(s.GetState()), Conclusion: conclusionText(s.GetConclusion()),
			StartedAt: jsonTime(s.GetStartedAt()), CompletedAt: jsonTime(s.GetCompletedAt()), LogStart: s.GetLogStart(), LogEnd: s.GetLogEnd(),
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
		Short: "List, read, watch, cancel and re-run CI runs",
		Long: "Work with the CI runs of a repository (the workflows that .github/workflows start on a push).\n" +
			"Reading needs the access of reading the repository; cancel and rerun need the ci:write scope.",
	}
	run.AddCommand(newRunListCmd(env), newRunViewCmd(env), newRunWatchCmd(env), newRunCancelCmd(env), newRunRerunCmd(env))
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
		Use:   "list <repo>",
		Short: "List the runs of a repository",
		Long: "List the runs of a repository, newest first. --workflow keeps one workflow file (its path or its\n" +
			"file name), --branch one branch, --status one status (queued, in_progress, completed)." + jsonRunHelp + "\n--json prints an array of runs.",
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
		c, err := env.client(cmd, true)
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
				strconv.FormatInt(r.GetId(), 10), runResult(r.GetStatus(), r.GetConclusion()), r.GetWorkflowName(),
				strings.TrimPrefix(strings.TrimPrefix(r.GetRef(), "refs/heads/"), "refs/tags/"), shortSha(r.GetSha()),
				r.GetActor(), shortTime(r.GetCreatedAt(), "-"), r.GetTitle(),
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "RESULT", "WORKFLOW", "REF", "COMMIT", "ACTOR", "CREATED", "TITLE"}, rows)
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
	var logFlag bool
	var jobFlag string
	cmd := &cobra.Command{
		Use:   "view <repo> <run id>",
		Short: "Show a run with its jobs and steps, or the log of a job",
		Long: "Show a run: its state, its jobs and their steps. --attempt reads an earlier attempt.\n" +
			"With --log, print the log of the run's jobs (all of them, or the one chosen by --job: its id\n" +
			"or its name) instead; the lines are printed as the runner sent them, prefixed by nothing." + jsonRunHelp,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().Int32Var(&attempt, "attempt", 0, "the attempt to read (default: the latest)")
	cmd.Flags().BoolVar(&logFlag, "log", false, "print the log of the jobs instead of the summary")
	cmd.Flags().StringVar(&jobFlag, "job", "", "with --log, only this job (its id or its name)")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
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
		if *asJSON {
			d := jsonRunDetail{jsonRun: toJSONRun(res.Msg.GetRun()), LatestAttempt: res.Msg.GetLatestAttempt(), Jobs: []jsonRunJob{}}
			for _, j := range res.Msg.GetJobs() {
				d.Jobs = append(d.Jobs, toJSONRunJob(j))
			}
			return writeJSON(cmd.OutOrStdout(), d)
		}
		return printRun(cmd.OutOrStdout(), res.Msg)
	}
	return cmd
}

func printRun(w io.Writer, r *ermite.GetRunResponse) error {
	run := r.GetRun()
	pairs := [][2]string{
		{"run", fmt.Sprintf("%d (#%d of %s)", run.GetId(), run.GetNumber(), run.GetWorkflowName())},
		{"result", runResult(run.GetStatus(), run.GetConclusion())},
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
	if j.GetStatus() == ermite.CiStatus_CI_STATUS_QUEUED && !follow {
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
		Long: "Print the state of a run as its jobs change, until it completes. With --log, also stream the log\n" +
			"of its jobs. The exit status is 0 for a run that succeeded and 1 for any other conclusion, so\n" +
			"`ermite run watch` can gate a script.",
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
		c, err := env.client(cmd, true)
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
		Long:              "Ask the runners to stop the jobs of a run that is not over; jobs that no runner has yet are cancelled at once. Needs the ci:write scope.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
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
		Long:              "Start a new attempt of a completed run, with the same commit and workflow file. With --failed, only the jobs that did not succeed (and the jobs that need them) run again. Needs the ci:write scope." + jsonRunHelp,
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
		c, err := env.client(cmd, true)
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
