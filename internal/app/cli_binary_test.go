// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

// runBinary runs the built ermite with only the given variables.
func runBinary(t *testing.T, vars []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(baseEnv(), vars...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return out.String(), errb.String(), code
}

func TestBinaryRemoteCommandErrorIsOneLineAndExitsOne(t *testing.T) {
	_, stderr, code := runBinary(t, nil, "repo", "list")
	assert.Equal(t, 1, code)
	assert.Regexp(t, `^ermite: ERMITE_URL is not set[^\n]*\n$`, stderr)
}

func TestBinaryAgainstStubServer(t *testing.T) {
	f := &fakeAPI{listRepos: func(*ermite.ListReposRequest) (*ermite.ListReposResponse, error) {
		return &ermite.ListReposResponse{Repos: []*ermite.Repo{repoAlpha}}, nil
	}}
	url := f.serve(t)
	stdout, stderr, code := runBinary(t, []string{"ERMITE_URL=" + url, "ERMITE_TOKEN=ermite_t"}, "repo", "list", "--json")
	assert.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, `"name": "alpha"`)
	assert.Contains(t, f.header[0].Get("User-Agent"), "ermite/")
	assert.Contains(t, f.header[0].Get("User-Agent"), "(cli)")

	_, stderr, code = runBinary(t, []string{"ERMITE_URL=" + url}, "repo", "delete", "alpha")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "ermite: aborted")
}
