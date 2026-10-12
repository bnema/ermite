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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/githttp"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

const (
	oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func repoName(t *testing.T, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

// startHooks serves a hook socket in a short temporary directory.
func startHooks(t *testing.T, h ports.PushHookService) (*githttp.HookServer, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "hook.sock")
	s := githttp.NewHookServer(sock, h)
	require.NoError(t, s.Listen(context.Background()))
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
	t.Parallel()
	_, sock := startHooks(t, mocks.NewPushHookService(t))
	fi, err := os.Stat(sock)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	assert.NotZero(t, fi.Mode()&os.ModeSocket)
}

func TestHookServerReplacesStaleSocketFile(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "hk")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "hook.sock")
	require.NoError(t, os.WriteFile(sock, []byte("stale"), 0o600))
	s := githttp.NewHookServer(sock, mocks.NewPushHookService(t))
	require.NoError(t, s.Listen(context.Background()))
	s.Close(context.Background())
}

func TestHookServerRejectsTooLongSocketPath(t *testing.T) {
	t.Parallel()
	s := githttp.NewHookServer("/"+strings.Repeat("a", 120)+"/hook.sock", mocks.NewPushHookService(t))
	err := s.Listen(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too long")
}

func TestHookProtocol(t *testing.T) {
	hooks := mocks.NewPushHookService(t)
	srv, sock := startHooks(t, hooks)
	p := domain.TokenPrincipal(domain.Token{ID: "tok1", Scopes: []domain.Scope{domain.ScopeRepoWrite}})
	tokenA, releaseA := srv.Register(context.Background(), repoName(t, "alpha"), p)
	tokenB, releaseB := srv.Register(context.Background(), repoName(t, "beta"), domain.Root())
	assert.NotEqual(t, tokenA, tokenB)
	assert.Len(t, tokenA, 64)

	upd := `[{"old":"` + domain.ZeroOID + `","new":"` + oidA + `","ref":"refs/heads/main"}]`

	t.Run("pre-receive is attributed to the repository and principal of its token", func(t *testing.T) {
		updates := []domain.RefUpdate{{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"}}
		hooks.EXPECT().Check(mock.Anything, mock.MatchedBy(func(got domain.Principal) bool { return got.TokenID() == "tok1" }),
			repoName(t, "alpha"), updates).Return(nil).Once()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.True(t, r.OK)

		hooks.EXPECT().Record(mock.Anything, mock.MatchedBy(func(got domain.Principal) bool { return got.IsRoot() }),
			repoName(t, "beta"), updates).Return(nil).Once()
		r = raw(t, sock, `{"token":"`+tokenB+`","hook":"post-receive","updates":`+upd+`}`)
		assert.True(t, r.OK)
	})

	t.Run("a repository sent by the client is ignored", func(t *testing.T) {
		hooks.EXPECT().Check(mock.Anything, mock.Anything, repoName(t, "alpha"), mock.Anything).Return(nil).Once()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","repo":"beta","updates":[]}`)
		assert.True(t, r.OK)
	})

	t.Run("refusal carries the message", func(t *testing.T) {
		hooks.EXPECT().Check(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(errors.Join(domain.ErrRejected, errors.New("protected branch"))).Once()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.False(t, r.OK)
		assert.Contains(t, r.Message, "rejected")
	})

	t.Run("an internal error fails closed without leaking details", func(t *testing.T) {
		hooks.EXPECT().Check(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(errors.New("disk /secret/path exploded")).Once()
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":`+upd+`}`)
		assert.False(t, r.OK)
		assert.NotContains(t, r.Message, "/secret/path")
	})

	t.Run("post-receive failure is still ok for git, with a note", func(t *testing.T) {
		hooks.EXPECT().Record(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(errors.New("boom")).Once()
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
		r := raw(t, sock, `{"token":"`+tokenA+`","hook":"pre-receive","updates":[]}`)
		assert.False(t, r.OK, "released token")
		assert.Contains(t, r.Message, "token")
		// The hooks mock expects no call: no use case ran.
		releaseB()
	})

	t.Run("malformed requests are refused", func(t *testing.T) {
		tok, rel := srv.Register(context.Background(), repoName(t, "gamma"), domain.Root())
		defer rel()
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
		// The hooks mock expects no call: no use case ran.
	})

	t.Run("a connection that closes without a request does not hurt", func(t *testing.T) {
		conn, err := net.Dial("unix", sock)
		require.NoError(t, err)
		_ = conn.Close()
		r := raw(t, sock, `{"token":"x","hook":"pre-receive","updates":[]}`)
		assert.False(t, r.OK)
	})
}

// A shutdown while a push is being recorded must not drop the recording: the
// push is already in the repository.
func TestHookRecordSurvivesShutdown(t *testing.T) {
	t.Parallel()
	hooks := mocks.NewPushHookService(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	var cancelled, hasDeadline bool
	var ctxMu sync.Mutex
	hooks.EXPECT().Record(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _ domain.Principal, _ domain.RepoName, _ []domain.RefUpdate) error {
			close(started)
			<-proceed
			ctxMu.Lock()
			defer ctxMu.Unlock()
			cancelled = ctx.Err() != nil
			_, hasDeadline = ctx.Deadline()
			return nil
		}).Once()

	dir, err := os.MkdirTemp("", "hk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "hook.sock")
	srv := githttp.NewHookServer(sock, hooks)
	require.NoError(t, srv.Listen(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	tok, rel := srv.Register(context.Background(), repoName(t, "alpha"), domain.Root())
	defer rel()
	replies := make(chan githttp.HookReply, 1)
	go func() {
		replies <- raw(t, sock, `{"token":"`+tok+`","hook":"post-receive","updates":[]}`)
	}()
	<-started
	cancel() // the server is shutting down while Record runs
	close(proceed)

	select {
	case r := <-replies:
		assert.True(t, r.OK)
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("hook server did not stop")
	}
	ctxMu.Lock()
	defer ctxMu.Unlock()
	assert.False(t, cancelled, "Record is not cancelled by the shutdown")
	assert.True(t, hasDeadline, "but it is bounded")
}

func TestHookConcurrentTokens(t *testing.T) {
	t.Parallel()
	hooks := mocks.NewPushHookService(t)
	srv, sock := startHooks(t, hooks)
	var wg sync.WaitGroup
	// Each of the 16 tokens reaches the use case for its own repository, once.
	for i := range 16 {
		hooks.EXPECT().Record(mock.Anything, mock.Anything, repoName(t, "r"+string(rune('a'+i))), mock.Anything).Return(nil).Once()
	}
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := repoName(t, "r"+string(rune('a'+i)))
			tok, rel := srv.Register(context.Background(), name, domain.Root())
			defer rel()
			r := raw(t, sock, `{"token":"`+tok+`","hook":"post-receive","updates":[{"old":"`+domain.ZeroOID+`","new":"`+oidB+`","ref":"refs/heads/x"}]}`)
			assert.True(t, r.OK)
		}()
	}
	wg.Wait()
}

func TestRunHook(t *testing.T) {
	ctx := context.Background()
	hooks := mocks.NewPushHookService(t)
	srv, sock := startHooks(t, hooks)
	tok, rel := srv.Register(context.Background(), repoName(t, "alpha"), domain.Root())
	defer rel()
	input := domain.ZeroOID + " " + oidA + " refs/heads/main\n" + oidA + " " + oidB + " refs/heads/dev\n"

	t.Run("sends the lines git feeds the hook", func(t *testing.T) {
		var errOut bytes.Buffer
		hooks.EXPECT().Check(mock.Anything, mock.Anything, repoName(t, "alpha"), []domain.RefUpdate{
			{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/main"},
			{Old: oidA, New: oidB, Ref: "refs/heads/dev"},
		}).Return(nil).Once()
		require.NoError(t, githttp.RunHook(ctx, "pre-receive", strings.NewReader(input), &errOut, sock, tok))
		assert.Empty(t, errOut.String())
	})

	t.Run("a ref name with spaces survives", func(t *testing.T) {
		hooks.EXPECT().Record(mock.Anything, mock.Anything, repoName(t, "alpha"),
			[]domain.RefUpdate{{Old: domain.ZeroOID, New: oidA, Ref: "refs/heads/a b"}}).Return(nil).Once()
		require.NoError(t, githttp.RunHook(ctx, "post-receive", strings.NewReader(domain.ZeroOID+" "+oidA+" refs/heads/a b\n"), &bytes.Buffer{}, sock, tok))
	})

	t.Run("pre-receive refusal prints the message and fails", func(t *testing.T) {
		hooks.EXPECT().Check(mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(errors.Join(domain.ErrRejected, errors.New("nope"))).Once()
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
	t.Parallel()
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
	t.Parallel()
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
