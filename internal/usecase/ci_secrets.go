// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Which secrets a job receives (CI-3 plan D2, least disclosure).
//
// A job gets the secrets its workflow refers to and no others, so that a job
// that never reads a secret never holds it. ermite walks every expression of
// the YAML subtree of the job, and the workflow-level "env", with the
// expression parser of ermite-actions: `secrets.NAME` and `secrets['NAME']`
// name one secret; any other use of the context (a bare `secrets`,
// `toJSON(secrets)`, `secrets[matrix.x]`, `secrets.*`) names all of them.
// Missing one would only give the job an empty secret (a parity bug), never
// a leaked one. The walk reads the YAML rather than the parsed model so that a
// field the model does not list is still seen.

// secretWalks counts the walks, for the tests of the cache.
var secretWalks atomic.Int64

// secretUse is the result of the walk.
type secretUse struct {
	// All is set when the job uses the secrets context in a way that does not
	// name a secret.
	All bool
	// Names are the secrets named, upper-cased and sorted.
	Names []string
}

// none reports whether the job refers to no secret.
func (u secretUse) none() bool { return !u.All && len(u.Names) == 0 }

// maxSecretWalkDepth bounds the YAML nesting followed (aliases included); a
// workflow nested deeper is read as using every secret.
const maxSecretWalkDepth = 200

// secretsUsedBy walks the job and the workflow-level env of a workflow file.
// A file that does not parse as YAML, or that nests too deep, uses all the
// secrets: the safe reading for the run, whose job will fail on it anyway.
func secretsUsedBy(workflowYAML, jobID string) secretUse {
	secretWalks.Add(1)
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(workflowYAML), &doc); err != nil || len(doc.Content) == 0 {
		return secretUse{All: true}
	}
	w := &secretWalk{names: map[string]bool{}}
	root := w.deref(doc.Content[0])
	if root.Kind != yaml.MappingNode {
		return secretUse{All: true}
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		switch key {
		case "env":
			w.node(root.Content[i+1], 0)
		case "jobs":
			jobs := w.deref(root.Content[i+1])
			for j := 0; jobs.Kind == yaml.MappingNode && j+1 < len(jobs.Content); j += 2 {
				if jobs.Content[j].Value == jobID {
					w.node(jobs.Content[j+1], 0)
				}
			}
		}
	}
	use := secretUse{All: w.all || w.tooDeep}
	for n := range w.names {
		use.Names = append(use.Names, n)
	}
	slices.Sort(use.Names)
	return use
}

type secretWalk struct {
	names   map[string]bool
	all     bool
	tooDeep bool
}

func (w *secretWalk) deref(n *yaml.Node) *yaml.Node {
	for range 16 {
		if n.Kind != yaml.AliasNode || n.Alias == nil {
			return n
		}
		n = n.Alias
	}
	return n
}

// node visits every key and value below n.
func (w *secretWalk) node(n *yaml.Node, depth int) {
	if depth > maxSecretWalkDepth {
		w.tooDeep = true
		return
	}
	n = w.deref(n)
	switch n.Kind {
	case yaml.ScalarNode:
		w.text(n.Value)
	case yaml.SequenceNode, yaml.MappingNode, yaml.DocumentNode:
		for _, c := range n.Content {
			w.node(c, depth+1)
		}
	}
}

// text reads the ${{ }} expressions of a string.
func (w *secretWalk) text(s string) {
	for _, src := range expressionsIn(s) {
		w.expression(src)
	}
}

// expressionsIn returns the text inside each ${{ }} of s. A quote opens a
// string literal inside the expression, where "}}" does not close it.
func expressionsIn(s string) []string {
	var out []string
	for {
		start := strings.Index(s, "${{")
		if start < 0 {
			return out
		}
		s = s[start+3:]
		inStr, end := false, -1
		for i := 0; i < len(s); i++ {
			switch {
			case s[i] == '\'':
				inStr = !inStr // '' inside a literal toggles twice
			case !inStr && s[i] == '}' && i+1 < len(s) && s[i+1] == '}':
				end = i
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			out = append(out, s) // unterminated: the workflow does not parse, read it all
			return out
		}
		out = append(out, s[:end])
		s = s[end+2:]
	}
}

var (
	exprContexts = []string{"github", "needs", "strategy", "matrix", "job", "jobs", "runner", "env", "vars", "secrets", "steps", "inputs"}
	exprFuncs    = []expr.FuncInfo{
		{Name: "hashFiles", MinArgs: 1, MaxArgs: -1}, {Name: "success"}, {Name: "failure"}, {Name: "always"}, {Name: "cancelled"},
	}
)

func (w *secretWalk) expression(src string) {
	node, err := expr.Parse(src, exprContexts, exprFuncs)
	if err != nil || node == nil {
		// Does not parse: the job will fail on it, but never miss a use.
		if strings.Contains(strings.ToLower(src), "secrets") {
			w.all = true
		}
		return
	}
	w.visit(node)
}

func isSecretsContext(n expr.Node) bool {
	c, ok := n.(*expr.ContextAccess)
	return ok && strings.EqualFold(c.Name.Lexeme, "secrets")
}

func (w *secretWalk) visit(n expr.Node) {
	switch n := n.(type) {
	case *expr.ContextAccess:
		if isSecretsContext(n) {
			w.all = true // a bare use of the context
		}
	case *expr.IndexAccess:
		if isSecretsContext(n.Expr) {
			if lit, ok := n.Index.(*expr.Literal); ok {
				if s, ok := lit.Value.(expr.String); ok {
					w.names[strings.ToUpper(string(s))] = true
					return
				}
			}
			w.all = true // secrets[x], secrets.*: not a name
			w.visit(n.Index)
			return
		}
		w.visit(n.Expr)
		w.visit(n.Index)
	case *expr.Unary:
		w.visit(n.Expr)
	case *expr.Binary:
		w.visit(n.Left)
		w.visit(n.Right)
	case *expr.Logical:
		for _, a := range n.Args {
			w.visit(a)
		}
	case *expr.Grouping:
		w.visit(n.Group)
	case *expr.FunctionCall:
		for _, a := range n.Args {
			w.visit(a)
		}
	case *expr.Literal, *expr.Star, nil:
	default:
		panic(fmt.Sprintf("secretsUsedBy: unknown expression node %T", n)) // closed set of package expr
	}
}

// --- variables, environments and secrets of a job ---

// varsDictionary is the "vars" context for the merged variables.
func varsDictionary(vars map[string]string) *expr.Dictionary {
	d := expr.NewDictionary()
	for _, k := range sortedKeys(vars) {
		d.Add(k, expr.String(vars[k]))
	}
	return d
}

// varsContext is the "vars" context for what runs before a job has an
// environment (run-name, matrix limits, if, strategy, runs-on, name): the
// variables of the instance overridden by the repository's. It is empty when
// the server resolves none. A failure is returned: running on partial
// variables would give wrong results silently.
func (c *CI) varsContext(ctx context.Context, repo domain.RepoName) (*expr.Dictionary, error) {
	if c.cfg.Settings == nil {
		return expr.NewDictionary(), nil
	}
	got, err := c.cfg.Settings.Resolve(ctx, ports.JobSettingsRequest{Repo: repo})
	if err != nil {
		return nil, fmt.Errorf("read the variables: %w", err)
	}
	return varsDictionary(got.Vars), nil
}

// environmentOf evaluates the "environment:" of a job (a name, an expression or
// a mapping with a name and an url, which is ignored) and returns the name, ""
// for a job without one.
func environmentOf(ctx context.Context, wj *workflow.Job, env template.Env) (string, error) {
	if wj.Environment == nil {
		return "", nil
	}
	tok := wj.Environment
	if m, ok := tok.(*template.Mapping); ok {
		tok = m.Find("name")
		if tok == nil {
			return "", fmt.Errorf("job '%s': environment needs a name", wj.ID)
		}
	}
	name, err := template.EvaluateString(ctx, tok, env)
	if err != nil {
		return "", err
	}
	// Validated as evaluated, never trimmed: the name a workflow gives is the
	// name the API and the CLI accept.
	name, err = domain.ParseEnvironmentName(name)
	if err != nil {
		var te *domain.TextError
		if errors.As(err, &te) {
			return "", fmt.Errorf("job '%s': the environment name %q %s", wj.ID, name, te.Reason)
		}
		return "", fmt.Errorf("job '%s': invalid environment name %q", wj.ID, name)
	}
	return name, nil
}

// settingsOf resolves the variables and secrets of a job at its assignment:
// the variables always, the secrets only for a trusted run and only the ones
// the job's workflow refers to. The result is for the runner and nowhere else.
func (c *CI) settingsOf(ctx context.Context, rs *runState, j domain.CIJob) (ports.JobSettings, error) {
	if c.cfg.Settings == nil {
		return ports.JobSettings{}, nil
	}
	req := ports.JobSettingsRequest{
		Repo: rs.run.Repo, Environment: j.Environment,
		// An environment is created on first use, by a run of the owner: a run
		// that is not trusted does not add environments to the repository.
		CreateEnvironment: j.Environment != "" && rs.run.Trusted,
	}
	if rs.run.Trusted {
		use := c.flows.secretUse(rs.run.ID, rs.src.WorkflowYAML, j.Key)
		req.WantSecrets, req.AllSecrets, req.SecretNames = !use.none(), use.All, use.Names
	}
	got, err := c.cfg.Settings.Resolve(ctx, req)
	if err != nil {
		return ports.JobSettings{}, &settingsError{err}
	}
	return got, nil
}

// settingsError is a failure to read the variables and secrets of a job. It is
// its own type because the callers that skip a job gone or taken meanwhile
// (errNoJob, ErrNotFound, ErrConflict) must never skip on it: an error of the
// settings, even one that wraps ErrConflict (the creation of an environment
// ran out of retries), is the caller's to see, and changes nothing.
type settingsError struct{ err error }

func (e *settingsError) Error() string {
	return "read the variables and secrets of the job: " + e.err.Error()
}
func (e *settingsError) Unwrap() error { return e.err }

// isSettingsError reports whether err comes from reading the settings of a job.
func isSettingsError(err error) bool {
	var se *settingsError
	return errors.As(err, &se)
}

// withVars returns the contexts JSON of a job with its "vars" replaced by the
// current variables, so that a variable changed while the job waited is seen.
// The contexts hold no secret.
func withVars(contexts string, vars map[string]string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(contexts), &m); err != nil {
		return contexts
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return contexts
	}
	m["vars"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return contexts
	}
	return string(out)
}
