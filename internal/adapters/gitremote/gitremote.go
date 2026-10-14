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
	// DefaultTimeout is how long ListRefs may take in all, its attempts
	// included. It is below the 30 s the runner gives a call.
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
	// Proxy is the proxy environment forwarded to git, as NAME=value pairs: see
	// ProxyEnv. Nothing else of the environment of ermite reaches git.
	Proxy []string
}

// proxyVars are the variables git (through libcurl) reads to reach github.com
// through a proxy. They are the only ones of the environment of ermite that
// the ls-remote processes get.
var proxyVars = []string{
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "ALL_PROXY",
	"https_proxy", "http_proxy", "no_proxy", "all_proxy",
}

// ProxyEnv picks the proxy variables from an environment lookup (os.Getenv),
// for Config.Proxy. Empty ones are left out.
func ProxyEnv(get func(string) string) []string {
	var out []string
	for _, name := range proxyVars {
		if v := get(name); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
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

// attempts is how many times a ls-remote that timed out is run: a connection
// that github.com never answered (a lost SYN) is often not repeated by a new
// one, which may also reach another address.
const attempts = 2

// ListRefs lists the branches and tags of owner/repo, with the peeled commit
// of the annotated tags. Timeout bounds the whole call, attempts and clean-up
// included: the time to clean up after an attempt (kill, WaitDelay) is
// reserved before it runs, and a ls-remote that timed out is run once more
// with what is left, at most half. The cancellation of ctx is returned as it
// is, not retried.
func (r *Remote) ListRefs(ctx context.Context, owner, repo string) ([]ports.RemoteRef, error) {
	if !validSegment(owner) || !validSegment(repo) {
		return nil, domain.ErrNotFound
	}
	deadline := time.Now().Add(r.cfg.Timeout)
	cleanup := max(min(2*time.Second, r.cfg.Timeout/8), time.Millisecond)
	ran := 0
	for ran < attempts {
		run := min(r.cfg.Timeout/attempts, time.Until(deadline)-cleanup)
		if run <= 0 {
			break
		}
		ran++
		refs, timedOut, err := r.listOnce(ctx, owner, repo, run, cleanup)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller gave up
		case !timedOut:
			return refs, err
		}
	}
	return nil, fmt.Errorf("%w: git ls-remote %s/%s did not answer within %s (%d attempts)", domain.ErrUnavailable, owner, repo, r.cfg.Timeout, ran)
}

// listOnce runs one git ls-remote for at most d, then cleans up for at most
// cleanup. timedOut tells that d ran out and parent did not.
func (r *Remote) listOnce(parent context.Context, owner, repo string, d, cleanup time.Duration) (refs []ports.RemoteRef, timedOut bool, _ error) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	out := &limitWriter{max: r.cfg.MaxOutput, cancel: cancel}
	var stderr bytes.Buffer
	url := strings.TrimSuffix(r.cfg.BaseURL, "/") + "/" + owner + "/" + repo
	cmd := exec.Command(r.cfg.Bin,
		"-c", "credential.helper=", "-c", "core.askPass=", "-c", "http.followRedirects=false",
		"ls-remote", "--", url, "refs/heads/*", "refs/tags/*")
	cmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + r.cfg.Home,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never",
		"GIT_ALLOW_PROTOCOL=" + r.cfg.AllowedProtocols, "LC_ALL=C", "PATH=" + r.cfg.Path,
	}
	cmd.Env = append(cmd.Env, r.cfg.Proxy...)
	cmd.Dir = r.cfg.Home
	cmd.Stdout, cmd.Stderr = out, &limitedBuffer{b: &stderr, max: 4096}
	// The backstop for a helper that escaped the kill and holds the pipes: the
	// time reserved for it by ListRefs.
	cmd.WaitDelay = cleanup
	err := run(ctx, cmd)
	switch {
	case out.exceeded:
		return nil, false, fmt.Errorf("%w: git ls-remote %s/%s listed more than %d bytes", domain.ErrUnavailable, owner, repo, r.cfg.MaxOutput)
	case err != nil && ctx.Err() != nil:
		return nil, true, nil
	case err != nil:
		msg := stderr.String()
		if notFound(msg) {
			return nil, false, domain.ErrNotFound
		}
		if firstLine(msg) == "" {
			msg = err.Error()
		}
		return nil, false, fmt.Errorf("%w: git ls-remote %s/%s: %v", domain.ErrUnavailable, owner, repo, firstLine(msg))
	}
	return parse(out.buf.String()), false, nil
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
