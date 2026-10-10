// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-api/gen/ermite"
)

var (
	repoAlpha = &ermite.Repo{
		Name: "alpha", Description: "The first one", Visibility: ermite.Visibility_VISIBILITY_PUBLIC,
		DefaultBranch: "main", CreatedAt: ts("2026-01-02T03:04:05Z"), PushedAt: ts("2026-03-04T05:06:07Z"),
		Settings: &ermite.RepoSettings{
			AllowedMergeModes:  []ermite.MergeMode{ermite.MergeMode_MERGE_MODE_FAST_FORWARD, ermite.MergeMode_MERGE_MODE_SQUASH},
			FailingChecksBlock: true,
		},
	}
	repoBeta = &ermite.Repo{
		Name: "beta", Visibility: ermite.Visibility_VISIBILITY_PRIVATE,
		DefaultBranch: "trunk", CreatedAt: ts("2026-02-02T00:00:00Z"),
		Settings: &ermite.RepoSettings{AllowedMergeModes: []ermite.MergeMode{ermite.MergeMode_MERGE_MODE_SQUASH}},
	}
)

func envFor(url string) map[string]string {
	return map[string]string{"ERMITE_URL": url, "ERMITE_TOKEN": "ermite_tok"}
}

func TestRepoListTable(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha, repoBeta}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "list").ok(t)
	assert.Equal(t, ""+
		"NAME   VISIBILITY  BRANCH  LAST PUSH         DESCRIPTION\n"+
		"alpha  public      main    2026-03-04 05:06  The first one\n"+
		"beta   private     trunk   never             \n", res.stdout)
	assert.Empty(t, res.stderr)
	assert.Equal(t, "Bearer ermite_tok", f.header[0].Get("Authorization"))
	assert.Equal(t, "ermite/v0.0.1 (cli)", f.header[0].Get("User-Agent"))
}

func TestRepoListJSON(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha, repoBeta}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "list", "--json").ok(t)
	assert.JSONEq(t, `[
		{"name":"alpha","description":"The first one","visibility":"public","default_branch":"main","created_at":"2026-01-02T03:04:05Z","pushed_at":"2026-03-04T05:06:07Z","merge_modes":["fast-forward","squash"],"failing_checks_block":true},
		{"name":"beta","description":"","visibility":"private","default_branch":"trunk","created_at":"2026-02-02T00:00:00Z","pushed_at":null,"merge_modes":["squash"],"failing_checks_block":false}
	]`, res.stdout)
}

func TestRepoListEmpty(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "repo", "list").ok(t)
	assert.Empty(t, res.stdout)
	assert.Equal(t, "ermite: no repositories\n", res.stderr)
	res = runCLI(t, envFor(url), "", "repo", "list", "--json").ok(t)
	assert.Equal(t, "[]\n", res.stdout, "an empty list is [] not null")
}

func TestRepoListFollowsPages(t *testing.T) {
	f := &fakeAPI{listRepos: func(r *ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		if r.GetPageToken() == "" {
			return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}, NextPageToken: "p2"}, nil
		}
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoBeta}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "list", "--json").ok(t)
	assert.Contains(t, res.stdout, `"alpha"`)
	assert.Contains(t, res.stdout, `"beta"`)
	_, reqs := f.snapshot()
	require.Len(t, reqs, 2)
	assert.Equal(t, "p2", reqs[1].(*ermite.ListReposRequest).GetPageToken())
}

func TestRepoShow(t *testing.T) {
	f := &fakeAPI{getRepo: func(r *ermite.GetRepoRequest) (*ermite.GetRepoResponse, error) {
		return &ermite.GetRepoResponse{Repo: repoAlpha}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "repo", "show", "alpha").ok(t)
	assert.Equal(t, ""+
		"name                  alpha\n"+
		"visibility            public\n"+
		"default branch        main\n"+
		"description           The first one\n"+
		"created               2026-01-02 03:04\n"+
		"last push             2026-03-04 05:06\n"+
		"merge modes           fast-forward, squash\n"+
		"failing checks block  true\n", res.stdout)
	_, reqs := f.snapshot()
	assert.Equal(t, "alpha", reqs[0].(*ermite.GetRepoRequest).GetRepo())

	res = runCLI(t, envFor(url), "", "repo", "show", "alpha", "--json").ok(t)
	assert.JSONEq(t, `{"name":"alpha","description":"The first one","visibility":"public","default_branch":"main","created_at":"2026-01-02T03:04:05Z","pushed_at":"2026-03-04T05:06:07Z","merge_modes":["fast-forward","squash"],"failing_checks_block":true}`, res.stdout)
}

func TestRepoShowNeedsOneArg(t *testing.T) {
	res := runCLI(t, envFor("http://127.0.0.1:1"), "", "repo", "show")
	require.Error(t, res.err)
}

func TestRepoCreateMapsFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want ermite.Visibility
		desc string
	}{
		{"default is private", []string{"x"}, ermite.Visibility_VISIBILITY_PRIVATE, ""},
		{"--visibility public", []string{"x", "--visibility", "public", "--description", "d d"}, ermite.Visibility_VISIBILITY_PUBLIC, "d d"},
		{"case insensitive", []string{"x", "--visibility", "Public"}, ermite.Visibility_VISIBILITY_PUBLIC, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeAPI{createRepo: func(r *ermite.CreateRepoRequest) (*ermite.CreateRepoResponse, error) {
				return &ermite.CreateRepoResponse{Repo: repoAlpha}, nil
			}}
			runCLI(t, envFor(f.serve(t)), "", append([]string{"repo", "create"}, tt.args...)...).ok(t)
			_, reqs := f.snapshot()
			require.Len(t, reqs, 1)
			got := reqs[0].(*ermite.CreateRepoRequest)
			assert.Equal(t, "x", got.GetName())
			assert.Equal(t, tt.want, got.GetVisibility())
			assert.Equal(t, tt.desc, got.GetDescription())
		})
	}
}

func TestRepoCreateRejectsBadFlagsWithoutCalling(t *testing.T) {
	f := &fakeAPI{}
	url := f.serve(t)
	for _, args := range [][]string{
		{"repo", "create", "x", "--visibility", "secret"},
	} {
		res := runCLI(t, envFor(url), "", args...)
		require.Error(t, res.err, "%v", args)
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls)
}

func TestRepoCreateJSON(t *testing.T) {
	f := &fakeAPI{createRepo: func(r *ermite.CreateRepoRequest) (*ermite.CreateRepoResponse, error) {
		return &ermite.CreateRepoResponse{Repo: repoBeta}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "create", "beta", "--json").ok(t)
	assert.JSONEq(t, `{"name":"beta","description":"","visibility":"private","default_branch":"trunk","created_at":"2026-02-02T00:00:00Z","pushed_at":null,"merge_modes":["squash"],"failing_checks_block":false}`, res.stdout)
}

func TestRepoEditMask(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		paths []string
		check func(*testing.T, *ermite.UpdateRepoRequest)
	}{
		{"description only", []string{"--description", "new"}, []string{"description"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.Equal(t, "new", r.GetDescription())
		}},
		{"clear description", []string{"--description", ""}, []string{"description"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.Empty(t, r.GetDescription())
		}},
		{"visibility", []string{"--visibility", "public"}, []string{"visibility"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.Equal(t, ermite.Visibility_VISIBILITY_PUBLIC, r.GetVisibility())
		}},
		{"default branch", []string{"--default-branch", "refs/heads/dev"}, []string{"default_branch"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.Equal(t, "dev", r.GetDefaultBranch())
		}},
		{"merge modes", []string{"--merge-modes", "squash,ff"}, []string{"settings.allowed_merge_modes"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.Equal(t, []ermite.MergeMode{ermite.MergeMode_MERGE_MODE_SQUASH, ermite.MergeMode_MERGE_MODE_FAST_FORWARD}, r.GetSettings().GetAllowedMergeModes())
		}},
		{"failing checks do not block", []string{"--failing-checks-block=false"}, []string{"settings.failing_checks_block"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.False(t, r.GetSettings().GetFailingChecksBlock())
			assert.Empty(t, r.GetSettings().GetAllowedMergeModes())
		}},
		{"failing checks block", []string{"--failing-checks-block"}, []string{"settings.failing_checks_block"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {
			assert.True(t, r.GetSettings().GetFailingChecksBlock())
		}},
		{"all", []string{"--default-branch", "dev", "--visibility", "private", "--description", "d"},
			[]string{"description", "visibility", "default_branch"}, func(t *testing.T, r *ermite.UpdateRepoRequest) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeAPI{updateRepo: func(r *ermite.UpdateRepoRequest) (*ermite.UpdateRepoResponse, error) {
				return &ermite.UpdateRepoResponse{Repo: repoAlpha}, nil
			}}
			runCLI(t, envFor(f.serve(t)), "", append([]string{"repo", "edit", "alpha"}, tt.args...)...).ok(t)
			_, reqs := f.snapshot()
			require.Len(t, reqs, 1)
			got := reqs[0].(*ermite.UpdateRepoRequest)
			assert.Equal(t, "alpha", got.GetRepo())
			assert.ElementsMatch(t, tt.paths, got.GetUpdateMask().GetPaths())
			tt.check(t, got)
		})
	}
}

func TestRepoEditRejects(t *testing.T) {
	f := &fakeAPI{}
	url := f.serve(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"repo", "edit", "alpha"}, "nothing to change"},
		{[]string{"repo", "edit", "alpha", "--visibility", "x"}, "invalid visibility"},
		{[]string{"repo", "edit", "alpha", "--default-branch", " "}, "must not be empty"},
		{[]string{"repo", "edit", "alpha", "--merge-modes", "rebase"}, "invalid merge mode"},
		{[]string{"repo", "edit", "alpha", "--merge-modes", ""}, "at least one mode"},
	} {
		res := runCLI(t, envFor(url), "", tt.args...)
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), tt.want)
	}
	calls, _ := f.snapshot()
	assert.Empty(t, calls)
}

func TestRepoDeleteConfirmation(t *testing.T) {
	newFake := func() *fakeAPI {
		return &fakeAPI{deleteRepo: func(*ermite.DeleteRepoRequest) (*ermite.DeleteRepoResponse, error) {
			return &ermite.DeleteRepoResponse{}, nil
		}}
	}
	t.Run("typed name deletes", func(t *testing.T) {
		f := newFake()
		res := runCLI(t, envFor(f.serve(t)), "alpha\n", "repo", "delete", "alpha").ok(t)
		assert.Equal(t, "deleted alpha\n", res.stdout)
		assert.Contains(t, res.stderr, "Type its name to confirm")
		calls, reqs := f.snapshot()
		assert.Equal(t, []string{"DeleteRepo"}, calls)
		assert.Equal(t, "alpha", reqs[0].(*ermite.DeleteRepoRequest).GetRepo())
	})
	t.Run("last line without newline is accepted", func(t *testing.T) {
		f := newFake()
		runCLI(t, envFor(f.serve(t)), "alpha", "repo", "delete", "alpha").ok(t)
		calls, _ := f.snapshot()
		assert.Equal(t, []string{"DeleteRepo"}, calls)
	})
	t.Run("wrong name aborts", func(t *testing.T) {
		f := newFake()
		res := runCLI(t, envFor(f.serve(t)), "beta\n", "repo", "delete", "alpha")
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "did not match")
		calls, _ := f.snapshot()
		assert.Empty(t, calls)
	})
	t.Run("empty stdin aborts", func(t *testing.T) {
		f := newFake()
		res := runCLI(t, envFor(f.serve(t)), "", "repo", "delete", "alpha")
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "--yes")
		calls, _ := f.snapshot()
		assert.Empty(t, calls)
	})
	t.Run("--yes skips the question", func(t *testing.T) {
		f := newFake()
		res := runCLI(t, envFor(f.serve(t)), "", "repo", "delete", "alpha", "--yes").ok(t)
		assert.Equal(t, "deleted alpha\n", res.stdout)
		assert.NotContains(t, res.stderr, "confirm")
	})
	t.Run("-y", func(t *testing.T) {
		f := newFake()
		runCLI(t, envFor(f.serve(t)), "", "repo", "delete", "alpha", "-y").ok(t)
	})
}

func TestRepoErrorsAreFriendly(t *testing.T) {
	f := &fakeAPI{getRepo: func(*ermite.GetRepoRequest) (*ermite.GetRepoResponse, error) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("repository \"nope\" not found"))
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "show", "nope")
	require.Error(t, res.err)
	assert.Equal(t, "not found: repository \"nope\" not found", res.err.Error())
}

func TestUnauthenticatedHintsAtToken(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}}
	res := runCLI(t, map[string]string{"ERMITE_URL": f.serve(t)}, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ERMITE_TOKEN")
}

func TestMissingURL(t *testing.T) {
	res := runCLI(t, nil, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ERMITE_URL is not set")
}

func TestInvalidURL(t *testing.T) {
	res := runCLI(t, map[string]string{"ERMITE_URL": "git.example.com"}, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ERMITE_URL")
	assert.Contains(t, res.err.Error(), "http")
}

func TestUnreachableServer(t *testing.T) {
	res := runCLI(t, envFor("http://127.0.0.1:1"), "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "cannot reach the server at http://127.0.0.1:1")
	assert.NotContains(t, res.err.Error(), "\n")
}

func TestInsecureTokenWarningOnStderr(t *testing.T) {
	// A non-loopback http URL: nothing listens there, the warning comes first.
	res := runCLI(t, map[string]string{"ERMITE_URL": "http://ermite.invalid:1", "ERMITE_TOKEN": "ermite_tok"}, "", "repo", "list")
	require.Error(t, res.err)
	assert.Contains(t, res.stderr, "ermite: warning: ERMITE_TOKEN is sent over plain http to ermite.invalid")
	assert.NotContains(t, res.stderr, "ermite_tok")
}

func TestLoopbackHTTPDoesNotWarn(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "list").ok(t)
	assert.Empty(t, res.stderr)
}

func TestTerminalControlCharactersAreNeutralised(t *testing.T) {
	evil := &ermite.Repo{Name: "x", Description: "a\x1b[31mred\nline", Visibility: ermite.Visibility_VISIBILITY_PUBLIC}
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{evil}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "repo", "list").ok(t)
	assert.NotContains(t, res.stdout, "\x1b")
	assert.Equal(t, 2, len(splitLines(res.stdout)), "header and one row")
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestRepoNameCompletion(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha, repoBeta}}, nil
	}}
	vars := envFor(f.serve(t))
	res := runCLI(t, vars, "", "__complete", "repo", "show", "a").ok(t)
	assert.Contains(t, res.stdout, "alpha\tpublic, The first one")
	assert.NotContains(t, res.stdout, "beta")

	res = runCLI(t, vars, "", "__complete", "repo", "delete", "").ok(t)
	assert.Contains(t, res.stdout, "alpha")
	assert.Contains(t, res.stdout, "beta")

	// A second argument gets no repository names.
	res = runCLI(t, vars, "", "__complete", "repo", "show", "alpha", "").ok(t)
	assert.NotContains(t, res.stdout, "beta")

	res = runCLI(t, vars, "", "__complete", "watch", "").ok(t)
	assert.Contains(t, res.stdout, "alpha")
}

func TestCompletionIsSilentOnErrors(t *testing.T) {
	denied := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}}
	for name, vars := range map[string]map[string]string{
		"no config":   nil,
		"bad url":     {"ERMITE_URL": "nope"},
		"unreachable": envFor("http://ermite.invalid:1"),
		"rejected":    envFor(denied.serve(t)),
	} {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, vars, "", "__complete", "repo", "show", "").ok(t)
			assert.NotContains(t, res.stdout, "alpha")
			assert.NotContains(t, res.stderr, "warning", "completion must not print warnings")
			assert.Contains(t, res.stdout, ":4", "cobra's no-file-completion directive")
		})
	}
}

func TestShellCompletionScriptIsAvailable(t *testing.T) {
	res := runCLI(t, nil, "", "completion", "bash").ok(t)
	assert.Contains(t, res.stdout, "bash completion")
}

func TestToJSONRepoFromProto(t *testing.T) {
	r := toJSONRepo(proto.Clone(repoBeta).(*ermite.Repo))
	assert.Nil(t, r.PushedAt)
	assert.Equal(t, "private", r.Visibility)
	assert.Equal(t, "unknown", toJSONRepo(&ermite.Repo{}).Visibility)
}
