// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// The events workflow_dispatch, schedule and workflow_run (CI-3, D5), and create
// and delete (ci_refevents.go). They share
// the run machinery of the push (startRun); what differs is the github context
// and, for workflow_dispatch, the typed inputs.

const (
	eventDispatch    = "workflow_dispatch"
	eventSchedule    = "schedule"
	eventWorkflowRun = "workflow_run"
	eventPullTarget  = "pull_request_target"

	// maxDispatchInputs is the number of top-level inputs of a workflow_dispatch
	// (docs.github.com, "Events that trigger workflows", workflow_dispatch: up to
	// 25 top-level inputs; recalled from memory, not fetched), and
	// maxDispatchPayload the size of their values together in characters (the
	// REST reference of "Create a workflow dispatch event": at most 65,535).
	maxDispatchInputs  = 25
	maxDispatchPayload = 65535
)

// customEvent is what a run of workflow_dispatch, schedule, workflow_run,
// create or delete is for, beside the commit (pushFacts.custom).
type customEvent struct {
	name  string
	title string
	// inputs are the values of github.event.inputs (strings), nil when the
	// workflow declares none (workflow_dispatch).
	inputs map[string]string
	// cron is the schedule entry that fired (schedule).
	cron string
	// triggered is the run a workflow_run is for.
	triggered *workflowRunFacts
	// ref is the branch or tag created or deleted (create, delete).
	ref string
}

// workflowRunFacts is the triggering run of a workflow_run.
type workflowRunFacts struct {
	run domain.CIRun
	// action is "requested" or "completed".
	action string
	// headBranch is the branch of the triggering run: the source branch of a
	// merge request, else the branch or tag the run is on.
	headBranch string
}

// resolveDispatchInputs checks the values given to a workflow_dispatch against
// the inputs the workflow declares and returns github.event.inputs: the value
// given, else the default, else (for a choice) its first option; an optional
// input with none of them is absent. Errors are domain.ErrInvalid.
func resolveDispatchInputs(defs []workflow.Input, given map[string]string) (map[string]string, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, args...))
	}
	if len(defs) > maxDispatchInputs {
		return nil, invalid("the workflow declares %d inputs: at most %d are allowed", len(defs), maxDispatchInputs)
	}
	var unknown []string
	for k := range given {
		if !slices.ContainsFunc(defs, func(in workflow.Input) bool { return in.Name == k }) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, invalid("Unexpected inputs provided: [%s]", strings.Join(unknown, ", "))
	}
	out := map[string]string{}
	size := 0
	for _, in := range defs {
		v, ok := given[in.Name]
		if ok && v == "" && in.Required && !in.HasDefault {
			ok = false
		}
		if !ok {
			switch {
			case in.HasDefault:
				v, ok = in.Default, true
			case in.Required:
				return nil, invalid("Required input '%s' not provided", in.Name)
			case in.Type == "choice" && len(in.Options) > 0:
				v, ok = in.Options[0], true
			}
		}
		if !ok {
			continue
		}
		if err := checkInputValue(in, v); err != nil {
			return nil, err
		}
		size += len(in.Name) + len(v)
		out[in.Name] = v
	}
	if size > maxDispatchPayload {
		return nil, invalid("the inputs are larger than %d characters", maxDispatchPayload)
	}
	return out, nil
}

// checkInputValue checks the text of one input against its type.
func checkInputValue(in workflow.Input, v string) error {
	bad := func(what string) error {
		return fmt.Errorf("%w: input '%s' %s", domain.ErrInvalid, in.Name, what)
	}
	switch in.Type {
	case "boolean":
		if v != "true" && v != "false" {
			return bad("must be 'true' or 'false'")
		}
	case "number":
		if _, ok := parseInputNumber(v); !ok {
			return bad("must be a number")
		}
	case "choice":
		if !slices.Contains(in.Options, v) {
			return bad("must be one of: " + strings.Join(in.Options, ", "))
		}
	case "environment":
		if _, err := domain.ParseEnvironmentName(v); err != nil {
			return bad("must be the name of an environment")
		}
	}
	return nil
}

// parseInputNumber reads a decimal number: no hexadecimal, no underscores, no
// NaN or infinity.
func parseInputNumber(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "xX_pP") {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// typedInputs is the "inputs" context of a workflow_dispatch run: each declared
// input with the type of its declaration (boolean, number or string), the zero
// of the type when it has no value.
func typedInputs(defs []workflow.Input, values map[string]string) *expr.Dictionary {
	d := expr.NewDictionary()
	for _, in := range defs {
		s, ok := values[in.Name]
		switch in.Type {
		case "boolean":
			d.Add(in.Name, expr.Boolean(ok && s == "true"))
		case "number":
			f, _ := parseInputNumber(s)
			d.Add(in.Name, expr.Number(f))
		default:
			d.Add(in.Name, expr.String(s))
		}
	}
	return d
}

// eventInputs is the "inputs" context of the top level of a run: the typed
// inputs of a workflow_dispatch run (read back from github.event.inputs of its
// github context), empty for every other event.
func eventInputs(wf *workflow.Workflow, githubJSON string) *expr.Dictionary {
	if wf == nil || wf.On == nil || !strings.Contains(githubJSON, `"event_name":"`+eventDispatch+`"`) {
		return expr.NewDictionary()
	}
	ev := wf.On.Event(eventDispatch)
	if ev == nil {
		return expr.NewDictionary()
	}
	values := map[string]string{}
	if v, err := expr.FromJSON([]byte(githubJSON)); err == nil {
		if gh, ok := v.(*expr.Dictionary); ok {
			if e, ok := gh.Get("event"); ok {
				if ed, ok := e.(*expr.Dictionary); ok {
					if in, ok := ed.Get("inputs"); ok {
						if id, ok := in.(*expr.Dictionary); ok {
							for _, p := range id.Pairs() {
								values[p.Key] = expr.CoerceString(p.Value)
							}
						}
					}
				}
			}
		}
	}
	return typedInputs(ev.Inputs, values)
}

// customGithubJSON is the github context of a workflow_dispatch, schedule,
// workflow_run, create or delete run.
func (c *CI) customGithubJSON(repo domain.Repo, run domain.CIRun, f pushFacts) string {
	u := c.urls(repo)
	id := c.identity(run.Actor)
	cu := f.custom
	event := map[string]any{
		"repository": c.repoObj(repo, u, f.defaultBranch, run.CreatedAt),
		"sender":     u.userObj(id, false),
	}
	switch cu.name {
	case eventDispatch:
		event["ref"] = run.Ref
		event["workflow"] = run.WorkflowPath
		if cu.inputs != nil {
			event["inputs"] = cu.inputs
		} else {
			event["inputs"] = nil
		}
	case eventSchedule:
		event["schedule"] = cu.cron
	case eventWorkflowRun:
		t := cu.triggered
		event["action"] = t.action
		event["workflow_run"] = c.workflowRunObj(repo, u, t)
		event["workflow"] = c.workflowObj(u, t.run.WorkflowPath, t.run.WorkflowName, t.run.CreatedAt)
	case eventCreate, eventDelete:
		refEventPayload(cu.name, cu.ref, f.defaultBranch, event)
	}
	refName, refType := strings.TrimPrefix(run.Ref, "refs/heads/"), "branch"
	if tag, ok := strings.CutPrefix(run.Ref, "refs/tags/"); ok {
		refName, refType = tag, "tag"
	}
	return c.githubContext(repo, run, ghEvent{name: cu.name, refName: refName, refType: refType, payload: event})
}

// workflowObj is the "workflow" object of a payload.
func (c *CI) workflowObj(u ghURLs, path, name string, at interface{ Unix() int64 }) map[string]any {
	id := stableID("workflow", u.full+"\x00"+path)
	return map[string]any{
		"id": id, "node_id": nodeID("02", "Workflow", id), "name": name, "path": path, "state": "active",
		"url":        u.api + "/repos/" + u.full + "/actions/workflows/" + strconv.FormatInt(id, 10),
		"html_url":   u.repo + "/blob/HEAD/" + path,
		"badge_url":  u.repo + "/workflows/" + path + "/badge.svg",
		"created_at": at.Unix(), "updated_at": at.Unix(),
	}
}

// workflowRunObj is the "workflow_run" object of the payload: the fields ermite
// can fill about the triggering run.
func (c *CI) workflowRunObj(repo domain.Repo, u ghURLs, t *workflowRunFacts) map[string]any {
	r := t.run
	var conclusion any
	if r.Conclusion != domain.CINoConclusion {
		conclusion = string(r.Conclusion)
	}
	id := strconv.FormatInt(r.ID, 10)
	actor := u.userObj(c.identity(r.Actor), false)
	obj := map[string]any{
		"id": r.ID, "name": r.WorkflowName, "display_title": r.Title, "head_branch": t.headBranch, "head_sha": r.CheckSha(),
		"path": r.WorkflowPath, "run_number": r.Number, "run_attempt": r.Attempt, "event": r.Event,
		"status": string(r.Status), "conclusion": conclusion,
		"workflow_id": stableID("workflow", u.full+"\x00"+r.WorkflowPath),
		"url":         u.api + "/repos/" + u.full + "/actions/runs/" + id,
		"html_url":    u.repo + "/actions/runs/" + id,
		"actor":       actor, "triggering_actor": actor,
		"created_at": r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"updated_at": c.now().Format("2006-01-02T15:04:05Z07:00"),
		"repository": c.repoObj(repo, u, "", r.CreatedAt),
	}
	if !r.StartedAt.IsZero() {
		obj["run_started_at"] = r.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return obj
}
