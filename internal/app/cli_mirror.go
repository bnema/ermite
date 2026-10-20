// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-api/gen/ermite"
)

// mirrorJSON keeps the API's snake_case field names, including unset defaults.
func mirrorJSON(cmd *cobra.Command, msg proto.Message) error {
	b, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(b))
	return err
}

func newMirrorCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "mirror", Short: "Manage non-forcing push mirrors"}
	key := &cobra.Command{Use: "key", Short: "Show or rotate the mirror SSH key"}
	for _, rotate := range []bool{false, true} {
		name := "show"
		if rotate {
			name = "rotate"
		}
		sub := &cobra.Command{Use: name, Short: "Print the mirror authorized_keys line", Args: cobra.NoArgs,
			Long: "Print only the authorized_keys line on stdout; requires authentication and ERMITE_SECRETS_KEY on the server.\nPipe show into: gh ssh-key add - --title \"ermite mirror\". Rotation requires owner rights and takes effect immediately."}
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
				noticef(cmd, "remotes refuse pushes until this key is added")
			} else {
				res, e := c.Mirrors().GetMirrorKey(env.ctx(cmd), connect.NewRequest(&ermite.GetMirrorKeyRequest{}))
				if e != nil {
					return c.Describe(e)
				}
				k = res.Msg.GetKey()
			}
			if *asJSON {
				return mirrorJSON(cmd, k)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), clean(k.GetAuthorizedKey()))
			return err
		}
		key.AddCommand(sub)
	}
	cmd.AddCommand(key, newMirrorConfigCmd(env), newMirrorAddCmd(env), newMirrorStatusCmd(env), newMirrorSyncCmd(env))
	for _, name := range []string{"remove", "exclude", "include"} {
		sub := &cobra.Command{Use: name + " <repo>", Short: name + " a repository mirror", Args: cobra.ExactArgs(1)}
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
	cmd := &cobra.Command{Use: "config", Short: "Patch the global mirror policy (owner only)", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&enabled, "enabled", false, "enable the global mirror policy")
	cmd.Flags().StringVar(&url, "url", "", "SSH URL template containing {repo}")
	cmd.Flags().StringVar(&cron, "cron", "", "five-field UTC cron (minimum gap five minutes)")
	cmd.Flags().BoolVar(&all, "all-branches", false, "mirror all branches instead of only the default branch")
	asJSON := addJSONFlag(cmd)
	output := func(cmd *cobra.Command, g *ermite.MirrorConfig, json bool) error {
		if json {
			return mirrorJSON(cmd, g)
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
	show := &cobra.Command{Use: "show", Short: "Show the global mirror policy", Args: cobra.NoArgs}
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
	cmd := &cobra.Command{Use: "status [repo]", Short: "Show mirror states, reasons and next runs", Args: cobra.MaximumNArgs(1)}
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
				return mirrorJSON(cmd, res.Msg.GetStatus())
			}
			statuses = append(statuses, res.Msg.GetStatus())
		} else {
			res, err := c.Mirrors().ListMirrorStatus(env.ctx(cmd), connect.NewRequest(&ermite.ListMirrorStatusRequest{}))
			if err != nil {
				return c.Describe(err)
			}
			if *asJSON {
				// Marshal each status with proto JSON, but retain the CLI list convention: an array.
				items := make([]string, 0, len(res.Msg.GetStatuses()))
				for _, s := range res.Msg.GetStatuses() {
					b, e := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(s)
					if e != nil {
						return e
					}
					items = append(items, string(b))
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "["+strings.Join(items, ",")+"]")
				return err
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

func newMirrorSyncCmd(env *cliEnv) *cobra.Command {
	var all, force bool
	cmd := &cobra.Command{Use: "sync <repo>|--all", Short: "Sync mirrors now", Long: "Sync one repository, or every repository with --all (owner only).\n--force never force-pushes: it only skips the no-op check and throttle and lifts a pause.", Args: func(_ *cobra.Command, args []string) error {
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
		rows := make([][]string, 0, len(res.Msg.GetResults()))
		for _, r := range res.Msg.GetResults() {
			rows = append(rows, []string{r.GetRepo(), mirrorStateName(r.GetStatus().GetState()), strconv.FormatBool(r.GetQueued()), strconv.FormatBool(r.GetNoOp()), r.GetStatus().GetReason()})
		}
		return table(cmd.OutOrStdout(), []string{"REPO", "STATE", "QUEUED", "NO-OP", "REASON"}, rows)
	}
	return cmd
}
