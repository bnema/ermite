// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

// `ermite workflow run`: start a workflow that has a workflow_dispatch trigger,
// like `gh workflow run`. `workflow list|disable|enable` show and set the state
// of the workflow files of a repository, like `gh workflow list|disable|enable`.

func newWorkflowCmd(env *cliEnv) *cobra.Command {
	wf := &cobra.Command{
		Use:   "workflow",
		Short: "Start, list, disable and enable workflows",
		Long:  "Manage repository workflows.\n" + jsonFieldsHelp("a workflow", jsonWorkflow{}),
	}
	wf.AddCommand(newWorkflowRunCmd(env), newWorkflowListCmd(env), newWorkflowDisableCmd(env), newWorkflowEnableCmd(env))
	return wf
}

// parseInputFlags reads the -f key=value flags. The first = separates the name
// from the value, so a value may hold =; a repeated name takes the last value.
func parseInputFlags(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(flags))
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid input %q: use -f name=value", f)
		}
		out[k] = v
	}
	return out, nil
}

func newWorkflowRunCmd(env *cliEnv) *cobra.Command {
	var repo, ref string
	var fields []string
	cmd := &cobra.Command{
		Use:   "run <workflow file> --repo <repo>",
		Short: "Run a workflow that has a workflow_dispatch trigger",
		Long:  "Start a workflow on a branch or tag (--ref; default: default branch). The workflow at that ref must declare workflow_dispatch. -f name=value supplies declared inputs; the server checks types, required inputs and choices. The file accepts a path or filename. Access needs ci:write.\nJSON: a run; see `ermite run --help`.",
		Example: "  ermite workflow run release.yml --repo app -f version=1.2.3\n" +
			"  ermite workflow run .github/workflows/ci.yml --repo app --ref feature/x",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
	}
	addRepoFlag(cmd, env, &repo)
	cmd.Flags().StringVar(&ref, "ref", "", "the branch or tag to run on (default: the default branch)")
	cmd.Flags().StringArrayVarP(&fields, "field", "f", nil, "an input of the workflow as name=value (repeatable)")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		inputs, err := parseInputFlags(fields)
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().DispatchWorkflow(env.ctx(cmd), connect.NewRequest(&ermite.DispatchWorkflowRequest{
			Repo: repo, WorkflowPath: args[0], Ref: ref, Inputs: inputs,
		}))
		if err != nil {
			return err
		}
		run := res.Msg.GetRun()
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONRun(run))
		}
		noticef(cmd, "run %d of %s started on %s", run.GetId(), run.GetWorkflowName(), refText(run.GetRef()))
		return nil
	}
	return cmd
}

type jsonWorkflow struct {
	Repo  string `json:"repo"`
	File  string `json:"file" help:".github/workflows/<file>"`
	Name  string `json:"name" help:"the default-branch workflow name:, else its path; empty if absent on the default branch"`
	State string `json:"state" help:"enabled or disabled"`
}

func workflowStateText(s ermite.WorkflowState) string {
	if s == ermite.WorkflowState_WORKFLOW_STATE_DISABLED {
		return "disabled"
	}
	return "enabled"
}

func toJSONWorkflow(repo string, w *ermite.Workflow) jsonWorkflow {
	return jsonWorkflow{Repo: repo, File: w.GetPath(), Name: w.GetName(), State: workflowStateText(w.GetState())}
}

func newWorkflowListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list <repo>",
		Short:             "List repository workflows and their states",
		Long:              "List default-branch .github/workflows files and states, ordered by file. Disabled workflows appear even if their files are absent or the repository is empty. Private reads need repo:read.\nJSON: an array of workflows; see `ermite workflow --help`.",
		Example:           "  ermite workflow list app\n  ermite workflow list app --json",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().ListWorkflows(env.ctx(cmd), connect.NewRequest(&ermite.ListWorkflowsRequest{Repo: args[0]}))
		if err != nil {
			return c.Describe(err)
		}
		list := res.Msg.GetWorkflows()
		if *asJSON {
			out := make([]jsonWorkflow, 0, len(list))
			for _, w := range list {
				out = append(out, toJSONWorkflow(args[0], w))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(list) == 0 {
			noticef(cmd, "no workflows")
			return nil
		}
		rows := make([][]string, 0, len(list))
		for _, w := range list {
			rows = append(rows, []string{w.GetPath(), w.GetName(), workflowStateText(w.GetState())})
		}
		return table(cmd.OutOrStdout(), []string{"FILE", "NAME", "STATE"}, rows)
	}
	return cmd
}

func newWorkflowDisableCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "disable <repo> <file>",
		Short:             "Stop a workflow from starting runs",
		Long:              "Disable a repository workflow, like `gh workflow disable`. No event (push, pull_request, pull_request_target, schedule, workflow_run, create, delete, workflow_dispatch) can start it; existing runs continue. State belongs to the repository and file path, not a branch.\nThe file accepts a path or filename (release.yml) and need not exist: disabling works after `ermite repo create`, before the first push. Already-disabled workflows stay unchanged. `ermite workflow enable` reverses this. Access needs repo:admin.\nJSON: a workflow; see `ermite workflow --help`.",
		Example:           "  ermite workflow disable app release.yml\n  ermite workflow disable app .github/workflows/codeql.yml",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().DisableWorkflow(env.ctx(cmd), connect.NewRequest(&ermite.DisableWorkflowRequest{Repo: args[0], Workflow: args[1]}))
		if err != nil {
			return c.Describe(err)
		}
		return printWorkflowState(cmd, *asJSON, args[0], res.Msg.GetWorkflow(), "disabled")
	}
	return cmd
}

func newWorkflowEnableCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "enable <repo> <file>",
		Short:             "Let a disabled workflow start runs again",
		Long:              "Enable a repository workflow, like `gh workflow enable`. Missed events are not replayed. Already-enabled workflows stay unchanged. The file accepts a path or filename. Access needs repo:admin.\nJSON: a workflow; see `ermite workflow --help`.",
		Example:           "  ermite workflow enable app release.yml",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Runs().EnableWorkflow(env.ctx(cmd), connect.NewRequest(&ermite.EnableWorkflowRequest{Repo: args[0], Workflow: args[1]}))
		if err != nil {
			return c.Describe(err)
		}
		return printWorkflowState(cmd, *asJSON, args[0], res.Msg.GetWorkflow(), "enabled")
	}
	return cmd
}

func printWorkflowState(cmd *cobra.Command, asJSON bool, repo string, w *ermite.Workflow, state string) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONWorkflow(repo, w))
	}
	noticef(cmd, "%s %s in %s", state, clean(w.GetPath()), clean(repo))
	return nil
}
