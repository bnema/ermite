// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// clients are the generated Connect clients of one caller.
type clients struct {
	repo   ermiteconnect.RepoServiceClient
	git    ermiteconnect.GitServiceClient
	token  ermiteconnect.TokenServiceClient
	event  ermiteconnect.EventServiceClient
	server ermiteconnect.ServerServiceClient
	mr     ermiteconnect.MergeRequestServiceClient

	session ermiteconnect.SessionServiceClient
}

// bearerTransport adds the Authorization header of a secret, if any.
type bearerTransport struct {
	secret string
	// freshClient, when set, gives every request its own client address
	// through X-Forwarded-For (the harness trusts the loopback proxy), so that
	// many failed calls do not block the one client of the test.
	freshClient *atomic.Uint32
	next        http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.secret != "" {
		r.Header.Set("Authorization", "Bearer "+b.secret)
	}
	if b.freshClient != nil {
		n := b.freshClient.Add(1)
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", n%250+1))
	}
	return b.next.RoundTrip(r)
}

// newClients builds the clients of the API at base for a secret ("" is
// anonymous), speaking the Connect protocol with the given options.
func newClients(base, secret string, opts ...connect.ClientOption) *clients {
	hc := &http.Client{Transport: bearerTransport{secret: secret, next: http.DefaultTransport}}
	return newClientsOn(hc, base, opts...)
}

// newDisposableClients is newClients for a caller whose failures must not
// count against the others: every call comes from a new client address.
func newDisposableClients(base, secret string) *clients {
	hc := &http.Client{Transport: bearerTransport{secret: secret, freshClient: new(atomic.Uint32), next: http.DefaultTransport}}
	return newClientsOn(hc, base)
}

func newClientsOn(hc connect.HTTPClient, base string, opts ...connect.ClientOption) *clients {
	return &clients{
		repo:   ermiteconnect.NewRepoServiceClient(hc, base, opts...),
		git:    ermiteconnect.NewGitServiceClient(hc, base, opts...),
		token:  ermiteconnect.NewTokenServiceClient(hc, base, opts...),
		event:  ermiteconnect.NewEventServiceClient(hc, base, opts...),
		server: ermiteconnect.NewServerServiceClient(hc, base, opts...),
		mr:     ermiteconnect.NewMergeRequestServiceClient(hc, base, opts...),

		session: ermiteconnect.NewSessionServiceClient(hc, base, opts...),
	}
}

// api returns the clients of a caller of the harness' server.
func (h *harness) api(secret string, opts ...connect.ClientOption) *clients {
	return newClients(h.url("/-/api"), secret, opts...)
}

// codeName names the Connect code of err, "ok" for nil.
func codeName(err error) string {
	if err == nil {
		return "ok"
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code().String()
	}
	return "non-connect: " + err.Error()
}

// seed creates a repository through the use case as root and pushes one
// commit per file to its main branch with git.
func (h *harness) seed(name string, vis domain.Visibility, files ...string) {
	h.t.Helper()
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name(name), Visibility: vis})
	require.NoError(h.t, err)
	if len(files) == 0 {
		return
	}
	wt := h.workTree("main", files...)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/"+name+".git"), "main")
}

// writeFile commits a file with the given bytes to a work tree.
func (h *harness) writeFile(wt, path string, data []byte) {
	h.t.Helper()
	full := filepath.Join(wt, path)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(h.t, os.WriteFile(full, data, 0o644))
	h.mustGit(wt, "add", path)
	h.mustGit(wt, "commit", "-q", "-m", "add "+path)
}

// stream is a running Watch: its events, and how it ended.
type stream struct {
	events chan *pb.Event
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

// watch starts a Watch and collects its events. The stream is cancelled when
// the test ends.
func (h *harness) watch(c *clients, resume string, topics ...string) *stream {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	st, err := c.event.Watch(ctx, connect.NewRequest(&pb.WatchRequest{Topics: topics, ResumeAfter: resume}))
	require.NoError(h.t, err)
	s := &stream{events: make(chan *pb.Event, 256), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		defer close(s.events)
		for st.Receive() {
			s.events <- st.Msg().GetEvent()
		}
		s.err = st.Err()
		_ = st.Close()
	}()
	return s
}

// next returns the next event or fails after d.
func (s *stream) next(t *testing.T, d time.Duration) *pb.Event {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatalf("the stream ended: %v", s.err)
		}
		return ev
	case <-time.After(d):
		t.Fatalf("no event within %v", d)
	}
	return nil
}

// readBlob reads a whole GetBlob stream and returns its messages.
func readBlob(ctx context.Context, c *clients, req *pb.GetBlobRequest) ([]*pb.GetBlobResponse, error) {
	st, err := c.git.GetBlob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	var msgs []*pb.GetBlobResponse
	for st.Receive() {
		msgs = append(msgs, st.Msg())
	}
	if err := st.Err(); err != nil && !errors.Is(err, io.EOF) {
		return msgs, err
	}
	return msgs, nil
}
