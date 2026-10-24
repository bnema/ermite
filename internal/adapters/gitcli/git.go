// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gitcli implements the Git ports over the git binary. Every
// invocation runs in an isolated environment that is built here and never
// inherits the caller's.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures a Git adapter. The adapter never reads the process
// environment: the caller passes what it needs.
type Config struct {
	// Bin is the git executable: a name looked up in the process PATH, or a path.
	// It is required; the default belongs to the configuration.
	Bin string
	// DataDir is the data root (absolute). repos/, repos/.staging/ and
	// trash/ below it must exist; run/git-home/ is the HOME of git.
	DataDir string
	// Path is the PATH given to git, which it uses to find its helpers.
	Path string
	// MaxReceiveBytes is the largest pack a push may send
	// (receive.maxInputSize); zero leaves Git unlimited.
	MaxReceiveBytes int64
	// CatFileIdle is how long a repository's long-lived `git cat-file`
	// process may stay unused before it is stopped. Zero means 60 seconds.
	CatFileIdle time.Duration
	// MaxCatFiles bounds the long-lived cat-file processes. Zero means 16.
	MaxCatFiles int
	// MaxExec bounds the one-shot git processes that run at once (every
	// command but the long-lived cat-file processes and the blob streams);
	// callers beyond it wait for a slot until their context ends. Zero means
	// max(4, 2*GOMAXPROCS).
	MaxExec int
	// MaxBlobStreams bounds the open blob streams, each of which holds a git
	// process until its reader is closed. Streams beyond it are refused with
	// domain.ErrResourceExhausted rather than queued: a slow reader could hold
	// a slot for long. Zero means 32.
	MaxBlobStreams int
}

// Git is the git CLI adapter for ports.GitRepos.
type Git struct {
	bin     string
	dataDir string
	env     []string
	maxRecv int64

	mu      sync.Mutex // guards backend
	backend string     // git-http-backend, resolved on first use

	cat *catPool // long-lived cat-file processes of the read side

	onTrash atomic.Pointer[func(dir string)] // see OnTrash

	exec  chan struct{} // slots of the one-shot processes, see Config.MaxExec
	blobs chan struct{} // slots of the blob streams, see Config.MaxBlobStreams
}

// Defaults of the process bounds.
const defaultMaxBlobStreams = 32

func defaultMaxExec() int { return max(4, 2*runtime.GOMAXPROCS(0)) }

// New builds an adapter. It does not run git: call CheckVersion.
func New(cfg Config) *Git {
	env := buildEnv(cfg.DataDir, cfg.Path)
	maxExec, maxBlobs := cfg.MaxExec, cfg.MaxBlobStreams
	if maxExec <= 0 {
		maxExec = defaultMaxExec()
	}
	if maxBlobs <= 0 {
		maxBlobs = defaultMaxBlobStreams
	}
	return &Git{
		bin: cfg.Bin, dataDir: cfg.DataDir, env: env, maxRecv: cfg.MaxReceiveBytes,
		cat:   newCatPool(cfg.Bin, env, cfg.CatFileIdle, cfg.MaxCatFiles),
		exec:  make(chan struct{}, maxExec),
		blobs: make(chan struct{}, maxBlobs),
	}
}

// Close stops the long-lived git processes of the read side and waits for
// them, until ctx ends. Reads after Close fail with ErrClosed. Close is
// idempotent; the adapter's write side keeps working.
func (g *Git) Close(ctx context.Context) error { return g.cat.close(ctx) }

// buildEnv returns the complete environment of every git invocation. Nothing
// else is passed: no GIT_* variable, no user HOME, no locale.
func buildEnv(dataDir, path string) []string {
	return []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + filepath.Join(dataDir, "run", "git-home"),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"PATH=" + path,
	}
}

// commandTimeout bounds one git invocation when ctx has no deadline.
const commandTimeout = 2 * time.Minute

// acquireExec takes a slot of the one-shot processes, waiting until ctx ends.
// The wait counts against the deadline of ctx, so a saturated server answers a
// request with its deadline rather than hanging it.
func (g *Git) acquireExec(ctx context.Context) (release func(), err error) {
	select {
	case g.exec <- struct{}{}:
		return func() { <-g.exec }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run executes git with args in the isolated environment and returns stdout.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	return g.runInput(ctx, nil, args...)
}

// runInput is run with the given standard input; nil means none.
func (g *Git) runInput(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	out, err := g.runCapture(ctx, stdin, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// runCapture is runInput that also returns the standard output of a failed
// run.
func (g *Git) runCapture(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	return g.runCaptureEnv(ctx, nil, stdin, args...)
}

// runCaptureEnv is runCapture with extra environment variables added to the
// isolated environment.
func (g *Git) runCaptureEnv(ctx context.Context, extra []string, stdin io.Reader, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	release, err := g.acquireExec(ctx)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	defer release()
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Env = g.env
	if len(extra) > 0 {
		cmd.Env = append(slices.Clone(g.env), extra...)
	}
	cmd.Dir = g.dataDir
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && msg != "" {
			return stdout.String(), fmt.Errorf("git %s: %w: %s", subcommand(args), err, msg)
		}
		return stdout.String(), fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	return stdout.String(), nil
}
