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
// like `gh workflow run`.

func newWorkflowCmd(env *cliEnv) *cobra.Command {
	wf := &cobra.Command{
		Use:   "workflow",
		Short: "Start workflows by hand",
	}
	wf.AddCommand(newWorkflowRunCmd(env))
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
		Long: "Start a run of a workflow file on a branch or a tag (the default branch without --ref). The workflow is read at\n" +
			"that ref and must have a workflow_dispatch trigger; -f name=value gives the inputs it declares (checked by type,\n" +
			"required inputs and choices). The workflow file is its path or its file name. Needs the ci:write scope." + jsonRunHelp,
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
