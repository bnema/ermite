// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/domain"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type call struct {
	hook    string
	p       domain.Principal
	repo    domain.RepoName
	updates []domain.RefUpdate
}

type fakeHooks struct {
	mu       sync.Mutex
	calls    []call
	checkErr error
	recErr   error
}

func (f *fakeHooks) Check(_ context.Context, p domain.Principal, n domain.RepoName, u []domain.RefUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{"check", p, n, u})
	return f.checkErr
}

func (f *fakeHooks) Record(_ context.Context, p domain.Principal, n domain.RepoName, u []domain.RefUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{"record", p, n, u})
	return f.recErr
}

func (f *fakeHooks) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func repoName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

// startHooks serves a hook socket in a short temporary directory.
func startHooks(t *testing.T, h githttp.PushHooks) (*githttp.HookServer, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "hook.sock")
	s := githttp.NewHookServer(sock, h)
	require.NoError(t, s.Listen())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("hook server did not stop")
		}
	})
	return s, sock
}

// raw sends one line and reads the reply line.
func raw(t *testing.T, sock, line string) githttp.HookReply {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(line + "\n"))
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var r githttp.HookReply
	require.NoError(t, json.Unmarshal(reply, &r), string(reply))
	return r
}

func TestHookSocketMode(t *testing.T) {
	_, sock := startHooks(t, &fakeHooks{})
	fi, err := os.Stat(sock)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	assert.NotZero(t, fi.Mode()&os.ModeSocket)
}

func TestHookServerReplacesStaleSocketFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "hk")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "hook.sock")
	require.NoError(t, os.WriteFile(sock, []byte("stale"), 0o600))
	s := githttp.NewHookServer(sock, &fakeHooks{})
	require.NoError(t, s.Listen())
	s.Close()
}

func TestHookServerRejectsTooLongSocketPath(t *testing.T) {
	s := githttp.NewHookServer("/"+strings.Repeat("a", 120)+"/hook.sock", &fakeHooks{})
	err := s.Listen()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too long")
}

func TestHookProtocol(t *testing.T) {
	fh := &fakeHooks{}
	srv, sock := startHooks(t, fh)
	p := domain.TokenPrincipal(domain.Token{ID: "tok1", Scopes: []domain.Scope{domain.ScopeRepoWrite}})
	tokenA, releaseA := srv.Register(repoName(t, "alpha"), p)
	tokenB, releaseB := srv.Register(repoName(t, "beta"), domain.Root())
	assert.NotEqual(t, tokenA, tokenB)
	assert.Len(t, tokenA, 64)

	upd := `[{"old":"` + domain.ZeroOID + `","new":"` + oidA + `","ref":"refs/heads/main"}]`

	t.Run("pre-receive is attributed to the repository and principal of its token", func(t *testing.T) {
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.True(t, r.OK)
		c := fh.last()
		assert.Equal(t, "check", c.hook)
		assert.Equal(t, repoName(t, "alpha"), c.repo)
		assert.Equal(t, "tok1", c.p.TokenID())
		assert.Equal(t, []domain.RefUpdate{{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"}}, c.updates)

		r = raw(t, sock, `{"token":"`+tokenB+`","hook":"post-receive","updates":`+upd+`}`)
		assert.True(t, r.OK)
		c = fh.last()
		assert.Equal(t, "record", c.hook)
		assert.Equal(t, repoName(t, "beta"), c.repo)
		assert.True(t, c.p.IsRoot())
	})

	t.Run("a repository sent by the client is ignored", func(t *testing.T) {
		raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","repo":"beta","updates":[]}`)
		assert.Equal(t, repoName(t, "alpha"), fh.last().repo)
	})

	t.Run("refusal carries the message", func(t *testing.T) {
		fh.checkErr = errors.Join(domain.ErrRejected, errors.New("protected branch"))
		defer func() { fh.checkErr = nil }()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.False(t, r.OK)
		assert.Contains(t, r.Message, "rejected")
	})

	t.Run("an internal error fails closed without leaking details", func(t *testing.T) {
		fh.checkErr = errors.New("disk /secret/path exploded")
		defer func() { fh.checkErr = nil }()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.False(t, r.OK)
		assert.NotContains(t, r.Message, "/secret/path")
	})

	t.Run("post-receive failure is still ok for git, with a note", func(t *testing.T) {
		fh.recErr = errors.New("boom")
		defer func() { fh.recErr = nil }()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"post-receive","updates":`+upd+`}`)
		assert.True(t, r.OK)
		assert.NotEmpty(t, r.Message)
	})

	t.Run("unknown, empty and released tokens are refused", func(t *testing.T) {
		for _, tok := range []string{"nope", ""} {
			r := raw(t, sock, `{"token":"`+tok+`","hook":"pre-receive","updates":[]}`)
			assert.False(t, r.OK, tok)
		}
		releaseA()
		before := len(fh.calls)
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":[]}`)
		assert.False(t, r.OK, "released token")
		assert.Contains(t, r.Message, "token")
		assert.Len(t, fh.calls, before, "no use case ran")
		releaseB()
	})

	t.Run("malformed requests are refused", func(t *testing.T) {
		tok, rel := srv.Register(repoName(t, "gamma"), domain.Root())
		defer rel()
		before := len(fh.calls)
		for _, line := range []string{
			`not json`,
			`{}`,
			`{"token":"` + tok + `","hook":"update","updates":[]}`,
			`{"token":"` + tok + `","hook":"pre-receive","updates":[{"old":"zz","new":"` + oidA + `","ref":"refs/heads/x"}]}`,
			`{"token":"` + tok + `","hook":"pre-receive","updates":[{"old":"` + oidA + `","new":"` + oidA + `","ref":""}]}`,
		} {
			r := raw(t, sock, line)
			assert.False(t, r.OK, line)
		}
		assert.Len(t, fh.calls, before)
	})

	t.Run("a connection that closes without a request does not hurt", func(t *testing.T) {
		conn, err := net.Dial("unix", sock)
		require.NoError(t, err)
		_ = conn.Close()
		r := raw(t, sock, `{"token":"x","hook":"pre-receive","updates":[]}`)
		assert.False(t, r.OK)
	})
}

func TestHookConcurrentTokens(t *testing.T) {
	fh := &fakeHooks{}
	srv, sock := startHooks(t, fh)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := repoName(t, "r"+string(rune('a'+i)))
			tok, rel := srv.Register(name, domain.Root())
			defer rel()
			r := raw(t, sock, `{"token":"`+tok+`","hook":"post-receive","updates":[{"old":"`+domain.ZeroOID+`","new":"`+oidB+`","ref":"refs/heads/x"}]}`)
			assert.True(t, r.OK)
		}()
	}
	wg.Wait()
	seen := map[domain.RepoName]bool{}
	for _, c := range fh.calls {
		seen[c.repo] = true
	}
	assert.Len(t, seen, 16)
}

func TestRunHook(t *testing.T) {
	ctx := context.Background()
	fh := &fakeHooks{}
	srv, sock := startHooks(t, fh)
	tok, rel := srv.Register(repoName(t, "alpha"), domain.Root())
	defer rel()
	input := domain.ZeroOID + " " + oidA + " refs/heads/main\n" + oidA + " " + oidB + " refs/heads/dev\n"

	t.Run("sends the lines git feeds the hook", func(t *testing.T) {
		var errOut bytes.Buffer
		require.NoError(t, githttp.RunHook(ctx, "pre-receive", strings.NewReader(input), &errOut, sock, tok))
		assert.Equal(t, []domain.RefUpdate{
			{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"},
			{Old: oidA, New: oidB, Ref: "refs/heads/dev"},
		}, fh.last().updates)
		assert.Empty(t, errOut.String())
	})

	t.Run("a ref name with spaces survives", func(t *testing.T) {
		require.NoError(t, githttp.RunHook(ctx, "post-receive", strings.NewReader(domain.ZeroOID+" "+oidA+" refs/heads/a b\n"), &bytes.Buffer{}, sock, tok))
		assert.Equal(t, "refs/heads/a b", fh.last().updates[0].Ref)
	})

	t.Run("pre-receive refusal prints the message and fails", func(t *testing.T) {
		fh.checkErr = errors.Join(domain.ErrRejected, errors.New("nope"))
		defer func() { fh.checkErr = nil }()
		var errOut bytes.Buffer
		err := githttp.RunHook(ctx, "pre-receive", strings.NewReader(input), &errOut, sock, tok)
		require.ErrorIs(t, err, githttp.ErrHookRejected)
		assert.Contains(t, errOut.String(), "rejected")
	})

	t.Run("pre-receive fails closed", func(t *testing.T) {
		for name, args := range map[string][2]string{
			"no socket":      {"", tok},
			"no token":       {sock, ""},
			"socket missing": {filepath.Join(t.TempDir(), "gone.sock"), tok},
			"unknown token":  {sock, "unknown"},
		} {
			var errOut bytes.Buffer
			err := githttp.RunHook(ctx, "pre-receive", strings.NewReader(input), &errOut, args[0], args[1])
			assert.ErrorIs(t, err, githttp.ErrHookRejected, name)
		}
		err := githttp.RunHook(ctx, "pre-receive", strings.NewReader("garbage\n"), &bytes.Buffer{}, sock, tok)
		assert.ErrorIs(t, err, githttp.ErrHookRejected, "unreadable input")
		assert.Error(t, githttp.RunHook(ctx, "update", strings.NewReader(input), &bytes.Buffer{}, sock, tok))
	})

	t.Run("a server that never answers is cut off by the context", func(t *testing.T) {
		dir, err := os.MkdirTemp("", "hk")
		require.NoError(t, err)
		defer func() { _ = os.RemoveAll(dir) }()
		silent := filepath.Join(dir, "s.sock")
		ln, err := net.Listen("unix", silent)
		require.NoError(t, err)
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
			}
		}()
		cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		err = githttp.RunHook(cctx, "pre-receive", strings.NewReader(input), &bytes.Buffer{}, silent, tok)
		assert.ErrorIs(t, err, githttp.ErrHookRejected)
		assert.Less(t, time.Since(start), 5*time.Second)
	})

	t.Run("post-receive never fails the push", func(t *testing.T) {
		var errOut bytes.Buffer
		require.NoError(t, githttp.RunHook(ctx, "post-receive", strings.NewReader(input), &errOut, filepath.Join(t.TempDir(), "gone.sock"), tok))
		assert.Contains(t, errOut.String(), "post-receive")
		errOut.Reset()
		require.NoError(t, githttp.RunHook(ctx, "post-receive", strings.NewReader(input), &errOut, "", ""))
		assert.NotEmpty(t, errOut.String())
	})
}

func TestWriteHookScripts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "hooks")
	exe := "/opt/it's here/\"ermite\" $HOME `x`/ermite"
	require.NoError(t, githttp.WriteHookScripts(dir, exe))

	for _, hook := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(dir, hook)
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(body), "#!/bin/sh\n"))
		assert.Contains(t, string(body), "exec '/opt/it'\\''s here/\"ermite\" $HOME `x`/ermite' hook "+hook+"\n")
	}
	// Rewriting replaces the files and leaves nothing behind.
	require.NoError(t, githttp.WriteHookScripts(dir, "/bin/true"))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	assert.Error(t, githttp.WriteHookScripts(dir, "relative/ermite"))
	assert.Error(t, githttp.WriteHookScripts(dir, "/bin/er\nmite"))
}

// The shell really passes the awkward path and the arguments through.
func TestHookScriptExecsTheBinaryWithQuotedPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "it's a \"dir\" $X `y` ; z")
	require.NoError(t, os.MkdirAll(base, 0o755))
	exe := filepath.Join(base, "fake ermite")
	out := filepath.Join(t.TempDir(), "args")
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s|' \"$@\" > '"+out+"'\ncat > '"+out+".stdin'\n"), 0o755))
	dir := t.TempDir()
	require.NoError(t, githttp.WriteHookScripts(dir, exe))

	cmd := exec.Command(filepath.Join(dir, "pre-receive"))
	cmd.Stdin = strings.NewReader("line\n")
	require.NoError(t, cmd.Run())
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "hook|pre-receive|", string(got))
	in, err := os.ReadFile(out + ".stdin")
	require.NoError(t, err)
	assert.Equal(t, "line\n", string(in), "stdin reaches the binary")
}
