// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	return newRootCmdWith(defaultCLIEnv())
}

// newRootCmdWith builds the command tree around env, which the remote commands
// use to reach the environment, the clock and the build version.
func newRootCmdWith(env *cliEnv) *cobra.Command {
	root := &cobra.Command{
		Use:   "ermite",
		Short: "A single-user Git forge",
		Long: "ermite is a single-user Git forge: `ermite serve` runs the server, the other commands\n" +
			"drive a running server over its API.\n\n" +
			"Remote commands need the server address (http:// or https://) and, except for public reads,\n" +
			"a bearer token (see `ermite token create`). Each comes from, in order:\n\n" +
			"  ERMITE_URL / ERMITE_TOKEN                    the environment\n" +
			"  url / token.cmd in ~/.config/ermite/config   the config file (ERMITE_CONFIG selects another;\n                                               `ermite config show` prints its path)\n\n" +
			"token.cmd is a command line that prints the token, run only when a command needs it, so the\n" +
			"token stays in your secret manager (pass, gopass, op, bw, secret-tool…), never in the file.\n" +
			"See `ermite config --help`.",
		SilenceUsage:  true,
		SilenceErrors: true, // cmd/ermite prints the error once
	}
	root.AddCommand(
		newServeCmd(), newHookCmd(),
		newVersionCmd(env), newRepoCmd(env), newTokenCmd(env), newWatchCmd(env),
		newMRCmd(env), newIssueCmd(env), newSigningKeyCmd(env), newWebCmd(env), newConfigCmd(env),
		newRunCmd(env), newWorkflowCmd(env), newRunnerCmd(env), newSecretCmd(env), newVariableCmd(env), newEnvironmentCmd(env), newOIDCCmd(env),
	)
	return root
}
