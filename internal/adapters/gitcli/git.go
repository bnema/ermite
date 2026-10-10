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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
}

// New builds an adapter. It does not run git: call CheckVersion.
func New(cfg Config) *Git {
	env := buildEnv(cfg.DataDir, cfg.Path)
	return &Git{
		bin: cfg.Bin, dataDir: cfg.DataDir, env: env, maxRecv: cfg.MaxReceiveBytes,
		cat: newCatPool(cfg.Bin, env, cfg.CatFileIdle, cfg.MaxCatFiles),
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

// run executes git with args in the isolated environment and returns stdout.
func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Env = g.env
	cmd.Dir = g.dataDir
	cmd.WaitDelay = 5 * time.Second
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
			return "", fmt.Errorf("git %s: %w: %s", subcommand(args), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	return stdout.String(), nil
}
