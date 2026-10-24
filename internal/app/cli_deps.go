// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

const jsonDepsHelp = `
JSON fields of a repository (--json): repo, updates (ecosystem, name, directory, current, latest, kind
(patch, minor, major), new), checked_at, snoozed_until (RFC 3339, UTC, null when unset),
merge_request_number (0 = none), state (idle, applying, failed), error.`

type jsonDepsUpdate struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Kind      string `json:"kind"`
	New       bool   `json:"new"`
}

type jsonDepsRepo struct {
	Repo               string           `json:"repo"`
	Updates            []jsonDepsUpdate `json:"updates"`
	CheckedAt          *string          `json:"checked_at"`
	SnoozedUntil       *string          `json:"snoozed_until"`
	MergeRequestNumber int64            `json:"merge_request_number"`
	State              string           `json:"state"`
	Error              string           `json:"error"`
}

func toJSONDepsRepo(r *ermite.RepoDependencyUpdates) jsonDepsRepo {
	out := jsonDepsRepo{
		Repo: r.GetRepo(), Updates: []jsonDepsUpdate{}, CheckedAt: jsonTime(r.GetCheckedAt()), SnoozedUntil: jsonTime(r.GetSnoozedUntil()),
		MergeRequestNumber: r.GetMergeRequestNumber(), State: depsStateName(r.GetState()), Error: r.GetError(),
	}
	for _, u := range r.GetUpdates() {
		out.Updates = append(out.Updates, jsonDepsUpdate{
			Ecosystem: u.GetEcosystem(), Name: u.GetName(), Directory: u.GetDirectory(), Current: u.GetCurrent(),
			Latest: u.GetLatest(), Kind: depsKindName(u.GetKind()), New: u.GetNew(),
		})
	}
	return out
}

func depsStateName(s ermite.RepoDependencyUpdates_State) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "STATE_"))
}

func depsKindName(k ermite.DependencyUpdate_Kind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "KIND_"))
}

// depsExpected turns the updates of a listing into the versions to confirm.
func depsExpected(r *ermite.RepoDependencyUpdates) []*ermite.DependencyVersion {
	out := make([]*ermite.DependencyVersion, 0, len(r.GetUpdates()))
	for _, u := range r.GetUpdates() {
		out = append(out, &ermite.DependencyVersion{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetLatest()})
	}
	return out
}

func newDepsCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "deps", Short: "Bernard: pending dependency updates (owner only)"}
	cmd.AddCommand(newDepsListCmd(env), newDepsCheckCmd(env))
	for _, name := range []string{"open", "ignore", "snooze"} {
		cmd.AddCommand(newDepsActionCmd(env, name))
	}
	return cmd
}

func printDepsRepo(cmd *cobra.Command, r *ermite.RepoDependencyUpdates) error {
	pairs := [][2]string{{"repo", r.GetRepo()}, {"state", depsStateName(r.GetState())}}
	if r.GetError() != "" {
		pairs = append(pairs, [2]string{"error", r.GetError()})
	}
	if r.GetMergeRequestNumber() != 0 {
		pairs = append(pairs, [2]string{"merge request", fmt.Sprintf("#%d", r.GetMergeRequestNumber())})
	}
	pairs = append(pairs, [2]string{"checked", shortTime(r.GetCheckedAt(), "never")}, [2]string{"snoozed until", shortTime(r.GetSnoozedUntil(), "-")})
	if err := fields(cmd.OutOrStdout(), pairs); err != nil {
		return err
	}
	return printDepsUpdates(cmd, r.GetUpdates())
}

func printDepsUpdates(cmd *cobra.Command, ups []*ermite.DependencyUpdate) error {
	rows := make([][]string, 0, len(ups))
	for _, u := range ups {
		mark := ""
		if u.GetNew() {
			mark = "new"
		}
		rows = append(rows, []string{u.GetName(), u.GetCurrent(), u.GetLatest(), depsKindName(u.GetKind()), u.GetDirectory(), mark})
	}
	return table(cmd.OutOrStdout(), []string{"NAME", "CURRENT", "LATEST", "KIND", "DIRECTORY", ""}, rows)
}

func newDepsListCmd(env *cliEnv) *cobra.Command {
	var repo string
	cmd := &cobra.Command{Use: "list", Short: "List the repositories with pending dependency updates", Args: cobra.NoArgs,
		Long: "List pending updates, snoozed repositories included (--repo limits to one)." + jsonDepsHelp}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		req := &ermite.ListDependencyUpdatesRequest{}
		if cmd.Flags().Changed("repo") {
			req.Repo = &repo
		}
		res, err := c.Deps().ListDependencyUpdates(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			out := []jsonDepsRepo{}
			for _, r := range res.Msg.GetRepos() {
				out = append(out, toJSONDepsRepo(r))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		rows := [][]string{}
		for _, r := range res.Msg.GetRepos() {
			for _, u := range r.GetUpdates() {
				mark := ""
				if u.GetNew() {
					mark = "new"
				}
				rows = append(rows, []string{r.GetRepo(), u.GetName(), u.GetCurrent(), u.GetLatest(), depsKindName(u.GetKind()), mark, depsStateName(r.GetState())})
			}
		}
		return table(cmd.OutOrStdout(), []string{"REPO", "NAME", "CURRENT", "LATEST", "KIND", "", "STATE"}, rows)
	}
	return cmd
}

func newDepsCheckCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "check <repo>", Short: "Check the dependencies of a repository now", Args: cobra.ExactArgs(1),
		Long: "Check a non-archived repository now and print its pending updates." + jsonDepsHelp}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Deps().CheckDependencies(env.ctx(cmd), connect.NewRequest(&ermite.CheckDependenciesRequest{Repo: args[0]}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONDepsRepo(res.Msg.GetUpdates()))
		}
		return printDepsRepo(cmd, res.Msg.GetUpdates())
	}
	return cmd
}

func newDepsActionCmd(env *cliEnv, name string) *cobra.Command {
	short := map[string]string{
		"open":   "Open or update the Bernard merge request",
		"ignore": "Ignore the pending versions of a repository",
		"snooze": "Hide a repository's updates for the snooze delay",
	}[name]
	long := short + "."
	if name != "snooze" {
		long += "\nThe pending list is read first and sent back as the versions you confirm: if it changes in\nbetween, the server refuses (failed precondition)."
	}
	cmd := &cobra.Command{Use: name + " <repo>", Short: short, Long: long + jsonDepsHelp, Args: cobra.ExactArgs(1)}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		var got *ermite.RepoDependencyUpdates
		if name == "snooze" {
			res, err := c.Deps().SnoozeDependencyUpdates(ctx, connect.NewRequest(&ermite.SnoozeDependencyUpdatesRequest{Repo: args[0]}))
			if err != nil {
				return c.Describe(err)
			}
			got = res.Msg.GetUpdates()
		} else {
			repo := args[0]
			list, err := c.Deps().ListDependencyUpdates(ctx, connect.NewRequest(&ermite.ListDependencyUpdatesRequest{Repo: &repo}))
			if err != nil {
				return c.Describe(err)
			}
			var cur *ermite.RepoDependencyUpdates
			for _, r := range list.Msg.GetRepos() {
				if r.GetRepo() == repo {
					cur = r
				}
			}
			if cur == nil {
				return fmt.Errorf("no pending dependency updates for %s", clean(repo))
			}
			expected := depsExpected(cur)
			if name == "open" {
				res, err := c.Deps().OpenDependencyUpdates(ctx, connect.NewRequest(&ermite.OpenDependencyUpdatesRequest{Repo: repo, ExpectedVersions: expected}))
				if err != nil {
					return c.Describe(err)
				}
				got = res.Msg.GetUpdates()
			} else {
				res, err := c.Deps().IgnoreDependencyUpdates(ctx, connect.NewRequest(&ermite.IgnoreDependencyUpdatesRequest{Repo: repo, ExpectedVersions: expected}))
				if err != nil {
					return c.Describe(err)
				}
				got = res.Msg.GetUpdates()
			}
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONDepsRepo(got))
		}
		noticef(cmd, "%s: %s", name, clean(args[0]))
		return nil
	}
	return cmd
}
