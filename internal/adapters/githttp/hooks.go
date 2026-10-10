// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// PushHooks is what the hook socket asks of the use cases. app.Pushes fits.
type PushHooks interface {
	// Check decides a push before Git applies it; an error matching
	// domain.ErrRejected refuses it, any other error refuses it as a failure.
	Check(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error
	// Record handles an accepted push.
	Record(ctx context.Context, p domain.Principal, name domain.RepoName, updates []domain.RefUpdate) error
}

// readTimeout bounds how long a connection may take to send its request line.
const readTimeout = 30 * time.Second

type hookEntry struct {
	repo      domain.RepoName
	principal domain.Principal
}

// HookServer registers the per-request hook tokens and serves the Unix
// socket the hooks call back on. It implements HookTokens.
type HookServer struct {
	path  string
	hooks PushHooks

	mu     sync.RWMutex
	tokens map[string]hookEntry

	connMu sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

var _ HookTokens = (*HookServer)(nil)

// NewHookServer builds a server for the socket at path. Call Serve to listen.
func NewHookServer(path string, hooks PushHooks) *HookServer {
	return &HookServer{path: path, hooks: hooks, tokens: make(map[string]hookEntry), conns: make(map[net.Conn]struct{})}
}

// Socket returns the path of the socket.
func (s *HookServer) Socket() string { return s.path }

// Register binds a fresh random token to the repository and principal of a
// push. The returned release revokes it.
func (s *HookServer) Register(repo domain.RepoName, p domain.Principal) (string, func()) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// No entropy: hand out a token that matches nothing, so the hooks fail closed.
		return "unavailable", func() {}
	}
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	s.tokens[token] = hookEntry{repo: repo, principal: p}
	s.mu.Unlock()
	return token, func() {
		s.mu.Lock()
		delete(s.tokens, token)
		s.mu.Unlock()
	}
}

func (s *HookServer) lookup(token string) (hookEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tokens[token]
	return e, ok
}

// Listen creates the socket with mode 0600 (replacing a stale file) and
// returns once it accepts connections. Serve then handles them.
func (s *HookServer) Listen() error {
	// sun_path holds 108 bytes on Linux, 104 on the BSDs and macOS.
	if len(s.path) >= 100 {
		return fmt.Errorf("hook socket path is too long for a Unix socket (%d bytes, limit about 100): %s; use a shorter %s", len(s.path), s.path, "ERMITE_DATA_DIR")
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale hook socket: %w", err)
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listen on hook socket %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod hook socket: %w", err)
	}
	s.connMu.Lock()
	s.ln = ln
	s.connMu.Unlock()
	return nil
}

// Serve accepts connections until ctx ends or Close is called, then waits for
// the requests in progress. The logger of ctx is used throughout. It returns
// nil after a clean stop.
func (s *HookServer) Serve(ctx context.Context) error {
	s.connMu.Lock()
	ln := s.ln
	s.connMu.Unlock()
	if ln == nil {
		return errors.New("hook server is not listening")
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("socket", s.path).Msg("hook socket listening")

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
		}
		_ = ln.Close()
	}()
	defer close(stopped)

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.wg.Wait()
			_ = os.Remove(s.path)
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept on hook socket: %w", err)
		}
		s.connMu.Lock()
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				_ = conn.Close()
				s.connMu.Lock()
				delete(s.conns, conn)
				s.connMu.Unlock()
			}()
			s.handle(ctx, conn)
		}()
	}
}

// Close stops listening and drops the connections in progress.
func (s *HookServer) Close() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
}

// handle serves one connection: one request line, one reply line.
func (s *HookServer) handle(ctx context.Context, conn net.Conn) {
	log := zerowrap.FromCtx(ctx)
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	line, err := readLine(conn)
	if err != nil {
		log.Warn().Err(err).Msg("read hook request")
		s.reply(ctx, conn, HookReply{Message: "ermite: malformed hook request"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var req HookRequest
	if err := json.Unmarshal(line, &req); err != nil {
		log.Warn().Err(err).Msg("decode hook request")
		s.reply(ctx, conn, HookReply{Message: "ermite: malformed hook request"})
		return
	}
	updates, err := req.refUpdates()
	if err != nil {
		log.Warn().Err(err).Str("hook", req.Hook).Msg("invalid hook request")
		s.reply(ctx, conn, HookReply{Message: "ermite: invalid hook request"})
		return
	}
	entry, ok := s.lookup(req.Token)
	if !ok {
		log.Warn().Str("hook", req.Hook).Msg("hook request with an unknown or expired token")
		s.reply(ctx, conn, HookReply{Message: "ermite: unknown or expired hook token"})
		return
	}

	rlog := log.WithField("repo", entry.repo.String())
	switch req.Hook {
	case HookPreReceive:
		err := s.hooks.Check(ctx, entry.principal, entry.repo, updates)
		switch {
		case err == nil:
			s.reply(ctx, conn, HookReply{OK: true})
		case errors.Is(err, domain.ErrRejected):
			rlog.Warn().Err(err).Int("refs", len(updates)).Msg("push rejected by pre-receive")
			s.reply(ctx, conn, HookReply{Message: "ermite: push rejected: " + err.Error()})
		default:
			rlog.Error().Err(err).Msg("pre-receive check failed")
			s.reply(ctx, conn, HookReply{Message: "ermite: push refused: internal error"})
		}
	case HookPostReceive:
		if err := s.hooks.Record(ctx, entry.principal, entry.repo, updates); err != nil {
			rlog.Error().Err(err).Msg("post-receive recording failed")
			s.reply(ctx, conn, HookReply{OK: true, Message: "ermite: the push succeeded but recording it failed"})
			return
		}
		s.reply(ctx, conn, HookReply{OK: true})
	}
}

func (s *HookServer) reply(ctx context.Context, conn net.Conn, r HookReply) {
	b, err := encodeLine(r)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(readTimeout))
	if _, err := conn.Write(b); err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("write hook reply")
	}
}

// readLine reads one line of at most maxHookMessage bytes.
func readLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, maxHookMessage+1), 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return nil, err
	}
	if len(line) > maxHookMessage {
		return nil, errors.New("hook message too long")
	}
	return line, nil
}
