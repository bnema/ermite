// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"
)

// readTextFile accepts files and stdin only, with the same 1 MiB text limit.
func readTextFile(cmd *cobra.Command, path string) (string, error) {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // a path supplied by the user
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 {
		return "", fmt.Errorf("text file exceeds the 1 MiB limit")
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func newIssueEditCmd(env *cliEnv) *cobra.Command { return newItemEditCmd(env, false) }
func newMREditCmd(env *cliEnv) *cobra.Command    { return newItemEditCmd(env, true) }

func newItemEditCmd(env *cliEnv, mr bool) *cobra.Command {
	var title, path string
	label, field, scope, feature := "issue", "body", "issue:write", client.FeatureIssueEdit
	help := "\nJSON: an issue (see `ermite issue --help`)."
	completion := completeIssueRepoThenNumber(env)
	if mr {
		label, field, scope, feature = "merge request", "description", "mr:write", client.FeatureMREdit
		help = "\nJSON: a merge request (see `ermite mr --help`)."
		completion = completeMRRepoThenNumber(env)
	}
	flag := field + "-file"
	cmd := &cobra.Command{Use: "edit <repo> <number>", Short: "Edit " + map[bool]string{true: "a merge request", false: "an issue"}[mr], Long: "Edit " + label + " title and/or " + field + ". Only supplied fields change.\n" +
		"The title is one line of at most 120 characters. Read the " + field + " from a file only;\n" +
		"--" + flag + " - reads standard input (at most 1 MiB). An empty file clears the " + field + ".\n" +
		"At least one flag is required. Prints a confirmation, then the full web URL.\nNeeds the " + scope + " scope." + help, Args: cobra.ExactArgs(2), ValidArgsFunction: completion}
	cmd.Flags().StringVar(&title, "title", "", "replacement title")
	cmd.Flags().StringVar(&path, flag, "", "read the replacement "+field+" from this file, or - for standard input")
	_ = cmd.MarkFlagFilename(flag)
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("title") && !cmd.Flags().Changed(flag) {
			return fmt.Errorf("at least one of --title or --%s is required", flag)
		}
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		var t, text *string
		if cmd.Flags().Changed("title") {
			t = &title
		}
		if cmd.Flags().Changed(flag) {
			value, err := readTextFile(cmd, path)
			if err != nil {
				return fmt.Errorf("read --%s: %w", flag, err)
			}
			text = &value
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if err := requireFeature(env.ctx(cmd), c, feature, "cannot edit "+label+"s"); err != nil {
			return err
		}
		if mr {
			res, err := c.MergeRequests().EditMergeRequest(env.ctx(cmd), connect.NewRequest(&ermite.EditMergeRequestRequest{Repo: args[0], Number: n, Title: t, Description: text}))
			if err != nil {
				return c.Describe(err)
			}
			return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "Edited", c.URL())
		}
		res, err := c.Issues().EditIssue(env.ctx(cmd), connect.NewRequest(&ermite.EditIssueRequest{Repo: args[0], Number: n, Title: t, Body: text}))
		if err != nil {
			return c.Describe(err)
		}
		return outputIssue(cmd, res.Msg.GetIssue(), *asJSON, "Edited", c.URL())
	}
	return cmd
}
