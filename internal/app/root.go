// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"

	"github.com/spf13/cobra"
)

// helpWidth is the column at which wrapHelp breaks the long help of commands.
const helpWidth = 100

func newRootCmd() *cobra.Command {
	return newRootCmdWith(defaultCLIEnv())
}

// newRootCmdWith builds the command tree around env, which the remote commands
// use to reach the environment, the clock and the build version.
func newRootCmdWith(env *cliEnv) *cobra.Command {
	root := &cobra.Command{
		Use:   "ermite",
		Short: "A single-user Git forge",
		Long: "ermite is a single-user Git forge: `ermite serve` runs the server; other commands use its API.\n" +
			"\n" +
			"Remote commands need an http(s) server URL and, except for public reads, a bearer token\n" +
			"(see `ermite token create`). Environment values take precedence:\n" +
			"  ERMITE_URL / ERMITE_TOKEN   environment\n" +
			"  url / token.cmd            ~/.config/ermite/config (override: ERMITE_CONFIG)\n" +
			"`ermite config show` prints the file path.\n" +
			"\n" +
			"token.cmd runs only when needed and prints the token. Keep tokens in a secret manager\n" +
			"(pass, gopass, op, bw, secret-tool...), never in the file. See `ermite config --help`.",
		SilenceUsage:  true,
		SilenceErrors: true, // cmd/ermite prints the error once
	}
	root.AddCommand(
		newServeCmd(), newHookCmd(),
		newMirrorCmd(env), newVersionCmd(env), newRepoCmd(env), newTokenCmd(env), newWatchCmd(env),
		newMRCmd(env), newIssueCmd(env), newReleaseCmd(env), newSigningKeyCmd(env), newWebCmd(env), newConfigCmd(env),
		newRunCmd(env), newWorkflowCmd(env), newRunnerCmd(env), newSecretCmd(env), newVariableCmd(env), newEnvironmentCmd(env), newOIDCCmd(env), newDepsCmd(env), newJournalCmd(env),
	)
	wrapLongHelp(root)
	return root
}

// wrapLongHelp wraps the long help of cmd and its subcommands at helpWidth.
func wrapLongHelp(cmd *cobra.Command) {
	cmd.Long = wrapHelp(cmd.Long, helpWidth)
	for _, sub := range cmd.Commands() {
		wrapLongHelp(sub)
	}
}

// wrapHelp breaks the lines of text longer than width at spaces. Indented lines
// (examples, tables) and words longer than width are kept as they are. A `code`
// span counts as one word, so that a command is never split over two lines.
func wrapHelp(text string, width int) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) <= width || strings.HasPrefix(line, " ") {
			out = append(out, line)
			continue
		}
		var b strings.Builder
		for _, word := range helpWords(line) {
			if b.Len() > 0 && b.Len()+1+len(word) > width {
				out = append(out, b.String())
				b.Reset()
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(word)
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "\n")
}

// helpWords splits line at spaces, except inside a `code` span.
func helpWords(line string) []string {
	var words []string
	for _, w := range strings.Fields(line) {
		if n := len(words); n > 0 && strings.Count(words[n-1], "`")%2 == 1 {
			words[n-1] += " " + w
			continue
		}
		words = append(words, w)
	}
	return words
}
