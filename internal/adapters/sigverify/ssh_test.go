// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify_test

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/adapters/sigverify"
	"bnema.dev/ermite/internal/adapters/sshsig"
	"bnema.dev/ermite/internal/domain"
)

func fingerprint(t *testing.T, authorizedKey string) string {
	t.Helper()
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	require.NoError(t, err)
	return ssh.FingerprintSHA256(pk)
}

func TestSSHVerify(t *testing.T) {
	const email = "dev@example.org"
	keys := map[string]struct{ private, public string }{}
	for name, args := range map[string][]string{
		"ed25519": {"ed25519"},
		"rsa":     {"rsa", "-b", "2048"},
		"ecdsa":   {"ecdsa", "-b", "256"},
	} {
		priv, pub := sshKey(t, args[0], args[1:]...)
		keys[name] = struct{ private, public string }{priv, pub}
	}

	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			c := sshCommit(t, key.private, email)
			fp := fingerprint(t, key.public)
			other := keys["ed25519"]
			if name == "ed25519" {
				other = keys["rsa"]
			}
			tamperedPayload := append(bytes.Clone(c.payload), '!')
			tamperedSig := bytes.Replace(c.signature, []byte("\n"), []byte("\nAAAA"), 2)

			tests := []struct {
				name      string
				payload   []byte
				signature []byte
				keys      []domain.TrustedKey
				email     string
				want      domain.SignatureStatus
				wantKey   bool
			}{
				{"verified", c.payload, c.signature, []domain.TrustedKey{trustedSSH(key.public, email)}, email, domain.StatusVerified, true},
				{"verified with the canonical key", c.payload, c.signature, []domain.TrustedKey{trustedSSH(strings.Join(strings.Fields(key.public)[:2], " "), email)}, email, domain.StatusVerified, true},
				{"email case is ignored", c.payload, c.signature, []domain.TrustedKey{trustedSSH(key.public, email)}, "DEV@Example.ORG", domain.StatusVerified, true},
				{"verified among other keys", c.payload, c.signature, []domain.TrustedKey{trustedSSH(other.public, email), trustedGPG("not a key", email), trustedSSH(key.public, email)}, email, domain.StatusVerified, true},
				{"email mismatch", c.payload, c.signature, []domain.TrustedKey{trustedSSH(key.public, "other@example.org")}, email, domain.StatusEmailMismatch, true},
				{"unknown key", c.payload, c.signature, []domain.TrustedKey{trustedSSH(other.public, email)}, email, domain.StatusUnknownKey, false},
				{"no trusted key", c.payload, c.signature, nil, email, domain.StatusUnknownKey, false},
				{"gpg keys are ignored", c.payload, c.signature, []domain.TrustedKey{{ID: "x", Format: domain.FormatGPG, PublicKey: key.public, Emails: []string{email}}}, email, domain.StatusUnknownKey, false},
				{"tampered payload", tamperedPayload, c.signature, []domain.TrustedKey{trustedSSH(key.public, email)}, email, domain.StatusBadSignature, false},
				{"tampered signature", c.payload, tamperedSig, []domain.TrustedKey{trustedSSH(key.public, email)}, email, domain.StatusBadSignature, false},
				{"empty signature", c.payload, nil, []domain.TrustedKey{trustedSSH(key.public, email)}, email, domain.StatusBadSignature, false},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					v := sigverify.SSH{}.Verify(tt.payload, tt.signature, tt.email, c.signedAt, tt.keys)
					assert.Equal(t, domain.FormatSSH, v.Format)
					assert.Equal(t, tt.want, v.Status)
					if tt.wantKey {
						assert.Equal(t, "ssh-id", v.KeyID)
						assert.Equal(t, "ssh key", v.KeyTitle)
					} else {
						assert.Empty(t, v.KeyID)
						assert.Empty(t, v.KeyTitle)
					}
					if tt.want == domain.StatusVerified || tt.want == domain.StatusEmailMismatch || tt.want == domain.StatusUnknownKey {
						assert.Equal(t, fp, v.Fingerprint)
					}
				})
			}
		})
	}
}

func TestSSHVerifyWrongNamespace(t *testing.T) {
	const email = "dev@example.org"
	priv, pub := sshKey(t, "ed25519")
	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@b> 1 +0000\n\nmsg\n")
	// Reading stdin, ssh-keygen -Y sign writes the armored signature to stdout.
	sig := run(t, "", nil, string(payload), "ssh-keygen", "-q", "-Y", "sign", "-n", "file", "-f", priv)
	require.Contains(t, sig, "BEGIN SSH SIGNATURE")

	v := sigverify.SSH{}.Verify(payload, []byte(sig), email, 0, []domain.TrustedKey{trustedSSH(pub, email)})
	assert.Equal(t, domain.StatusBadSignature, v.Status)
	assert.Equal(t, fingerprint(t, pub), v.Fingerprint, "the key is still named")

	// The same signing in namespace git verifies: the namespace is the only change.
	sig = run(t, "", nil, string(payload), "ssh-keygen", "-q", "-Y", "sign", "-n", "git", "-f", priv)
	v = sigverify.SSH{}.Verify(payload, []byte(sig), email, 0, []domain.TrustedKey{trustedSSH(pub, email)})
	assert.Equal(t, domain.StatusVerified, v.Status)
}

func TestSSHVerifyHostileInput(t *testing.T) {
	const email = "dev@example.org"
	_, pub := sshKey(t, "ed25519")
	keys := []domain.TrustedKey{trustedSSH(pub, email)}
	valid := func() []byte {
		s, err := sshsig.New(bytes.Repeat([]byte{9}, ed25519.SeedSize))
		require.NoError(t, err)
		sig, err := s.Sign([]byte("payload"))
		require.NoError(t, err)
		return []byte(sig)
	}()

	huge := []byte("-----BEGIN SSH SIGNATURE-----\n" + strings.Repeat("A", 70<<10) + "\n-----END SSH SIGNATURE-----\n")
	tests := []struct {
		name string
		sig  []byte
	}{
		{"not armored", []byte("hello")},
		{"no end line", bytes.TrimSuffix(valid, []byte("-----END SSH SIGNATURE-----\n"))},
		{"bad base64", []byte("-----BEGIN SSH SIGNATURE-----\n!!!!\n-----END SSH SIGNATURE-----\n")},
		{"wrong magic", []byte("-----BEGIN SSH SIGNATURE-----\nQUJDREVGR0g=\n-----END SSH SIGNATURE-----\n")},
		{"magic only", []byte("-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----\n")},
		{"larger than 64 KiB", huge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v domain.CommitVerification
			require.NotPanics(t, func() { v = sigverify.SSH{}.Verify([]byte("payload"), tt.sig, email, 0, keys) })
			assert.Equal(t, domain.StatusBadSignature, v.Status)
			assert.Equal(t, domain.FormatSSH, v.Format)
		})
	}

	t.Run("every truncation of a valid signature", func(t *testing.T) {
		for i := range valid {
			require.NotPanics(t, func() { sigverify.SSH{}.Verify([]byte("payload"), valid[:i], email, 0, keys) })
		}
	})
}

// TestSSHVerifyInstanceKey checks the signature of the commits ermite itself
// creates (ADR 0022) against the public key it publishes.
func TestSSHVerifyInstanceKey(t *testing.T) {
	const email = "ermite@example.org"
	signer, err := sshsig.New(bytes.Repeat([]byte{5}, ed25519.SeedSize))
	require.NoError(t, err)
	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor E <ermite@example.org> 1 +0000\ncommitter E <ermite@example.org> 1 +0000\n\nmsg\n")
	sig, err := signer.Sign(payload)
	require.NoError(t, err)

	key := domain.TrustedKey{ID: signer.PublicKey().Fingerprint, Format: domain.FormatSSH, Title: "instance", PublicKey: signer.PublicKey().AuthorizedKey, Emails: []string{email}, Builtin: true}
	v := sigverify.Verifier{}.Verify(payload, []byte(sig), email, 1, []domain.TrustedKey{key})
	assert.Equal(t, domain.CommitVerification{
		Format: domain.FormatSSH, Status: domain.StatusVerified,
		Fingerprint: signer.PublicKey().Fingerprint, KeyID: key.ID, KeyTitle: "instance",
	}, v)

	v = sigverify.Verifier{}.Verify(append(payload, 'x'), []byte(sig), email, 1, []domain.TrustedKey{key})
	assert.Equal(t, domain.StatusBadSignature, v.Status)
}

// TestSSHVerifyRSASignatureFormat checks that the RSA signature ssh-keygen
// makes (rsa-sha2-512) verifies; the legacy SHA-1 format is refused by the
// verifier.
func TestSSHVerifyRSASignatureFormat(t *testing.T) {
	const email = "dev@example.org"
	priv, pub := sshKey(t, "rsa", "-b", "2048")
	payload := []byte("payload\n")
	sig := run(t, "", nil, string(payload), "ssh-keygen", "-q", "-Y", "sign", "-n", "git", "-f", priv)
	v := sigverify.SSH{}.Verify(payload, []byte(sig), email, 0, []domain.TrustedKey{trustedSSH(pub, email)})
	assert.Equal(t, domain.StatusVerified, v.Status)
}
