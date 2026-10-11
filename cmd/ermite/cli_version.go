// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// jsonVersion is the stable JSON shape of `ermite version --json`.
type jsonVersion struct {
	Client jsonClientVersion  `json:"client"`
	Server *jsonServerVersion `json:"server"`
}

type jsonClientVersion struct {
	Version string `json:"version"`
}

type jsonServerVersion struct {
	URL             string   `json:"url"`
	Version         string   `json:"version"`
	Features        []string `json:"features"`
	MissingFeatures []string `json:"missing_features"`
}

func newVersionCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the client version and, when ERMITE_URL is set, the server's",
		Long: "Print the version of this client and, when ERMITE_URL is set, the version of the server and\n" +
			"the optional features of the API it implements. A warning on standard error says which of the\n" +
			"features this client uses the server lacks. Without ERMITE_URL only the client is shown.\n\n" +
			"JSON fields (--json): client.version; server, null without ERMITE_URL, with url, version,\n" +
			"features (array of identifiers) and missing_features (the ones this client uses that the\n" +
			"server lacks).",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		clientLine := "ermite " + env.version
		url := strings.TrimSpace(env.getenv(client.EnvURL))
		if url == "" {
			if *asJSON {
				return writeJSON(out, jsonVersion{Client: jsonClientVersion{Version: env.version}})
			}
			_, err := fmt.Fprintln(out, clientLine)
			return err
		}

		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.Server().GetVersion(env.ctx(cmd), connect.NewRequest(&ermite.GetVersionRequest{}))
		if err != nil {
			// Still show what is known locally before failing.
			if !*asJSON {
				_, _ = fmt.Fprintln(out, clientLine)
			}
			return c.Describe(err)
		}
		features := res.Msg.GetFeatures()
		if features == nil {
			features = []string{}
		}
		missing := client.MissingFeatures(features)
		if *asJSON {
			if missing == nil {
				missing = []string{}
			}
			err = writeJSON(out, jsonVersion{
				Client: jsonClientVersion{Version: env.version},
				Server: &jsonServerVersion{URL: c.URL(), Version: res.Msg.GetVersion(), Features: features, MissingFeatures: missing},
			})
		} else {
			err = fields(out, [][2]string{
				{"client", clientLine},
				{"server", res.Msg.GetVersion() + " at " + c.URL()},
				{"features", strings.Join(features, ", ")},
			})
		}
		if err != nil {
			return err
		}
		for _, f := range missing {
			warnf(cmd, "the server does not support %s, which %s", f, featureUse[f])
		}
		return nil
	}
	return cmd
}

// featureUse says what breaks without a feature, for the version warning.
var featureUse = map[string]string{
	client.FeatureRepoUpdate:        "`ermite repo edit` needs: the command will fail on this server",
	client.FeatureEventsResume:      "`ermite watch` uses to resume after a reconnection: events during an outage may be missed",
	client.FeatureMergeRequests:     "`ermite mr` needs: the commands will fail on this server",
	client.FeatureMergeDeleteBranch: "`ermite mr merge --delete-branch` needs: the merge works but the branch is kept",
	client.FeatureSigningKey:        "`ermite signing-key show` needs: the command will fail on this server",
	client.FeatureSession:           "`ermite web login` needs: the command will fail on this server",
}
