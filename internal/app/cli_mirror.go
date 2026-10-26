// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

const jsonMirrorKeyHelp = `
JSON fields of a key (--json): authorized_key, fingerprint, created_at (RFC 3339, UTC).`

const jsonMirrorConfigHelp = `
JSON fields of the policy (--json): enabled, url_template, cron, all_branches, updated_at (RFC 3339, UTC, null if never set).`

const jsonMirrorStatusHelp = `
JSON fields of a status (--json): repo, enabled, source (disabled, global, override, excluded), url
(admins only, else empty), cron, all_branches, state (disabled, ok, failing, paused), failure_class,
reason, detail (admins only), consecutive_failures, last_attempt_at, last_success_at, next_retry_at,
next_run_at, paused_at (RFC 3339, UTC, null when unset).`

type jsonMirrorKey struct {
	AuthorizedKey string  `json:"authorized_key"`
	Fingerprint   string  `json:"fingerprint"`
	CreatedAt     *string `json:"created_at"`
}

type jsonMirrorConfig struct {
	Enabled     bool    `json:"enabled"`
	URLTemplate string  `json:"url_template"`
	Cron        string  `json:"cron"`
	AllBranches bool    `json:"all_branches"`
	UpdatedAt   *string `json:"updated_at"`
}

type jsonMirrorStatus struct {
	Repo                string  `json:"repo"`
	Enabled             bool    `json:"enabled"`
	Source              string  `json:"source"`
	URL                 string  `json:"url"`
	Cron                string  `json:"cron"`
	AllBranches         bool    `json:"all_branches"`
	State               string  `json:"state"`
	FailureClass        string  `json:"failure_class"`
	Reason              string  `json:"reason"`
	Detail              string  `json:"detail"`
	ConsecutiveFailures int32   `json:"consecutive_failures"`
	LastAttemptAt       *string `json:"last_attempt_at"`
	LastSuccessAt       *string `json:"last_success_at"`
	NextRetryAt         *string `json:"next_retry_at"`
	NextRunAt           *string `json:"next_run_at"`
	PausedAt            *string `json:"paused_at"`
}

func toJSONMirrorKey(k *ermite.MirrorKey) jsonMirrorKey {
	return jsonMirrorKey{AuthorizedKey: k.GetAuthorizedKey(), Fingerprint: k.GetFingerprint(), CreatedAt: jsonTime(k.GetCreatedAt())}
}

func toJSONMirrorConfig(g *ermite.MirrorConfig) jsonMirrorConfig {
	return jsonMirrorConfig{Enabled: g.GetEnabled(), URLTemplate: g.GetUrlTemplate(), Cron: g.GetCron(), AllBranches: g.GetAllBranches(), UpdatedAt: jsonTime(g.GetUpdatedAt())}
}

func mirrorSourceName(s ermite.MirrorSource) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "MIRROR_SOURCE_"))
}

func toJSONMirrorStatus(s *ermite.MirrorStatus) jsonMirrorStatus {
	t := s.GetTarget()
	return jsonMirrorStatus{
		Repo: t.GetRepo(), Enabled: t.GetEnabled(), Source: mirrorSourceName(t.GetSource()), URL: t.GetUrl(), Cron: t.GetCron(), AllBranches: t.GetAllBranches(),
		State: mirrorStateName(s.GetState()), FailureClass: s.GetFailureClass(), Reason: s.GetReason(), Detail: s.GetDetail(), ConsecutiveFailures: s.GetConsecutiveFailures(),
		LastAttemptAt: jsonTime(s.GetLastAttemptAt()), LastSuccessAt: jsonTime(s.GetLastSuccessAt()), NextRetryAt: jsonTime(s.GetNextRetryAt()), NextRunAt: jsonTime(s.GetNextRunAt()), PausedAt: jsonTime(s.GetPausedAt()),
	}
}

func newMirrorCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "mirror", Short: "Manage non-forcing push mirrors"}
	key := &cobra.Command{Use: "key", Short: "Show or rotate the mirror SSH key", Long: "Manage the mirror SSH key." + jsonMirrorKeyHelp}
	for _, rotate := range []bool{false, true} {
		name, short := "show", "Print the mirror authorized_keys line"
		long := "Print only the authorized_keys line on stdout; requires authentication and ERMITE_SECRETS_KEY on the server.\nPipe it into: gh ssh-key add - --title \"ermite mirror\"." + "\nJSON: a key; see `ermite mirror key --help`."
		if rotate {
			name, short = "rotate", "Replace the mirror SSH key (owner only)"
			long = "Replace the mirror SSH key at once and print the new authorized_keys line on stdout; the replaced\nkey's fingerprint goes to stderr. Every mirror fails until the new key is added to the remotes.\nRequires owner rights and ERMITE_SECRETS_KEY on the server." + "\nJSON: a key; see `ermite mirror key --help`."
		}
		sub := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, Long: long}
		asJSON := addJSONFlag(sub)
		sub.RunE = func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(cmd)
			if err != nil {
				return err
			}
			var k *ermite.MirrorKey
			if rotate {
				res, e := c.Mirrors().RotateMirrorKey(env.ctx(cmd), connect.NewRequest(&ermite.RotateMirrorKeyRequest{}))
				if e != nil {
					return c.Describe(e)
				}
				k = res.Msg.GetKey()
				if prev := res.Msg.GetPreviousFingerprint(); prev != "" {
					noticef(cmd, "replaced key %s", clean(prev))
				}
				noticef(cmd, "remotes refuse pushes until this key is added")
			} else {
				res, e := c.Mirrors().GetMirrorKey(env.ctx(cmd), connect.NewRequest(&ermite.GetMirrorKeyRequest{}))
				if e != nil {
					return c.Describe(e)
				}
				k = res.Msg.GetKey()
			}
			if *asJSON {
				return writeJSON(cmd.OutOrStdout(), toJSONMirrorKey(k))
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), clean(k.GetAuthorizedKey()))
			return err
		}
		key.AddCommand(sub)
	}
	cmd.AddCommand(key, newMirrorConfigCmd(env), newMirrorAddCmd(env), newMirrorStatusCmd(env), newMirrorSyncCmd(env))
	for _, name := range []string{"remove", "exclude", "include"} {
		sub := &cobra.Command{Use: name + " <repo>", Short: strings.ToUpper(name[:1]) + name[1:] + " a repository mirror", Args: cobra.ExactArgs(1)}
		sub.RunE = func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd)
			if err != nil {
				return err
			}
			if name == "remove" {
				_, err = c.Mirrors().RemoveRepoMirror(env.ctx(cmd), connect.NewRequest(&ermite.RemoveRepoMirrorRequest{Repo: args[0]}))
			} else {
				_, err = c.Mirrors().SetRepoMirrorExcluded(env.ctx(cmd), connect.NewRequest(&ermite.SetRepoMirrorExcludedRequest{Repo: args[0], Excluded: name == "exclude"}))
			}
			if err != nil {
				return c.Describe(err)
			}
			noticef(cmd, "%s: %s", name, clean(args[0]))
			return nil
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func newMirrorConfigCmd(env *cliEnv) *cobra.Command {
	var enabled, all bool
	var url, cron string
	cmd := &cobra.Command{Use: "config", Short: "Patch the global mirror policy (owner only)", Long: "Change the flags given and keep the other settings; use `config show` to read the policy." + jsonMirrorConfigHelp, Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&enabled, "enabled", false, "enable the global mirror policy")
	cmd.Flags().StringVar(&url, "url", "", "SSH URL template containing {repo}")
	cmd.Flags().StringVar(&cron, "cron", "", "five-field UTC cron (minimum gap five minutes)")
	cmd.Flags().BoolVar(&all, "all-branches", false, "mirror all branches instead of only the default branch")
	asJSON := addJSONFlag(cmd)
	output := func(cmd *cobra.Command, g *ermite.MirrorConfig, json bool) error {
		if json {
			return writeJSON(cmd.OutOrStdout(), toJSONMirrorConfig(g))
		}
		return fields(cmd.OutOrStdout(), [][2]string{{"enabled", strconv.FormatBool(g.GetEnabled())}, {"url", g.GetUrlTemplate()}, {"cron (UTC)", g.GetCron()}, {"all branches", strconv.FormatBool(g.GetAllBranches())}})
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		req := &ermite.SetMirrorConfigRequest{}
		if cmd.Flags().Changed("enabled") {
			req.Enabled = &enabled
		}
		if cmd.Flags().Changed("url") {
			req.UrlTemplate = &url
		}
		if cmd.Flags().Changed("cron") {
			req.Cron = &cron
		}
		if cmd.Flags().Changed("all-branches") {
			req.AllBranches = &all
		}
		if req.Enabled == nil && req.UrlTemplate == nil && req.Cron == nil && req.AllBranches == nil {
			return fmt.Errorf("specify at least one config flag, or use `mirror config show`")
		}
		res, err := c.Mirrors().SetMirrorConfig(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return output(cmd, res.Msg.GetConfig(), *asJSON)
	}
	show := &cobra.Command{Use: "show", Short: "Show the global mirror policy", Long: "Show the global mirror policy (owner only)." + jsonMirrorConfigHelp, Args: cobra.NoArgs}
	showJSON := addJSONFlag(show)
	show.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Mirrors().GetMirrorConfig(env.ctx(cmd), connect.NewRequest(&ermite.GetMirrorConfigRequest{}))
		if err != nil {
			return c.Describe(err)
		}
		return output(cmd, res.Msg.GetConfig(), *showJSON)
	}
	cmd.AddCommand(show)
	return cmd
}

func newMirrorAddCmd(env *cliEnv) *cobra.Command {
	var cron string
	var all bool
	cmd := &cobra.Command{Use: "add <repo> <url>", Short: "Set a repository mirror override", Args: cobra.ExactArgs(2)}
	cmd.Flags().StringVar(&cron, "cron", "", "five-field UTC cron (empty inherits global)")
	cmd.Flags().BoolVar(&all, "all-branches", false, "mirror all branches (omitted inherits global)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		req := &ermite.SetRepoMirrorRequest{Repo: args[0], Url: args[1], Cron: cron}
		if cmd.Flags().Changed("all-branches") {
			req.AllBranches = &all
		}
		_, err = c.Mirrors().SetRepoMirror(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		noticef(cmd, "mirror configured for %s", clean(args[0]))
		return nil
	}
	return cmd
}

func mirrorStateName(s ermite.MirrorState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "MIRROR_STATE_"))
}

func newMirrorStatusCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "status [repo]", Short: "Show mirror states, reasons and next runs", Long: "Show one repository, or every repository (owner only).\n--json prints an object for one repository and an array for all." + jsonMirrorStatusHelp, Args: cobra.MaximumNArgs(1)}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		var statuses []*ermite.MirrorStatus
		if len(args) == 1 {
			res, err := c.Mirrors().GetMirrorStatus(env.ctx(cmd), connect.NewRequest(&ermite.GetMirrorStatusRequest{Repo: args[0]}))
			if err != nil {
				return c.Describe(err)
			}
			if *asJSON {
				return writeJSON(cmd.OutOrStdout(), toJSONMirrorStatus(res.Msg.GetStatus()))
			}
			statuses = append(statuses, res.Msg.GetStatus())
		} else {
			res, err := c.Mirrors().ListMirrorStatus(env.ctx(cmd), connect.NewRequest(&ermite.ListMirrorStatusRequest{}))
			if err != nil {
				return c.Describe(err)
			}
			if *asJSON {
				items := make([]jsonMirrorStatus, 0, len(res.Msg.GetStatuses()))
				for _, st := range res.Msg.GetStatuses() {
					items = append(items, toJSONMirrorStatus(st))
				}
				return writeJSON(cmd.OutOrStdout(), items)
			}
			statuses = res.Msg.GetStatuses()
		}
		rows := make([][]string, 0, len(statuses))
		for _, s := range statuses {
			rows = append(rows, []string{s.GetTarget().GetRepo(), mirrorStateName(s.GetState()), s.GetReason(), shortTime(s.GetLastSuccessAt(), "-"), shortTime(s.GetNextRunAt(), "-"), strconv.Itoa(int(s.GetConsecutiveFailures()))})
		}
		return table(cmd.OutOrStdout(), []string{"REPO", "STATE", "REASON", "LAST SUCCESS", "NEXT RUN", "FAILURES"}, rows)
	}
	return cmd
}

// newMirrorSyncCmd describes the server's wait bound (usecase.DefaultMirrorSyncWait, 8 s).
func newMirrorSyncCmd(env *cliEnv) *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{Use: "sync <repo>|--all", Short: "Sync mirrors now", Long: "Sync one repository, or every repository with --all (owner only).\nThe server pushes a few repositories at a time. The command waits about 8 s; a sync still running or waiting\nthen is reported as queued, keeps going on the server, and its outcome is shown by `ermite mirror status`.\n--force never force-pushes: it only skips the no-op check and throttle and lifts a pause.", Args: func(_ *cobra.Command, args []string) error {
		if (all && len(args) == 0) || (!all && len(args) == 1) {
			return nil
		}
		return fmt.Errorf("specify exactly one repository or --all")
	}}
	cmd.Flags().BoolVar(&all, "all", false, "sync all repository mirrors")
	cmd.Flags().BoolVar(&force, "force", false, "skip no-op check/throttle and lift pause; never force-push")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		req := &ermite.SyncMirrorRequest{All: all, Force: force}
		if !all {
			req.Repo = args[0]
		}
		res, err := c.Mirrors().SyncMirror(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		failed, pending := 0, 0
		rows := make([][]string, 0, len(res.Msg.GetResults()))
		for _, r := range res.Msg.GetResults() {
			if r.GetQueued() {
				pending++
			}
			reason := r.GetStatus().GetReason()
			if r.GetError() != "" {
				failed++
				reason = r.GetError()
			}
			rows = append(rows, []string{r.GetRepo(), mirrorStateName(r.GetStatus().GetState()), strconv.FormatBool(r.GetQueued()), strconv.FormatBool(r.GetNoOp()), reason})
		}
		if err := table(cmd.OutOrStdout(), []string{"REPO", "STATE", "QUEUED", "NO-OP", "REASON"}, rows); err != nil {
			return err
		}
		if pending > 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%d of %d mirror syncs still running: they continue in the background; check `ermite mirror status`\n", pending, len(rows))
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d mirror syncs failed", failed, len(rows))
		}
		return nil
	}
	return cmd
}
