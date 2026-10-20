// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gitpush implements bounded, non-forcing mirror pushes over git/ssh.
package gitpush

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/bnema/zerowrap"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Bin, Path, Home, KnownHosts, TempDir string
	Timeout                              time.Duration
	MaxOutput                            int
	AllowedProtocols                     string
	SSHOptions                           []string
	RepoDir                              func(domain.RepoName) string
}
type Pusher struct{ cfg Config }

var _ ports.MirrorPusher = (*Pusher)(nil)

func New(cfg Config) *Pusher {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 1 << 20
	}
	if cfg.AllowedProtocols == "" {
		cfg.AllowedProtocols = "ssh"
	}
	cfg.SSHOptions = append([]string(nil), cfg.SSHOptions...)
	return &Pusher{cfg: cfg}
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// validRefspec deliberately accepts only explicit same-name heads and tags.
// Empty sources, wildcards, force prefixes and revision expressions never reach git.
func validRefspec(s string) bool {
	a, b, ok := strings.Cut(s, ":")
	if !ok || a != b || !strings.HasPrefix(a, "refs/heads/") && !strings.HasPrefix(a, "refs/tags/") {
		return false
	}
	if strings.ContainsAny(a, "+*?[:\\^~ ") || strings.Contains(a, "..") || strings.Contains(a, "@{") || strings.Contains(a, "//") || strings.HasSuffix(a, "/") || strings.HasSuffix(a, ".") {
		return false
	}
	for _, part := range strings.Split(a, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return !strings.ContainsFunc(a, unicode.IsControl)
}

func (p *Pusher) Push(parent context.Context, req ports.MirrorPush) (result ports.MirrorPushResult, err error) {
	start := time.Now()
	defer func() { result.Duration = time.Since(start) }()
	internal := func(detail string) error {
		return &domain.MirrorError{Class: domain.MirrorClassInternal, Detail: detail}
	}
	if err := parent.Err(); err != nil {
		return result, err
	}
	if len(req.PrivateKey) != ed25519.PrivateKeySize {
		return result, internal("invalid mirror private key")
	}
	if p.cfg.RepoDir == nil || p.cfg.TempDir == "" || p.cfg.Home == "" || p.cfg.KnownHosts == "" {
		return result, internal("mirror transport directories are not configured")
	}
	if req.URL == "" || strings.HasPrefix(req.URL, "-") || strings.ContainsFunc(req.URL, unicode.IsControl) {
		return result, internal("invalid mirror target")
	}
	for _, ref := range req.Refspecs {
		if !validRefspec(ref) {
			return result, internal("invalid mirror refspec")
		}
	}
	if len(req.Refspecs) == 0 {
		return result, nil
	} // never let git select implicit refspecs
	ctx, cancel := context.WithTimeout(parent, p.cfg.Timeout)
	defer cancel()
	if err := os.MkdirAll(p.cfg.TempDir, 0700); err != nil {
		return result, internal("create mirror key directory")
	}
	if err := os.Chmod(p.cfg.TempDir, 0700); err != nil {
		return result, internal("protect mirror key directory")
	}
	dir, e := os.MkdirTemp(p.cfg.TempDir, "push-")
	if e != nil {
		return result, internal("create temporary mirror directory")
	}
	defer func() {
		// The directory holds the private key: a leftover is reported, and resetRunDir wipes it at
		// the next start.
		if e := os.RemoveAll(dir); e != nil {
			log := zerowrap.FromCtx(parent)
			log.Warn().Err(e).Msg("remove the temporary mirror key directory")
		}
	}()
	block, e := ssh.MarshalPrivateKey(req.PrivateKey, "")
	if e != nil {
		return result, internal("encode mirror private key")
	}
	f, e := os.CreateTemp(dir, "key-")
	if e != nil {
		return result, internal("create temporary mirror key")
	}
	keyPath := f.Name()
	_, e = f.Write(pem.EncodeToMemory(block))
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return result, internal("write temporary mirror key")
	}
	// -F /dev/null prevents ~/.ssh/config and system config from adding credentials
	// or weakening the required options. Required options precede caller extras.
	command := "ssh -F /dev/null -i " + quote(keyPath)
	for _, opt := range append([]string{"IdentitiesOnly=yes", "BatchMode=yes", "StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + p.cfg.KnownHosts, "ConnectTimeout=30", "ServerAliveInterval=15", "ServerAliveCountMax=3"}, p.cfg.SSHOptions...) {
		command += " -o " + quote(opt)
	}
	cleanup := max(min(2*time.Second, p.cfg.Timeout/8), time.Millisecond)
	runCtx, stop := context.WithTimeout(ctx, max(p.cfg.Timeout-cleanup, time.Millisecond))
	defer stop()
	stdout := &limitWriter{max: p.cfg.MaxOutput, cancel: stop}
	stderr := &limitWriter{max: p.cfg.MaxOutput, cancel: stop}
	args := []string{"-C", p.cfg.RepoDir(req.Repo), "push", "--porcelain", "--no-verify", "--atomic", "--", req.URL}
	args = append(args, req.Refspecs...)
	cmd := exec.Command(p.cfg.Bin, args...)
	cmd.Env = []string{"PATH=" + p.cfg.Path, "HOME=" + p.cfg.Home, "GIT_ALLOW_PROTOCOL=" + p.cfg.AllowedProtocols, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=" + command, "LC_ALL=C"}
	cmd.Dir = p.cfg.Home
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = cleanup
	runErr := run(runCtx, cmd)
	if parent.Err() != nil {
		return result, parent.Err()
	}
	rejected := false
	for line := range strings.SplitSeq(stdout.buf.String(), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || len(fields[0]) != 1 {
			continue
		}
		var status ports.MirrorRefStatus
		switch fields[0] {
		case " ", "*":
			status = ports.MirrorRefOK
		case "=":
			status = ports.MirrorRefUpToDate
		case "!":
			status = ports.MirrorRefRejected
			rejected = true
		default:
			continue
		}
		_, ref, _ := strings.Cut(fields[1], ":")
		result.Refs = append(result.Refs, ports.MirrorRefOutcome{Ref: ref, Status: status, Reason: safeDetail(fields[2], req.URL)})
	}
	if stdout.exceeded || stderr.exceeded {
		return result, &domain.MirrorError{Class: domain.MirrorClassTransient, Detail: "mirror output limit exceeded"}
	}
	if runCtx.Err() != nil {
		return result, &domain.MirrorError{Class: domain.MirrorClassTransient, Detail: "mirror push timed out"}
	}
	if runErr != nil || rejected {
		// The remote's own messages ("remote: ...") are not ssh/git diagnostics: a hosting
		// message must not pick the failure class. They stay in the detail.
		class := domain.ClassifyMirrorOutput(withoutRemoteLines(stderr.buf.String())+"\n"+stdout.buf.String(), runErr, false)
		if rejected && class == domain.MirrorClassNone {
			class = domain.MirrorClassDiverged
		}
		var execErr *exec.Error
		if errors.As(runErr, &execErr) || localGitError(stderr.buf.String()) {
			class = domain.MirrorClassInternal
		}
		detail := safeDetail(stderr.buf.String(), req.URL)
		if detail == "" {
			detail = "git push failed"
		}
		return result, &domain.MirrorError{Class: class, Detail: detail}
	}
	return result, nil
}

// withoutRemoteLines drops the lines the remote sent through git ("remote: ...").
func withoutRemoteLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "remote:") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func localGitError(stderr string) bool {
	for _, marker := range []string{"cannot change to", "not a git repository", "src refspec", "bad object", "unable to read"} {
		if strings.Contains(strings.ToLower(stderr), marker) {
			return true
		}
	}
	return false
}

// maxDetail caps a detail in bytes; a cut inside a multibyte character is repaired.
const maxDetail = 4096

// safeDetail removes the complete target before taking a bounded stderr tail.
// SSH diagnostics may repeat the user separately, so suppress user@ tokens too.
func safeDetail(s, url string) string {
	s = strings.ReplaceAll(s, url, "[mirror target]")
	words := strings.Fields(s)
	for i, w := range words {
		if strings.Contains(w, "@") || strings.Contains(w, "://") {
			words[i] = "[redacted]"
		}
	}
	s = strings.Join(words, " ")
	if len(s) > maxDetail {
		s = s[len(s)-maxDetail:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) { // do not start inside a character
			s = s[1:]
		}
	}
	return strings.ToValidUTF8(s, "?")
}

type limitWriter struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.max-w.buf.Len() {
		w.exceeded = true
		w.cancel()
		return 0, errors.New("output limit reached")
	}
	return w.buf.Write(p)
}
