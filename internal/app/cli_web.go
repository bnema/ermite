// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
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
		Long: "These commands manage the owner's browser sessions and passkeys on a remote server.\n" +
			"They require the admin token, or a token with every scope and no repository or branch restrictions.",
	}
	web.AddCommand(newWebLoginCmd(env), newWebLogoutCmd(env), newWebPasskeyCmd(env))
	return web
}

func newWebLoginCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Print a one-time link that signs your browser in",
		Long: "The command prints a one-time login link on stdout. It signs browsers in as owner\n" +
			"for 12 hours (private repositories, merge requests). The link expires after about a minute\n" +
			"or on server restart. It is secret: printed, never stored or logged. Keep it out of chats\n" +
			"and tickets. The page signs in automatically; scriptless link previews cannot use it.\n" +
			"\n" +
			"JSON (--json): url, expires_at (RFC 3339 UTC).",
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
		noticef(cmd, "open this link in your browser; it works once, until %s UTC", shortTime(res.Msg.GetExpiresAt(), "shortly"))
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

// jsonPasskey is the stable JSON shape of a passkey. It never holds key
// material.
type jsonPasskey struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  *string `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	Synced     bool    `json:"synced"`
}

const jsonPasskeyHelp = "\n\nJSON fields (--json): id (credential id, base64url), name, created_at (RFC 3339 UTC), last_used_at\n" +
	"(null if never used), synced (the authenticator reports the passkey as backed up)."

func newWebPasskeyCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "passkey",
		Short: "List or remove the passkeys that sign the owner in",
		Long: "List or remove the passkeys that sign the owner in to the web interface. Adding one needs a browser:\n" +
			"sign in (`ermite web login`), then open /-/passkeys. Passkeys are only available when the server's\n" +
			"public URL is https (or localhost) with a host name, not an IP address.",
	}
	cmd.AddCommand(newWebPasskeyListCmd(env), newWebPasskeyRemoveCmd(env))
	return cmd
}

func listPasskeys(ctx context.Context, c *client.Client) ([]*ermite.Passkey, error) {
	res, err := c.Sessions().ListPasskeys(ctx, connect.NewRequest(&ermite.ListPasskeysRequest{}))
	if err != nil {
		return nil, c.Describe(err)
	}
	return res.Msg.GetPasskeys(), nil
}

func newWebPasskeyListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered passkeys",
		Long: "List the passkeys, oldest first, with the id to give to `ermite web passkey remove`." + jsonPasskeyHelp +
			"\n--json prints an array of passkeys.",
		Args: cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		passkeys, err := listPasskeys(env.ctx(cmd), c)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonPasskey, 0, len(passkeys))
			for _, p := range passkeys {
				out = append(out, jsonPasskey{
					ID: p.GetId(), Name: p.GetName(), CreatedAt: jsonTime(p.GetCreatedAt()),
					LastUsedAt: jsonTime(p.GetLastUsedAt()), Synced: p.GetSynced(),
				})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(passkeys) == 0 {
			noticef(cmd, "no passkeys")
			return nil
		}
		rows := make([][]string, 0, len(passkeys))
		for _, p := range passkeys {
			synced := "no"
			if p.GetSynced() {
				synced = "yes"
			}
			rows = append(rows, []string{
				p.GetId(), p.GetName(), shortTime(p.GetCreatedAt(), "-"), shortTime(p.GetLastUsedAt(), "never"), synced,
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "NAME", "CREATED", "LAST USED", "SYNCED"}, rows)
	}
	return cmd
}

func newWebPasskeyRemoveCmd(env *cliEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a passkey and sign every browser out",
		Long: "Remove a passkey by id (see `ermite web passkey list`). It can no longer sign in. Removing one also\n" +
			"signs every browser out, like `ermite web logout --all`: web sessions are not tied to the passkey that\n" +
			"opened them. Sign in again with another passkey or `ermite web login`.",
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			ids := complete(env, cmd, toComplete, listPasskeys, func(p *ermite.Passkey) (string, string) {
				return p.GetId(), clean(p.GetName())
			})
			return ids, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd)
			if err != nil {
				return err
			}
			if _, err := c.Sessions().RemovePasskey(env.ctx(cmd), connect.NewRequest(&ermite.RemovePasskeyRequest{Id: args[0]})); err != nil {
				return c.Describe(err)
			}
			noticef(cmd, "passkey removed; every browser is signed out")
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			return err
		},
	}
}
