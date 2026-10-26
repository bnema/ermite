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
		Long: "Run a single-user Git forge: `ermite serve` starts the server; other commands use its API.\n" +
			"\n" +
			"Remote commands need an http(s) server URL and, except for public reads, a bearer token\n" +
			"(see `ermite token create`). Environment values take precedence:\n" +
			"  ERMITE_URL / ERMITE_TOKEN   environment\n" +
			"  url / token.cmd            ~/.config/ermite/config (override: ERMITE_CONFIG)\n" +
			"`ermite config show` prints the file path.\n" +
			"\n" +
			"token.cmd runs only when needed and prints the token. Keep tokens in a secret manager\n" +
			"(pass, gopass, op, bw, secret-tool…), never in the file. See `ermite config --help`.",
		SilenceUsage:  true,
		SilenceErrors: true, // cmd/ermite prints the error once
	}
	root.AddCommand(
		newServeCmd(), newHookCmd(),
		newMirrorCmd(env), newVersionCmd(env), newRepoCmd(env), newTokenCmd(env), newWatchCmd(env),
		newMRCmd(env), newIssueCmd(env), newReleaseCmd(env), newSigningKeyCmd(env), newWebCmd(env), newConfigCmd(env),
		newRunCmd(env), newWorkflowCmd(env), newRunnerCmd(env), newSecretCmd(env), newVariableCmd(env), newEnvironmentCmd(env), newOIDCCmd(env), newDepsCmd(env), newJournalCmd(env),
	)
	return root
}
