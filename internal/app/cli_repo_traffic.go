// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"strconv"

	"bnema.dev/ermite-api/gen/ermite"
	"github.com/spf13/cobra"
)

type jsonTrafficCounts struct {
	Views        uint64 `json:"views"`
	ViewUniques  uint64 `json:"view_uniques"`
	Clones       uint64 `json:"clones"`
	CloneUniques uint64 `json:"clone_uniques"`
	Partial      bool   `json:"partial" help:"whether counts are incomplete"`
}
type jsonTrafficPoint struct {
	Start  jsonInstant       `json:"start"`
	Counts jsonTrafficCounts `json:"counts"`
}
type jsonTrafficReferrer struct {
	Domain string `json:"domain"`
	Count  uint64 `json:"count"`
}
type jsonRepoTraffic struct {
	Totals    jsonTrafficCounts     `json:"totals"`
	Points    []jsonTrafficPoint    `json:"points" help:"oldest first, zero-filled; hourly for 24h, daily for 30d"`
	Referrers []jsonTrafficReferrer `json:"referrers" help:"top 10 over 30 days"`
}

func toJSONTrafficCounts(c *ermite.TrafficCounts) jsonTrafficCounts {
	return jsonTrafficCounts{Views: c.GetViews(), ViewUniques: c.GetViewUniques(), Clones: c.GetClones(), CloneUniques: c.GetCloneUniques(), Partial: c.GetPartial()}
}
func toJSONRepoTraffic(r *ermite.GetRepoTrafficResponse) jsonRepoTraffic {
	out := jsonRepoTraffic{Totals: toJSONTrafficCounts(r.GetTotals()), Points: []jsonTrafficPoint{}, Referrers: []jsonTrafficReferrer{}}
	for _, p := range r.GetPoints() {
		out.Points = append(out.Points, jsonTrafficPoint{Start: jsonTime(p.GetStart()), Counts: toJSONTrafficCounts(p.GetCounts())})
	}
	for _, r := range r.GetReferrers() {
		out.Referrers = append(out.Referrers, jsonTrafficReferrer{Domain: r.GetDomain(), Count: r.GetCount()})
	}
	return out
}
func newRepoTrafficCmd(env *cliEnv) *cobra.Command {
	var window string
	cmd := &cobra.Command{
		Use: "traffic <repo>", Short: "Show repository views, clones and referrers",
		Long: "Show repository traffic in UTC, including the current hour or day. Public repositories need no token; private repositories need repo:read. Referrers always cover 30 days.\n" + jsonFieldsHelp("repository traffic", jsonRepoTraffic{}),
		Args: cobra.ExactArgs(1), ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&window, "range", "30d", "reporting range: 24h or 30d")
	_ = cmd.RegisterFlagCompletionFunc("range", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"24h", "30d"}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var r ermite.TrafficRange
		switch window {
		case "24h":
			r = ermite.TrafficRange_TRAFFIC_RANGE_24H
		case "30d":
			r = ermite.TrafficRange_TRAFFIC_RANGE_30D
		default:
			return fmt.Errorf("invalid range %q: use 24h or 30d", window)
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Repos().GetRepoTraffic(env.ctx(cmd), &ermite.GetRepoTrafficRequest{Repo: args[0], Range: r})
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONRepoTraffic(res))
		}
		w := cmd.OutOrStdout()
		totals := res.GetTotals()
		if _, err = fmt.Fprintf(w, "views %d · %d unique\nclones %d · %d unique\n", totals.GetViews(), totals.GetViewUniques(), totals.GetClones(), totals.GetCloneUniques()); err != nil {
			return err
		}
		if totals.GetPartial() {
			if _, err = fmt.Fprintln(w, "counts are partial"); err != nil {
				return err
			}
		}
		rows := make([][]string, 0, len(res.GetPoints()))
		layout, heading := "2006-01-02", "DATE (UTC)"
		if window == "24h" {
			layout, heading = "2006-01-02 15:00", "HOUR (UTC)"
		}
		for _, p := range res.GetPoints() {
			rows = append(rows, []string{p.GetStart().AsTime().UTC().Format(layout), strconv.FormatUint(p.GetCounts().GetViews(), 10), strconv.FormatUint(p.GetCounts().GetClones(), 10)})
		}
		if err = table(w, []string{heading, "VIEWS", "CLONES"}, rows); err != nil {
			return err
		}
		if _, err = fmt.Fprintln(w, "referrers (30d)"); err != nil {
			return err
		}
		if len(res.GetReferrers()) == 0 {
			_, err = fmt.Fprintln(w, "none")
			return err
		}
		rows = rows[:0]
		for _, r := range res.GetReferrers() {
			rows = append(rows, []string{r.GetDomain(), strconv.FormatUint(r.GetCount(), 10)})
		}
		return table(w, []string{"DOMAIN", "COUNT"}, rows)
	}
	return cmd
}
