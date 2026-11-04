// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

type jsonDepsUpdate struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Kind      string `json:"kind" help:"patch, minor or major"`
	New       bool   `json:"new" help:"absent from the previous check"`
	NewName   string `json:"new_name" help:"new module path, Go path major only"`
}

type jsonDepsBatch struct {
	Key                string           `json:"key" help:"empty = base"`
	Updates            []jsonDepsUpdate `json:"updates"`
	State              string           `json:"state" help:"idle, queued, drafted, applying or failed"`
	Error              string           `json:"error"`
	RunID              int64            `json:"run_id" help:"0 = none"`
	MergeRequestNumber int64            `json:"merge_request_number" help:"0 = none"`
	Outdated           bool             `json:"outdated" help:"the merge request holds other versions"`
	Adopted            bool             `json:"adopted" help:"the branch was changed by hand: Bernard no longer rewrites it"`
	QueuePosition      int32            `json:"queue_position" help:"1 = next; 0 = not queued"`
	Branch             string           `json:"branch"`
	Repro              []string         `json:"repro" help:"local commands that redo a failed update"`
}

type jsonDepsName struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

type jsonDepsRepo struct {
	Repo               string           `json:"repo"`
	Updates            []jsonDepsUpdate `json:"updates" help:"base batch"`
	CheckedAt          jsonInstant      `json:"checked_at"`
	SnoozedUntil       jsonInstant      `json:"snoozed_until"`
	MergeRequestNumber int64            `json:"merge_request_number" help:"0 = none"`
	State              string           `json:"state" help:"idle, applying or failed"`
	Error              string           `json:"error"`
	RunID              int64            `json:"run_id" help:"the run applying the updates, or the last one that tried; 0 = none"`
	Batches            []jsonDepsBatch  `json:"batches"`
	AutoOpen           bool             `json:"auto_open" help:"Bernard opens the base batch's merge request by itself"`
	SplitOut           []jsonDepsName   `json:"split_out" help:"dependencies whose majors have their own batch"`
}

func toJSONDepsRepo(r *ermite.RepoDependencyUpdates) jsonDepsRepo {
	base := &ermite.DependencyBatch{}
	for _, b := range depsBatches(r) {
		if b.GetKey() == "" {
			base = b
			break
		}
	}
	out := jsonDepsRepo{
		Repo: r.GetRepo(), Updates: toJSONDepsRepoUpdates(base.GetUpdates()), CheckedAt: jsonTime(r.GetCheckedAt()), SnoozedUntil: jsonTime(r.GetSnoozedUntil()),
		MergeRequestNumber: base.GetMergeRequestNumber(), State: depsStateName(base.GetState()), Error: base.GetError(), RunID: base.GetRunId(),
		Batches: []jsonDepsBatch{}, AutoOpen: r.GetAutoOpen(), SplitOut: []jsonDepsName{},
	}
	for _, b := range r.GetBatches() {
		out.Batches = append(out.Batches, jsonDepsBatch{
			Key: b.GetKey(), Updates: toJSONDepsRepoUpdates(b.GetUpdates()), State: depsStateName(b.GetState()), Error: b.GetError(),
			RunID: b.GetRunId(), MergeRequestNumber: b.GetMergeRequestNumber(), Outdated: b.GetOutdated(),
			Adopted: b.GetAdopted(), QueuePosition: b.GetQueuePosition(), Branch: b.GetBranch(), Repro: append([]string{}, b.GetRepro()...),
		})
	}
	for _, n := range r.GetSplitOut() {
		out.SplitOut = append(out.SplitOut, jsonDepsName{Ecosystem: n.GetEcosystem(), Name: n.GetName()})
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
func depsExpected(r *ermite.DependencyBatch) []*ermite.DependencyVersion {
	out := make([]*ermite.DependencyVersion, 0, len(r.GetUpdates()))
	for _, u := range r.GetUpdates() {
		out = append(out, &ermite.DependencyVersion{Ecosystem: u.GetEcosystem(), Directory: u.GetDirectory(), Name: u.GetName(), Version: u.GetLatest()})
	}
	return out
}

func newDepsCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "deps", Short: "Bernard: pending dependency updates (owner only)", Long: "Manage pending dependency updates (owner only).\n" +
		jsonFieldsHelp("a repository", jsonDepsRepo{}) + " The log of a run, failed ones included: ermite run view <repo> <run> --log."}
	cmd.AddCommand(newDepsListCmd(env), newDepsCheckCmd(env), newDepsAutoCmd(env), newDepsSplitCmd(env))
	for _, name := range []string{"open", "ignore", "snooze"} {
		cmd.AddCommand(newDepsActionCmd(env, name))
	}
	return cmd
}

// depsBatchLabel is how tables and flags name a batch: "base" for the empty key.
func depsBatchLabel(key string) string {
	if key == "" {
		return depsBaseBatch
	}
	return key
}

// depsBaseBatch names the base batch (empty key) in tables and in --batch.
const depsBaseBatch = "base"

func printDepsRepo(cmd *cobra.Command, r *ermite.RepoDependencyUpdates) error {
	pairs := [][2]string{{"repo", r.GetRepo()}, {"checked", shortTime(r.GetCheckedAt(), "never")}, {"snoozed until", shortTime(r.GetSnoozedUntil(), "-")}, {"auto open", depsOnOff(r.GetAutoOpen())}}
	for _, n := range r.GetSplitOut() {
		pairs = append(pairs, [2]string{"split out", n.GetEcosystem() + " " + n.GetName()})
	}
	batches := depsBatches(r)
	for _, b := range batches {
		pairs = append(pairs, depsBatchDetails(r.GetRepo(), b)...)
	}
	if err := fields(cmd.OutOrStdout(), pairs); err != nil {
		return err
	}
	rows := [][]string{}
	for _, b := range batches {
		rows = append(rows, depsBatchRows(b, true)...)
	}
	return table(cmd.OutOrStdout(), []string{"BATCH", "NAME", "CURRENT", "LATEST", "KIND", "DIRECTORY", "", "STATE", "RUN"}, rows)
}

// depsBatchDetails lists the state, error, run and merge request of a batch.
func depsBatchDetails(repo string, b *ermite.DependencyBatch) [][2]string {
	pairs := [][2]string{{"batch", depsBatchLabel(b.GetKey())}, {"state", depsStateName(b.GetState())}}
	if b.GetQueuePosition() != 0 {
		pairs = append(pairs, [2]string{"queue position", strconv.Itoa(int(b.GetQueuePosition()))})
	}
	if b.GetAdopted() {
		pairs = append(pairs, [2]string{"branch", b.GetBranch() + " (changed by hand)"})
	}
	if b.GetError() != "" {
		for i, line := range strings.Split(b.GetError(), "\n") {
			label := ""
			if i == 0 {
				label = "error"
			}
			pairs = append(pairs, [2]string{label, line})
		}
	}
	if b.GetRunId() != 0 {
		pairs = append(pairs, [2]string{"run", strconv.FormatInt(b.GetRunId(), 10)})
		if b.GetState() == ermite.RepoDependencyUpdates_STATE_FAILED {
			pairs = append(pairs, [2]string{"log", fmt.Sprintf("ermite run view %s %d --log", repo, b.GetRunId())})
		}
	}
	if b.GetMergeRequestNumber() != 0 {
		mr := fmt.Sprintf("#%d", b.GetMergeRequestNumber())
		if b.GetOutdated() {
			mr += " (outdated)"
		}
		pairs = append(pairs, [2]string{"merge request", mr})
	}
	return pairs
}

// depsBatchRows is one table row per update of a batch: BATCH, NAME, CURRENT,
// LATEST, KIND, [DIRECTORY,] new mark, STATE, RUN.
func depsBatchRows(b *ermite.DependencyBatch, withDir bool) [][]string {
	run := "-"
	if b.GetRunId() != 0 {
		run = strconv.FormatInt(b.GetRunId(), 10)
	}
	rows := make([][]string, 0, len(b.GetUpdates()))
	for _, u := range b.GetUpdates() {
		mark := ""
		if u.GetNew() {
			mark = "new"
		}
		row := []string{depsBatchLabel(b.GetKey()), u.GetName(), u.GetCurrent(), u.GetLatest(), depsKindName(u.GetKind())}
		if withDir {
			row = append(row, u.GetDirectory())
		}
		rows = append(rows, append(row, mark, depsStateCell(b), run))
	}
	return rows
}

// depsStateCell is the STATE of a batch with its queue position ("queued #2")
// and an adopted branch ("idle adopted").
func depsStateCell(b *ermite.DependencyBatch) string {
	s := depsStateName(b.GetState())
	if b.GetQueuePosition() != 0 {
		s += fmt.Sprintf(" #%d", b.GetQueuePosition())
	}
	if b.GetAdopted() {
		s += " adopted"
	}
	return s
}

// depsRepoCell is the REPO of a listing, marked when Bernard does not open the
// base batch by itself.
func depsRepoCell(r *ermite.RepoDependencyUpdates) string {
	if !r.GetAutoOpen() {
		return r.GetRepo() + " (auto off)"
	}
	return r.GetRepo()
}

func depsOnOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func newDepsListCmd(env *cliEnv) *cobra.Command {
	var repo string
	cmd := &cobra.Command{Use: "list", Short: "List the repositories with pending dependency updates", Args: cobra.NoArgs,
		Long: "List pending updates, snoozed repositories included (--repo limits to one)." +
			"\nSTATE shows the queue position (queued #1 is next) and \"adopted\" when the branch was changed by hand; REPO is marked \"(auto off)\" when Bernard does not open the base merge request by itself." +
			"\nJSON: repository updates; see `ermite deps --help`."}
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
		res, err := c.Deps().ListDependencyUpdates(env.ctx(cmd), req)
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			out := []jsonDepsRepo{}
			for _, r := range res.GetRepos() {
				out = append(out, toJSONDepsRepo(r))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		rows := [][]string{}
		for _, r := range res.GetRepos() {
			for _, batch := range depsBatches(r) {
				for _, row := range depsBatchRows(batch, false) {
					rows = append(rows, append([]string{depsRepoCell(r)}, row...))
				}
			}
		}
		return table(cmd.OutOrStdout(), []string{"REPO", "BATCH", "NAME", "CURRENT", "LATEST", "KIND", "", "STATE", "RUN"}, rows)
	}
	return cmd
}

func newDepsCheckCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "check <repo>", Short: "Check the dependencies of a repository now", Args: cobra.ExactArgs(1),
		Long: "Check a non-archived repository now and print its pending updates." + "\nJSON: repository updates; see `ermite deps --help`."}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.clientWithTimeout(cmd, 90*time.Second)
		if err != nil {
			return err
		}
		res, err := c.Deps().CheckDependencies(env.ctx(cmd), &ermite.CheckDependenciesRequest{Repo: args[0]})
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONDepsRepo(res.GetUpdates()))
		}
		return printDepsRepo(cmd, res.GetUpdates())
	}
	return cmd
}

func newDepsActionCmd(env *cliEnv, name string) *cobra.Command {
	var batchKey string
	short := map[string]string{
		"open":   "Open or update the Bernard merge request of a batch",
		"ignore": "Ignore the pending versions of a batch",
		"snooze": "Hide a repository's updates for the snooze delay",
	}[name]
	long := short + "."
	if name != "snooze" {
		long += "\nThe command reads the pending versions and sends them back as a precondition: the server refuses if they changed meanwhile (failed precondition)."
	}
	cmd := &cobra.Command{Use: name + " <repo>", Short: short, Long: long + "\nJSON: repository updates; see `ermite deps --help`.", Args: cobra.ExactArgs(1)}
	if name != "snooze" {
		cmd.Flags().StringVar(&batchKey, "batch", depsBaseBatch, "`KEY` of the batch, from the BATCH column of deps list; base = patch and minor updates")
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		var got *ermite.RepoDependencyUpdates
		if name == "snooze" {
			res, err := c.Deps().SnoozeDependencyUpdates(ctx, &ermite.SnoozeDependencyUpdatesRequest{Repo: args[0]})
			if err != nil {
				return c.Describe(err)
			}
			got = res.GetUpdates()
		} else {
			repo := args[0]
			list, err := c.Deps().ListDependencyUpdates(ctx, &ermite.ListDependencyUpdatesRequest{Repo: &repo})
			if err != nil {
				return c.Describe(err)
			}
			var cur *ermite.RepoDependencyUpdates
			for _, r := range list.GetRepos() {
				if r.GetRepo() == repo {
					cur = r
				}
			}
			if cur == nil {
				return fmt.Errorf("no pending dependency updates for %s", clean(repo))
			}
			if batchKey == depsBaseBatch {
				batchKey = ""
			}
			var selected *ermite.DependencyBatch
			for _, batch := range depsBatches(cur) {
				if batch.GetKey() == batchKey {
					selected = batch
					break
				}
			}
			if selected == nil {
				return fmt.Errorf("no dependency batch %q for %s", clean(depsBatchLabel(batchKey)), clean(repo))
			}
			expected := depsExpected(selected)
			if name == "open" {
				res, err := c.Deps().OpenDependencyUpdates(ctx, &ermite.OpenDependencyUpdatesRequest{Repo: repo, Batch: batchKey, ExpectedVersions: expected})
				if err != nil {
					return c.Describe(err)
				}
				got = res.GetUpdates()
			} else {
				res, err := c.Deps().IgnoreDependencyUpdates(ctx, &ermite.IgnoreDependencyUpdatesRequest{Repo: repo, Batch: batchKey, ExpectedVersions: expected})
				if err != nil {
					return c.Describe(err)
				}
				got = res.GetUpdates()
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

func newDepsAutoCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "auto <repo> on|off", Short: "Turn Bernard's automatic merge request on or off for a repository",
		Long: "Turn on or off the merge request Bernard opens by itself for the patch and minor updates (the base batch) of a repository, after each check." +
			" Turning it on queues the base batch at once when it has updates and no current merge request. Majors are never opened by themselves. ERMITE_DEPS_AUTO_OPEN=0 turns it off for every repository." +
			"\nJSON: repository updates; see `ermite deps --help`.",
		Args: cobra.ExactArgs(2), ValidArgs: []string{"on", "off"}}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var on bool
		switch args[1] {
		case "on":
			on = true
		case "off":
		default:
			return fmt.Errorf("auto takes on or off, not %q", clean(args[1]))
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Deps().SetDependencyAutoOpen(env.ctx(cmd), &ermite.SetDependencyAutoOpenRequest{Repo: args[0], Enabled: on})
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONDepsRepo(res.GetUpdates()))
		}
		noticef(cmd, "auto open %s: %s", args[1], clean(args[0]))
		return nil
	}
	return cmd
}

func newDepsSplitCmd(env *cliEnv) *cobra.Command {
	var join bool
	cmd := &cobra.Command{Use: "split <repo> <ecosystem> <name>", Short: "Give a dependency's majors their own batch, or join them back",
		Long: "Pull the major updates of one dependency out of their family batch into a batch of their own (--join puts them back)." +
			" ECOSYSTEM is gomod, github-actions or npm; NAME is the module path, owner/repository of the action, or npm package, as in deps list." +
			" Refused while a batch it changes is applying or queued." +
			"\nJSON: repository updates; see `ermite deps --help`.",
		Args: cobra.ExactArgs(3)}
	cmd.Flags().BoolVar(&join, "join", false, "join the dependency back to its family batch")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Deps().SplitDependency(env.ctx(cmd), &ermite.SplitDependencyRequest{Repo: args[0], Ecosystem: args[1], Name: args[2], Split: !join})
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONDepsRepo(res.GetUpdates()))
		}
		verb := "split"
		if join {
			verb = "join"
		}
		noticef(cmd, "%s: %s %s %s", verb, clean(args[0]), clean(args[1]), clean(args[2]))
		return nil
	}
	return cmd
}

func toJSONDepsRepoUpdates(ups []*ermite.DependencyUpdate) []jsonDepsUpdate {
	out := []jsonDepsUpdate{}
	for _, u := range ups {
		out = append(out, jsonDepsUpdate{Ecosystem: u.GetEcosystem(), Name: u.GetName(), Directory: u.GetDirectory(), Current: u.GetCurrent(), Latest: u.GetLatest(), Kind: depsKindName(u.GetKind()), New: u.GetNew(), NewName: u.GetNewName()})
	}
	return out
}

// depsBatches retains compatibility with servers predating batch support.
func depsBatches(r *ermite.RepoDependencyUpdates) []*ermite.DependencyBatch {
	if len(r.GetBatches()) != 0 {
		return r.GetBatches()
	}
	//nolint:staticcheck // reads the deprecated base fields of servers without batches
	return []*ermite.DependencyBatch{{Updates: r.GetUpdates(), State: r.GetState(), Error: r.GetError(), RunId: r.GetRunId(), MergeRequestNumber: r.GetMergeRequestNumber()}}
}
