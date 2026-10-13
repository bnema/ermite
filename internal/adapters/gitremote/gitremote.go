// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gitremote lists the refs of repositories on github.com with
// `git ls-remote`, for the resolution of actions (ports.GitRemote). It sends no
// credentials, never prompts, and bounds the time and the output of git.
package gitremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Defaults of the bounds.
const (
	// DefaultTimeout is how long one ls-remote may take.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxOutput is the most bytes of ls-remote output read: a repository
	// with 100,000 tags lists about 8 MB. More is refused as unavailable.
	DefaultMaxOutput = 16 << 20
	// DefaultBaseURL is where the repositories are.
	DefaultBaseURL = "https://github.com"
)

// Config configures a Remote.
type Config struct {
	// Bin is the git executable. Required.
	Bin string
	// Path is the PATH given to git, which it uses to find its helpers.
	Path string
	// Home is the HOME of git; it should be an empty directory the process
	// owns, so that no user configuration or credential is read.
	Home string
	// BaseURL, Timeout and MaxOutput default to the constants above; tests
	// replace them.
	BaseURL   string
	Timeout   time.Duration
	MaxOutput int
	// AllowedProtocols is GIT_ALLOW_PROTOCOL: "https" by default.
	AllowedProtocols string
}

// Remote implements ports.GitRemote over the git binary.
type Remote struct{ cfg Config }

var _ ports.GitRemote = (*Remote)(nil)

// New builds a Remote.
func New(cfg Config) *Remote {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = DefaultMaxOutput
	}
	if cfg.AllowedProtocols == "" {
		cfg.AllowedProtocols = "https"
	}
	return &Remote{cfg: cfg}
}

// validSegment reports whether s can be an owner or a repository name on
// GitHub: letters, digits, '.', '_' and '-', not starting with '-' or '.'.
func validSegment(s string) bool {
	if s == "" || len(s) > 100 || s[0] == '-' || s[0] == '.' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// limitWriter keeps at most max bytes and records that more came.
type limitWriter struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("output limit reached")
	}
	return w.buf.Write(p)
}

// ListRefs lists the branches and tags of owner/repo, with the peeled commit
// of the annotated tags.
func (r *Remote) ListRefs(ctx context.Context, owner, repo string) ([]ports.RemoteRef, error) {
	if !validSegment(owner) || !validSegment(repo) {
		return nil, domain.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	out := &limitWriter{max: r.cfg.MaxOutput, cancel: cancel}
	var stderr bytes.Buffer
	url := strings.TrimSuffix(r.cfg.BaseURL, "/") + "/" + owner + "/" + repo
	cmd := exec.CommandContext(ctx, r.cfg.Bin,
		"-c", "credential.helper=", "-c", "core.askPass=", "-c", "http.followRedirects=false",
		"ls-remote", "--", url, "refs/heads/*", "refs/tags/*")
	cmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + r.cfg.Home,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never",
		"GIT_ALLOW_PROTOCOL=" + r.cfg.AllowedProtocols, "LC_ALL=C", "PATH=" + r.cfg.Path,
	}
	cmd.Dir = r.cfg.Home
	cmd.Stdout, cmd.Stderr = out, &limitedBuffer{b: &stderr, max: 4096}
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	switch {
	case out.exceeded:
		return nil, fmt.Errorf("%w: git ls-remote %s/%s listed more than %d bytes", domain.ErrUnavailable, owner, repo, r.cfg.MaxOutput)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("%w: git ls-remote %s/%s took more than %s", domain.ErrUnavailable, owner, repo, r.cfg.Timeout)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		msg := stderr.String()
		if notFound(msg) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("%w: git ls-remote %s/%s: %v", domain.ErrUnavailable, owner, repo, firstLine(msg))
	}
	return parse(out.buf.String()), nil
}

// notFound recognises what git says when github.com answers 404 or asks for
// credentials, which it does for a repository that does not exist as for a
// private one, and the prompt being disabled.
func notFound(stderr string) bool {
	for _, s := range []string{"Repository not found", "terminal prompts disabled", "could not read Username", "Authentication failed", "returned error: 404"} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// limitedBuffer keeps the first max bytes written and drops the rest.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// parse reads the lines "<id>\t<ref>" of ls-remote. A "<ref>^{}" line gives
// the peeled commit of the annotated tag listed before it.
func parse(out string) []ports.RemoteRef {
	var refs []ports.RemoteRef
	index := map[string]int{}
	for line := range strings.SplitSeq(out, "\n") {
		id, name, ok := strings.Cut(line, "\t")
		if !ok || len(id) != domain.ObjectIDLen {
			continue
		}
		if base, peeled := strings.CutSuffix(name, "^{}"); peeled {
			if i, ok := index[base]; ok {
				refs[i].Peeled = id
			}
			continue
		}
		index[name] = len(refs)
		refs = append(refs, ports.RemoteRef{Name: name, ID: id})
	}
	return refs
}
