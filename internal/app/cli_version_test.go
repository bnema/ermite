// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite/internal/adapters/client"
)

// allFeatures are the features this client uses.
var allFeatures = []string{"repo.update", "events.resume", "merge_requests", "merge_requests.delete_branch", "signing_key", "session", "token.branch_prefixes", "ci.runs", "ci.runners", "ci.secrets", "ci.dispatch", "passkeys"}

// Every feature the client uses needs a sentence for the version warning.
func TestEveryUsedFeatureExplainsWhatBreaks(t *testing.T) {
	for _, f := range client.UsedFeatures() {
		assert.NotEmpty(t, featureUse[f], f)
	}
}

func versionFake(features ...string) *fakeAPI {
	return &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Version: "v0.4.0", Features: features}, nil
	}}
}

func TestVersionClientOnlyWithoutURL(t *testing.T) {
	res := runCLI(t, nil, "", "version").ok(t)
	assert.Equal(t, "ermite v0.0.1\n", res.stdout)
	assert.Empty(t, res.stderr)

	res = runCLI(t, nil, "", "version", "--json").ok(t)
	assert.JSONEq(t, `{"client":{"version":"v0.0.1"},"server":null}`, res.stdout)
}

func TestVersionWithServer(t *testing.T) {
	f := versionFake(allFeatures...)
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "version").ok(t)
	assert.Equal(t, "client    ermite v0.0.1\nserver    v0.4.0 at "+url+"\nfeatures  repo.update, events.resume, merge_requests, merge_requests.delete_branch, signing_key, session, token.branch_prefixes, ci.runs, ci.runners, ci.secrets, ci.dispatch, passkeys\n", res.stdout)
	assert.Empty(t, res.stderr, "no warning when every feature is there")

	res = runCLI(t, envFor(url), "", "version", "--json").ok(t)
	assert.JSONEq(t, `{"client":{"version":"v0.0.1"},"server":{"url":"`+url+`","version":"v0.4.0","features":["repo.update","events.resume","merge_requests","merge_requests.delete_branch","signing_key","session","token.branch_prefixes","ci.runs","ci.runners","ci.secrets","ci.dispatch","passkeys"],"missing_features":[]}}`, res.stdout)
}

func TestVersionWarnsAboutMissingFeatures(t *testing.T) {
	f := versionFake("repo.update", "merge_requests", "merge_requests.delete_branch", "signing_key", "session", "token.branch_prefixes", "ci.runs", "ci.runners", "ci.secrets", "ci.dispatch", "passkeys")
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "version").ok(t)
	assert.Contains(t, res.stdout, "v0.4.0")
	assert.Contains(t, res.stderr, "ermite: warning: the server does not support events.resume")
	assert.NotContains(t, res.stderr, "repo.update")

	res = runCLI(t, envFor(url), "", "version", "--json").ok(t)
	assert.JSONEq(t, `{"client":{"version":"v0.0.1"},"server":{"url":"`+url+`","version":"v0.4.0","features":["repo.update","merge_requests","merge_requests.delete_branch","signing_key","session","token.branch_prefixes","ci.runs","ci.runners","ci.secrets","ci.dispatch","passkeys"],"missing_features":["events.resume"]}}`, res.stdout)
	assert.Contains(t, res.stderr, "events.resume", "the warning also goes to stderr with --json")

	f = versionFake()
	res = runCLI(t, envFor(f.serve(t)), "", "version").ok(t)
	assert.Contains(t, res.stderr, "repo.update")
	assert.Contains(t, res.stderr, "events.resume")
	assert.Contains(t, res.stderr, "passkeys")
}

// A server without passkeys (http public URL, or an IP address host) works for
// everything else: only `ermite web passkey` is warned about.
func TestVersionWarnsAboutMissingPasskeys(t *testing.T) {
	f := versionFake(allFeatures[:len(allFeatures)-1]...)
	res := runCLI(t, envFor(f.serve(t)), "", "version", "--json").ok(t)
	assert.Contains(t, res.stdout, `"missing_features": [
      "passkeys"
    ]`)
	assert.Equal(t, 1, strings.Count(res.stderr, "warning:"), res.stderr)
	assert.Contains(t, res.stderr, "the server does not support passkeys, which `ermite web passkey` needs")
	assert.Contains(t, res.stderr, "https public URL")
}

func TestVersionServerFailureStillShowsClient(t *testing.T) {
	f := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "version")
	require.Error(t, res.err)
	assert.Equal(t, "ermite v0.0.1\n", res.stdout)
	assert.Contains(t, res.err.Error(), "server error: boom")
}

func TestVersionWorksWithoutToken(t *testing.T) {
	f := versionFake(allFeatures...)
	runCLI(t, map[string]string{"ERMITE_URL": f.serve(t)}, "", "version").ok(t)
	assert.Empty(t, f.header[0].Values("Authorization"))
}
