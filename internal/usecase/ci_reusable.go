// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// Reusable workflows (CI-3, D7). ermite owns the job graph, so a job with
// "uses:" is expanded at planning, not by the runner:
//
//   - the job becomes a call record (one per matrix leg) that never runs; it ends
//     when the jobs of its scope have, with their aggregate conclusion, and its
//     outputs are the callee's "on.workflow_call.outputs";
//   - each job of the called workflow becomes a record of that scope, keyed
//     "<caller key>/<callee id>", named "<caller name> / <callee name>", planned
//     when the callee's own "needs" have ended (the caller's "needs" ended
//     before the call was expanded, so they hold for every callee root);
//   - the called file is kept in the call record, so every job of the call runs
//     from the file of the commit that was resolved, and the runner is given that
//     file and the callee job id.
//
// Texts are the ones of actions/runner (ReusableWorkflowsLoader.cs,
// WorkflowTemplateConverter.cs, PermissionsHelper.cs) and languageservices
// (validate.ts) where they exist.

// maxReusableWorkflowDepth is the nesting limit of reusable workflows
// (languageservices workflow-parser/src/model/convert.ts:49, 4; the runner's
// check is ReusableWorkflowsLoader.LoadRecursive).
const maxReusableWorkflowDepth = 4

// maxJobIndex is the highest job position a check id can hold (checkIndexBits).
const maxJobIndex = 1<<checkIndexBits - 1

// callRef is a parsed "uses:" of a job.
type callRef struct {
	local       bool
	owner, repo string
	path        string // .github/workflows/x.yml
	ref         string
}

// parseCallRef reads the "uses:" of a job calling a workflow. The texts are
// languageservices' (validate.ts validateWorkflowUsesFormat).
func parseCallRef(uses string) (callRef, string) {
	bad := func(reason string) (callRef, string) {
		return callRef{}, "Invalid workflow reference '" + uses + "': " + reason
	}
	if strings.HasPrefix(uses, "./.github/workflows/") {
		switch {
		case strings.Contains(uses, "@"):
			return bad("cannot specify version when calling local workflows")
		case !strings.HasSuffix(uses, ".yml") && !strings.HasSuffix(uses, ".yaml"):
			return bad("workflow file should have either a '.yml' or '.yaml' file extension")
		case len(strings.Split(uses, "/")) != 4:
			return bad("workflows must be defined at the top level of the .github/workflows/ directory")
		case strings.HasSuffix(uses, "/.yml") || strings.HasSuffix(uses, "/.yaml"):
			return bad("invalid workflow file name")
		}
		return callRef{local: true, path: strings.TrimPrefix(uses, "./")}, ""
	}
	if strings.HasPrefix(uses, "./") {
		return bad("local workflow references must be rooted in '.github/workflows'")
	}
	parts := strings.Split(uses, "@")
	switch {
	case len(parts) == 1 || (len(parts) == 2 && parts[1] == ""):
		return bad("no version specified")
	case len(parts) > 2:
		return bad("too many '@' in workflow reference")
	}
	i := strings.Index(parts[0], ".github/workflows/")
	if i < 0 {
		return bad("references to workflows must be rooted in '.github/workflows'")
	}
	nwo := strings.FieldsFunc(parts[0][:i], func(r rune) bool { return r == '/' })
	if len(nwo) != 2 {
		return bad("references to workflows must be prefixed with format 'owner/repository/' or './' for local workflows")
	}
	if !validActionName(nwo[0]) || !validActionName(nwo[1]) {
		return bad("owner name must be a valid repository owner name")
	}
	path := parts[0][i:]
	for _, s := range strings.Split(path, "/") {
		if s == "" || s == "." || s == ".." {
			return bad("references to workflows must be rooted in '.github/workflows'")
		}
	}
	return callRef{owner: nwo[0], repo: nwo[1], path: path, ref: parts[1]}, ""
}

// callFailure is a call that cannot be expanded; text is the job's error.
type callFailure struct{ text string }

func (e callFailure) Error() string { return e.text }

func isCallFailure(err error) (string, bool) {
	var cf callFailure
	if errors.As(err, &cf) {
		return cf.text, true
	}
	return "", false
}

// calledFile is a workflow file read for a call.
type calledFile struct {
	repository, path, ref, sha string
	yaml                       []byte
}

// loadCallee finds the file a "uses:" names. home is the call whose file holds
// the "uses:" (nil for the run's own workflow): a local reference is read in
// the repository and at the commit of that file.
func (c *CI) loadCallee(ctx context.Context, rs *runState, home *domain.CICall, rec domain.CIJob, ref callRef) (calledFile, error) {
	var f calledFile
	if ref.local {
		f.path = ref.path
		if home == nil {
			f.repository, f.sha = rs.run.Repo.String(), rs.run.Sha
		} else {
			f.repository, f.sha = home.Repository, home.Sha
		}
		return f, c.readCallee(ctx, &f)
	}
	f.path, f.ref = ref.path, ref.ref
	ar := actionRef{uses: ref.owner + "/" + ref.repo + "@" + ref.ref, owner: ref.owner, repo: ref.repo, ref: ref.ref}
	by := c.onBehalfOf(*rs.src)
	// One resolution per reference and attempt: what this planning resolved
	// already (not stored yet), else what the attempt stored, else a fresh one.
	var res domain.CIActionResolution
	found := false
	for _, r := range rs.resolutions {
		if r.RunID == rec.RunID && r.RunAttempt == rec.RunAttempt && r.Key == ar.key() {
			res, found = r, true
			break
		}
	}
	if !found {
		stored, err := c.cfg.Store.GetActionResolution(ctx, rec.RunID, rec.RunAttempt, ar.key())
		switch {
		case err == nil:
			res = stored
		case errors.Is(err, domain.ErrNotFound):
			var msg string
			res, msg, err = c.resolveAction(ctx, rec, ar, c.newRemoteRefs(rec.RunAttempt), by)
			if err != nil {
				return f, err
			}
			if msg != "" {
				return f, callFailure{msg}
			}
			rs.resolutions = append(rs.resolutions, res)
		default:
			return f, err
		}
	}
	// The source guard of ResolveActions (answer), whatever the resolution came
	// from: a repository of this server that the actor cannot read is never
	// served from anywhere else, and a stored ermite resolution of a repository
	// that is gone has nothing to serve.
	repo, onErmite, err := c.ermiteRepoOf(ctx, ar)
	if err != nil {
		return f, err
	}
	if onErmite {
		if ok, err := c.pusherCanRead(ctx, by, rs.run.Repo, repo); err != nil {
			return f, err
		} else if !ok {
			return f, callFailure{errUnresolvedRepo(ar)}
		}
	}
	f.sha = res.ResolvedSHA
	switch res.Source {
	case domain.CIActionErmite:
		if !onErmite {
			return f, callFailure{errUnresolvedRepo(ar)}
		}
		f.repository = repo.Name.String()
	default:
		f.repository = ref.owner + "/" + ref.repo
	}
	return f, c.readCallee(ctx, &f)
}

// readCallee fills f.yaml from the repository of the server or github.com.
func (c *CI) readCallee(ctx context.Context, f *calledFile) error {
	notFound := callFailure{"Unable to find reusable workflow '" + f.path + "' in '" + f.repository + "' at '" + f.sha + "'"}
	if owner, repo, ok := strings.Cut(f.repository, "/"); ok {
		if c.cfg.Workflows == nil {
			return fmt.Errorf("%w: this server cannot reach github.com", domain.ErrUnavailable)
		}
		data, err := c.cfg.Workflows.FetchWorkflow(ctx, owner, repo, f.sha, f.path, maxCIWorkflowBytes)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return notFound
		case errors.Is(err, domain.ErrResourceExhausted):
			return callFailure{f.path + ": the workflow file is larger than 1 MiB"}
		case err != nil:
			return err
		}
		f.yaml = data
		return nil
	}
	name, err := domain.ParseRepoName(f.repository)
	if err != nil {
		return notFound
	}
	id, err := domain.ParseObjectID(f.sha)
	if err != nil {
		return notFound
	}
	tp, err := domain.ParseTreePath(f.path)
	if err != nil {
		return notFound
	}
	_, rc, err := c.cfg.Reader.OpenBlob(ctx, name, domain.RevisionOf(id), tp)
	if errors.Is(err, domain.ErrNotFound) {
		return notFound
	}
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxCIWorkflowBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxCIWorkflowBytes {
		return callFailure{f.path + ": the workflow file is larger than 1 MiB"}
	}
	f.yaml = data
	return nil
}

// callerPrefix is the text GitHub puts before an error of the called workflow
// (TemplateStrings: CalledWorkflowNotValidWithErrors, LineColumn).
func callerPrefix(wj *workflow.Job) string {
	file, pos := "", template.Pos{}
	if wj.Uses != nil {
		file, pos = wj.Uses.File(), wj.Uses.Pos()
	}
	return fmt.Sprintf("In %s (Line: %d, Col: %d): Error from called workflow", file, pos.Line, pos.Col)
}

// calleeSpec is what ermite reads of "on.workflow_call" that the workflow model
// does not keep: the outputs and the secrets (ermite-actions parses the inputs).
type calleeSpec struct {
	outputs map[string]string
	secrets map[string]bool // name (upper case) to required
}

// readCalleeSpec reads on.workflow_call.outputs and .secrets from the YAML.
func readCalleeSpec(src []byte) calleeSpec {
	spec := calleeSpec{outputs: map[string]string{}, secrets: map[string]bool{}}
	var doc yaml.Node
	if yaml.Unmarshal(src, &doc) != nil || len(doc.Content) == 0 {
		return spec
	}
	root := derefYAML(doc.Content[0])
	for i := 0; root.Kind == yaml.MappingNode && i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "on" {
			continue
		}
		on := derefYAML(root.Content[i+1])
		for j := 0; on.Kind == yaml.MappingNode && j+1 < len(on.Content); j += 2 {
			if on.Content[j].Value != "workflow_call" {
				continue
			}
			wc := derefYAML(on.Content[j+1])
			for k := 0; wc.Kind == yaml.MappingNode && k+1 < len(wc.Content); k += 2 {
				body := derefYAML(wc.Content[k+1])
				for m := 0; body.Kind == yaml.MappingNode && m+1 < len(body.Content); m += 2 {
					name, def := body.Content[m].Value, derefYAML(body.Content[m+1])
					switch wc.Content[k].Value {
					case "outputs":
						for q := 0; def.Kind == yaml.MappingNode && q+1 < len(def.Content); q += 2 {
							if def.Content[q].Value == "value" {
								spec.outputs[name] = derefYAML(def.Content[q+1]).Value
							}
						}
					case "secrets":
						req := false
						for q := 0; def.Kind == yaml.MappingNode && q+1 < len(def.Content); q += 2 {
							if def.Content[q].Value == "required" {
								req = derefYAML(def.Content[q+1]).Value == "true"
							}
						}
						spec.secrets[strings.ToUpper(name)] = req
					}
				}
			}
		}
	}
	return spec
}

func derefYAML(n *yaml.Node) *yaml.Node {
	for range 16 {
		if n.Kind != yaml.AliasNode || n.Alias == nil {
			return n
		}
		n = n.Alias
	}
	return n
}

// callInputs checks the "with:" of a call against the inputs of the called
// workflow and returns the inputs context: the value given, else the default,
// else the zero of the type. Texts: WorkflowTemplateConverter.cs
// (ConvertToWorkflowJobInputs, ValidateUndefinedParameters).
func callInputs(ctx context.Context, defs []workflow.Input, given *expr.Dictionary, defEnv template.Env) (*expr.Dictionary, error) {
	out := expr.NewDictionary()
	byName := map[string]bool{}
	for _, in := range defs {
		byName[strings.ToLower(in.Name)] = true
		var v expr.Value
		have := false
		if given != nil {
			for _, p := range given.Pairs() {
				if strings.EqualFold(p.Key, in.Name) {
					cv, err := coerceInput(in, p.Value)
					if err != nil {
						return nil, err
					}
					v, have = cv, true
				}
			}
		}
		if !have && in.HasDefault {
			dv, err := evalInterpolated(ctx, in.Default, defEnv)
			if err != nil {
				return nil, err
			}
			cv, err := coerceInput(in, dv)
			if err != nil {
				return nil, err
			}
			v, have = cv, true
		}
		if !have && in.Required {
			return nil, callFailure{"Input " + in.Name + " is required, but not provided while calling."}
		}
		if !have {
			switch in.Type {
			case "boolean":
				v = expr.Boolean(false)
			case "number":
				v = expr.Number(0)
			default:
				v = expr.String("")
			}
		}
		out.Add(in.Name, v)
	}
	if given != nil {
		for _, p := range given.Pairs() {
			if !byName[strings.ToLower(p.Key)] {
				return nil, callFailure{"Invalid input, " + p.Key + " is not defined in the referenced workflow."}
			}
		}
	}
	return out, nil
}

// coerceInput converts a value to the type of an input, as the template engine
// does for a boolean, number or string (GetDefaultValueByType,
// ConvertToInputValueDefinedType): a mismatch is "Unexpected value".
func coerceInput(in workflow.Input, v expr.Value) (expr.Value, error) {
	unexpected := func() (expr.Value, error) {
		return nil, callFailure{"Unexpected value '" + expr.CoerceString(v) + "' for input '" + in.Name + "' of type '" + in.Type + "'"}
	}
	switch in.Type {
	case "boolean":
		switch x := v.(type) {
		case expr.Boolean:
			return x, nil
		case expr.String:
			switch strings.ToLower(string(x)) {
			case "true":
				return expr.Boolean(true), nil
			case "false":
				return expr.Boolean(false), nil
			}
		}
		return unexpected()
	case "number":
		switch x := v.(type) {
		case expr.Number:
			return x, nil
		case expr.String:
			if f, err := strconv.ParseFloat(strings.TrimSpace(string(x)), 64); err == nil {
				return expr.Number(f), nil
			}
		}
		return unexpected()
	}
	switch v.(type) {
	case expr.String, expr.Number, expr.Boolean:
		if s, ok := v.(expr.String); ok {
			return s, nil
		}
		return expr.String(expr.CoerceString(v)), nil
	}
	return unexpected()
}

// evalInterpolated evaluates a string with ${{ }} expressions (a default of an
// input, the value of an output). A string that is one expression keeps the
// type of its value.
func evalInterpolated(ctx context.Context, s string, env template.Env) (expr.Value, error) {
	if !strings.Contains(s, "${{") {
		return expr.String(s), nil
	}
	ctxNames := make([]string, 0, len(env.Contexts))
	for k := range env.Contexts {
		ctxNames = append(ctxNames, k)
	}
	eval := func(src string) (expr.Value, error) {
		node, err := expr.Parse(src, ctxNames, exprFuncs)
		if err != nil {
			return nil, err
		}
		return expr.Evaluate(ctx, node, expr.EvalContext{Contexts: env.Contexts})
	}
	exprs := expressionsIn(s)
	if t := strings.TrimSpace(s); len(exprs) == 1 && strings.HasPrefix(t, "${{") && strings.HasSuffix(t, "}}") && strings.Count(t, "${{") == 1 {
		return eval(exprs[0])
	}
	var b strings.Builder
	rest := s
	for _, src := range exprs {
		i := strings.Index(rest, "${{")
		b.WriteString(rest[:i])
		v, err := eval(src)
		if err != nil {
			return nil, err
		}
		b.WriteString(expr.CoerceString(v))
		rest = rest[i+3+len(src)+2:]
	}
	b.WriteString(rest)
	return expr.String(b.String()), nil
}

// calleeOf parses the workflow a call record kept.
func (rs *runState) calleeOf(call *domain.CIJob) (*workflow.Workflow, error) {
	if call == nil || call.Call == nil {
		return nil, fmt.Errorf("the call of this job is gone")
	}
	return rs.c.flows.parseCalled(call.Call.Path, call.Call.YAML)
}

// scopeOf finds the call record of a scoped job, nil for a job of the run's own
// workflow.
func (rs *runState) scopeOf(j domain.CIJob) *domain.CIJob {
	if j.Scope == "" {
		return nil
	}
	return rs.find(j.Scope)
}

// inputsOf is the "inputs" context of a job: its call's inputs, empty at the
// top.
func (rs *runState) inputsOf(j domain.CIJob) *expr.Dictionary {
	if sc := rs.scopeOf(j); sc != nil && sc.Call != nil {
		if v, err := expr.FromJSON([]byte(sc.Call.InputsJSON)); err == nil {
			if d, ok := v.(*expr.Dictionary); ok {
				return d
			}
		}
		return expr.NewDictionary()
	}
	if j.Scope == "" && rs.run.Event == eventDispatch {
		// The inputs of a workflow_dispatch run, typed by their declaration.
		if wf, err := rs.c.flows.get(rs.run.ID, rs.run.WorkflowPath, rs.src.WorkflowYAML); err == nil {
			return eventInputs(wf, rs.src.GithubJSON)
		}
	}
	return expr.NewDictionary()
}

// instancesIn returns the indexes of the records of job id in a scope.
func (rs *runState) instancesIn(scope, id string) []int {
	var out []int
	for i, j := range rs.jobs {
		if j.Scope == scope && ((scope == "" && j.Key == id) || (scope != "" && j.CallID == id)) {
			out = append(out, i)
		}
	}
	return out
}

// rootKey is the key of the job of the run's own workflow a record descends
// from.
func rootKey(key string) string {
	root, _, _ := strings.Cut(key, "/")
	return root
}

// nextIndex is a position for a job added to the run.
func (rs *runState) nextIndex() int {
	n := 0
	for _, j := range rs.jobs {
		n = max(n, j.Index+1)
	}
	return n
}

// planCall expands a job that calls a reusable workflow. rec (at idx) becomes
// the first call record, one per matrix leg; the jobs of the called workflow are
// added as waiting records of the scope of each leg.
func (rs *runState) planCall(ctx context.Context, wf *workflow.Workflow, wj *workflow.Job, idx int, env template.Env) error {
	rec := rs.jobs[idx]
	parent := rs.scopeOf(rec)
	var home *domain.CICall
	depth := 1
	if parent != nil {
		home, depth = parent.Call, parent.Call.Depth+1
	}
	if depth > maxReusableWorkflowDepth {
		return callFailure{fmt.Sprintf("Nested reusable workflow depth exceeded %d.", maxReusableWorkflowDepth)}
	}
	ref, text := parseCallRef(wj.Uses.Value)
	if text != "" {
		return callFailure{text}
	}
	file, err := rs.c.loadCallee(ctx, rs, home, rec, ref)
	if err != nil {
		return err
	}
	prefix := callerPrefix(wj)
	cw, errs := parseWorkflow(file.path, file.yaml)
	if len(errs) > 0 {
		return callFailure{prefix + " " + errs[0].Error()}
	}
	wc := cw.On.Event("workflow_call")
	if wc == nil {
		return callFailure{prefix + " workflow_call key is not defined in the referenced workflow."}
	}
	spec := readCalleeSpec(file.yaml)

	eff, err := rs.callPermissions(wf, wj, rec, parent)
	if err != nil {
		return err
	}
	plan, err := wj.EvaluateStrategy(ctx, env)
	if err != nil {
		return err
	}
	vars, _ := env.Contexts["vars"].(*expr.Dictionary)
	if vars == nil {
		vars = expr.NewDictionary()
	}
	gh := env.Contexts["github"]

	// The records of the run are bounded before anything changes: the calls, and
	// the jobs of the called workflow for each (a job that is a matrix adds its
	// legs when it is planned, checked there).
	if err := rs.checkJobBudget(len(plan.Combos) - 1 + len(plan.Combos)*len(cw.Jobs)); err != nil {
		return err
	}
	legs := make([]domain.CIJob, 0, len(plan.Combos))
	for i := range plan.Combos {
		menv, err := plan.MatrixEnv(env, i)
		if err != nil {
			return err
		}
		var given *expr.Dictionary
		if wj.With != nil {
			v, err := template.Evaluate(ctx, wj.With, menv)
			if err != nil {
				return err
			}
			given, _ = v.(*expr.Dictionary)
		}
		inEnv := template.Env{Contexts: map[string]expr.Value{"github": gh, "inputs": expr.NewDictionary(), "vars": vars}}
		inputs, err := callInputs(ctx, wc.Inputs, given, inEnv)
		if err != nil {
			if text, ok := isCallFailure(err); ok {
				return callFailure{prefix + " " + text}
			}
			return err
		}
		// The group of the calling job (job-level, evaluated like the group of
		// any job) and the workflow-level group of the called workflow
		// (evaluated with the caller's github context and the call's inputs).
		// The call record is the entry of both.
		var jobConc, wfConc domain.CIConcurrency
		if wj.Concurrency != nil {
			if jobConc, err = evalConcurrency(ctx, wj.Concurrency, menv); err != nil {
				return err
			}
			jobConc.Evaluated = true
		}
		if cw.Concurrency != nil {
			wenv := template.Env{Contexts: map[string]expr.Value{"github": gh, "inputs": inputs, "vars": vars}}
			if wfConc, err = evalConcurrency(ctx, cw.Concurrency, wenv); err != nil {
				return callFailure{prefix + " " + err.Error()}
			}
			wfConc.Evaluated = true
		}
		call := domain.CICall{
			Uses: wj.Uses.Value, Path: file.path, Repository: file.repository, Ref: file.ref, Sha: file.sha, YAML: string(file.yaml),
			InputsJSON: compactJSON(expr.ToJSON(inputs)), Depth: depth, Expanded: true, Concurrency: wfConc,
		}
		if err := rs.callSecrets(wj, menv, spec, &call); err != nil {
			if text, ok := isCallFailure(err); ok {
				return callFailure{prefix + " " + text}
			}
			return err
		}
		name, err := wj.DisplayName(ctx, menv, plan, i)
		if err != nil {
			return err
		}
		leg := rec
		leg.Combo = i
		leg.ID = domain.NewJobID(rs.run.ID, rec.Index, i)
		if i > 0 {
			leg.Version = 0
		}
		leg.Name = strings.TrimSpace(name)
		if parent != nil {
			leg.Name = parent.Name + " / " + leg.Name // "<caller> / <nested caller>"
		}
		leg.Call, leg.Planned, leg.Phase = &call, true, domain.CIPhaseWaiting
		leg.Permissions = eff
		leg.Concurrency = jobConc
		leg.MatrixJSON = compactJSON(expr.ToJSON(plan.Combos[i].Values))
		leg.ContextsJSON, leg.ComboTotal = "{}", len(plan.Combos)
		leg.FailFast, leg.MaxParallel = plan.FailFast, plan.MaxParallel
		leg.Labels, leg.TimeoutMinutes = nil, 0
		legs = append(legs, leg)
	}

	// The new positions follow the highest one in use.
	next := rs.nextIndex()
	if next+len(legs)*len(cw.Jobs) > maxJobIndex+1 {
		return callFailure{"The workflow has more jobs than the server tracks (" + strconv.Itoa(maxJobIndex+1) + ")."}
	}
	rs.jobs[idx] = legs[0]
	rs.touch(&rs.jobs[idx])
	for _, l := range legs[1:] {
		rs.jobs = append(rs.jobs, l)
		rs.dirty[l.ID] = true
	}
	next = rs.nextIndex()
	now := rs.c.now()
	for _, l := range legs {
		for _, cj := range cw.Jobs {
			needs := make([]string, len(cj.Needs))
			for i, n := range cj.Needs {
				needs[i] = l.Key + "/" + n
			}
			id := domain.NewJobID(rs.run.ID, next, 0)
			rs.jobs = append(rs.jobs, domain.CIJob{
				ID: id, RunID: rs.run.ID, Attempt: rs.attempt, RunAttempt: rs.attempt, Repo: rs.run.Repo, Key: l.Key + "/" + cj.ID,
				Name: cj.ID, Phase: domain.CIPhaseWaiting, Needs: needs, Index: next, CreatedAt: now, Scope: l.ID, CallID: cj.ID,
			})
			rs.dirty[id] = true
			next++
		}
	}
	rs.sortJobs()
	return nil
}

// callPermissions is the permissions a call grants to the jobs of its scope: the
// "permissions:" of the calling job, else of its workflow, else those the
// containing call allows. A reusable workflow can only keep or lower them: a
// request above the allowed is the error of PermissionsHelper.cs.
func (rs *runState) callPermissions(wf *workflow.Workflow, wj *workflow.Job, rec domain.CIJob, parent *domain.CIJob) (map[string]string, error) {
	got := domain.ResolvePermissions(permissionSpec(wf.Permissions), permissionSpec(wj.Permissions))
	if parent == nil {
		return got, nil
	}
	allowed := domain.TokenPermissions(parent.Permissions, true)
	if got == nil {
		return allowed, nil
	}
	if asked, ok := domain.PermissionViolations(got, allowed); len(asked) > 0 {
		return nil, callFailure{fmt.Sprintf("Error calling workflow '%s'. The nested job '%s' is requesting '%s', but is only allowed '%s'.",
			parent.Call.Uses, wj.ID, strings.Join(asked, ", "), strings.Join(ok, ", "))}
	}
	return got, nil
}

// scopedPermissions are the permissions of a job of a called workflow: its own
// "permissions:", else its workflow's, else what the call allows; asking for
// more than the call allows is an error.
func (rs *runState) scopedPermissions(wf *workflow.Workflow, wj *workflow.Job, call *domain.CIJob) (map[string]string, error) {
	allowed := domain.TokenPermissions(call.Permissions, true)
	got := domain.ResolvePermissions(permissionSpec(wf.Permissions), permissionSpec(wj.Permissions))
	if got == nil {
		return call.Permissions, nil
	}
	if asked, ok := domain.PermissionViolations(got, allowed); len(asked) > 0 {
		what := "The nested job '" + wj.ID + "'"
		if wj.Permissions == nil {
			what = "The workflow"
		}
		return nil, callFailure{fmt.Sprintf("Error calling workflow '%s'. %s is requesting '%s', but is only allowed '%s'.",
			call.Call.Uses, what, strings.Join(asked, ", "), strings.Join(ok, ", "))}
	}
	return got, nil
}

// callSecrets reads the "secrets:" of a call: "inherit", or a mapping. Planning
// validates names only (the expressions were checked against the contexts the
// schema allows when the file was parsed) and keeps no expression and no value:
// the mapping is read from the pinned workflow file, as a template token, when
// a job is assigned (callMapping, mapSecrets), and evaluated once there. Only
// the plain contexts the expressions read (needs, matrix, strategy, inputs,
// vars: none is secret) are kept, in call.SecretCtx, as they are now.
func (rs *runState) callSecrets(wj *workflow.Job, env template.Env, spec calleeSpec, call *domain.CICall) error {
	if wj.InheritSecrets {
		call.Inherit = true
		return nil
	}
	if wj.Secrets == nil {
		return rs.checkRequiredSecrets(spec, map[string]bool{})
	}
	call.Secrets = map[string]string{}
	given := map[string]bool{}
	for _, p := range wj.Secrets.Pairs {
		name := strings.ToUpper(p.Key.String())
		if _, ok := spec.secrets[name]; !ok {
			return callFailure{"Invalid secret, " + p.Key.String() + " is not defined in the referenced workflow."}
		}
		given[name] = true
		call.Secrets[name] = "" // the name only
	}
	plain := map[string]json.RawMessage{}
	for _, k := range []string{"needs", "matrix", "strategy", "inputs", "vars"} {
		if v, ok := env.Contexts[k]; ok && v != nil {
			plain[k] = json.RawMessage(compactJSON(expr.ToJSON(v)))
		}
	}
	b, err := json.Marshal(plain)
	if err != nil {
		return err
	}
	call.SecretCtx = string(b)
	return rs.checkRequiredSecrets(spec, given)
}

// callMapping returns the "secrets:" mapping of a call as the template tokens of
// the workflow file that holds the calling job: the run's own workflow, or the
// called workflow of the call that contains it. The tokens come from the file
// itself and are never rebuilt from text; names are upper-cased.
func (rs *runState) callMapping(cr *domain.CIJob) (map[string]template.Token, error) {
	path, yamlText, id := rs.run.WorkflowPath, rs.src.WorkflowYAML, cr.Key
	if p := rs.scopeOf(*cr); p != nil && p.Call != nil {
		path, yamlText, id = p.Call.Path, p.Call.YAML, cr.CallID
	}
	wf, err := rs.c.flows.parseCalled(path, yamlText)
	if err != nil {
		return nil, err
	}
	wj := wf.Job(id)
	if wj == nil {
		return nil, fmt.Errorf("the calling job is gone")
	}
	out := map[string]template.Token{}
	if wj.Secrets != nil {
		for _, p := range wj.Secrets.Pairs {
			out[strings.ToUpper(p.Key.String())] = p.Value
		}
	}
	return out, nil
}

// secretsReadBy returns the secrets a template token refers to: the expressions
// of the token are parsed, not the text they produce.
func secretsReadBy(t template.Token) secretUse {
	w := &secretWalk{names: map[string]bool{}}
	template.Walk(t, func(x template.Token) bool {
		if be, ok := x.(*template.BasicExpression); ok {
			w.expression(be.Expression)
		}
		return true
	})
	use := secretUse{All: w.all || w.tooDeep}
	for n := range w.names {
		use.Names = append(use.Names, n)
	}
	slices.Sort(use.Names)
	return use
}

func (rs *runState) checkRequiredSecrets(spec calleeSpec, given map[string]bool) error {
	for _, name := range sortedKeys(spec.secrets) {
		if spec.secrets[name] && !given[name] {
			return callFailure{"Secret " + name + " is required, but not provided while calling."}
		}
	}
	return nil
}

// chainOf returns the call records a job runs under, innermost first.
func (rs *runState) chainOf(j domain.CIJob) []*domain.CIJob {
	var out []*domain.CIJob
	for sc := rs.scopeOf(j); sc != nil && len(out) <= maxReusableWorkflowDepth; sc = rs.scopeOf(*sc) {
		out = append(out, sc)
	}
	return out
}

// scopedSecretUse is the secrets of the run's own workflow that a job of a
// called workflow reads, through the "secrets:" of each call that leads to it.
// inner is the use inside the called workflow of the job.
func (rs *runState) scopedSecretUse(j domain.CIJob) (top, inner secretUse) {
	chain := rs.chainOf(j)
	inner = secretsUsedBy(chain[0].Call.YAML, j.CallID)
	cur := inner
	for _, c := range chain {
		cur = rs.translateUse(cur, c)
	}
	return cur, inner
}

// translateUse turns the secrets a called workflow reads into the secrets its
// caller must hold: with "inherit" the same ones, else the secrets that the
// expressions of the call's mapping, for the ones it reads, refer to (all of
// them when an expression is not a plain name). A mapping that cannot be read
// asks for all of them: the safe side for the job, which then gets none it
// cannot evaluate.
func (rs *runState) translateUse(use secretUse, cr *domain.CIJob) secretUse {
	if cr.Call.Inherit {
		return use
	}
	mapping, err := rs.callMapping(cr)
	if err != nil {
		return secretUse{All: true}
	}
	var out secretUse
	add := func(t template.Token) {
		u := secretsReadBy(t)
		out.All = out.All || u.All
		for _, n := range u.Names {
			if !slices.Contains(out.Names, n) {
				out.Names = append(out.Names, n)
			}
		}
	}
	for name, t := range mapping {
		if use.All || slices.Contains(use.Names, name) {
			add(t)
		}
	}
	slices.Sort(out.Names)
	return out
}

// mapSecrets gives a job of a called workflow the secrets it reads, named as the
// called workflow names them. top holds the secrets of the run's own workflow
// that it may use (empty for a run that withholds them: the caller decides who
// gets any). From the outermost call, each mapping value is the template token
// of the workflow file, evaluated once with the secrets of its calling scope and
// the plain contexts the call kept; the result is a final string, never parsed
// again, and is the secrets of the scope below. A secret not passed (not in
// "secrets:" and no "inherit") is not there. A value that does not evaluate is
// empty, and nothing of the error is logged: it may hold the secret.
func (rs *runState) mapSecrets(ctx context.Context, j domain.CIJob, top map[string]string, inner secretUse) map[string]string {
	chain := rs.chainOf(j)
	vals := top
	gh, ghErr := rs.githubValue("", rs.run.Attempt)
	for i := len(chain) - 1; i >= 0; i-- {
		cr := chain[i]
		if cr.Call.Inherit {
			continue
		}
		mapping, merr := rs.callMapping(cr)
		sd := expr.NewDictionary()
		for _, k := range sortedKeys(vals) {
			sd.Add(k, expr.String(vals[k]))
		}
		env := template.Env{Contexts: map[string]expr.Value{"secrets": sd}}
		if ghErr == nil {
			env.Contexts["github"] = gh
		}
		var plain map[string]json.RawMessage
		_ = json.Unmarshal([]byte(cr.Call.SecretCtx), &plain)
		for k, raw := range plain {
			if v, err := expr.FromJSON(raw); err == nil {
				env.Contexts[k] = v
			}
		}
		next := map[string]string{}
		for name := range cr.Call.Secrets {
			t, ok := mapping[name]
			if merr != nil || !ok {
				next[name] = ""
				continue
			}
			v, err := template.EvaluateString(ctx, t, env)
			if err != nil {
				// Never the error: a format() error holds its arguments.
				ctxLog(ctx).Warn().Str("secret", name).Msg("ci: a secret passed to a called workflow could not evaluate: it is empty")
				v = ""
			}
			next[name] = v
		}
		vals = next
	}
	out := map[string]string{}
	for n, v := range vals {
		if inner.All || slices.Contains(inner.Names, n) {
			out[n] = v
		}
	}
	return out
}

// callHeld reports whether a call holds the concurrency groups it asked for: the
// group of the calling job and the workflow-level group of the called workflow.
// The jobs of a call are not planned before.
func callHeld(call *domain.CIJob) bool {
	if call.Call == nil || !call.Call.Admitted {
		return false
	}
	if call.Concurrency.Group != "" && !call.Concurrency.FromRun && call.Concurrency.State != domain.CIConcurrencyHolding {
		return false
	}
	return call.Call == nil || call.Call.Concurrency.Group == "" || call.Call.Concurrency.State == domain.CIConcurrencyHolding
}

// admitCalls lets the calls of a matrix start while the max-parallel of the
// calling job's strategy allows it, in the order of the legs, as promote does
// for the jobs that run. A call that is admitted asks for its groups and has its
// jobs planned; one that is not waits, and starts nothing.
func (rs *runState) admitCalls() bool {
	running := map[string]int{}
	for _, j := range rs.jobs {
		if j.Call != nil && j.Call.Admitted && !j.Ended() {
			running[j.Scope+"\x00"+j.Key]++
		}
	}
	changed := false
	for i := range rs.jobs {
		j := &rs.jobs[i]
		if j.Call == nil || j.Call.Admitted || j.Ended() || j.Phase != domain.CIPhaseWaiting {
			continue
		}
		k := j.Scope + "\x00" + j.Key
		if j.MaxParallel > 0 && running[k] >= j.MaxParallel {
			continue
		}
		call := *j.Call
		call.Admitted = true
		j.Call, changed = &call, true
		running[k]++
		rs.touch(j)
	}
	return changed
}

// checkJobBudget refuses an expansion that would give the run more job records
// than a commit may have checks (MaxCIChecks: each record has one check, and the
// merge rule cannot read more). extra is the number of records the expansion
// adds (the record of the job being expanded becomes the first of its legs).
// GitHub's text is the runner's (JobCountValidator.cs); its limit applies to the
// jobs across all referenced files, and the matrix of one job is capped at 256
// by ermite-actions (ErrTooManyConfigurations).
func (rs *runState) checkJobBudget(extra int) error {
	if len(rs.jobs)+extra > MaxCIChecks {
		return callFailure{fmt.Sprintf("Workflows may not contain more than %d jobs across all referenced files", MaxCIChecks)}
	}
	return nil
}

// holdCalls asks for the turn of the calls that wait for a group: the group of
// the calling job first, then the one of the called workflow, so that a call
// waits in one group at a time. The call record is the entry, and holds both
// until it ends (releaseEnded).
func (rs *runState) holdCalls(ctx context.Context) (bool, error) {
	changed := false
	for i := range rs.jobs {
		j := &rs.jobs[i]
		if j.Call == nil || !j.Call.Expanded || !j.Call.Admitted || j.Ended() || j.Phase != domain.CIPhaseWaiting {
			continue
		}
		entry := domain.CIConcurrencyEntry{RunID: j.RunID, Attempt: j.RunAttempt, JobID: j.ID}
		if g := j.Concurrency; g.Group != "" && !g.FromRun && g.State != domain.CIConcurrencyHolding {
			e := entry
			e.Cancel, e.Max = g.Cancel, g.Max
			st, err := rs.acquire(ctx, g.Group, e)
			if err != nil {
				return changed, err
			}
			if st != g.State {
				j.Concurrency.State, changed = st, true
				rs.touch(j)
			}
			if st != domain.CIConcurrencyHolding {
				continue
			}
		}
		if g := j.Call.Concurrency; g.Group != "" && g.State != domain.CIConcurrencyHolding {
			e := entry
			e.Cancel, e.Max = g.Cancel, g.Max
			st, err := rs.acquire(ctx, g.Group, e)
			if err != nil {
				return changed, err
			}
			if st != g.State {
				call := *j.Call
				call.Concurrency.State = st
				j.Call, changed = &call, true
				rs.touch(j)
			}
		}
	}
	return changed, nil
}

// advanceCalls plans the jobs of called workflows whose needs ended and ends
// the call records whose jobs have. It returns at the first change, since a
// change reorders the records.
func (rs *runState) advanceCalls(ctx context.Context) (bool, error) {
	if rs.admitCalls() {
		return true, nil
	}
	if changed, err := rs.holdCalls(ctx); err != nil || changed {
		return changed, err
	}
	for i := range rs.jobs {
		rec := rs.jobs[i]
		if rec.Scope == "" || rec.Planned || rec.Ended() || rec.Phase != domain.CIPhaseWaiting {
			continue
		}
		call := rs.scopeOf(rec)
		if call == nil {
			rs.end(&rs.jobs[i], domain.CIFailure, "the call of this job is gone")
			return true, nil
		}
		if call.Ended() {
			rs.end(&rs.jobs[i], domain.CICancelled, "")
			return true, nil
		}
		if call.Call == nil || !call.Call.Admitted {
			continue // the max-parallel of the calling job holds the call back
		}
		if !callHeld(call) {
			// Waits for the group of its call: shown as pending, as the jobs
			// of a run behind a workflow-level group.
			if rs.run.Concurrency.Group == "" && rec.Concurrency.State != domain.CIConcurrencyPending {
				g := call.Concurrency
				if g.Group == "" || g.State == domain.CIConcurrencyHolding {
					g = call.Call.Concurrency
				}
				rs.jobs[i].Concurrency = domain.CIConcurrency{Group: g.Group, State: domain.CIConcurrencyPending, FromRun: true, Evaluated: true}
				rs.touch(&rs.jobs[i])
				return true, nil
			}
			continue
		}
		if rec.Concurrency.FromRun && rs.run.Concurrency.Group == "" {
			rs.jobs[i].Concurrency = domain.CIConcurrency{}
			rs.touch(&rs.jobs[i])
		}
		cw, err := rs.calleeOf(call)
		if err != nil {
			rs.end(&rs.jobs[i], domain.CIFailure, err.Error())
			return true, nil
		}
		cj := cw.Job(rec.CallID)
		if cj == nil {
			rs.end(&rs.jobs[i], domain.CIFailure, "the called workflow has no job '"+rec.CallID+"'")
			return true, nil
		}
		if len(rs.instancesIn(rec.Scope, rec.CallID)) != 1 {
			continue
		}
		needs, st, ready := rs.needsOf(cw, cj, rec.Scope)
		if !ready {
			continue
		}
		rs.planJob(ctx, cw, cj, i, needs, st)
		return true, nil
	}
	for i := range rs.jobs {
		call := &rs.jobs[i]
		if call.Call == nil || !call.Call.Expanded {
			continue
		}
		var scoped []int
		allEnded := true
		for k, j := range rs.jobs {
			if j.Scope == call.ID {
				scoped = append(scoped, k)
				allEnded = allEnded && j.Ended()
			}
		}
		if call.Ended() {
			changed := false
			for _, k := range scoped {
				s := &rs.jobs[k]
				switch {
				case s.Ended():
				case s.Active():
					rs.requestCancel(s, domain.CICancelUser, endedCallMessage(*call))
				default:
					rs.end(s, domain.CICancelled, "")
					changed = true
				}
			}
			if changed {
				return true, nil
			}
			continue
		}
		if allEnded {
			rs.finishCall(ctx, i, scoped)
			return true, nil
		}
	}
	return false, nil
}

// finishCall ends a call record with the conclusion of its jobs and the outputs
// of the called workflow.
func (rs *runState) finishCall(ctx context.Context, idx int, scoped []int) {
	call := &rs.jobs[idx]
	var top []domain.CIJob
	for _, k := range scoped {
		top = append(top, rs.jobs[k])
	}
	conc, msg := domain.CISuccess, ""
	switch result(top) {
	case "failure":
		conc = domain.CIFailure
		msg = "A job of the called workflow failed"
		for _, j := range top {
			if j.Conclusion.IsFailure() && !j.ContinueOnError {
				msg = "The job '" + j.Name + "' of the called workflow failed"
				break
			}
		}
	case "cancelled":
		conc = domain.CICancelled
	case "skipped":
		conc = domain.CISkipped
	}
	jobs := expr.NewDictionary()
	for _, id := range distinctCallIDs(top) {
		var insts []domain.CIJob
		for _, j := range top {
			if j.CallID == id {
				insts = append(insts, j)
			}
		}
		outs := expr.NewDictionary()
		for _, in := range insts {
			for _, k := range sortedKeys(in.Outputs) {
				if v := in.Outputs[k]; v != "" || !hasKey(outs, k) {
					setKey(outs, k, expr.String(v))
				}
			}
		}
		d := expr.NewDictionary()
		d.Add("result", expr.String(result(insts)))
		d.Add("outputs", outs)
		jobs.Add(id, d)
	}
	outputs := map[string]string{}
	spec := readCalleeSpec([]byte(call.Call.YAML))
	if len(spec.outputs) > 0 {
		gh, err := rs.githubValue(call.Key, rs.run.Attempt)
		vars, verr := rs.c.varsContext(ctx, rs.run.Repo)
		if err == nil && verr == nil {
			env := template.Env{Contexts: map[string]expr.Value{"github": gh, "inputs": expr.NewDictionary(), "vars": vars, "jobs": jobs}}
			// The inputs of the call itself.
			if v, err := expr.FromJSON([]byte(call.Call.InputsJSON)); err == nil {
				env.Contexts["inputs"] = v
			}
			for _, name := range sortedKeys(spec.outputs) {
				v, err := evalInterpolated(ctx, spec.outputs[name], env)
				if err != nil {
					ctxLog(ctx).Warn().Str("output", name).Msg("ci: an output of a called workflow could not evaluate")
					continue
				}
				outputs[name] = expr.CoerceString(v)
			}
		}
	}
	call.Outputs = outputs
	rs.end(call, conc, msg)
}

func distinctCallIDs(js []domain.CIJob) []string {
	var out []string
	for _, j := range js {
		if !slices.Contains(out, j.CallID) {
			out = append(out, j.CallID)
		}
	}
	return out
}

// parseCalled parses a called workflow, once per distinct file.
func (w *wfCache) parseCalled(path, yamlText string) (*workflow.Workflow, error) {
	sum := sha256.Sum256([]byte(path + "\x00" + yamlText))
	key := hex.EncodeToString(sum[:])
	w.mu.Lock()
	if wf, ok := w.called[key]; ok {
		w.mu.Unlock()
		return wf, nil
	}
	w.mu.Unlock()
	wf, errs := workflow.Parse(path, []byte(yamlText))
	if len(errs) > 0 {
		return nil, fmt.Errorf("the called workflow no longer parses: %s", errs[0].Error())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.called) >= w.max {
		clear(w.called)
	}
	w.called[key] = wf
	return wf, nil
}

// jobKeysOf are the keys of the "job" context the server knows. A job of a
// called workflow names the called workflow in them (workflow_ref,
// workflow_sha, workflow_repository, workflow_file_path); github.workflow_ref
// and github.workflow_sha stay the caller's (the github context is the
// caller's).
func (rs *runState) jobKeysOf(ctx context.Context, base domain.CIJob, combo int) ordered {
	keys := rs.c.jobKeys(ctx, rs.run, base.Index, combo)
	call := rs.scopeOf(base)
	if call == nil || call.Call == nil {
		return keys
	}
	c := call.Call
	full := c.Repository
	if !strings.Contains(full, "/") {
		full = rs.c.cfg.Owner + "/" + full
	}
	ref := c.Ref
	if ref == "" {
		ref = rs.run.Ref
	}
	return ordered{
		keys[0], {"workflow_ref", full + "/" + c.Path + "@" + ref}, {"workflow_sha", c.Sha},
		{"workflow_repository", full}, {"workflow_file_path", c.Path},
	}
}

// endedCallMessage is what the jobs of a call that ended are told: the reason
// the call was cancelled for (a newer call of its concurrency group), else that
// the calling job ended.
func endedCallMessage(call domain.CIJob) string {
	if call.Concurrency.Preempted || call.Error != "" {
		return call.Error
	}
	return "Canceled because the calling job ended"
}

// callResolutions copies the resolutions of the reusable workflows that the
// attempt of rs called to the attempt next.
func (c *CI) callResolutions(ctx context.Context, rs *runState, next int32) ([]domain.CIActionResolution, error) {
	var out []domain.CIActionResolution
	seen := map[string]bool{}
	for _, j := range rs.jobs {
		if j.Call == nil {
			continue
		}
		ref, text := parseCallRef(j.Call.Uses)
		if text != "" || ref.local {
			continue
		}
		key := actionRef{owner: ref.owner, repo: ref.repo, ref: ref.ref}.key()
		if seen[key] {
			continue
		}
		seen[key] = true
		res, err := c.cfg.Store.GetActionResolution(ctx, rs.run.ID, rs.attempt, key)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		res.RunAttempt = next
		out = append(out, res)
	}
	return out, nil
}
