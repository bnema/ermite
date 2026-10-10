// SPDX-License-Identifier: AGPL-3.0-or-later

package sshsig_test

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/adapters/sshsig"
)

func seed(b byte) []byte { return bytes.Repeat([]byte{b}, ed25519.SeedSize) }

func TestPublicKey(t *testing.T) {
	s, err := sshsig.New(seed(7))
	require.NoError(t, err)
	pub := s.PublicKey()

	assert.True(t, strings.HasPrefix(pub.AuthorizedKey, "ssh-ed25519 "))
	assert.NotContains(t, pub.AuthorizedKey, "\n")
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub.AuthorizedKey))
	require.NoError(t, err)
	assert.Equal(t, ssh.FingerprintSHA256(parsed), pub.Fingerprint)
	assert.True(t, strings.HasPrefix(pub.Fingerprint, "SHA256:"))

	// The same seed is the same key; another seed another key.
	again, err := sshsig.New(seed(7))
	require.NoError(t, err)
	assert.Equal(t, pub, again.PublicKey())
	other, err := sshsig.New(seed(8))
	require.NoError(t, err)
	assert.NotEqual(t, pub, other.PublicKey())
}

func TestNewRejectsBadSeed(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 31)
	_, err := sshsig.New(secret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), string(secret))
}

func TestSignArmor(t *testing.T) {
	s, err := sshsig.New(seed(1))
	require.NoError(t, err)
	armored, err := s.Sign([]byte("payload"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSuffix(armored, "\n"), "\n")
	assert.Equal(t, "-----BEGIN SSH SIGNATURE-----", lines[0])
	assert.Equal(t, "-----END SSH SIGNATURE-----", lines[len(lines)-1])
	for _, l := range lines[1 : len(lines)-2] {
		assert.Len(t, l, 70)
	}
}

// TestSignVerifiesWithSSHKeygen checks the signature of a message with the
// reference implementation, `ssh-keygen -Y verify` (namespace git), and that a
// changed message or namespace does not verify.
func TestSignVerifiesWithSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("ssh-keygen not found: CI must have it")
		}
		t.Skip("ssh-keygen not found: install openssh to check signatures against the reference")
	}
	s, err := sshsig.New(seed(3))
	require.NoError(t, err)
	msg := []byte("tree 0000\nparent 1111\n\nmessage\n")
	armored, err := s.Sign(msg)
	require.NoError(t, err)

	dir := t.TempDir()
	sigFile := filepath.Join(dir, "sig")
	require.NoError(t, os.WriteFile(sigFile, []byte(armored), 0o600))
	signers := filepath.Join(dir, "allowed_signers")
	require.NoError(t, os.WriteFile(signers, []byte("ermite@example.org "+s.PublicKey().AuthorizedKey+"\n"), 0o600))

	verify := func(namespace string, data []byte) error {
		cmd := exec.Command(keygen, "-Y", "verify", "-f", signers, "-I", "ermite@example.org", "-n", namespace, "-s", sigFile)
		cmd.Stdin = bytes.NewReader(data)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("ssh-keygen: %s", out)
		}
		return err
	}
	require.NoError(t, verify("git", msg))
	assert.Error(t, verify("git", append(msg, 'x')), "a changed message must not verify")
	assert.Error(t, verify("file", msg), "another namespace must not verify")
}
