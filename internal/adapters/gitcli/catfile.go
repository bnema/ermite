// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"
)

// ErrClosed is returned by reads after Close.
var ErrClosed = errors.New("gitcli: reader is closed")

// errObjectMissing is what a cat-file lookup answers for an unknown name.
var errObjectMissing = errors.New("object missing")

// procError is an I/O or protocol failure of a cat-file process: the process
// is dropped and the request may be retried on a fresh one.
type procError struct{ err error }

func (e *procError) Error() string { return "git cat-file: " + e.err.Error() }
func (e *procError) Unwrap() error { return e.err }

// Defaults of the process pool.
const (
	defaultIdleTimeout  = 60 * time.Second
	defaultMaxProcesses = 16
)

// catPool keeps one long-lived `git cat-file --batch-command` per repository.
//
//   - Access to one process is serialized: one request in flight at a time.
//   - A process nobody used for the idle timeout is killed.
//   - At most max processes are kept. Starting another one first evicts the
//     least recently used idle process; a process that is in use is never
//     evicted, so the bound is soft when more than max repositories are read
//     at the same moment.
//   - A process that fails, or whose request was cancelled (its output is then
//     in an unknown state), is killed and dropped; the next request starts a
//     fresh one, and a request that hit a dead process is retried once.
type catPool struct {
	bin  string
	env  []string
	idle time.Duration
	max  int

	mu      sync.Mutex
	procs   map[string]*catProc
	streams map[*blobStream]struct{} // one-shot blob processes, see OpenBlob
	closed  bool
	wg      sync.WaitGroup // reapers of started processes
}

// catProc is one cat-file process. Fields below the line are guarded by
// catPool.mu.
type catProc struct {
	dir    string
	log    zerowrap.Logger // of the request that started the process
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *tailBuffer
	sem    chan struct{} // capacity 1: the request in flight

	broken bool // set by the holder of sem on a protocol error

	users    int // requests waiting for or holding sem
	lastUsed time.Time
	timer    *time.Timer
	gen      uint64
	dead     bool
}

func newCatPool(bin string, env []string, idle time.Duration, maxProcs int) *catPool {
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	if maxProcs <= 0 {
		maxProcs = defaultMaxProcesses
	}
	return &catPool{
		bin: bin, env: env, idle: idle, max: maxProcs,
		procs:   make(map[string]*catProc),
		streams: make(map[*blobStream]struct{}),
	}
}

// do runs fn with exclusive use of the cat-file process of the repository in
// dir, starting it if needed. fn must not keep the connection after returning.
func (p *catPool) do(ctx context.Context, dir string, fn func(*catConn) error) error {
	for attempt := 0; ; attempt++ {
		cp, err := p.acquire(ctx, dir)
		if err != nil {
			return err
		}
		err = p.run(ctx, cp, fn)
		var pe *procError
		if !errors.As(err, &pe) {
			return err
		}
		switch {
		case p.isClosed():
			return ErrClosed
		case attempt == 0 && ctx.Err() == nil:
			continue // the process died while idle: retry once on a fresh one
		}
		return err
	}
}

func (p *catPool) run(ctx context.Context, cp *catProc, fn func(*catConn) error) error {
	// A cancelled request leaves the process mid-answer: kill it.
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.dropLocked(cp)
		p.mu.Unlock()
	})
	err := fn(&catConn{cp: cp})
	cancelled := !stop()
	if cancelled && err != nil {
		err = ctx.Err()
	}
	p.release(cp, cp.broken || cancelled)
	<-cp.sem
	return err
}

func (p *catPool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *catPool) acquire(ctx context.Context, dir string) (*catProc, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		cp := p.procs[dir]
		if cp == nil {
			var err error
			if cp, err = p.startLocked(ctx, dir); err != nil {
				p.mu.Unlock()
				return nil, err
			}
		}
		cp.users++
		cp.gen++
		if cp.timer != nil {
			cp.timer.Stop()
			cp.timer = nil
		}
		p.mu.Unlock()

		select {
		case cp.sem <- struct{}{}:
		case <-ctx.Done():
			p.release(cp, false)
			return nil, ctx.Err()
		}
		p.mu.Lock()
		dead := cp.dead
		p.mu.Unlock()
		if !dead {
			return cp, nil
		}
		// Dropped while we waited for our turn.
		<-cp.sem
		p.release(cp, false)
	}
}

// release ends one use of cp. A broken process is dropped; an unused one
// starts its idle countdown.
func (p *catPool) release(cp *catProc, broken bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp.users--
	if broken {
		p.dropLocked(cp)
		return
	}
	if cp.users == 0 && !cp.dead && !p.closed {
		cp.lastUsed = time.Now()
		gen := cp.gen
		cp.timer = time.AfterFunc(p.idle, func() { p.expire(cp, gen) })
	}
}

// expire kills cp if it has not been used since the timer was armed.
func (p *catPool) expire(cp *catProc, gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cp.gen != gen || cp.users != 0 || cp.dead {
		return
	}
	cp.log.Debug().Str("dir", cp.dir).Msg("cat-file idle, stopping")
	p.dropLocked(cp)
}

// dropLocked removes cp from the pool and kills it. The reaper goroutine
// collects the process.
func (p *catPool) dropLocked(cp *catProc) {
	if cp.dead {
		return
	}
	cp.dead = true
	if p.procs[cp.dir] == cp {
		delete(p.procs, cp.dir)
	}
	if cp.timer != nil {
		cp.timer.Stop()
		cp.timer = nil
	}
	_ = cp.in.Close()
	_ = cp.cmd.Process.Kill()
}

func (p *catPool) startLocked(ctx context.Context, dir string) (*catProc, error) {
	for len(p.procs) >= p.max {
		var victim *catProc
		for _, c := range p.procs {
			if c.users == 0 && (victim == nil || c.lastUsed.Before(victim.lastUsed)) {
				victim = c
			}
		}
		if victim == nil {
			break // all in use: exceed the bound rather than block
		}
		victim.log.Debug().Str("dir", victim.dir).Msg("cat-file pool full, evicting the least recently used")
		p.dropLocked(victim)
	}
	cmd := exec.Command(p.bin, "--git-dir="+dir, "--no-replace-objects", "--no-optional-locks", "cat-file", "--batch-command="+catFormat, "-Z")
	cmd.Env = p.env
	cmd.Dir = filepath.Dir(dir)
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}
	cp := &catProc{
		dir: dir, log: zerowrap.FromCtx(ctx), cmd: cmd, in: in, out: bufio.NewReaderSize(stdout, 64*1024),
		stderr: stderr, sem: make(chan struct{}, 1), lastUsed: time.Now(),
	}
	p.procs[dir] = cp
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = cmd.Wait()
	}()
	cp.log.Debug().Str("dir", dir).Int("pid", cmd.Process.Pid).Msg("cat-file started")
	return cp, nil
}

// close kills every process and waits, until ctx ends, for them to be
// collected. Later requests fail with ErrClosed.
func (p *catPool) close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	for _, cp := range p.procs {
		p.dropLocked(cp)
	}
	streams := make([]*blobStream, 0, len(p.streams))
	for s := range p.streams {
		streams = append(streams, s)
	}
	p.mu.Unlock()
	for _, s := range streams {
		s.kill()
	}

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for git cat-file processes: %w", ctx.Err())
	}
}

// tailBuffer keeps the first max bytes written to it, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *tailBuffer) reset() {
	b.mu.Lock()
	b.buf.Reset()
	b.mu.Unlock()
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.buf.String())
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}

// catInfo is the header git prints for an object.
type catInfo struct {
	ID   string
	Type string
	Size int64
	// Mode is the Git mode of a "<tree-ish>:<path>" object, 0 otherwise.
	Mode uint32
}

// catFormat is the answer format of `info` and `contents`.
const catFormat = "%(objectname) %(objecttype) %(objectsize) %(objectmode)"

// catConn is exclusive use of one cat-file process. Names are objects
// expressions such as "<commit>:<path>" or "<ref>^{commit}"; they must not
// contain NUL or line breaks.
type catConn struct{ cp *catProc }

func (c *catConn) fail(err error) error {
	c.cp.broken = true
	if s := c.cp.stderr.String(); s != "" {
		err = fmt.Errorf("%w: %s", err, s)
	}
	return &procError{err: err}
}

func (c *catConn) send(cmd, name string) error {
	if strings.ContainsAny(name, "\x00\n") {
		return fmt.Errorf("invalid object name %q", name)
	}
	c.cp.stderr.reset()
	if _, err := io.WriteString(c.cp.in, cmd+" "+name+"\x00"); err != nil {
		return c.fail(err)
	}
	return nil
}

// header reads the answer line of a request: the object header, or
// errObjectMissing.
func (c *catConn) header(name string) (catInfo, error) {
	line, err := c.cp.out.ReadString(0)
	if err != nil {
		return catInfo{}, c.fail(err)
	}
	line = strings.TrimSuffix(line, "\x00")
	if strings.HasSuffix(line, " missing") || strings.HasSuffix(line, " ambiguous") {
		return catInfo{}, fmt.Errorf("%w: %s", errObjectMissing, name)
	}
	f := strings.Split(line, " ")
	if len(f) != 4 || len(f[0]) != 40 {
		return catInfo{}, c.fail(fmt.Errorf("unexpected answer %q", truncateText(line)))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || size < 0 {
		return catInfo{}, c.fail(fmt.Errorf("unexpected size in %q", truncateText(line)))
	}
	var mode uint64
	if f[3] != "" {
		if mode, err = strconv.ParseUint(f[3], 8, 32); err != nil {
			return catInfo{}, c.fail(fmt.Errorf("unexpected mode in %q", truncateText(line)))
		}
	}
	return catInfo{ID: f[0], Type: f[1], Size: size, Mode: uint32(mode)}, nil
}

// info returns the header of an object without its content.
func (c *catConn) info(name string) (catInfo, error) {
	if err := c.send("info", name); err != nil {
		return catInfo{}, err
	}
	return c.header(name)
}

// contents returns an object with its content. It fails if the object is
// larger than max, after which the process is dropped.
func (c *catConn) contents(name string, max int64) (catInfo, []byte, error) {
	if err := c.send("contents", name); err != nil {
		return catInfo{}, nil, err
	}
	h, err := c.header(name)
	if err != nil {
		return catInfo{}, nil, err
	}
	if h.Size > max {
		c.cp.broken = true
		return catInfo{}, nil, fmt.Errorf("object %s is %d bytes, more than %d", name, h.Size, max)
	}
	data := make([]byte, h.Size)
	if _, err := io.ReadFull(c.cp.out, data); err != nil {
		return catInfo{}, nil, c.fail(err)
	}
	if b, err := c.cp.out.ReadByte(); err != nil || b != 0 {
		return catInfo{}, nil, c.fail(fmt.Errorf("missing terminator after %s", name))
	}
	return h, data, nil
}

func truncateText(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// addStream registers a one-shot blob process so that close kills it.
func (p *catPool) addStream(s *blobStream) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.streams[s] = struct{}{}
	return nil
}

func (p *catPool) removeStream(s *blobStream) {
	p.mu.Lock()
	delete(p.streams, s)
	p.mu.Unlock()
}
