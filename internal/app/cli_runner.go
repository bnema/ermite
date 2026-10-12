// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

type jsonRunner struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Labels       []string `json:"labels"`
	Isolation    string   `json:"isolation"`
	Version      string   `json:"version"`
	Arch         string   `json:"arch"`
	Os           string   `json:"os"`
	RegisteredAt *string  `json:"registered_at"`
	LastSeen     *string  `json:"last_seen"`
	Busy         bool     `json:"busy"`
}

const jsonRunnerHelp = `
JSON fields of a runner (--json): id, name, labels, isolation (vm, container-sysbox,
container-privileged), version, arch, os, registered_at, last_seen (RFC 3339 UTC, null if never
seen) and busy. A runner not seen for more than about 2 minutes is offline.`

func newRunnerCmd(env *cliEnv) *cobra.Command {
	runner := &cobra.Command{
		Use:   "runner",
		Short: "List and remove CI runners",
		Long: "Manage the CI runners registered on a remote server. A runner registers itself with a token\n" +
			"made by `ermite token create --preset runner`. Needs the runner:register scope, on a token that is\n" +
			"not limited to some repositories.",
	}
	runner.AddCommand(newRunnerListCmd(env), newRunnerRemoveCmd(env))
	return runner
}

func newRunnerListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered runners",
		Long:  "List the registered runners ordered by name." + jsonRunnerHelp + "\n--json prints an array of runners.",
		Args:  cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		runners, err := collect(ctx, func(token string) ([]*ermite.Runner, string, error) {
			res, err := c.Runs().ListRunners(ctx, connect.NewRequest(&ermite.ListRunnersRequest{PageToken: token}))
			if err != nil {
				return nil, "", err
			}
			return res.Msg.GetRunners(), res.Msg.GetNextPageToken(), nil
		})
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonRunner, 0, len(runners))
			for _, r := range runners {
				labels := r.GetLabels()
				if labels == nil {
					labels = []string{}
				}
				out = append(out, jsonRunner{
					ID: r.GetId(), Name: r.GetName(), Labels: labels, Isolation: isolationText(r.GetIsolation()), Version: r.GetVersion(),
					Arch: r.GetArch(), Os: r.GetOs(), RegisteredAt: jsonTime(r.GetRegisteredAt()), LastSeen: jsonTime(r.GetLastSeen()), Busy: r.GetBusy(),
				})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(runners) == 0 {
			noticef(cmd, "no runners: register one with `ermite-runner register`")
			return nil
		}
		rows := make([][]string, 0, len(runners))
		for _, r := range runners {
			state := "offline"
			if ls := r.GetLastSeen(); ls.IsValid() && env.now().Sub(ls.AsTime()) < 2*time.Minute {
				state = "online"
				if r.GetBusy() {
					state = "busy"
				}
			}
			rows = append(rows, []string{
				r.GetId(), r.GetName(), state, isolationText(r.GetIsolation()), r.GetVersion(),
				strings.Join(r.GetLabels(), ","), shortTime(r.GetLastSeen(), "never"),
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "NAME", "STATE", "ISOLATION", "VERSION", "LABELS", "LAST SEEN"}, rows)
	}
	return cmd
}

func newRunnerRemoveCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <runner id>",
		Short: "Remove a runner",
		Long: "Remove a runner: its token stops working at once and it gets no more jobs. A job it is running is not\n" +
			"affected: it ends, or fails as a lost runner when its heartbeats stop.",
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			out := complete(env, cmd, prefix, func(ctx context.Context, c *client.Client) ([]*ermite.Runner, error) {
				res, err := c.Runs().ListRunners(ctx, connect.NewRequest(&ermite.ListRunnersRequest{}))
				if err != nil {
					return nil, err
				}
				return res.Msg.GetRunners(), nil
			}, func(r *ermite.Runner) (string, string) { return r.GetId(), r.GetName() })
			return out, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		if _, err := c.Runs().RemoveRunner(env.ctx(cmd), connect.NewRequest(&ermite.RemoveRunnerRequest{RunnerId: args[0]})); err != nil {
			var ce *connect.Error
			if errors.As(err, &ce) && ce.Code() == connect.CodeNotFound {
				return fmt.Errorf("no runner has the id %q", args[0])
			}
			return err
		}
		noticef(cmd, "runner %s removed", args[0])
		return nil
	}
	return cmd
}
