// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

func TestTokenCreateMapsFlags(t *testing.T) {
	f := &fakeAPI{createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
		return &ermite.CreateTokenResponse{
			Token: &ermite.Token{
				Id: "tok1", Label: r.GetLabel(), Scopes: r.GetScopes(), Repos: r.GetRepos(),
				ExpiresAt: r.GetExpiresAt(), CreatedAt: ts("2026-10-10T12:00:00Z"),
			},
			Secret: "ermite_SECRETVALUE",
		}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "ci runner",
		"--preset", "ci", "--scope", "repo:write,mr:write", "--repo", "alpha", "--repo", "beta", "--repo", "alpha", "--expires", "30d").ok(t)

	_, reqs := f.snapshot()
	got := reqs[0].(*ermite.CreateTokenRequest)
	assert.Equal(t, "ci runner", got.GetLabel())
	assert.Equal(t, []ermite.Scope{ermite.Scope_SCOPE_REPO_READ, ermite.Scope_SCOPE_REPO_WRITE, ermite.Scope_SCOPE_MR_WRITE}, got.GetScopes())
	assert.Equal(t, []string{"alpha", "beta"}, got.GetRepos())
	assert.Equal(t, testNow.Add(30*24*time.Hour), got.GetExpiresAt().AsTime())

	// The secret alone is on stdout, details are on stderr.
	assert.Equal(t, "ermite_SECRETVALUE\n", res.stdout)
	assert.Contains(t, res.stderr, `created token tok1 "ci runner"`)
	assert.Contains(t, res.stderr, "repo:read, repo:write, mr:write")
	assert.Contains(t, res.stderr, "alpha, beta")
	assert.Contains(t, res.stderr, "shown once")
	assert.NotContains(t, res.stderr, "SECRETVALUE")
}

// lastCreateToken returns the last CreateToken request the fake received.
func lastCreateToken(t *testing.T, f *fakeAPI) *ermite.CreateTokenRequest {
	t.Helper()
	calls, reqs := f.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == "CreateToken" {
			return reqs[i].(*ermite.CreateTokenRequest)
		}
	}
	t.Fatal("no CreateToken call")
	return nil
}

// withBranchPrefixes is a GetVersion that lists token.branch_prefixes.
func withBranchPrefixes(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
	return &ermite.GetVersionResponse{Version: "v0.4.0", Features: []string{"token.branch_prefixes"}}, nil
}

func TestTokenCreateBranchPrefixes(t *testing.T) {
	echo := func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
		return &ermite.CreateTokenResponse{
			Token: &ermite.Token{
				Id: "tok1", Label: r.GetLabel(), Scopes: r.GetScopes(), BranchPrefixes: r.GetBranchPrefixes(),
				CreatedAt: ts("2026-10-10T12:00:00Z"),
			},
			Secret: "ermite_S",
		}, nil
	}

	t.Run("maps the repeatable flag, trims and drops duplicates", func(t *testing.T) {
		f := &fakeAPI{createToken: echo, getVersion: withBranchPrefixes}
		res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "claude", "--preset", "agent", "--repo", "alpha",
			"--branch-prefix", "agent/claude/", "--branch-prefix", " bot/ ", "--branch-prefix", "agent/claude/").ok(t)
		assert.Equal(t, []string{"agent/claude/", "bot/"}, lastCreateToken(t, f).GetBranchPrefixes())
		assert.Equal(t, "ermite_S\n", res.stdout)
		assert.Contains(t, res.stderr, "branches")
		assert.Contains(t, res.stderr, "agent/claude/, bot/")
	})

	t.Run("without the flag the token shows every branch", func(t *testing.T) {
		f := &fakeAPI{createToken: echo}
		res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "ci").ok(t)
		_, reqs := f.snapshot()
		assert.Empty(t, reqs[0].(*ermite.CreateTokenRequest).GetBranchPrefixes())
		assert.Regexp(t, `branches\s+all`, res.stderr)
	})

	t.Run("json", func(t *testing.T) {
		f := &fakeAPI{createToken: echo, getVersion: withBranchPrefixes}
		res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "l", "--scope", "repo:write", "--branch-prefix", "a/", "--json").ok(t)
		assert.JSONEq(t, `{"id":"tok1","label":"l","scopes":["repo:write"],"repos":[],"branch_prefixes":["a/"],"expires_at":null,"created_at":"2026-10-10T12:00:00Z","last_used_at":null,"secret":"ermite_S"}`, res.stdout)
	})

	t.Run("a server without the feature is asked nothing", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			version func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error)
			wantMsg string
		}{
			{"feature missing", func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
				return &ermite.GetVersionResponse{Version: "v0.3.0", Features: []string{"repo.update", "session"}}, nil
			}, "does not support token.branch_prefixes"},
			{"version unreadable", nil, "does not implement this call"}, // the fake answers unimplemented
			{"server unavailable", func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
			}, "cannot reach the server"},
		} {
			f := &fakeAPI{createToken: echo, getVersion: tt.version}
			res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "agent", "--branch-prefix", "agent/x/")
			require.Error(t, res.err, tt.name)
			assert.Contains(t, strings.ToLower(res.err.Error()), tt.wantMsg, tt.name)
			calls, _ := f.snapshot()
			assert.NotContains(t, calls, "CreateToken", "%s: no token is created", tt.name)
			assert.Empty(t, res.stdout, tt.name)
		}
	})

	t.Run("without --branch-prefix the features are not read", func(t *testing.T) {
		f := &fakeAPI{createToken: echo} // no getVersion: it would answer unimplemented
		runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "ci").ok(t)
		calls, _ := f.snapshot()
		assert.NotContains(t, calls, "GetVersion")
	})

	t.Run("a server that ignores the field does not hand out an unlimited token", func(t *testing.T) {
		f := &fakeAPI{createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
			return &ermite.CreateTokenResponse{
				Token:  &ermite.Token{Id: "tok9", Label: r.GetLabel(), Scopes: r.GetScopes(), CreatedAt: ts("2026-10-10T12:00:00Z")},
				Secret: "ermite_UNLIMITED",
			}, nil
		}, getVersion: withBranchPrefixes}
		res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", "agent", "--branch-prefix", "agent/x/")
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "without the requested branch prefixes")
		assert.Contains(t, res.err.Error(), "ermite token revoke tok9")
		assert.NotContains(t, res.stdout, "UNLIMITED")
		assert.NotContains(t, res.stderr, "UNLIMITED")
		assert.NotContains(t, res.err.Error(), "UNLIMITED")
	})
}

func TestTokenListBranchPrefixes(t *testing.T) {
	f := &fakeAPI{listTokens: func(*ermite.ListTokensRequest) (*ermite.ListTokensResponse, error) {
		return &ermite.ListTokensResponse{Tokens: []*ermite.Token{
			{Id: "t2", Label: "claude", Scopes: []ermite.Scope{ermite.Scope_SCOPE_REPO_WRITE}, BranchPrefixes: []string{"agent/claude/", "bot/"}, CreatedAt: ts("2026-10-01T00:00:00Z")},
			{Id: "t1", Label: "ci", Scopes: []ermite.Scope{ermite.Scope_SCOPE_REPO_READ}, CreatedAt: ts("2026-09-01T00:00:00Z")},
		}}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "token", "list").ok(t)
	assert.Equal(t, ""+
		"ID  LABEL   SCOPES      REPOS  BRANCHES            EXPIRES  LAST USED\n"+
		"t2  claude  repo:write  all    agent/claude/,bot/  never    never\n"+
		"t1  ci      repo:read   all    all                 never    never\n", res.stdout)

	res = runCLI(t, envFor(url), "", "token", "list", "--json").ok(t)
	assert.JSONEq(t, `[
		{"id":"t2","label":"claude","scopes":["repo:write"],"repos":[],"branch_prefixes":["agent/claude/","bot/"],"expires_at":null,"created_at":"2026-10-01T00:00:00Z","last_used_at":null},
		{"id":"t1","label":"ci","scopes":["repo:read"],"repos":[],"branch_prefixes":[],"expires_at":null,"created_at":"2026-09-01T00:00:00Z","last_used_at":null}
	]`, res.stdout)
}

func TestTokenCreatePresetExpansionOnTheWire(t *testing.T) {
	for preset, want := range map[string][]ermite.Scope{
		"agent": {ermite.Scope_SCOPE_REPO_WRITE, ermite.Scope_SCOPE_MR_WRITE},
		"ci":    {ermite.Scope_SCOPE_REPO_READ},
		"cli":   client.AllScopes(),
	} {
		t.Run(preset, func(t *testing.T) {
			f := &fakeAPI{createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
				return &ermite.CreateTokenResponse{Token: &ermite.Token{Id: "t", Scopes: r.GetScopes()}, Secret: "s"}, nil
			}}
			runCLI(t, envFor(f.serve(t)), "", "token", "create", "x", "--preset", preset).ok(t)
			_, reqs := f.snapshot()
			got := reqs[0].(*ermite.CreateTokenRequest)
			assert.ElementsMatch(t, want, got.GetScopes())
			assert.Empty(t, got.GetRepos())
			assert.Nil(t, got.GetExpiresAt())
		})
	}
}

func TestTokenCreateJSON(t *testing.T) {
	f := &fakeAPI{createToken: func(r *ermite.CreateTokenRequest) (*ermite.CreateTokenResponse, error) {
		return &ermite.CreateTokenResponse{
			Token:  &ermite.Token{Id: "tok1", Label: "l", Scopes: r.GetScopes(), CreatedAt: ts("2026-10-10T12:00:00Z")},
			Secret: "ermite_S",
		}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "create", "l", "--scope", "repo:read", "--json").ok(t)
	assert.JSONEq(t, `{"id":"tok1","label":"l","scopes":["repo:read"],"repos":[],"branch_prefixes":[],"expires_at":null,"created_at":"2026-10-10T12:00:00Z","last_used_at":null,"secret":"ermite_S"}`, res.stdout)
	assert.Empty(t, res.stderr)
}

func TestTokenCreateValidatesBeforeCalling(t *testing.T) {
	f := &fakeAPI{}
	url := f.serve(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"x"}, "--preset"},
		{[]string{"x", "--preset", "god"}, "unknown preset"},
		{[]string{"x", "--scope", "nope"}, "unknown scope"},
		{[]string{"x", "--preset", "ci", "--expires", "yesterday"}, "--expires"},
		{[]string{" ", "--preset", "ci"}, "label"},
	} {
		res := runCLI(t, envFor(url), "", append([]string{"token", "create"}, tt.args...)...)
		require.Error(t, res.err, "%v", tt.args)
		assert.Contains(t, res.err.Error(), tt.want)
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls)
}

func TestTokenList(t *testing.T) {
	f := &fakeAPI{listTokens: func(*ermite.ListTokensRequest) (*ermite.ListTokensResponse, error) {
		return &ermite.ListTokensResponse{Tokens: []*ermite.Token{
			{Id: "t2", Label: "laptop", Scopes: []ermite.Scope{ermite.Scope_SCOPE_REPO_READ, ermite.Scope_SCOPE_REPO_WRITE},
				Repos: []string{"alpha", "beta"}, ExpiresAt: ts("2027-01-01T00:00:00Z"), CreatedAt: ts("2026-10-01T00:00:00Z"), LastUsedAt: ts("2026-10-09T08:30:00Z")},
			{Id: "t1", Label: "ci", Scopes: []ermite.Scope{ermite.Scope_SCOPE_REPO_READ}, CreatedAt: ts("2026-09-01T00:00:00Z")},
		}}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "token", "list").ok(t)
	assert.Equal(t, ""+
		"ID  LABEL   SCOPES                REPOS       BRANCHES  EXPIRES           LAST USED\n"+
		"t2  laptop  repo:read,repo:write  alpha,beta  all       2027-01-01 00:00  2026-10-09 08:30\n"+
		"t1  ci      repo:read             all         all       never             never\n", res.stdout)

	res = runCLI(t, envFor(url), "", "token", "list", "--json").ok(t)
	assert.JSONEq(t, `[
		{"id":"t2","label":"laptop","scopes":["repo:read","repo:write"],"repos":["alpha","beta"],"branch_prefixes":[],"expires_at":"2027-01-01T00:00:00Z","created_at":"2026-10-01T00:00:00Z","last_used_at":"2026-10-09T08:30:00Z"},
		{"id":"t1","label":"ci","scopes":["repo:read"],"repos":[],"branch_prefixes":[],"expires_at":null,"created_at":"2026-09-01T00:00:00Z","last_used_at":null}
	]`, res.stdout)
	assert.NotContains(t, res.stdout, "secret")
}

func TestTokenRevoke(t *testing.T) {
	f := &fakeAPI{revokeToken: func(*ermite.RevokeTokenRequest) (*ermite.RevokeTokenResponse, error) {
		return &ermite.RevokeTokenResponse{}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "token", "revoke", "t2").ok(t)
	assert.Equal(t, "revoked t2\n", res.stdout)
	_, reqs := f.snapshot()
	assert.Equal(t, "t2", reqs[0].(*ermite.RevokeTokenRequest).GetId())
}

func TestTokenCompletion(t *testing.T) {
	f := &fakeAPI{
		listTokens: func(*ermite.ListTokensRequest) (*ermite.ListTokensResponse, error) {
			return &ermite.ListTokensResponse{Tokens: []*ermite.Token{{Id: "abc1", Label: "laptop"}, {Id: "xyz2", Label: "ci"}}}, nil
		},
		listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
			return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}}, nil
		},
	}
	vars := envFor(f.serve(t))
	res := runCLI(t, vars, "", "__complete", "token", "revoke", "a").ok(t)
	assert.Contains(t, res.stdout, "abc1\tlaptop")
	assert.NotContains(t, res.stdout, "xyz2")

	res = runCLI(t, vars, "", "__complete", "token", "create", "x", "--preset", "").ok(t)
	assert.Contains(t, res.stdout, "agent")
	res = runCLI(t, vars, "", "__complete", "token", "create", "x", "--scope", "repo:").ok(t)
	assert.Contains(t, res.stdout, "repo:read")
	res = runCLI(t, vars, "", "__complete", "token", "create", "x", "--repo", "").ok(t)
	assert.Contains(t, res.stdout, "alpha")
}
