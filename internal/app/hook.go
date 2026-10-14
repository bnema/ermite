// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"

	"github.com/spf13/cobra"

	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/ports"
)

// newHookCmd is the hidden command that git's receive hooks run (through the
// scripts in run/hooks): it forwards the pushed ref updates to the server.
func newHookCmd() *cobra.Command {
	hook := &cobra.Command{
		Use:    "hook",
		Short:  "Run a Git receive hook (called by git, not by users)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	for _, name := range []string{githttp.HookPreReceive, githttp.HookPostReceive} {
		hook.AddCommand(&cobra.Command{
			Use:    name,
			Short:  "Report the pushed ref updates to the server",
			Hidden: true,
			Args:   cobra.NoArgs,
			// A refusal is the exit status; its message is already on stderr.
			SilenceErrors: true,
			SilenceUsage:  true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return githttp.RunHook(cmd.Context(), name, cmd.InOrStdin(), cmd.ErrOrStderr(),
					os.Getenv(githttp.EnvHookSocket), os.Getenv(githttp.EnvHookToken),
					ports.PushObjects{Dir: os.Getenv("GIT_OBJECT_DIRECTORY"), Alternates: os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES")})
			},
		})
	}
	return hook
}
