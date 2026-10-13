// SPDX-License-Identifier: AGPL-3.0-or-later

package githubapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
)

type rig struct {
	h    *Handler
	srv  *httptest.Server
	api  *mocks.GitHubAPIService
	auth *mocks.AuthService
}

func newRig(t *testing.T, mut func(*Config)) *rig {
	t.Helper()
	r := &rig{api: mocks.NewGitHubAPIService(t), auth: mocks.NewAuthService(t)}
	cfg := Config{Auth: r.auth, API: r.api}
	if mut != nil {
		mut(&cfg)
	}
	r.h = New(cfg)
	mux := http.NewServeMux()
	mux.Handle(Prefix+"/", r.h)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) do(t *testing.T, method, path, authorization string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+Prefix+path, nil)
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(b)
}

const tarballPath = "/repos/acme/proj/tarball/main"

// blockingArchive makes Tarball write one chunk and then wait for release, so
// that the archive stays in progress while the test looks at the others.
type blockingArchive struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingArchive() *blockingArchive {
	return &blockingArchive{started: make(chan struct{}, 32), release: make(chan struct{})}
}

func (b *blockingArchive) run(ctx context.Context, _ domain.Principal, _, _, _ string, begin func(string) io.Writer) error {
	w := begin("acme-proj-abcdef0.tar.gz")
	_, _ = w.Write([]byte("chunk"))
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

// A HEAD resolves the ref and answers the headers of the archive: git archive is
// not run (the mock fails on a call to Tarball), no slot is taken, and an error
// is answered as for a GET.
func TestHeadOfTheTarballRunsNoArchive(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(c *Config) { c.MaxArchives, c.MaxArchivesPerClient = 1, 1 })
	r.api.EXPECT().TarballName(mock.Anything, domain.Anonymous(), "acme", "proj", "main").Return("acme-proj-abcdef0.tar.gz", nil)
	r.api.EXPECT().TarballName(mock.Anything, domain.Anonymous(), "acme", "proj", "nope").Return("", domain.ErrNotFound)
	r.api.EXPECT().TarballName(mock.Anything, domain.Anonymous(), "acme", "proj", "").Return("", errors.New("disk"))

	resp, body := r.do(t, http.MethodHead, tarballPath, "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, body)
	assert.Equal(t, "attachment; filename=acme-proj-abcdef0.tar.gz", resp.Header.Get("Content-Disposition"))
	assert.Equal(t, "application/x-gzip", resp.Header.Get("Content-Type"))

	resp, body = r.do(t, http.MethodHead, "/repos/acme/proj/tarball/nope", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, body, "a HEAD has no body, the status says it")
	resp, _ = r.do(t, http.MethodHead, "/repos/acme/proj/tarball", "")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// The one slot is free for a GET after any number of HEADs.
	for range 5 {
		resp, _ := r.do(t, http.MethodHead, tarballPath, "")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

// The per-client cap answers 429 with Retry-After, the global cap 503 with
// Retry-After, and a finished archive gives its slot back.
func TestArchiveSlots(t *testing.T) {
	t.Parallel()
	t.Run("per client", func(t *testing.T) {
		t.Parallel()
		arch := newBlockingArchive()
		r := newRig(t, func(c *Config) { c.MaxArchives, c.MaxArchivesPerClient = 8, 2 })
		r.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(arch.run)

		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); r.do(t, http.MethodGet, tarballPath, "") }()
		}
		for range 2 {
			<-arch.started
		}
		resp, body := r.do(t, http.MethodGet, tarballPath, "")
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))
		assert.Contains(t, body, "Too many archives")
		assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

		// Other endpoints and HEAD are not archives.
		r.api.EXPECT().HashAlgorithm(mock.Anything, mock.Anything, "acme", "proj").Return("sha1", nil)
		resp, _ = r.do(t, http.MethodGet, "/repos/acme/proj/hash-algorithm", "")
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		close(arch.release)
		wg.Wait()
		// The slots are given back: two more go through.
		arch2 := newBlockingArchive()
		close(arch2.release)
		r.api.ExpectedCalls = nil
		r.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(arch2.run)
		for range 4 {
			resp, _ := r.do(t, http.MethodGet, tarballPath, "")
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("global", func(t *testing.T) {
		t.Parallel()
		arch := newBlockingArchive()
		r := newRig(t, func(c *Config) { c.MaxArchives, c.MaxArchivesPerClient = 1, 8 })
		r.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(arch.run)

		done := make(chan struct{})
		go func() { defer close(done); r.do(t, http.MethodGet, tarballPath, "") }()
		<-arch.started
		resp, body := r.do(t, http.MethodGet, tarballPath, "")
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))
		assert.Contains(t, body, "Server busy")
		close(arch.release)
		<-done

		// The client's own count was given back when the global slot refused it:
		// with a cap of one per client, the next one still gets a slot.
		r2 := newRig(t, func(c *Config) { c.MaxArchives, c.MaxArchivesPerClient = 1, 1 })
		arch2 := newBlockingArchive()
		r2.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(arch2.run)
		done2 := make(chan struct{})
		go func() { defer close(done2); r2.do(t, http.MethodGet, tarballPath, "") }()
		<-arch2.started
		resp, _ = r2.do(t, http.MethodGet, tarballPath, "")
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "the client cap answers first")
		close(arch2.release)
		<-done2
		close(arch.started)
	})

	t.Run("a server busy answer does not leak the client slot", func(t *testing.T) {
		t.Parallel()
		h := New(Config{MaxArchives: 1, MaxArchivesPerClient: 3, Auth: mocks.NewAuthService(t), API: mocks.NewGitHubAPIService(t)})
		h.archives <- struct{}{} // the server is full
		for range 10 {
			rec := httptest.NewRecorder()
			_, ok := h.acquire(rec, "client")
			require.False(t, ok)
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, "5", rec.Header().Get("Retry-After"))
		}
		assert.Empty(t, h.clients, "no client count was left behind")
		<-h.archives
		release, ok := h.acquire(httptest.NewRecorder(), "client")
		require.True(t, ok)
		release()
		assert.Empty(t, h.clients)
		assert.Empty(t, h.archives)
	})
}

// fakeRW is a response writer that can set a write deadline, like the one of
// net/http, and records the deadlines and the writes.
type fakeRW struct {
	header    http.Header
	body      strings.Builder
	deadlines []time.Time
	failWith  error
}

func (f *fakeRW) Header() http.Header { return f.header }
func (f *fakeRW) WriteHeader(int)     {}
func (f *fakeRW) Write(b []byte) (int, error) {
	if f.failWith != nil {
		return 0, f.failWith
	}
	return f.body.Write(b)
}
func (f *fakeRW) SetWriteDeadline(t time.Time) error {
	f.deadlines = append(f.deadlines, t)
	return nil
}

// The deadline of the connection moves forward before every chunk, by the idle
// timeout, so that only a client that stops reading is cut.
func TestIdleWriterMovesTheDeadlineForEveryChunk(t *testing.T) {
	t.Parallel()
	rw := &fakeRW{header: http.Header{}}
	w := &idleWriter{w: rw, rc: http.NewResponseController(rw), idle: 90 * time.Second}

	before := time.Now()
	for i := range 3 {
		n, err := w.Write([]byte("chunk"))
		require.NoError(t, err, i)
		assert.Equal(t, 5, n)
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, "chunkchunkchunk", rw.body.String())
	require.Len(t, rw.deadlines, 3, "one deadline per chunk")
	for i, d := range rw.deadlines {
		assert.WithinDuration(t, before.Add(90*time.Second), d, 2*time.Second, i)
		if i > 0 {
			assert.True(t, d.After(rw.deadlines[i-1]), "it moves forward: %d", i)
		}
	}

	// A write error is returned as it is.
	rw.failWith = errors.New("broken pipe")
	_, err := w.Write([]byte("x"))
	assert.EqualError(t, err, "broken pipe")

	// A writer that cannot set a deadline still writes: the bound is then not
	// applied here, and nothing fails.
	plain := httptest.NewRecorder()
	pw := &idleWriter{w: plain, rc: http.NewResponseController(plain), idle: time.Second}
	_, err = pw.Write([]byte("ok"))
	require.NoError(t, err)
	assert.Equal(t, "ok", plain.Body.String())
}

// A client that stops reading is cut after the idle timeout, with a real
// connection: the archive is cut short, the slot is given back, and the next
// download goes through.
func TestAClientThatStopsReadingIsCut(t *testing.T) {
	t.Parallel()
	var writeErr = make(chan error, 1)
	r := newRig(t, func(c *Config) {
		c.MaxArchives, c.MaxArchivesPerClient, c.WriteIdleTimeout = 2, 1, 300*time.Millisecond
	})
	chunk := make([]byte, 1<<20)
	r.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(
		func(ctx context.Context, _ domain.Principal, _, _, _ string, begin func(string) io.Writer) error {
			w := begin("a.tar.gz")
			// More than the socket buffers hold: the write blocks once the client
			// reads nothing, and fails when the idle deadline passes.
			for range 1024 {
				if _, err := w.Write(chunk); err != nil {
					writeErr <- err
					return err
				}
			}
			writeErr <- nil
			return nil
		}).Once()

	// The client takes the headers and reads no body.
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "GET "+Prefix+tarballPath+" HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)

	select {
	case err := <-writeErr:
		require.Error(t, err, "the server gave up on a client that reads nothing")
		assert.Contains(t, err.Error(), "i/o timeout")
	case <-time.After(15 * time.Second):
		t.Fatal("the stalled client was never cut")
	}

	// The slot came back: with a cap of one per client, another download starts.
	r.api.EXPECT().Tarball(mock.Anything, mock.Anything, "acme", "proj", "main", mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, _, _, _ string, begin func(string) io.Writer) error {
			_, err := begin("a.tar.gz").Write([]byte("fine"))
			return err
		}).Once()
	require.Eventually(t, func() bool {
		resp, body := r.do(t, http.MethodGet, tarballPath, "")
		return resp.StatusCode == http.StatusOK && body == "fine"
	}, 5*time.Second, 50*time.Millisecond)
}

// The credentials of a request are the forge's: Basic with any user, Bearer or
// token; none is anonymous and does not touch the failure limiter.
func TestCredentialsAndNotFound(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	p := domain.Root()
	r.auth.EXPECT().Authenticate(mock.Anything, "good").Return(p, nil)
	r.auth.EXPECT().Authenticate(mock.Anything, "bad").Return(domain.Principal{}, domain.ErrUnauthenticated)
	// A header that carries no usable token authenticates the empty secret.
	r.auth.EXPECT().Authenticate(mock.Anything, "").Return(domain.Principal{}, domain.ErrUnauthenticated)
	r.api.EXPECT().HashAlgorithm(mock.Anything, p, "acme", "proj").Return("sha1", nil)
	r.api.EXPECT().HashAlgorithm(mock.Anything, domain.Anonymous(), "acme", "proj").Return("sha1", nil)

	path := "/repos/acme/proj/hash-algorithm"
	for _, h := range []string{"Basic eC1hY2Nlc3MtdG9rZW46Z29vZA==", "Bearer good", "token good", "bearer good"} {
		resp, body := r.do(t, http.MethodGet, path, h)
		assert.Equal(t, http.StatusOK, resp.StatusCode, h)
		assert.JSONEq(t, `{"hash_algorithm":"sha1"}`, body)
	}
	resp, _ := r.do(t, http.MethodGet, path, "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "anonymous")
	for _, h := range []string{"Bearer bad", "Digest x", "Basic !!!", "Bearer"} {
		resp, body := r.do(t, http.MethodGet, path, h)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, h)
		assert.Contains(t, body, "Bad credentials")
	}

	// Not found is GitHub's body, for an unknown endpoint and for a use case that
	// says so, and a method other than GET and HEAD is 405.
	const gh = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	for _, p := range []string{"", "/", "/user", "/repos", "/repos/acme", "/repos//proj", "/repos/acme/proj/issues", "/repos/acme/proj/hash-algorithm/x"} {
		resp, body := r.do(t, http.MethodGet, p, "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		assert.JSONEq(t, gh, body, p)
	}
	r.api.EXPECT().GetRepo(mock.Anything, domain.Anonymous(), "acme", "gone").Return(nil, domain.ErrNotFound)
	resp, body := r.do(t, http.MethodGet, "/repos/acme/gone", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.JSONEq(t, gh, body)
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+Prefix+path, nil)
	post, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = post.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, post.StatusCode)
	assert.Equal(t, "GET, HEAD", post.Header.Get("Allow"))

	// A ref with slashes is passed whole.
	r.api.EXPECT().TarballName(mock.Anything, domain.Anonymous(), "acme", "proj", "feature/x/y").Return("n.tar.gz", nil)
	resp, _ = r.do(t, http.MethodHead, "/repos/acme/proj/tarball/feature/x/y", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
