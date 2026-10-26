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
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
	Repos  []string `json:"repos"`
	// BranchPrefixes limits the branches the token may write; empty means all.
	BranchPrefixes []string `json:"branch_prefixes"`
	ExpiresAt      *string  `json:"expires_at"`
	CreatedAt      *string  `json:"created_at"`
	LastUsedAt     *string  `json:"last_used_at"`
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
	prefixes := t.GetBranchPrefixes()
	if prefixes == nil {
		prefixes = []string{}
	}
	return jsonToken{
		ID: t.GetId(), Label: t.GetLabel(), Scopes: scopes, Repos: repos, BranchPrefixes: prefixes,
		ExpiresAt: jsonTime(t.GetExpiresAt()), CreatedAt: jsonTime(t.GetCreatedAt()), LastUsedAt: jsonTime(t.GetLastUsedAt()),
	}
}

const jsonTokenHelp = `
JSON fields of a token (--json): id, label, scopes (array of names like "repo:read"),
repos (array; empty means every repository), branch_prefixes (array; empty means every branch),
expires_at, created_at and last_used_at (RFC 3339 UTC, null when unset or never used).
Create also returns secret (shown only once).`

func newTokenCmd(env *cliEnv) *cobra.Command {
	token := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens on a remote server",
		Long:  "Create, list and revoke API tokens. Needs the token:admin scope (or the server's admin token)." + jsonTokenHelp,
	}
	token.AddCommand(newTokenCreateCmd(env), newTokenListCmd(env), newTokenRevokeCmd(env))
	return token
}

func newTokenCreateCmd(env *cliEnv) *cobra.Command {
	var (
		preset   string
		scopes   []string
		repos    []string
		prefixes []string
		expires  string
	)
	cmd := &cobra.Command{
		Use:   "create <label>",
		Short: "Create a token and print its secret once",
		Long: "Create a token. The secret is printed once on standard output (and nowhere else: pipe it\n" +
			"or store it right away); details go to standard error. With --json, standard output is one\n" +
			"object holding the token fields and the secret.\n\n" +
			"What the token may do is chosen with --preset and/or --scope (they add up); there is no default:\n" +
			"  cli    every scope, including mr:review for verdicts, for your own daily use\n" +
			"  agent  repo:write, mr:write\n" +
			"  runner runner:register (a CI runner host)\n" +
			"  ci     repo:read\n" +
			"Scopes are repo:read, repo:write, repo:admin, mr:write, mr:merge, issue:write and more; give\n" +
			"issue:write (a server with the issues feature) to let a token write issues.\n" +
			"A token cannot be more powerful than the one that creates it. --repo limits it to the named\n" +
			"repositories (default: all). --expires takes 30d, 2w, 12h or a date; default: never.\n\n" +
			"--branch-prefix limits the branches the token may write (default: all). Each prefix ends with /\n" +
			"(for example agent/claude/). The token can push, force-push and delete only branches under its\n" +
			"prefixes, and merge only into them; it cannot push tags, create repositories or delete one,\n" +
			"and it never has the rights of the owner. It can only create tokens within its own prefixes.\n" +
			"To give an AI agent its own space:\n" +
			"  ermite token create claude --preset agent --scope repo:read --repo <repo> --branch-prefix agent/<name>/\n\n" +
			"The label is free text of up to 100 bytes, except \"owner\" and \"root\" (in any case): they are\n" +
			"what the audit trail shows for the admin token and the web owner, so the server refuses them." + "\nJSON: a token; see `ermite token --help`.",
		Args: cobra.ExactArgs(1),
	}
	cmd.Flags().StringVar(&preset, "preset", "", "scope preset: "+strings.Join(client.PresetNames, ", "))
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "scope to grant, repeatable or comma-separated (for example repo:read)")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "limit the token to this repository, repeatable (default: all)")
	cmd.Flags().StringSliceVar(&prefixes, "branch-prefix", nil, "limit the branches the token may write to this prefix, ending with / (for example agent/claude/); repeatable (default: all)")
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
		req, err := buildCreateToken(args[0], preset, scopes, repos, prefixes, expires, env.now())
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		if len(req.GetBranchPrefixes()) > 0 {
			if err := requireBranchPrefixes(env.ctx(cmd), c); err != nil {
				return err
			}
		}
		// A preset such as cli grows with the contract and must keep working on an
		// older server: issue:write from a preset alone is dropped there, while an
		// explicit --scope issue:write is refused.
		if slices.Contains(req.Scopes, ermite.Scope_SCOPE_ISSUE_WRITE) {
			if err := requireIssues(env.ctx(cmd), c); err != nil {
				if !errors.Is(err, errNoIssues) || explicitScope(scopes, ermite.Scope_SCOPE_ISSUE_WRITE) {
					return fmt.Errorf("%w (needed for the issue:write scope; nothing was created)", err)
				}
				req.Scopes = slices.DeleteFunc(req.Scopes, func(s ermite.Scope) bool { return s == ermite.Scope_SCOPE_ISSUE_WRITE })
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: the server has no issues feature, so the token has no issue:write scope")
			}
		}
		if slices.Contains(req.Scopes, ermite.Scope_SCOPE_RELEASE_WRITE) {
			if err := requireReleases(env.ctx(cmd), c); err != nil {
				if !errors.Is(err, errNoReleases) || explicitScope(scopes, ermite.Scope_SCOPE_RELEASE_WRITE) {
					return fmt.Errorf("%w (needed for the release:write scope; nothing was created)", err)
				}
				req.Scopes = slices.DeleteFunc(req.Scopes, func(s ermite.Scope) bool { return s == ermite.Scope_SCOPE_RELEASE_WRITE })
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: the server has no releases feature, so the token has no release:write scope")
			}
		}
		if slices.Contains(req.Scopes, ermite.Scope_SCOPE_JOURNAL_READ) {
			if err := requireJournal(env.ctx(cmd), c); err != nil {
				if !errors.Is(err, errNoJournal) || explicitScope(scopes, ermite.Scope_SCOPE_JOURNAL_READ) {
					return fmt.Errorf("%w (needed for the journal:read scope; nothing was created)", err)
				}
				req.Scopes = slices.DeleteFunc(req.Scopes, func(s ermite.Scope) bool { return s == ermite.Scope_SCOPE_JOURNAL_READ })
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: the server has no journal feature, so the token has no journal:read scope")
			}
		}
		res, err := c.Tokens().CreateToken(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		if err := checkBranchPrefixes(req, res.Msg.GetToken()); err != nil {
			return err
		}
		return printCreatedToken(cmd, res.Msg, *asJSON)
	}
	return cmd
}

// explicitScope reports whether the --scope flags name want.
func explicitScope(flags []string, want ermite.Scope) bool {
	return slices.ContainsFunc(flags, func(f string) bool {
		s, err := client.ParseScope(f)
		return err == nil && s == want
	})
}

// requireBranchPrefixes refuses, before anything is created, to ask a server
// for a branch-limited token when it does not list token.branch_prefixes: such
// a server ignores the field. A server whose features cannot be read is refused
// too (fail closed).
func requireBranchPrefixes(ctx context.Context, c *client.Client) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), client.FeatureTokenBranchPrefixes) {
		return fmt.Errorf("the server does not support %s, so it would ignore --branch-prefix and create an unlimited token: nothing was created (upgrade the server, see `ermite version`)", client.FeatureTokenBranchPrefixes)
	}
	return nil
}

// checkBranchPrefixes is the backstop of requireBranchPrefixes: it refuses to hand out a token that is less limited than
// asked for. A server that predates branch prefixes ignores the field and
// creates an unlimited token: the secret is then not printed.
func checkBranchPrefixes(req *ermite.CreateTokenRequest, got *ermite.Token) error {
	if slices.Equal(req.GetBranchPrefixes(), got.GetBranchPrefixes()) {
		return nil
	}
	return fmt.Errorf("the server created token %s without the requested branch prefixes (it may be too old: "+
		"see `ermite version`); its secret is not shown, revoke it with `ermite token revoke %s`", got.GetId(), got.GetId())
}

// buildCreateToken maps the flags of `token create` to the request.
func buildCreateToken(label, preset string, scopeFlags, repoFlags, prefixFlags []string, expires string, now time.Time) (*ermite.CreateTokenRequest, error) {
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
	for _, bp := range prefixFlags {
		if bp = strings.TrimSpace(bp); bp != "" && !slices.Contains(req.BranchPrefixes, bp) {
			req.BranchPrefixes = append(req.BranchPrefixes, bp)
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
	branches := "all"
	if len(t.GetBranchPrefixes()) > 0 {
		branches = strings.Join(t.GetBranchPrefixes(), ", ")
	}
	noticef(cmd, "created token %s %q", t.GetId(), clean(t.GetLabel()))
	_ = fields(cmd.ErrOrStderr(), [][2]string{
		{"  scopes", strings.Join(client.ScopeNames(t.GetScopes()), ", ")},
		{"  repos", repos},
		{"  branches", branches},
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
		Long:  "List stored tokens, newest first. Secrets are never shown. The server's admin token is not listed." + "\nJSON: a token; see `ermite token --help`." + "\n--json prints an array of tokens.",
		Args:  cobra.NoArgs,
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := env.client(cmd)
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
			branches := "all"
			if len(t.GetBranchPrefixes()) > 0 {
				branches = strings.Join(t.GetBranchPrefixes(), ",")
			}
			rows = append(rows, []string{
				t.GetId(), t.GetLabel(), strings.Join(client.ScopeNames(t.GetScopes()), ","), repos, branches,
				shortTime(t.GetExpiresAt(), "never"), shortTime(t.GetLastUsedAt(), "never"),
			})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "LABEL", "SCOPES", "REPOS", "BRANCHES", "EXPIRES", "LAST USED"}, rows)
	}
	return cmd
}

func newTokenRevokeCmd(env *cliEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token",
		Long: "Revoke a token by id (see `ermite token list`). It stops working immediately.\n\n" +
			"A token with every scope, no --repo list and no --branch-prefix has the rights of the owner (it can open web\n" +
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
			c, err := env.client(cmd)
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
