// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

// need skips the test when a tool is missing, except in CI where it must exist.
func need(t *testing.T, tool string) string {
	t.Helper()
	path, err := exec.LookPath(tool)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s not found: CI must have it", tool)
		}
		t.Skipf("%s not found: install it to check signatures against real tools", tool)
	}
	return path
}

// run runs a command and returns its stdout; a failure fails the test with
// its output.
func run(t *testing.T, dir string, env []string, stdin string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), "%s %v: %s", name, args, stderr.String())
	return stdout.String()
}

// signedCommit is a commit as the verifier sees it.
type signedCommit struct {
	payload, signature []byte
	signedAt           int64
	email              string
}

// newRepo makes an empty Git repository with a committer identity.
func newRepo(t *testing.T) string {
	t.Helper()
	need(t, "git")
	dir := t.TempDir()
	run(t, dir, nil, "", "git", "init", "-q")
	return dir
}

// commit makes a signed empty commit in dir, with extra git config, and
// returns it split the way ermite reads it.
func commit(t *testing.T, dir, email string, env []string, config ...string) signedCommit {
	t.Helper()
	now := time.Now().Unix()
	args := []string{"-c", "user.name=Test", "-c", "user.email=" + email}
	for _, c := range config {
		args = append(args, "-c", c)
	}
	args = append(args, "commit", "-q", "--allow-empty", "-S", "-m", "signed commit")
	env = append(env,
		"GIT_COMMITTER_DATE="+strconv.FormatInt(now, 10)+" +0000",
		"GIT_AUTHOR_DATE="+strconv.FormatInt(now, 10)+" +0000",
	)
	run(t, dir, env, "", "git", args...)
	raw := run(t, dir, nil, "", "git", "cat-file", "commit", "HEAD")
	payload, signature, ok := domain.SplitCommitSignature([]byte(raw))
	require.True(t, ok, "the commit must be signed:\n%s", raw)
	return signedCommit{payload: payload, signature: signature, signedAt: now, email: email}
}

// sshKey makes an SSH key pair and returns the private key path and the
// public key line.
func sshKey(t *testing.T, typ string, extra ...string) (private, public string) {
	t.Helper()
	need(t, "ssh-keygen")
	private = filepath.Join(t.TempDir(), "key")
	args := append([]string{"-q", "-t", typ, "-N", "", "-C", "test comment", "-f", private}, extra...)
	run(t, "", nil, "", "ssh-keygen", args...)
	pub, err := os.ReadFile(private + ".pub")
	require.NoError(t, err)
	return private, strings.TrimSpace(string(pub))
}

// sshCommit makes a commit signed by an SSH private key.
func sshCommit(t *testing.T, private, email string) signedCommit {
	t.Helper()
	return commit(t, newRepo(t), email, nil, "gpg.format=ssh", "user.signingkey="+private)
}

// gpgHome is a throw-away GnuPG home directory. It lives in /tmp: the agent
// socket path is limited to 108 bytes.
type gpgHome struct {
	t    *testing.T
	home string
}

func newGPGHome(t *testing.T) gpgHome {
	t.Helper()
	need(t, "gpg")
	home, err := os.MkdirTemp("", "sigverify-gpg")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(home, 0o700))
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	})
	return gpgHome{t: t, home: home}
}

func (g gpgHome) gpg(stdin string, args ...string) string {
	g.t.Helper()
	return run(g.t, "", nil, stdin, "gpg", append([]string{"--homedir", g.home, "--batch", "--yes", "--no-tty"}, args...)...)
}

// generate makes a key without passphrase and returns its fingerprint. expire
// is a gpg expiration like "never" or "2d".
func (g gpgHome) generate(uid, algo, expire string) string {
	g.t.Helper()
	g.gpg("", "--passphrase", "", "--quick-gen-key", uid, algo, "sign", expire)
	return g.fingerprint(uid)
}

// fingerprint returns the primary key fingerprint of the key of uid.
func (g gpgHome) fingerprint(uid string) string {
	g.t.Helper()
	out := g.gpg("", "--with-colons", "--fingerprint", "--list-keys", uid)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "fpr:") {
			return strings.Split(l, ":")[9]
		}
	}
	g.t.Fatalf("no fingerprint in %q", out)
	return ""
}

// public exports the public keys, in one armor block.
func (g gpgHome) public(fpr ...string) string {
	g.t.Helper()
	return g.gpg("", append([]string{"--armor", "--export"}, fpr...)...)
}

func (g gpgHome) private(fpr string) string {
	g.t.Helper()
	return g.gpg("", "--armor", "--export-secret-keys", fpr)
}

// revoke publishes a revocation certificate of the key in the keyring.
func (g gpgHome) revoke(fpr string) {
	g.t.Helper()
	cert := filepath.Join(g.home, "revoke.asc")
	// --gen-revoke asks questions: it cannot run in batch mode.
	run(g.t, "", nil, "y\n0\n\ny\n", "gpg", "--homedir", g.home, "--no-tty", "--yes", "--command-fd", "0", "--output", cert, "--gen-revoke", fpr)
	g.gpg("", "--import", cert)
}

// detachSign signs data with the key (armored detached signature). extra are
// gpg options such as "--digest-algo", "SHA1" or "--faked-system-time", "<t>!".
func (g gpgHome) detachSign(fpr string, data []byte, extra ...string) []byte {
	g.t.Helper()
	args := append(append([]string{}, extra...), "--armor", "--detach-sign", "-u", fpr)
	return []byte(run(g.t, "", nil, string(data), "gpg", append([]string{"--homedir", g.home, "--batch", "--yes", "--no-tty"}, args...)...))
}

// fakedTime formats a time for gpg's --faked-system-time.
func fakedTime(t time.Time) string { return t.UTC().Format("20060102T150405") + "!" }

// commit makes a commit signed by the key.
func (g gpgHome) commit(fpr, email string) signedCommit {
	g.t.Helper()
	return commit(g.t, newRepo(g.t), email, []string{"GNUPGHOME=" + g.home}, "gpg.format=openpgp", "user.signingkey="+fpr)
}

func trustedSSH(public string, emails ...string) domain.TrustedKey {
	return domain.TrustedKey{ID: "ssh-id", Format: domain.FormatSSH, Title: "ssh key", PublicKey: public, Emails: emails}
}

func trustedGPG(public string, emails ...string) domain.TrustedKey {
	return domain.TrustedKey{ID: "gpg-id", Format: domain.FormatGPG, Title: "gpg key", PublicKey: public, Emails: emails}
}
