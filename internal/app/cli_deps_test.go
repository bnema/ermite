// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

var depsAlpha = &ermite.RepoDependencyUpdates{
	Repo: "alpha", CheckedAt: ts("2026-03-04T05:06:07Z"), State: ermite.RepoDependencyUpdates_STATE_IDLE,
	Updates: []*ermite.DependencyUpdate{
		{Ecosystem: "gomod", Name: "golang.org/x/text", Directory: "tools", Current: "v0.20.0", Latest: "v0.21.0", Kind: ermite.DependencyUpdate_KIND_MINOR, New: true},
		{Ecosystem: "github-actions", Name: "actions/checkout", Current: "v4", Latest: "v5", Kind: ermite.DependencyUpdate_KIND_MAJOR},
	},
}

func TestDepsListTableAndJSON(t *testing.T) {
	f := &fakeAPI{listDeps: func(r *ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error) {
		return &ermite.ListDependencyUpdatesResponse{Repos: []*ermite.RepoDependencyUpdates{depsAlpha}}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "list", "--repo", "alpha").ok(t)
	assert.Contains(t, res.stdout, "alpha  golang.org/x/text  v0.20.0  v0.21.0  minor  new  idle   -")
	_, reqs := f.snapshot()
	assert.Equal(t, "alpha", reqs[0].(*ermite.ListDependencyUpdatesRequest).GetRepo())

	res = runCLI(t, envFor(f.serve(t)), "", "deps", "list", "--json").ok(t)
	assert.JSONEq(t, `[{"repo":"alpha","updates":[
		{"ecosystem":"gomod","name":"golang.org/x/text","directory":"tools","current":"v0.20.0","latest":"v0.21.0","kind":"minor","new":true},
		{"ecosystem":"github-actions","name":"actions/checkout","directory":"","current":"v4","latest":"v5","kind":"major","new":false}],
		"checked_at":"2026-03-04T05:06:07Z","snoozed_until":null,"merge_request_number":0,"state":"idle","error":"","run_id":0}]`, res.stdout)
	_, reqs = f.snapshot()
	assert.Nil(t, reqs[1].(*ermite.ListDependencyUpdatesRequest).Repo, "no filter without --repo")
}

func TestDepsListShowsTheRun(t *testing.T) {
	failed := &ermite.RepoDependencyUpdates{
		Repo: "beta", State: ermite.RepoDependencyUpdates_STATE_FAILED, Error: "the run ended: failure", RunId: 12,
		Updates: []*ermite.DependencyUpdate{{Name: "x/y", Current: "v1", Latest: "v2", Kind: ermite.DependencyUpdate_KIND_MAJOR}},
	}
	f := &fakeAPI{
		listDeps: func(*ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error) {
			return &ermite.ListDependencyUpdatesResponse{Repos: []*ermite.RepoDependencyUpdates{failed}}, nil
		},
		checkDeps: func(*ermite.CheckDependenciesRequest) (*ermite.CheckDependenciesResponse, error) {
			return &ermite.CheckDependenciesResponse{Updates: failed}, nil
		},
	}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "list").ok(t)
	assert.Regexp(t, `beta\s+x/y\s+v1\s+v2\s+major\s+failed\s+12\n`, res.stdout)

	res = runCLI(t, envFor(f.serve(t)), "", "deps", "list", "--json").ok(t)
	assert.Contains(t, res.stdout, `"run_id": 12`)

	res = runCLI(t, envFor(f.serve(t)), "", "deps", "check", "beta").ok(t)
	assert.Regexp(t, `run\s+12\n`, res.stdout)
	assert.NotContains(t, res.stdout, "#12")
	assert.Contains(t, res.stdout, "ermite run view beta 12 --log")
}

func TestDepsCheck(t *testing.T) {
	f := &fakeAPI{checkDeps: func(r *ermite.CheckDependenciesRequest) (*ermite.CheckDependenciesResponse, error) {
		return &ermite.CheckDependenciesResponse{Updates: depsAlpha}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "check", "alpha", "--json").ok(t)
	assert.Contains(t, res.stdout, `"latest": "v0.21.0"`)
	res = runCLI(t, envFor(f.serve(t)), "", "deps", "check", "alpha").ok(t)
	assert.Contains(t, res.stdout, "state")
	assert.Contains(t, res.stdout, "golang.org/x/text")
	require.Error(t, runCLI(t, envFor(f.serve(t)), "", "deps", "check").err)
}

func TestDepsOpenAndIgnoreSendTheListedVersions(t *testing.T) {
	want := []*ermite.DependencyVersion{
		{Ecosystem: "gomod", Directory: "tools", Name: "golang.org/x/text", Version: "v0.21.0"},
		{Ecosystem: "github-actions", Name: "actions/checkout", Version: "v5"},
	}
	f := &fakeAPI{
		listDeps: func(*ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error) {
			return &ermite.ListDependencyUpdatesResponse{Repos: []*ermite.RepoDependencyUpdates{depsAlpha}}, nil
		},
		openDeps: func(*ermite.OpenDependencyUpdatesRequest) (*ermite.OpenDependencyUpdatesResponse, error) {
			return &ermite.OpenDependencyUpdatesResponse{Updates: depsAlpha}, nil
		},
		ignoreDeps: func(*ermite.IgnoreDependencyUpdatesRequest) (*ermite.IgnoreDependencyUpdatesResponse, error) {
			return &ermite.IgnoreDependencyUpdatesResponse{Updates: &ermite.RepoDependencyUpdates{Repo: "alpha"}}, nil
		},
	}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "deps", "open", "alpha").ok(t)
	assert.Equal(t, "ermite: open: alpha\n", res.stderr)
	res = runCLI(t, envFor(url), "", "deps", "ignore", "alpha", "--json").ok(t)
	assert.Contains(t, res.stdout, `"repo": "alpha"`)

	calls, reqs := f.snapshot()
	require.Equal(t, []string{"ListDependencyUpdates", "OpenDependencyUpdates", "ListDependencyUpdates", "IgnoreDependencyUpdates"}, calls)
	assert.Equal(t, "alpha", reqs[0].(*ermite.ListDependencyUpdatesRequest).GetRepo())
	open := reqs[1].(*ermite.OpenDependencyUpdatesRequest)
	require.Len(t, open.ExpectedVersions, 2)
	for i, w := range want {
		assert.Equal(t, w.Ecosystem, open.ExpectedVersions[i].Ecosystem)
		assert.Equal(t, w.Directory, open.ExpectedVersions[i].Directory)
		assert.Equal(t, w.Name, open.ExpectedVersions[i].Name)
		assert.Equal(t, w.Version, open.ExpectedVersions[i].Version)
	}
	assert.Len(t, reqs[3].(*ermite.IgnoreDependencyUpdatesRequest).ExpectedVersions, 2)
}

func TestDepsActionWithoutPendingUpdates(t *testing.T) {
	f := &fakeAPI{listDeps: func(*ermite.ListDependencyUpdatesRequest) (*ermite.ListDependencyUpdatesResponse, error) {
		return &ermite.ListDependencyUpdatesResponse{}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "open", "alpha")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "no pending dependency updates for alpha")
	calls, _ := f.snapshot()
	assert.Equal(t, []string{"ListDependencyUpdates"}, calls)
}

func TestDepsSnooze(t *testing.T) {
	f := &fakeAPI{snoozeDeps: func(*ermite.SnoozeDependencyUpdatesRequest) (*ermite.SnoozeDependencyUpdatesResponse, error) {
		return &ermite.SnoozeDependencyUpdatesResponse{Updates: depsAlpha}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "snooze", "alpha").ok(t)
	assert.Equal(t, "ermite: snooze: alpha\n", res.stderr)
	calls, _ := f.snapshot()
	assert.Equal(t, []string{"SnoozeDependencyUpdates"}, calls)
}

func TestDepsMultilineError(t *testing.T) {
	r := &ermite.RepoDependencyUpdates{Repo: "alpha", State: ermite.RepoDependencyUpdates_STATE_FAILED, Error: "command failed\ngo: requires newer Go"}
	f := &fakeAPI{checkDeps: func(_ *ermite.CheckDependenciesRequest) (*ermite.CheckDependenciesResponse, error) {
		return &ermite.CheckDependenciesResponse{Updates: r}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "deps", "check", "alpha").ok(t)
	assert.Contains(t, res.stdout, "error          command failed\n               go: requires newer Go")
	assert.Equal(t, r.Error, toJSONDepsRepo(r).Error)
}
