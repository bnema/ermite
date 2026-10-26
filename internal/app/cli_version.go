// SPDX-License-Identifier: AGPL-3.0-or-later

package app

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
		Short: "Print client and configured server versions",
		Long: "Print the client version. With a server URL (ERMITE_URL or config url; see `ermite config`),\n" +
			"also print the server version and optional API features. A warning on stderr names the features\n" +
			"this client uses that the server lacks. Without a URL, only the client is shown.\n" +
			"The command never runs token.cmd.\n" +
			"\n" +
			"JSON (--json): client.version; server (null without a URL), containing url, version,\n" +
			"features (identifier array), missing_features (features this client uses that the server lacks).",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		clientLine := "ermite " + env.version
		url, err := env.resolveURL(cmd, true)
		if err != nil {
			// The client version does not depend on the file: say what is wrong
			// and go on as if no URL were configured.
			warnf(cmd, "%s", err)
			url = ""
		}
		if url == "" {
			if *asJSON {
				return writeJSON(out, jsonVersion{Client: jsonClientVersion{Version: env.version}})
			}
			_, err := fmt.Fprintln(out, clientLine)
			return err
		}

		// GetVersion is anonymous: token.cmd is not run for it.
		c, err := env.clientNoTokenCmd(cmd, true)
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
	client.FeatureIssueEdit:           "`ermite issue edit` needs: the command will fail on this server",
	client.FeatureMREdit:              "`ermite mr edit` needs: the command will fail on this server",
	client.FeatureReleases:            "`ermite release` needs: the commands will fail on this server, and explicit release:write scopes are refused",
	client.FeatureRepoUpdate:          "`ermite repo edit` needs: the command will fail on this server",
	client.FeatureEventsResume:        "`ermite watch` uses to resume after a reconnection: events during an outage may be missed",
	client.FeatureMergeRequests:       "`ermite mr` needs: the commands will fail on this server",
	client.FeatureMergeDeleteBranch:   "`ermite mr merge --delete-branch` needs: the merge works but the branch is kept",
	client.FeatureIssues:              "`ermite issue` needs: the commands will fail on this server, and `repo edit --issues` and `token create --scope issue:write` are refused",
	client.FeatureMergeability:        "`ermite mr show` uses to show whether the request merges: the line is missing",
	client.FeatureSigningKey:          "`ermite signing-key show` needs: the command will fail on this server",
	client.FeatureSession:             "`ermite web login` needs: the command will fail on this server",
	client.FeatureTokenBranchPrefixes: "`ermite token create --branch-prefix` needs: the command is refused, and `token list` does not show the limits",
	client.FeatureCIRuns:              "`ermite run` needs: the commands will fail on this server",
	client.FeatureCIRunners:           "`ermite runner` needs: the commands will fail on this server",
	client.FeatureCISecrets:           "`ermite secret`, `variable` and `environment` need: the commands will fail on this server",
	client.FeatureCIDispatch:          "`ermite workflow run` needs: the command will fail on this server",
	client.FeatureCIWorkflows:         "`ermite workflow list`, `disable` and `enable` need: the commands will fail on this server",
	client.FeatureCIArtifacts:         "`ermite run artifacts` and `run download` need: the commands will fail on this server",
	client.FeaturePasskeys:            "`ermite web passkey` needs: the commands will fail on this server (passkeys need an https public URL with a DNS host name)",
	client.FeatureCommitSignatures:    "`ermite signing-key trust` needs: the commands will fail on this server",
	client.FeatureAutoMerge:           "`ermite mr merge --auto` and `repo edit --allow-auto-merge` need: the commands are refused",
	client.FeatureJournal:             "`ermite journal` and `release pending` need: the commands will fail on this server, and explicit journal:read scopes are refused",
}
