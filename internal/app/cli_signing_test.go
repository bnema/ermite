// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
)

// withCommitSignatures is a GetVersion that lists commit_signatures.
func withCommitSignatures(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
	return &ermite.GetVersionResponse{Version: "v0.7.0", Features: []string{"commit_signatures"}}, nil
}

const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLEEXAMPLE1234"

func lastRequest[T any](t *testing.T, f *fakeAPI, call string) *T {
	t.Helper()
	calls, reqs := f.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == call {
			return reqs[i].(*T)
		}
	}
	t.Fatalf("no %s call", call)
	return nil
}

func TestSigningKeyTrustList(t *testing.T) {
	keys := []*ermite.TrustedKey{
		{Id: "SHA256:builtin", Format: ermite.SignatureFormat_SIGNATURE_FORMAT_SSH, Title: "ermite", PublicKey: "ssh-ed25519 AAAA", Emails: []string{"ermite@example.com"}, Builtin: true},
		{Id: "ABCDEF0123", Format: ermite.SignatureFormat_SIGNATURE_FORMAT_GPG, Title: "work", PublicKey: "-----BEGIN PGP PUBLIC KEY BLOCK-----", Emails: []string{"a@b.c", "d@e.f"}, CreatedAt: ts("2026-10-10T12:00:00Z")},
	}
	f := &fakeAPI{getVersion: withCommitSignatures, listTrustedKeys: func(*ermite.ListTrustedKeysRequest) (*ermite.ListTrustedKeysResponse, error) {
		return &ermite.ListTrustedKeysResponse{Keys: keys}, nil
	}}
	url := f.serve(t)

	res := runCLI(t, envFor(url), "", "signing-key", "trust", "list").ok(t)
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 3, res.stdout)
	assert.Regexp(t, `^ID\s+FORMAT\s+TITLE\s+EMAILS\s+BUILTIN$`, lines[0])
	assert.Regexp(t, `^SHA256:builtin\s+ssh\s+ermite\s+ermite@example.com\s+yes$`, lines[1])
	assert.Regexp(t, `^ABCDEF0123\s+gpg\s+work\s+a@b.c,d@e.f\s+-$`, lines[2])

	res = runCLI(t, envFor(url), "", "signing-key", "trust", "list", "--json").ok(t)
	assert.JSONEq(t, `[
		{"id":"SHA256:builtin","format":"ssh","title":"ermite","public_key":"ssh-ed25519 AAAA","emails":["ermite@example.com"],"builtin":true,"created_at":null},
		{"id":"ABCDEF0123","format":"gpg","title":"work","public_key":"-----BEGIN PGP PUBLIC KEY BLOCK-----","emails":["a@b.c","d@e.f"],"builtin":false,"created_at":"2026-10-10T12:00:00Z"}
	]`, res.stdout)
}

func TestSigningKeyTrustListEmpty(t *testing.T) {
	f := &fakeAPI{getVersion: withCommitSignatures, listTrustedKeys: func(*ermite.ListTrustedKeysRequest) (*ermite.ListTrustedKeysResponse, error) {
		return &ermite.ListTrustedKeysResponse{}, nil
	}}
	url := f.serve(t)
	res := runCLI(t, envFor(url), "", "signing-key", "trust", "list").ok(t)
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "no trusted keys")
	res = runCLI(t, envFor(url), "", "signing-key", "trust", "list", "--json").ok(t)
	assert.JSONEq(t, `[]`, res.stdout)
}

func addEcho(r *ermite.AddTrustedKeyRequest) (*ermite.AddTrustedKeyResponse, error) {
	return &ermite.AddTrustedKeyResponse{Key: &ermite.TrustedKey{
		Id: "SHA256:new", Format: r.GetFormat(), Title: r.GetTitle(), PublicKey: r.GetPublicKey(), Emails: r.GetEmails(), CreatedAt: ts("2026-10-10T12:00:00Z"),
	}}, nil
}

func TestSigningKeyTrustAdd(t *testing.T) {
	t.Run("from a file", func(t *testing.T) {
		f := &fakeAPI{getVersion: withCommitSignatures, addTrustedKey: addEcho}
		file := filepath.Join(t.TempDir(), "id_ed25519.pub")
		require.NoError(t, os.WriteFile(file, []byte(testSSHKey+" me@laptop\n"), 0o600))
		res := runCLI(t, envFor(f.serve(t)), "", "signing-key", "trust", "add", "--format", "ssh", "--title", "laptop",
			"--email", "me@example.com", "--email", "me@work.example", file).ok(t)
		got := lastRequest[ermite.AddTrustedKeyRequest](t, f, "AddTrustedKey")
		assert.Equal(t, ermite.SignatureFormat_SIGNATURE_FORMAT_SSH, got.GetFormat())
		assert.Equal(t, "laptop", got.GetTitle())
		assert.Equal(t, testSSHKey+" me@laptop", got.GetPublicKey())
		assert.Equal(t, []string{"me@example.com", "me@work.example"}, got.GetEmails())
		assert.Equal(t, "SHA256:new\n", res.stdout)
		assert.Contains(t, res.stderr, `trusted ssh key "laptop"`)
	})

	t.Run("gpg from stdin, json", func(t *testing.T) {
		f := &fakeAPI{getVersion: withCommitSignatures, addTrustedKey: addEcho}
		armor := "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nxxxx\n-----END PGP PUBLIC KEY BLOCK-----\n"
		res := runCLI(t, envFor(f.serve(t)), armor, "signing-key", "trust", "add", "--format", "GPG", "--title", "work",
			"--email", "me@example.com", "--json", "-").ok(t)
		got := lastRequest[ermite.AddTrustedKeyRequest](t, f, "AddTrustedKey")
		assert.Equal(t, ermite.SignatureFormat_SIGNATURE_FORMAT_GPG, got.GetFormat())
		assert.Equal(t, strings.TrimSpace(armor), got.GetPublicKey())
		assert.JSONEq(t, `{"id":"SHA256:new","format":"gpg","title":"work","public_key":"`+strings.ReplaceAll(strings.TrimSpace(armor), "\n", `\n`)+`","emails":["me@example.com"],"builtin":false,"created_at":"2026-10-10T12:00:00Z"}`, res.stdout)
	})

	t.Run("flag and input errors send nothing", func(t *testing.T) {
		big := strings.Repeat("a", 64<<10+1)
		for name, tt := range map[string]struct {
			stdin string
			args  []string
			want  string
		}{
			"missing email":  {testSSHKey, []string{"--format", "ssh", "--title", "t", "-"}, `"email"`},
			"missing title":  {testSSHKey, []string{"--format", "ssh", "--email", "a@b.c", "-"}, `"title"`},
			"missing format": {testSSHKey, []string{"--title", "t", "--email", "a@b.c", "-"}, `"format"`},
			"x509":           {testSSHKey, []string{"--format", "x509", "--title", "t", "--email", "a@b.c", "-"}, "use ssh or gpg"},
			"empty input":    {"  \n", []string{"--format", "ssh", "--title", "t", "--email", "a@b.c", "-"}, "no key to read"},
			"too large":      {big, []string{"--format", "ssh", "--title", "t", "--email", "a@b.c", "-"}, "larger than 64 KiB"},
			"no such file":   {"", []string{"--format", "ssh", "--title", "t", "--email", "a@b.c", "/nonexistent/key.pub"}, "read /nonexistent/key.pub"},
		} {
			f := &fakeAPI{getVersion: withCommitSignatures, addTrustedKey: addEcho}
			res := runCLI(t, envFor(f.serve(t)), tt.stdin, append([]string{"signing-key", "trust", "add"}, tt.args...)...)
			require.Error(t, res.err, name)
			assert.Contains(t, res.err.Error(), tt.want, name)
			calls, _ := f.snapshot()
			assert.NotContains(t, calls, "AddTrustedKey", name)
		}
	})

	t.Run("server errors are described", func(t *testing.T) {
		for name, tt := range map[string]struct {
			err  error
			want string
		}{
			"not the owner": {connect.NewError(connect.CodePermissionDenied, errors.New("owner only")), "permission denied"},
			"duplicate":     {connect.NewError(connect.CodeAlreadyExists, errors.New("key is already trusted")), "already exists"},
			"invalid":       {connect.NewError(connect.CodeInvalidArgument, errors.New("not a public key")), "not a public key"},
		} {
			f := &fakeAPI{getVersion: withCommitSignatures, addTrustedKey: func(*ermite.AddTrustedKeyRequest) (*ermite.AddTrustedKeyResponse, error) { return nil, tt.err }}
			res := runCLI(t, envFor(f.serve(t)), testSSHKey, "signing-key", "trust", "add", "--format", "ssh", "--title", "t", "--email", "a@b.c", "-")
			require.Error(t, res.err, name)
			assert.Contains(t, res.err.Error(), tt.want, name)
			assert.Empty(t, res.stdout, name)
		}
	})
}

func TestSigningKeyTrustRemove(t *testing.T) {
	f := &fakeAPI{getVersion: withCommitSignatures, removeTrustedKey: func(*ermite.RemoveTrustedKeyRequest) (*ermite.RemoveTrustedKeyResponse, error) {
		return &ermite.RemoveTrustedKeyResponse{}, nil
	}}
	res := runCLI(t, envFor(f.serve(t)), "", "signing-key", "trust", "remove", "SHA256:abc").ok(t)
	assert.Equal(t, "SHA256:abc", lastRequest[ermite.RemoveTrustedKeyRequest](t, f, "RemoveTrustedKey").GetId())
	assert.Empty(t, res.stdout)
	assert.Contains(t, res.stderr, "removed trusted key SHA256:abc")

	f = &fakeAPI{getVersion: withCommitSignatures, removeTrustedKey: func(*ermite.RemoveTrustedKeyRequest) (*ermite.RemoveTrustedKeyResponse, error) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the builtin key cannot be removed"))
	}}
	res = runCLI(t, envFor(f.serve(t)), "", "signing-key", "trust", "remove", "SHA256:builtin")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "builtin key cannot be removed")

	require.Error(t, runCLI(t, envFor(f.serve(t)), "", "signing-key", "trust", "remove").err, "the id is required")
}

func TestSigningKeyTrustNeedsServerFeature(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error)
		want    string
	}{
		{"feature missing", func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
			return &ermite.GetVersionResponse{Version: "v0.6.0", Features: []string{"signing_key"}}, nil
		}, "does not support commit_signatures"},
		{"version unreadable", nil, "does not implement this call"},
		{"server unavailable", func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
		}, "cannot reach the server"},
	} {
		for _, args := range [][]string{
			{"list"},
			{"add", "--format", "ssh", "--title", "t", "--email", "a@b.c", "-"},
			{"remove", "SHA256:abc"},
		} {
			f := &fakeAPI{getVersion: tt.version}
			res := runCLI(t, envFor(f.serve(t)), testSSHKey, append([]string{"signing-key", "trust"}, args...)...)
			require.Error(t, res.err, tt.name)
			assert.Contains(t, strings.ToLower(res.err.Error()), tt.want, tt.name)
			calls, _ := f.snapshot()
			assert.Equal(t, []string{"GetVersion"}, calls, "%s %v: no key call", tt.name, args)
			assert.Empty(t, res.stdout)
		}
	}
}

func TestSigningKeyHelpMentionsTrust(t *testing.T) {
	res := runCLI(t, nil, "", "signing-key", "--help").ok(t)
	assert.Contains(t, res.stdout, "trust")
	assert.Contains(t, res.stdout, "commit_signatures")
	res = runCLI(t, nil, "", "signing-key", "trust", "list", "--help").ok(t)
	assert.Contains(t, res.stdout, "ermite signing-key trust --help")
}
