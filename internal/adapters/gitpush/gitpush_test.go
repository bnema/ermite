// SPDX-License-Identifier: AGPL-3.0-or-later

package gitpush

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.org", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.org"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}
func setup(t *testing.T) (Config, ports.MirrorPush) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	git(t, "init", "-b", "main", source)
	git(t, "-C", source, "commit", "--allow-empty", "-m", "first")
	bin, err := exec.LookPath("git")
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return Config{Bin: bin, Path: os.Getenv("PATH"), Home: t.TempDir(), KnownHosts: filepath.Join(root, "mirror_known_hosts"), TempDir: filepath.Join(root, "keys"), RepoDir: func(domain.RepoName) string { return source }, AllowedProtocols: "file", Timeout: 5 * time.Second}, ports.MirrorPush{PrivateKey: key, Refspecs: []string{"refs/heads/main:refs/heads/main"}}
}
func clean(t *testing.T, cfg Config) {
	t.Helper()
	entries, err := os.ReadDir(cfg.TempDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
func TestPushLocal(t *testing.T) {
	cfg, req := setup(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	git(t, "init", "--bare", remote)
	req.URL = "file://" + remote
	p := New(cfg)
	result, err := p.Push(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, result.Refs, 1)
	require.Equal(t, ports.MirrorRefOK, result.Refs[0].Status)
	clean(t, cfg)
	result, err = p.Push(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, ports.MirrorRefUpToDate, result.Refs[0].Status)
	clean(t, cfg)
	other := filepath.Join(t.TempDir(), "other")
	git(t, "init", "-b", "main", other)
	git(t, "-C", other, "commit", "--allow-empty", "-m", "unrelated")
	git(t, "-C", other, "push", "file://"+remote, "refs/heads/main:refs/heads/other")
	// Move remote main locally, without a force push, to establish divergence.
	otherID := git(t, "-C", other, "rev-parse", "HEAD")
	git(t, "--git-dir", remote, "update-ref", "refs/heads/main", otherID)
	_, err = p.Push(context.Background(), req)
	var me *domain.MirrorError
	require.ErrorAs(t, err, &me)
	require.Equal(t, domain.MirrorClassDiverged, me.Class)
	require.Equal(t, otherID, git(t, "--git-dir", remote, "rev-parse", "refs/heads/main"))
	clean(t, cfg)
	req.URL = "file://" + filepath.Join(t.TempDir(), "missing.git")
	_, err = p.Push(context.Background(), req)
	require.ErrorAs(t, err, &me)
	require.Equal(t, domain.MirrorClassRemoteNotFound, me.Class)
	clean(t, cfg)
}
func TestBoundsAndKeyEnvironment(t *testing.T) {
	for _, mode := range []string{"success", "error", "timeout", "stdout", "stderr", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			cfg, req := setup(t)
			req.URL = "file:///unused"
			script := filepath.Join(t.TempDir(), "git")
			report := filepath.Join(t.TempDir(), "env")
			body := "#!/bin/sh\nenv > '" + report + "'\nfor d in '" + cfg.TempDir + "'/push-*; do\n test \"$(stat -c %a \"$d\")\" = 700 || exit 90\n for k in \"$d\"/key-*; do test \"$(stat -c %a \"$k\")\" = 600 || exit 91; done\ndone\n"
			switch mode {
			case "error":
				body += "echo 'Permission denied (publickey)' >&2; exit 1\n"
			case "timeout", "cancel":
				body += "sleep 10\n"
			case "stdout":
				body += "head -c 4096 /dev/zero\n"
			case "stderr":
				body += "head -c 4096 /dev/zero >&2\n"
			}
			require.NoError(t, os.WriteFile(script, []byte(body), 0700))
			cfg.Bin = script
			cfg.MaxOutput = 1024
			cfg.Timeout = 5 * time.Second
			if mode == "timeout" {
				cfg.Timeout = 200 * time.Millisecond // the only case that must wait for the deadline
			}
			t.Setenv("ERMITE_PRIVATE_TEST", "not-forwarded")
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			_, err := New(cfg).Push(ctx, req)
			switch mode {
			case "success":
				require.NoError(t, err)
			case "cancel":
				require.ErrorIs(t, err, context.DeadlineExceeded)
			default:
				var me *domain.MirrorError
				require.ErrorAs(t, err, &me)
				want := domain.MirrorClassTransient
				if mode == "error" {
					want = domain.MirrorClassAuthDenied
				}
				require.Equal(t, want, me.Class)
			}
			clean(t, cfg)
			env, e := os.ReadFile(report)
			require.NoError(t, e)
			require.NotContains(t, string(env), "ERMITE_")
			require.Contains(t, string(env), "IdentitiesOnly=yes")
			require.Contains(t, string(env), "BatchMode=yes")
			require.Contains(t, string(env), "StrictHostKeyChecking=accept-new")
			require.Contains(t, string(env), "UserKnownHostsFile=")
			require.Contains(t, string(env), "ConnectTimeout=30")
			require.Contains(t, string(env), "ServerAliveInterval=15")
			require.Contains(t, string(env), "ServerAliveCountMax=3")
		})
	}
}
func TestClassifierSamples(t *testing.T) {
	for _, tc := range []struct {
		s string
		c domain.MirrorFailureClass
	}{{"Permission denied (publickey)", domain.MirrorClassAuthDenied}, {"Repository not found", domain.MirrorClassRemoteNotFound}, {"Host key verification failed", domain.MirrorClassHostKey}, {"Could not resolve hostname", domain.MirrorClassTransient}, {"non-fast-forward", domain.MirrorClassDiverged}} {
		require.Equal(t, tc.c, domain.ClassifyMirrorOutput(tc.s, errors.New("exit 1"), false))
	}
}
func TestRejectUnsafeRefspecs(t *testing.T) {
	for _, ref := range []string{"+refs/heads/main:refs/heads/main", ":refs/heads/main", "--mirror", "refs/heads/*:refs/heads/*", "refs/heads/main:refs/heads/other"} {
		require.False(t, validRefspec(ref))
	}
	require.True(t, validRefspec("refs/tags/v1:refs/tags/v1"))
}
func TestSafeDetail(t *testing.T) {
	s := safeDetail(strings.Repeat("x", 5000)+" ssh://user:password@host/repo user@host", "ssh://user:password@host/repo")
	require.LessOrEqual(t, len(s), 4096)
	require.NotContains(t, s, "password")
	require.NotContains(t, s, "user@")
}

func TestSafeDetailKeepsValidUTF8(t *testing.T) {
	// The cut falls inside a multibyte character, and the remote sent a stray byte.
	in := "\xff" + strings.Repeat("x", 3) + strings.Repeat("é", 4097) + "\xff end"
	var s string
	require.NotPanics(t, func() { s = safeDetail(in, "ssh://host/repo") })
	require.True(t, utf8.ValidString(s))
	require.LessOrEqual(t, len(s), maxDetail)
	require.True(t, strings.HasSuffix(s, "? end"))
}

func TestClassifyIgnoresRemoteLines(t *testing.T) {
	stderr := "remote: Permission denied (publickey) is what your CI says\nfatal: unable to access: Could not resolve hostname\n"
	require.Equal(t, domain.MirrorClassTransient, domain.ClassifyMirrorOutput(withoutRemoteLines(stderr), errors.New("exit 128"), false))
	require.Contains(t, withoutRemoteLines("remote: a\n  remote: b\nkept"), "kept")
	require.NotContains(t, withoutRemoteLines("remote: a\n  remote: b\nkept"), "remote:")
}

func TestRemoteLinesStayInDetailButNotInClass(t *testing.T) {
	cfg, req := setup(t)
	req.URL = "file:///unused"
	script := filepath.Join(t.TempDir(), "git")
	body := "#!/bin/sh\necho 'remote: error: non-fast-forward hook says no' >&2\necho 'fatal: Could not read from remote repository.' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0700))
	cfg.Bin = script
	_, err := New(cfg).Push(context.Background(), req)
	var me *domain.MirrorError
	require.ErrorAs(t, err, &me)
	require.Equal(t, domain.MirrorClassTransient, me.Class, "a message from the remote does not pick the class")
	require.Contains(t, me.Detail, "remote: error")
}
