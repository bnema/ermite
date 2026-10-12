// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite-api/gen/ermite"
)

// The RunService calls of the `run` and `runner` commands answer with empty
// results: these tests look at which address and token reach the server, not
// at the output.
func (f *fakeAPI) ListRuns(_ context.Context, r *connect.Request[ermite.ListRunsRequest]) (*connect.Response[ermite.ListRunsResponse], error) {
	return respond(f, "ListRuns", r, func(*ermite.ListRunsRequest) (*ermite.ListRunsResponse, error) {
		return &ermite.ListRunsResponse{}, nil
	})
}

func (f *fakeAPI) CancelRun(_ context.Context, r *connect.Request[ermite.CancelRunRequest]) (*connect.Response[ermite.CancelRunResponse], error) {
	return respond(f, "CancelRun", r, func(*ermite.CancelRunRequest) (*ermite.CancelRunResponse, error) {
		return &ermite.CancelRunResponse{}, nil
	})
}

func (f *fakeAPI) RerunRun(_ context.Context, r *connect.Request[ermite.RerunRunRequest]) (*connect.Response[ermite.RerunRunResponse], error) {
	return respond(f, "RerunRun", r, func(*ermite.RerunRunRequest) (*ermite.RerunRunResponse, error) {
		return &ermite.RerunRunResponse{}, nil
	})
}

func (f *fakeAPI) ListRunners(_ context.Context, r *connect.Request[ermite.ListRunnersRequest]) (*connect.Response[ermite.ListRunnersResponse], error) {
	return respond(f, "ListRunners", r, func(*ermite.ListRunnersRequest) (*ermite.ListRunnersResponse, error) {
		return &ermite.ListRunnersResponse{Runners: []*ermite.Runner{{Id: "r1", Name: "alpha"}}}, nil
	})
}

func (f *fakeAPI) RemoveRunner(_ context.Context, r *connect.Request[ermite.RemoveRunnerRequest]) (*connect.Response[ermite.RemoveRunnerResponse], error) {
	return respond(f, "RemoveRunner", r, func(*ermite.RemoveRunnerRequest) (*ermite.RemoveRunnerResponse, error) {
		return &ermite.RemoveRunnerResponse{}, nil
	})
}

// The `run` and `runner` commands read the server address and the token like
// every other remote command: the config file (url, token.cmd), with
// ERMITE_URL / ERMITE_TOKEN winning, and token.cmd run once and only for the
// file's own url.
func TestCIcommandsUseTheConfigFile(t *testing.T) {
	commands := map[string][]string{
		"run list":      {"run", "list", "proj"},
		"run cancel":    {"run", "cancel", "proj", "1"},
		"run rerun":     {"run", "rerun", "proj", "1"},
		"runner list":   {"runner", "list"},
		"runner remove": {"runner", "remove", "r1"},
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			f := &fakeAPI{}
			url := f.serve(t)

			// url and token.cmd from the file
			env := testEnv(t, nil)
			writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
			ran := tokenCmdStub(env, "ermite_cmd\n", nil)
			runCLIWith(t, env, "", args...).ok(t)
			assert.Equal(t, "Bearer ermite_cmd", lastAuth(f))
			assert.Len(t, *ran, 1)

			// ERMITE_TOKEN wins and token.cmd stays idle
			env = testEnv(t, map[string]string{"ERMITE_TOKEN": "ermite_env"})
			writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
			ran = tokenCmdStub(env, "ermite_cmd", nil)
			runCLIWith(t, env, "", args...).ok(t)
			assert.Equal(t, "Bearer ermite_env", lastAuth(f))
			assert.Empty(t, *ran)

			// another ERMITE_URL never receives the file's token
			other := &fakeAPI{}
			env = testEnv(t, map[string]string{"ERMITE_URL": other.serve(t)})
			writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
			ran = tokenCmdStub(env, "ermite_cmd", nil)
			runCLIWith(t, env, "", args...).ok(t)
			assert.Empty(t, lastAuth(other))
			assert.Empty(t, *ran)
		})
	}
}

// Completing a repository name for `run` or a runner id for `runner remove`
// must not run token.cmd: it may prompt, and a completion script cannot answer.
func TestCICompletionNeverRunsTokenCmd(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}}, nil
	}}
	url := f.serve(t)
	for name, args := range map[string][]string{
		"run list":      {"__complete", "run", "list", ""},
		"run cancel":    {"__complete", "run", "cancel", ""},
		"runner remove": {"__complete", "runner", "remove", ""},
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t, nil)
			writeConfigFile(t, env, "url = "+url+"\ntoken.cmd = get-token\n", 0o600)
			ran := tokenCmdStub(env, "ermite_cmd", nil)
			runCLIWith(t, env, "", args...).ok(t)
			assert.Empty(t, *ran)
			assert.Empty(t, lastAuth(f))
		})
	}
}
