// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
)

// jsonLoginLink is the stable JSON shape of `ermite web login --json`.
type jsonLoginLink struct {
	URL       string  `json:"url"`
	ExpiresAt *string `json:"expires_at"`
}

func newWebCmd(env *cliEnv) *cobra.Command {
	web := &cobra.Command{
		Use:   "web",
		Short: "Sign the owner in to the web interface of a remote server",
		Long: "Manage the owner's browser sessions on a remote server. They need the server's admin token, or a\n" +
			"token that holds every scope and is not limited to repositories.",
	}
	web.AddCommand(newWebLoginCmd(env), newWebLogoutCmd(env))
	return web
}

func newWebLoginCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Print a one-time link that signs your browser in",
		Long: "Ask the server for a login link and print it on standard output. Open it in your browser and confirm:\n" +
			"the browser is then signed in as the owner for 12 hours (private repositories, merge requests).\n\n" +
			"The link works once, for about a minute, and only until the server restarts. It is a secret: it is\n" +
			"printed and nowhere else (not stored, not logged); do not paste it into a chat or a ticket. Opening\n" +
			"it only shows a confirmation page; confirming it signs the browser in.\n\n" +
			"JSON fields (--json): url, expires_at (RFC 3339 UTC).",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.Sessions().CreateLoginLink(env.ctx(cmd), connect.NewRequest(&ermite.CreateLoginLinkRequest{}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), jsonLoginLink{URL: res.Msg.GetUrl(), ExpiresAt: jsonTime(res.Msg.GetExpiresAt())})
		}
		noticef(cmd, "open this link in your browser and confirm; it works once, until %s UTC", shortTime(res.Msg.GetExpiresAt(), "shortly"))
		_, err = fmt.Fprintln(cmd.OutOrStdout(), res.Msg.GetUrl())
		return err
	}
	return cmd
}

func newWebLogoutCmd(env *cliEnv) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout --all",
		Short: "Sign every browser out",
		Long: "End every web session at once, on every device. Pending login links and API tokens are not affected\n" +
			"(revoke tokens with `ermite token revoke`). To sign out one browser, use the sign out button on the page.",
		Args: cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&all, "all", false, "sign out every browser (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !all {
			return errors.New("--all is required: this signs every browser out (one browser signs out from its page)")
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if _, err := c.Sessions().SignOutEverywhere(env.ctx(cmd), connect.NewRequest(&ermite.SignOutEverywhereRequest{})); err != nil {
			return c.Describe(err)
		}
		noticef(cmd, "every web session is signed out")
		return nil
	}
	return cmd
}
