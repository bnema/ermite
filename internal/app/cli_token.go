// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// jsonToken is the stable JSON shape of a stored token. The secret is only
// present in the output of `token create`.
type jsonToken struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Scopes     []string `json:"scopes"`
	Repos      []string `json:"repos"`
	ExpiresAt  *string  `json:"expires_at"`
	CreatedAt  *string  `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
}

type jsonCreatedToken struct {
	jsonToken
	Secret string `json:"secret"`
}

func toJSONToken(t *ermite.Token) jsonToken {
	scopes := make([]string, 0, len(t.GetScopes()))
	for _, s := range t.GetScopes() {
		scopes = append(scopes, client.ScopeName(s))
	}
	repos := t.GetRepos()
	if repos == nil {
		repos = []string{}
	}
	return jsonToken{
		ID: t.GetId(), Label: t.GetLabel(), Scopes: scopes, Repos: repos,
		ExpiresAt: jsonTime(t.GetExpiresAt()), CreatedAt: jsonTime(t.GetCreatedAt()), LastUsedAt: jsonTime(t.GetLastUsedAt()),
	}
}

const jsonTokenHelp = `
JSON fields of a token (--json): id, label, scopes (array of names like "repo:read"),
repos (array; empty means every repository), expires_at, created_at and last_used_at
(RFC 3339 UTC, null when unset or never used).`

func newTokenCmd(env *cliEnv) *cobra.Command {
	token := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens on a remote server",
		Long:  "Create, list and revoke API tokens. Needs the token:admin scope (or the server's admin token).",
	}
	token.AddCommand(newTokenCreateCmd(env), newTokenListCmd(env), newTokenRevokeCmd(env))
	return token
}

func newTokenCreateCmd(env *cliEnv) *cobra.Command {
	var (
		preset  string
		scopes  []string
		repos   []string
		expires string
	)
	cmd := &cobra.Command{
		Use:   "create <label>",
		Short: "Create a token and print its secret once",
		Long: "Create a token. The secret is printed once on standard output (and nowhere else: pipe it\n" +
			"or store it right away); details go to standard error. With --json, standard output is one\n" +
			"object holding the token fields and the secret.\n\n" +
			"What the token may do is chosen with --preset and/or --scope (they add up); there is no default:\n" +
			"  cli    every scope, for your own daily use\n" +
			"  agent  repo:write, mr:write\n" +
			"  ci     repo:read\n" +
			"A token cannot be more powerful than the one that creates it. --repo limits it to the named\n" +
			"repositories (default: all). --expires takes 30d, 2w, 12h or a date; default: never.\n\n" +
			"The label is free text of up to 100 bytes, except \"owner\" and \"root\" (in any case): they are\n" +
			"what the audit trail shows for the admin token and the web owner, so the server refuses them." + jsonTokenHelp,
		Args: cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&preset, "preset", "", "scope preset: "+strings.Join(client.PresetNames, ", "))
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "scope to grant, repeatable or comma-separated (for example repo:read)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "limit the token to this repository, repeatable (default: all)")
	cmd.Flags().StringVar(&expires, "expires", "", "lifetime (30d, 2w, 12h) or date (2027-01-31); default: never")
	_ = cmd.RegisterFlagCompletionFunc("preset", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return client.PresetNames, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("scope", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return client.ScopeNames(client.AllScopes()), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("repo", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return repoNames(env, c, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		req, err := buildCreateToken(args[0], preset, scopes, repos, expires, env.now())
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.Tokens().CreateToken(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return printCreatedToken(cmd, res.Msg, *asJSON)
	}
	return cmd
}

// buildCreateToken maps the flags of `token create` to the request.
func buildCreateToken(label, preset string, scopeFlags, repoFlags []string, expires string, now time.Time) (*ermite.CreateTokenRequest, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, errors.New("the label must not be empty")
	}
	scopes, err := client.MergeScopes(preset, scopeFlags)
	if err != nil {
		return nil, err
	}
	req := &ermite.CreateTokenRequest{Label: label, Scopes: scopes}
	for _, r := range repoFlags {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(req.Repos, r) {
			req.Repos = append(req.Repos, r)
		}
	}
	if expires != "" {
		at, err := client.ParseExpiry(expires, now)
		if err != nil {
			return nil, err
		}
		req.ExpiresAt = timestamppb.New(at)
	}
	return req, nil
}

func printCreatedToken(cmd *cobra.Command, res *ermite.CreateTokenResponse, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), jsonCreatedToken{jsonToken: toJSONToken(res.GetToken()), Secret: res.GetSecret()})
	}
	t := res.GetToken()
	repos := "all"
	if len(t.GetRepos()) > 0 {
		repos = strings.Join(t.GetRepos(), ", ")
	}
	noticef(cmd, "created token %s %q", t.GetId(), clean(t.GetLabel()))
	_ = fields(cmd.ErrOrStderr(), [][2]string{
		{"  scopes", strings.Join(client.ScopeNames(t.GetScopes()), ", ")},
		{"  repos", repos},
		{"  expires", shortTime(t.GetExpiresAt(), "never")},
	})
	noticef(cmd, "the secret below is shown once and cannot be retrieved later")
	_, err := fmt.Fprintln(cmd.OutOrStdout(), res.GetSecret())
	return err
}

func listTokens(ctx context.Context, c *client.Client) ([]*ermite.Token, error) {
	return collect(ctx, func(page string) ([]*ermite.Token, string, error) {
		res, err := c.Tokens().ListTokens(ctx, connect.NewRequest(&ermite.ListTokensRequest{PageToken: page}))
		if err != nil {
			return nil, "", c.Describe(err)
		}
		return res.Msg.GetTokens(), res.Msg.GetNextPageToken(), nil
	})
}

func newTokenListCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored tokens",
		Long:  "List stored tokens, newest first. Secrets are never shown. The server's admin token is not listed." + jsonTokenHelp + "\n--json prints an array of tokens.",
		Args:  cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		tokens, err := listTokens(env.ctx(cmd), c)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonToken, 0, len(tokens))
			for _, t := range tokens {
				out = append(out, toJSONToken(t))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(tokens) == 0 {
			noticef(cmd, "no tokens")
			return nil
		}
		rows := make([][]string, 0, len(tokens))
		for _, t := range tokens {
			repos := "all"
			if len(t.GetRepos()) > 0 {
				repos = strings.Join(t.GetRepos(), ",")
			}
			rows = append(rows, []string{
				t.GetId(), t.GetLabel(), strings.Join(client.ScopeNames(t.GetScopes()), ","), repos,
				shortTime(t.GetExpiresAt(), "never"), shortTime(t.GetLastUsedAt(), "never"),
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "LABEL", "SCOPES", "REPOS", "EXPIRES", "LAST USED"}, rows)
	}
	return cmd
}

func newTokenRevokeCmd(env *cliEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token",
		Long: "Revoke a token by id (see `ermite token list`). It stops working immediately.\n\n" +
			"A token with every scope and no --repo list has the rights of the owner (it can open web\n" +
			"sessions). Revoking one also signs every browser out, like `ermite web logout --all`:\n" +
			"web sessions are not tied to the token that opened them. Sign in again with `ermite web login`.",
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			ids := complete(env, cmd, toComplete, listTokens, func(t *ermite.Token) (string, string) {
				return t.GetId(), clean(t.GetLabel())
			})
			return ids, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(cmd, true)
			if err != nil {
				return err
			}
			if _, err := c.Tokens().RevokeToken(env.ctx(cmd), connect.NewRequest(&ermite.RevokeTokenRequest{Id: args[0]})); err != nil {
				return c.Describe(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
			return err
		},
	}
}
