// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

var errNoJournal = fmt.Errorf("the server does not support %s, so it has no activity journal (upgrade the server, see `ermite version`)", client.FeatureJournal)

func requireJournal(ctx context.Context, c *client.Client) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), client.FeatureJournal) {
		return errNoJournal
	}
	return nil
}

func journalClient(env *cliEnv, cmd *cobra.Command) (*client.Client, error) {
	c, err := env.client(cmd)
	if err != nil {
		return nil, err
	}
	if err := requireJournal(env.ctx(cmd), c); err != nil {
		return nil, err
	}
	return c, nil
}

type jsonActivity struct {
	ID            string      `json:"id"`
	Kind          string      `json:"kind"`
	Repo          string      `json:"repo"`
	At            jsonInstant `json:"at"`
	Actor         string      `json:"actor"`
	Number        int64       `json:"number" help:"merge request or issue; 0 otherwise"`
	Ref           string      `json:"ref" help:"tag, release tag, pushed branch, or target branch of a merge request"`
	Title         string      `json:"title"`
	CommitCount   int32       `json:"commit_count" help:"pushes; at most 100"`
	CommitsCapped bool        `json:"commits_capped" help:"true when the push had more"`
	Commit        string      `json:"commit" help:"new head of a push"`
	Summary       string      `json:"summary" help:"subject of that commit"`
}

func toJSONActivity(e *ermite.ActivityEntry) jsonActivity {
	return jsonActivity{
		ID: e.GetId(), Kind: activityKindName(e.GetKind()), Repo: e.GetRepo(), At: jsonTime(e.GetAt()), Actor: e.GetActor(),
		Number: e.GetNumber(), Ref: e.GetRef(), Title: e.GetTitle(), CommitCount: e.GetCommitCount(),
		CommitsCapped: e.GetCommitsCapped(), Commit: e.GetCommit(), Summary: e.GetSummary(),
	}
}

// activityKindName is the kind as users type it: ACTIVITY_KIND_MR_MERGED is
// "mr_merged".
func activityKindName(k ermite.ActivityKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "ACTIVITY_KIND_"))
}

// activityKindNames lists the kinds --kind accepts.
func activityKindNames() []string {
	var names []string
	for n := int32(1); ; n++ {
		if _, ok := ermite.ActivityKind_name[n]; !ok {
			return names
		}
		names = append(names, activityKindName(ermite.ActivityKind(n)))
	}
}

func parseActivityKinds(flags []string) ([]ermite.ActivityKind, error) {
	var out []ermite.ActivityKind
	for _, f := range flags {
		for _, name := range strings.Split(f, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			v, ok := ermite.ActivityKind_value["ACTIVITY_KIND_"+strings.ToUpper(name)]
			if !ok || v == 0 {
				return nil, fmt.Errorf("unknown --kind %q: use one of %s", name, strings.Join(activityKindNames(), ", "))
			}
			if k := ermite.ActivityKind(v); !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out, nil
}

// parseSince reads --since: a duration back from now ("24h", "90m", "7d",
// "2w"; days and weeks are 24 h and 7 d), a date ("2026-10-01", midnight UTC)
// or an RFC 3339 instant. It must not be in the future.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("invalid --since %q: use a duration like 24h, 7d or 2w, a date like 2026-10-01, or an RFC 3339 time", s)
	var at time.Time
	switch {
	case s == "":
		return time.Time{}, bad
	case strings.HasSuffix(s, "d") || strings.HasSuffix(s, "w"):
		d, ok := client.ParseDayDuration(s)
		if !ok {
			return time.Time{}, bad
		}
		at = now.Add(-d)
	default:
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			at = now.Add(-d)
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			at = t
		} else if t, err := time.Parse(time.DateOnly, s); err == nil {
			at = t
		} else {
			return time.Time{}, bad
		}
	}
	if at.After(now) {
		return time.Time{}, fmt.Errorf("invalid --since %q: it is in the future", s)
	}
	return at.UTC(), nil
}

// activityDetail is the Detail cell of an entry.
func activityDetail(e *ermite.ActivityEntry) string {
	n := strconv.FormatInt(e.GetNumber(), 10)
	switch e.GetKind() {
	case ermite.ActivityKind_ACTIVITY_KIND_MR_OPENED, ermite.ActivityKind_ACTIVITY_KIND_MR_MERGED, ermite.ActivityKind_ACTIVITY_KIND_MR_CLOSED:
		return "!" + n + " " + e.GetTitle()
	case ermite.ActivityKind_ACTIVITY_KIND_ISSUE_OPENED, ermite.ActivityKind_ACTIVITY_KIND_ISSUE_CLOSED:
		return "#" + n + " " + e.GetTitle()
	case ermite.ActivityKind_ACTIVITY_KIND_RELEASE_PUBLISHED:
		if e.GetTitle() != "" && e.GetTitle() != e.GetRef() {
			return e.GetRef() + " " + e.GetTitle()
		}
		return e.GetRef()
	case ermite.ActivityKind_ACTIVITY_KIND_PUSH:
		out := e.GetRef() + ": " + commitCount(int(e.GetCommitCount()), e.GetCommitsCapped())
		if e.GetSummary() != "" {
			out += ", " + e.GetSummary()
		}
		return out
	}
	return e.GetRef()
}

// commitCount is "1 commit", "4 commits" or "100+ commits".
func commitCount(n int, capped bool) string {
	switch {
	case capped:
		return strconv.Itoa(n) + "+ commits"
	case n == 1:
		return "1 commit"
	}
	return strconv.Itoa(n) + " commits"
}

func newJournalCmd(env *cliEnv) *cobra.Command {
	var since, repo string
	var kinds []string
	var limit int
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Show recent repository activity",
		Long: "Show activity, newest first: merge requests and issues opened, merged or closed, tags,\n" +
			"releases and default-branch pushes. The server keeps entries 30 days. Needs owner rights or\n" +
			"journal:read (repository-limited tokens see only their repositories) and a server with the journal\n" +
			"feature.\n\nKinds: " + strings.Join(activityKindNames(), ", ") + ".\n" +
			jsonFieldsHelp("an entry", jsonActivity{}) + " --json prints an array, newest first.",
		Example: "  ermite journal --since 7d --kind mr_merged,tag_created\n  ermite journal --repo app --since 2026-10-01 --json",
		Args:    cobra.NoArgs,
	}
	cmd.Flags().StringVar(&since, "since", "24h", "start: a duration back (24h, 7d, 2w), a date (2026-10-01) or an RFC 3339 time")
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds, comma-separated or repeated")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum entries")
	asJSON := addJSONFlag(cmd)
	_ = cmd.RegisterFlagCompletionFunc("repo", completeRepoNames(env))
	_ = cmd.RegisterFlagCompletionFunc("kind", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return activityKindNames(), cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if limit <= 0 {
			return errors.New("--limit must be positive")
		}
		at, err := parseSince(since, env.now())
		if err != nil {
			return err
		}
		ks, err := parseActivityKinds(kinds)
		if err != nil {
			return err
		}
		c, err := journalClient(env, cmd)
		if err != nil {
			return err
		}
		req := &ermite.ListActivityRequest{Since: timestamppb.New(at), Kinds: ks}
		if repo != "" {
			req.Repo = &repo
		}
		all := []*ermite.ActivityEntry{}
		seen := map[string]bool{}
		for len(all) < limit {
			req.PageSize = int32(min(limit-len(all), 500))
			res, err := c.Journal().ListActivity(env.ctx(cmd), connect.NewRequest(req))
			if err != nil {
				return c.Describe(err)
			}
			all = append(all, res.Msg.GetEntries()...)
			next := res.Msg.GetNextPageToken()
			if next == "" {
				break
			}
			if seen[next] {
				return fmt.Errorf("the server sent the journal page token %q twice; stopping to avoid a loop", next)
			}
			seen[next] = true
			req.PageToken = next
		}
		if len(all) > limit {
			all = all[:limit]
		}
		if *asJSON {
			out := make([]jsonActivity, 0, len(all))
			for _, e := range all {
				out = append(out, toJSONActivity(e))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		rows := make([][]string, 0, len(all))
		for _, e := range all {
			rows = append(rows, []string{shortTime(e.GetAt(), "-"), e.GetRepo(), activityKindName(e.GetKind()), activityDetail(e), e.GetActor()})
		}
		return table(cmd.OutOrStdout(), []string{"WHEN", "REPO", "KIND", "DETAIL", "ACTOR"}, rows)
	}
	return cmd
}

type jsonUnreleased struct {
	Repo          string      `json:"repo"`
	LatestTag     string      `json:"latest_tag" help:"empty when none within 500 commits"`
	LatestTagAt   jsonInstant `json:"latest_tag_at"`
	CommitsSince  int32       `json:"commits_since" help:"at most 500"`
	CommitsCapped bool        `json:"commits_capped" help:"the walk hit the limit first; commits_since is then a lower bound"`
	DefaultBranch string      `json:"default_branch"`
}

// agoText is a rough age: "just now", "5 minutes ago", "3 hours ago", "4 days ago".
func agoText(now, t time.Time) string {
	d := now.Sub(t)
	unit := func(n int, word string) string {
		if n == 1 {
			return "1 " + word + " ago"
		}
		return strconv.Itoa(n) + " " + word + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return unit(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return unit(int(d/time.Hour), "hour")
	}
	return unit(int(d/(24*time.Hour)), "day")
}

func newReleasePendingCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pending [repo]",
		Short: "List the repositories with commits after their latest tag",
		Long: "List non-archived repositories whose default branch has commits after its nearest tag.\n" +
			"Repositories without a tag are left out. Needs owner rights or journal:read, and the journal feature.\n" +
			jsonFieldsHelp("a repository", jsonUnreleased{}) + " --json prints an array sorted by name.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := journalClient(env, cmd)
		if err != nil {
			return err
		}
		req := &ermite.ListUnreleasedRequest{}
		if len(args) == 1 {
			req.Repo = &args[0]
		}
		res, err := c.Releases().ListUnreleased(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			out := []jsonUnreleased{}
			for _, u := range res.Msg.GetRepos() {
				out = append(out, jsonUnreleased{
					Repo: u.GetRepo(), LatestTag: u.GetLatestTag(), LatestTagAt: jsonTime(u.GetLatestTagAt()),
					CommitsSince: u.GetCommitsSince(), CommitsCapped: u.GetCommitsCapped(), DefaultBranch: u.GetDefaultBranch(),
				})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(res.Msg.GetRepos()) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "every tagged repository is released")
			return err
		}
		rows := make([][2]string, 0, len(res.Msg.GetRepos()))
		for _, u := range res.Msg.GetRepos() {
			text := commitCount(int(u.GetCommitsSince()), u.GetCommitsCapped()) + " since "
			if u.GetLatestTag() == "" {
				text += "the latest tag"
			} else {
				text += u.GetLatestTag()
			}
			if u.GetLatestTagAt().IsValid() {
				text += " (" + agoText(env.now(), u.GetLatestTagAt().AsTime()) + ")"
			}
			rows = append(rows, [2]string{clean(u.GetRepo()), text})
		}
		return fields(cmd.OutOrStdout(), rows)
	}
	return cmd
}
