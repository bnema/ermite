// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// maxJobError bounds JobError text (4 KiB, run.proto).
const maxJobError = 4 << 10

// wfCache keeps the parsed workflows of the runs in flight, so that planning a
// job does not parse the file again.
type wfCache struct {
	mu  sync.Mutex
	max int
	m   map[int64]*workflow.Workflow
	// uses keeps which secrets each job of a run refers to (the names only,
	// never a value), per run then job key: the workflow file of a run does not
	// change, so the walk of its YAML is done once per job.
	uses map[int64]map[string]secretUse
	// called keeps the parsed reusable workflows, by hash of path and content.
	called map[string]*workflow.Workflow
}

func newWfCache(max int) *wfCache {
	return &wfCache{max: max, m: map[int64]*workflow.Workflow{}, uses: map[int64]map[string]secretUse{}, called: map[string]*workflow.Workflow{}}
}

// secretUse returns the secrets the job refers to, walking the YAML once per
// run and job.
func (w *wfCache) secretUse(runID int64, yaml, jobKey string) secretUse {
	w.mu.Lock()
	if u, ok := w.uses[runID][jobKey]; ok {
		w.mu.Unlock()
		return u
	}
	w.mu.Unlock()
	u := secretsUsedBy(yaml, jobKey)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.uses[runID] == nil {
		if len(w.uses) >= w.max {
			clear(w.uses)
		}
		w.uses[runID] = map[string]secretUse{}
	}
	w.uses[runID][jobKey] = u
	return u
}

func (w *wfCache) get(runID int64, path, yaml string) (*workflow.Workflow, error) {
	w.mu.Lock()
	if wf, ok := w.m[runID]; ok {
		w.mu.Unlock()
		return wf, nil
	}
	w.mu.Unlock()
	wf, errs := workflow.Parse(path, []byte(yaml))
	if len(errs) > 0 {
		return nil, fmt.Errorf("the workflow file no longer parses: %s", errs[0].Error())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.m) >= w.max {
		clear(w.m)
	}
	w.m[runID] = wf
	return wf, nil
}

// instances returns the indexes of the records of the job key, in order.
func (rs *runState) instances(key string) []int {
	var out []int
	for i, j := range rs.jobs {
		if j.Key == key {
			out = append(out, i)
		}
	}
	return out
}

func (rs *runState) sortJobs() {
	slices.SortStableFunc(rs.jobs, func(a, b domain.CIJob) int {
		if a.Index != b.Index {
			return a.Index - b.Index
		}
		return a.Combo - b.Combo
	})
}

// result is the aggregate conclusion of the instances of a job as the needs
// context shows it: success, failure, cancelled or skipped.
func result(insts []domain.CIJob) string {
	var failed, cancelled, ok bool
	for _, j := range insts {
		if j.ContinueOnError && j.Conclusion.IsFailure() {
			ok = true // recorded: needs.<job>.result is success
			continue
		}
		switch j.Conclusion {
		case domain.CIFailure, domain.CITimedOut, domain.CIActionRequired:
			failed = true
		case domain.CICancelled:
			cancelled = true
		case domain.CISkipped:
		default:
			ok = true
		}
	}
	switch {
	case failed:
		return "failure"
	case cancelled:
		return "cancelled"
	case ok:
		return "success"
	}
	return "skipped"
}

// needsOf computes the needs context and the status functions of a job from
// the jobs it needs, and whether they all have ended.
//
// The needs context holds the direct needs only, but the status functions look
// at the whole chain (wf.Ancestors): a job whose need ran through always()
// after a failure further up is skipped by its default success() (recorded:
// needs-failed-dependency, chain-after-always), and failure() is true when an
// ancestor failed (documented, not recorded). An ancestor beyond the direct
// needs only changes success() and failure(): cancelled() is not recorded for
// them, and a skipped ancestor behind a need that did run is not either.
func (rs *runState) needsOf(wf *workflow.Workflow, j *workflow.Job, scope string) (needs *expr.Dictionary, st workflow.StatusFuncs, ready bool) {
	needs = expr.NewDictionary()
	st = workflow.StatusFuncs{Success: true, Cancelled: rs.run.CancelRequested}
	for _, n := range wf.Ancestors(j.ID) {
		var insts []domain.CIJob
		for _, i := range rs.instancesIn(scope, n) {
			if rs.jobs[i].Ended() {
				insts = append(insts, rs.jobs[i])
			}
		}
		switch result(insts) {
		case "failure":
			st.Failure, st.Success = true, false
		case "cancelled":
			st.Success = false
		}
	}
	for _, n := range j.Needs {
		var insts []domain.CIJob
		for _, i := range rs.instancesIn(scope, n) {
			if !rs.jobs[i].Ended() {
				return nil, st, false
			}
			insts = append(insts, rs.jobs[i])
		}
		if len(insts) == 0 {
			return nil, st, false
		}
		res := result(insts)
		switch res {
		case "failure":
			st.Failure, st.Success = true, false
		case "cancelled":
			st.Cancelled, st.Success = true, false
		case "skipped":
			st.Success = false
		}
		outs := expr.NewDictionary()
		for _, in := range insts {
			for _, k := range sortedKeys(in.Outputs) {
				// Instances of a matrix share names: the last non-empty value wins.
				if v := in.Outputs[k]; v != "" || !hasKey(outs, k) {
					setKey(outs, k, expr.String(v))
				}
			}
		}
		d := expr.NewDictionary()
		d.Add("result", expr.String(res))
		d.Add("outputs", outs)
		needs.Add(n, d)
	}
	return needs, st, true
}

func hasKey(d *expr.Dictionary, k string) bool { _, ok := d.Get(k); return ok }

// setKey sets a key of a dictionary, replacing an existing value.
func setKey(d *expr.Dictionary, k string, v expr.Value) {
	if !d.Add(k, v) {
		pairs := d.Pairs()
		*d = *expr.NewDictionary()
		for _, p := range pairs {
			if strings.EqualFold(p.Key, k) {
				d.Add(p.Key, v)
			} else {
				d.Add(p.Key, p.Value)
			}
		}
	}
}

// githubValue is the github context of the run with the keys of the job.
func (rs *runState) githubValue(key string, attempt int32) (*expr.Dictionary, error) {
	v, err := expr.FromJSON([]byte(rs.src.GithubJSON))
	if err != nil {
		return nil, fmt.Errorf("the stored github context is not valid: %w", err)
	}
	d, ok := v.(*expr.Dictionary)
	if !ok {
		return nil, fmt.Errorf("the stored github context is not an object")
	}
	setKey(d, "job", expr.String(key))
	setKey(d, "run_attempt", expr.String(strconv.Itoa(int(attempt))))
	return d, nil
}

// trim cuts a message for a job error.
func trim(s string) string { return cutText(s, maxJobError) }

// end ends a job with a conclusion and an error text, completes its steps like
// the server does, and applies fail-fast to its siblings.
func (rs *runState) end(j *domain.CIJob, conclusion domain.CIConclusion, msg string) {
	now := rs.c.now()
	cancelish := conclusion == domain.CICancelled || conclusion == domain.CITimedOut
	for i := range j.Steps {
		s := &j.Steps[i]
		if s.State == domain.CIStepCompleted {
			continue
		}
		s.Conclusion = domain.CICancelled
		if s.State == domain.CIStepPending && !cancelish {
			s.Conclusion = domain.CISkipped
		}
		s.Outcome = s.Conclusion
		s.State, s.CompletedAt = domain.CIStepCompleted, now
	}
	j.Phase, j.Conclusion, j.CompletedAt = domain.CIPhaseEnded, conclusion, now
	if msg != "" {
		j.Error = trim(msg)
	}
	if j.HasToken() {
		j.TokenValidUntil = now.Add(domain.CITokenGrace)
	}
	rs.revokeTokens(j)
	rs.touch(j)
	if j.Deps != nil {
		rs.depsEnded = append(rs.depsEnded, j.ID)
	}
	rs.c.mu.Lock()
	delete(rs.c.heartbeats, jobRef(*j))
	rs.c.mu.Unlock()

	if j.FailFast && !j.ContinueOnError && conclusion.IsFailure() {
		for i := range rs.jobs {
			s := &rs.jobs[i]
			if s.Key != j.Key || s.Scope != j.Scope || s.ID == j.ID || s.Ended() {
				continue
			}
			if s.Active() {
				rs.requestCancel(s, domain.CICancelFailFast, "Canceled because another job of the matrix failed (fail-fast)")
			} else {
				rs.end(s, domain.CICancelled, "")
			}
		}
	}
}

// requestCancel asks the runner of a job to stop, once: the first reason wins.
func (rs *runState) requestCancel(j *domain.CIJob, reason domain.CICancelReason, msg string) {
	if j.CancelReason != domain.CICancelNone || j.Ended() {
		return
	}
	j.CancelReason, j.CancelMessage, j.CancelRequestedAt = reason, msg, rs.c.now()
	rs.touch(j)
}

// cancelAll cancels what is left of the run: jobs that no runner has are over
// at once, the ones a runner has get a cancel request.
func (rs *runState) cancelAll(msg string) {
	for i := range rs.jobs {
		j := &rs.jobs[i]
		switch {
		case j.Ended():
		case j.Active():
			rs.requestCancel(j, domain.CICancelUser, msg)
		default:
			rs.end(j, domain.CICancelled, "")
		}
	}
}

// advance plans the jobs that became ready and promotes queued matrix
// instances, until nothing changes.
func (rs *runState) advance(ctx context.Context) error {
	if !rs.latest() {
		return nil
	}
	if rs.run.CancelRequested {
		rs.cancelAll("Canceled by the owner")
		return nil
	}
	if rs.run.IsDeps() {
		return nil // its only job was created ready: nothing to plan
	}
	wf, err := rs.c.flows.get(rs.run.ID, rs.run.WorkflowPath, rs.src.WorkflowYAML)
	if err != nil {
		for i := range rs.jobs {
			if !rs.jobs[i].Ended() && !rs.jobs[i].Active() {
				rs.end(&rs.jobs[i], domain.CIFailure, err.Error())
			}
		}
		return nil
	}
	if ok, err := rs.acquireRun(ctx, wf); err != nil || !ok {
		return err // waits for its group, or was failed by it
	}
	for changed := true; changed; {
		changed = false
		for _, wj := range wf.Jobs {
			insts := rs.instances(wj.ID)
			if len(insts) != 1 {
				continue
			}
			rec := rs.jobs[insts[0]]
			if rec.Planned || rec.Ended() || rec.Phase != domain.CIPhaseWaiting {
				continue
			}
			needs, st, ready := rs.needsOf(wf, wj, "")
			if !ready {
				continue
			}
			rs.planJob(ctx, wf, wj, insts[0], needs, st)
			changed = true
		}
		// The jobs of called workflows, and the calls that have ended.
		ch, err := rs.advanceCalls(ctx)
		if err != nil {
			return err
		}
		if ch {
			changed = true
		}
	}
	ended, err := rs.promote(ctx)
	if err != nil || !ended {
		return err
	}
	// A job that no runner can take ended: the jobs that needed it, and the
	// instances max-parallel held back, are due.
	return rs.advance(ctx)
}

// promote makes planned instances ready while max-parallel allows it and the
// concurrency group of the job, if it has one of its own, gives it the turn.
// An instance that waits for its group is pending. A job that no registered
// runner (online or not) can take ends at once, which promote reports.
func (rs *runState) promote(ctx context.Context) (ended bool, err error) {
	now := rs.c.now()
	var runners []domain.CIRunner
	listed, listFailed := false, false
	counts := map[string]int{}
	for _, j := range rs.jobs {
		if j.Phase == domain.CIPhaseReady || j.Active() {
			counts[j.Scope+"\x00"+j.Key]++
		}
	}
	for i := range rs.jobs {
		j := &rs.jobs[i]
		if !j.Planned || j.Phase != domain.CIPhaseWaiting || j.Call != nil {
			continue // a call never runs: it ends with the jobs of its scope
		}
		if j.MaxParallel > 0 && counts[j.Scope+"\x00"+j.Key] >= j.MaxParallel {
			continue
		}
		if !listed && !listFailed {
			var err error
			if runners, err = rs.c.cfg.Store.ListRunners(ctx); err != nil {
				// A failed listing keeps the jobs queued: nothing is known.
				ctxLog(ctx).Warn().Err(err).Msg("listing the runners to match runs-on failed")
				listFailed = true
			} else {
				listed = true
			}
		}
		if listed && !runnerMatches(*j, runners) {
			rs.end(j, domain.CIFailure, noRunnerMessage(j.Labels))
			ended = true
			continue
		}
		if g := j.Concurrency; g.Group != "" && !g.FromRun {
			st, err := rs.acquire(ctx, g.Group, domain.CIConcurrencyEntry{RunID: j.RunID, Attempt: j.RunAttempt, JobID: j.ID, Cancel: g.Cancel, Max: g.Max})
			if err != nil {
				return ended, err
			}
			if st != g.State {
				j.Concurrency.State = st
				rs.touch(j)
			}
			if st != domain.CIConcurrencyHolding {
				continue
			}
		}
		j.Phase, j.ReadyAt = domain.CIPhaseReady, now
		counts[j.Scope+"\x00"+j.Key]++
		rs.touch(j)
	}
	return ended, nil
}

// runnerMatches reports whether a registered runner has the labels of the job,
// with the same rule the runners use to claim it.
func runnerMatches(j domain.CIJob, runners []domain.CIRunner) bool {
	return slices.ContainsFunc(runners, func(r domain.CIRunner) bool { return domain.LabelsMatch(j.Labels, r.Labels) })
}

func noRunnerMessage(labels []string) string {
	return "no registered runner matches runs-on: [" + strings.Join(labels, ", ") + "]"
}

// plan evaluates a job whose needs have ended: its condition, its strategy and
// the parts of every instance the server decides (name, labels, timeout,
// contexts). The record at idx becomes the first instance.
func (rs *runState) planJob(ctx context.Context, wf *workflow.Workflow, wj *workflow.Job, idx int, needs *expr.Dictionary, st workflow.StatusFuncs) {
	rec := rs.jobs[idx]
	fail := func(msg string) { rs.end(&rs.jobs[idx], domain.CIFailure, msg) }

	// github.job is empty while the job is evaluated (if, strategy, name,
	// runs-on...): the runner sets it. Recorded: ctx-github-names, where
	// "name: Display name of ${{ github.job }}" shows as "Display name of".
	gh, err := rs.githubValue("", rs.run.Attempt)
	if err != nil {
		fail(err.Error())
		return
	}
	vars, err := rs.c.varsContext(ctx, rs.run.Repo)
	if err != nil {
		fail(err.Error())
		return
	}
	env := template.Env{Contexts: map[string]expr.Value{
		"github": gh, "needs": needs, "inputs": rs.inputsOf(rec), "vars": vars,
	}}
	run, err := wj.EvaluateIf(ctx, env, st)
	switch {
	case err != nil:
		fail(err.Error())
		return
	case !run:
		rs.jobs[idx].Name = rs.skippedName(ctx, wj, env)
		if call := rs.scopeOf(rec); call != nil {
			rs.jobs[idx].Name = call.Name + " / " + rs.jobs[idx].Name
		}
		rs.end(&rs.jobs[idx], domain.CISkipped, "")
		return
	}
	if wj.IsReusable() {
		if err := rs.planCall(ctx, wf, wj, idx, env); err != nil {
			if text, ok := isCallFailure(err); ok {
				fail(text)
			} else {
				fail("reusable workflow " + wj.Uses.Value + ": " + err.Error())
			}
		}
		return
	}
	plan, err := wj.EvaluateStrategy(ctx, env)
	if err != nil {
		fail(err.Error())
		return
	}

	if err := rs.checkJobBudget(len(plan.Combos) - 1); err != nil {
		fail(err.Error())
		return
	}
	insts := make([]domain.CIJob, 0, len(plan.Combos))
	for i := range plan.Combos {
		menv, err := plan.MatrixEnv(env, i)
		if err != nil {
			fail(err.Error())
			return
		}
		inst, err := rs.instance(ctx, wf, wj, plan, rec, i, menv, needs)
		if err != nil {
			fail(err.Error())
			return
		}
		insts = append(insts, inst)
	}
	rs.jobs[idx] = insts[0]
	rs.touch(&rs.jobs[idx])
	for _, inst := range insts[1:] {
		rs.jobs = append(rs.jobs, inst)
		rs.dirty[inst.ID] = true
	}
	rs.sortJobs()
}

// skippedName is the best name for a job that did not run: its evaluated name
// when that does not read the matrix, else its key.
func (rs *runState) skippedName(ctx context.Context, wj *workflow.Job, env template.Env) string {
	if wj.HasName {
		if name, err := template.EvaluateString(ctx, wj.Name, env); err == nil && name != "" {
			return name
		}
	}
	return wj.ID
}

// instance builds the planned record of combination i of a job.
func (rs *runState) instance(ctx context.Context, wf *workflow.Workflow, wj *workflow.Job, plan *workflow.Strategy, base domain.CIJob, i int, env template.Env, needs *expr.Dictionary) (domain.CIJob, error) {
	j := base
	j.Combo = i
	j.ID = domain.NewJobID(rs.run.ID, base.Index, i)
	if i > 0 {
		j.Version = 0
	}
	name, err := wj.DisplayName(ctx, env, plan, i)
	if err != nil {
		return j, err
	}
	name = strings.TrimSpace(name) // GitHub shows "Display name of", not "Display name of "
	call := rs.scopeOf(base)
	if call != nil {
		name = call.Name + " / " + name // "<caller job name> / <callee job name>"
	}
	labels, _, err := wj.EvaluateRunsOn(ctx, env)
	if err != nil {
		return j, err
	}
	if len(labels) == 0 {
		return j, fmt.Errorf("job '%s' has no runs-on labels", wj.ID)
	}
	timeout := int32(domain.CIDefaultTimeoutMinutes)
	if wj.TimeoutMinutes != nil {
		n, err := template.EvaluateNumber(ctx, wj.TimeoutMinutes, env)
		if err != nil {
			return j, err
		}
		timeout = int32(min(max(math.Round(n), 1), 525600)) //nolint:gosec // clamped
	}
	gh, err := rs.githubValue(wj.ID, rs.run.Attempt)
	if err != nil {
		return j, err
	}
	matrix := plan.Combos[i].Values
	var matrixCtx expr.Value = matrix
	if !plan.HasMatrix {
		matrixCtx = expr.Null{} // GitHub: "matrix": null for a job without a matrix
	}
	environment, err := environmentOf(ctx, wj, env)
	if err != nil {
		return j, err
	}
	// The vars of planning (see planJob), not read again for each leg.
	vars, _ := env.Contexts["vars"].(*expr.Dictionary)
	if vars == nil {
		vars = expr.NewDictionary()
	}
	contexts, err := contextsJSON(gh, needs, plan.Context(i), matrixCtx, vars, rs.inputsOf(base), rs.jobKeysOf(ctx, base, i))
	if err != nil {
		return j, err
	}
	j.Environment = environment
	if call != nil {
		if j.Permissions, err = rs.scopedPermissions(wf, wj, call); err != nil {
			return j, err
		}
	} else {
		j.Permissions = domain.ResolvePermissions(permissionSpec(wf.Permissions), permissionSpec(wj.Permissions))
	}
	j.Name, j.Labels, j.TimeoutMinutes = name, labels, timeout
	j.MatrixJSON = compactJSON(expr.ToJSON(matrix))
	j.ContextsJSON, j.ComboTotal = contexts, len(plan.Combos)
	j.FailFast, j.MaxParallel = plan.FailFast, plan.MaxParallel
	j.ContinueOnError = false
	if wj.ContinueOnError != nil {
		// Evaluated with github, needs, strategy, matrix, vars and inputs.
		coe, err := template.EvaluateBool(ctx, wj.ContinueOnError, env)
		if err != nil {
			return j, err
		}
		j.ContinueOnError = coe
	}
	if wj.Concurrency != nil {
		// Evaluated with github, needs, strategy, matrix, inputs and vars;
		// this leg's own group, which it takes before it may run.
		conc, err := evalConcurrency(ctx, wj.Concurrency, env)
		if err != nil {
			return j, err
		}
		conc.Evaluated = true
		j.Concurrency = conc
	}
	j.Planned, j.Phase = true, domain.CIPhaseWaiting
	return j, nil
}

func compactJSON(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return "{}"
	}
	return b.String()
}

// contextsJSON is the JSON object of the contexts the server computes for a
// job (Job.contexts in runner.proto).
//
// "job" holds the keys of the job context the server knows (check_run_id,
// workflow_ref, workflow_sha, workflow_repository, workflow_file_path); the
// runner adds status. The runner copies the keys of this "job" into its job
// context (executor.State.initRest), whatever they are.
func contextsJSON(github, needs, strategy *expr.Dictionary, matrix expr.Value, vars, inputs *expr.Dictionary, job ordered) (string, error) {
	out := map[string]json.RawMessage{}
	for k, v := range map[string]expr.Value{
		"github": github, "needs": needs, "strategy": strategy, "matrix": matrix,
		"inputs": inputs, "vars": vars,
	} {
		out[k] = json.RawMessage(compactJSON(expr.ToJSON(v)))
	}
	jb, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	out["job"] = jb
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// permissionSpec reads the "permissions:" of a workflow or a job, nil for none.
func permissionSpec(p *workflow.Permissions) *domain.PermissionSpec {
	if p == nil {
		return nil
	}
	spec := &domain.PermissionSpec{All: p.All, Levels: make(map[string]string, len(p.Scopes))}
	for _, sc := range p.Scopes {
		spec.Levels[sc.Scope] = sc.Level
	}
	return spec
}
