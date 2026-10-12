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
			"Remote commands read ERMITE_URL (the server address, http:// or https://) and\n" +
			"ERMITE_TOKEN (a bearer token, see `ermite token create`) from the environment.",
		SilenceUsage:  true,
		SilenceErrors: true, // cmd/ermite prints the error once
	}
	root.AddCommand(
		newServeCmd(), newHookCmd(),
		newVersionCmd(env), newRepoCmd(env), newTokenCmd(env), newWatchCmd(env),
		newMRCmd(env), newSigningKeyCmd(env), newWebCmd(env), newConfigCmd(env),
	)
	return root
}
