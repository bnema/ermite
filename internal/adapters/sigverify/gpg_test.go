// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/sigverify"
	"bnema.dev/ermite/internal/domain"
)

func TestGPGVerify(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	for name, algo := range map[string]string{"ed25519": "ed25519", "rsa": "rsa2048"} {
		fpr := g.generate(name+" <"+email+">", algo, "never")
		pub := g.public(fpr)
		otherFpr := g.generate("other-"+name+" <other@example.org>", "ed25519", "never")
		otherPub := g.public(otherFpr)
		c := g.commit(fpr, email)

		t.Run(name, func(t *testing.T) {
			tamperedPayload := append(bytes.Clone(c.payload), '!')
			tests := []struct {
				name      string
				payload   []byte
				signature []byte
				keys      []domain.TrustedKey
				email     string
				want      domain.SignatureStatus
				wantFpr   string
				wantKey   bool
			}{
				{"verified", c.payload, c.signature, []domain.TrustedKey{trustedGPG(pub, email)}, email, domain.StatusVerified, fpr, true},
				{"email case is ignored", c.payload, c.signature, []domain.TrustedKey{trustedGPG(pub, email)}, "Dev@EXAMPLE.org", domain.StatusVerified, fpr, true},
				{"verified among other keys", c.payload, c.signature, []domain.TrustedKey{trustedGPG("garbage", email), {ID: "s", Format: domain.FormatSSH, PublicKey: "x"}, trustedGPG(otherPub, email), trustedGPG(pub, email)}, email, domain.StatusVerified, fpr, true},
				{"email mismatch", c.payload, c.signature, []domain.TrustedKey{trustedGPG(pub, "other@example.org")}, email, domain.StatusEmailMismatch, fpr, true},
				{"unknown key", c.payload, c.signature, []domain.TrustedKey{trustedGPG(otherPub, email)}, email, domain.StatusUnknownKey, strings.ToUpper(fpr[len(fpr)-16:]), false},
				{"no trusted key", c.payload, c.signature, nil, email, domain.StatusUnknownKey, strings.ToUpper(fpr[len(fpr)-16:]), false},
				{"ssh keys are ignored", c.payload, c.signature, []domain.TrustedKey{{ID: "x", Format: domain.FormatSSH, PublicKey: pub, Emails: []string{email}}}, email, domain.StatusUnknownKey, strings.ToUpper(fpr[len(fpr)-16:]), false},
				{"tampered payload", tamperedPayload, c.signature, []domain.TrustedKey{trustedGPG(pub, email)}, email, domain.StatusBadSignature, fpr, false},
				{"empty signature", c.payload, nil, []domain.TrustedKey{trustedGPG(pub, email)}, email, domain.StatusBadSignature, "", false},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					v := sigverify.GPG{}.Verify(tt.payload, tt.signature, tt.email, c.signedAt, tt.keys)
					assert.Equal(t, domain.FormatGPG, v.Format)
					assert.Equal(t, tt.want, v.Status)
					assert.Equal(t, tt.wantFpr, v.Fingerprint)
					if tt.wantKey {
						assert.Equal(t, "gpg-id", v.KeyID)
						assert.Equal(t, "gpg key", v.KeyTitle)
					} else {
						assert.Empty(t, v.KeyID)
						assert.Empty(t, v.KeyTitle)
					}
				})
			}
		})
	}
}

// TestGPGVerifyExpiredKey checks that expiry is judged at the creation time of
// the signature, not at the committer date nor now.
func TestGPGVerifyExpiredKey(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	day := 24 * time.Hour
	now := time.Now()
	// The key is created 10 days ago, signs 8 and 2 days ago, then gets an
	// expiry 3 days after its creation (published 9 days ago): it expired
	// 7 days ago, between the two signatures.
	fakedGPG := func(at time.Time, args ...string) {
		g.gpg("", append([]string{"--faked-system-time", fakedTime(at), "--passphrase", ""}, args...)...)
	}
	fakedGPG(now.Add(-10*day), "--quick-gen-key", "expiring <"+email+">", "ed25519", "sign", "never")
	fpr := g.fingerprint(email)
	payload := []byte("payload")
	validThen := g.detachSign(fpr, payload, "--faked-system-time", fakedTime(now.Add(-8*day)))
	expiredThen := g.detachSign(fpr, payload, "--faked-system-time", fakedTime(now.Add(-2*day)))
	fakedGPG(now.Add(-9*day), "--quick-set-expire", fpr, "3d")
	keys := []domain.TrustedKey{trustedGPG(g.public(fpr), email)}

	tests := []struct {
		name string
		sig  []byte
		want domain.SignatureStatus
	}{
		{"signed while the key was valid, key expired since", validThen, domain.StatusVerified},
		{"signed after the key expired", expiredThen, domain.StatusExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The committer date is irrelevant: any value gives the same status.
			for _, committed := range []int64{0, now.Add(-9 * day).Unix(), now.Unix(), now.Add(30 * day).Unix()} {
				v := sigverify.GPG{}.Verify(payload, tt.sig, email, committed, keys)
				assert.Equal(t, tt.want, v.Status, "committer date %d", committed)
				assert.Equal(t, fpr, v.Fingerprint)
			}
		})
	}
}

// TestGPGVerifyIgnoresCommitterDate: a signature that is valid and a key that
// never expires verify whatever the committer date says, including a date
// before the signature was made.
func TestGPGVerifyIgnoresCommitterDate(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	fpr := g.generate("dates <"+email+">", "ed25519", "never")
	keys := []domain.TrustedKey{trustedGPG(g.public(fpr), email)}
	c := g.commit(fpr, email)
	for _, delta := range []int64{-365 * 24 * 3600, -3600, 0, 3600} {
		v := sigverify.GPG{}.Verify(c.payload, c.signature, email, c.signedAt+delta, keys)
		assert.Equal(t, domain.StatusVerified, v.Status, "committer date %+d s", delta)
	}
}

// TestGPGVerifyWeakHash: the digest algorithm allowlist is enforced; the
// OpenPGP library does not reject a SHA-1 detached signature by itself.
func TestGPGVerifyWeakHash(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n\nmessage\n")
	for name, algo := range map[string]string{"rsa": "rsa2048", "ed25519": "ed25519"} {
		fpr := g.generate("weak-"+name+" <"+email+">", algo, "never")
		keys := []domain.TrustedKey{trustedGPG(g.public(fpr), email)}
		now := time.Now().Unix()
		tests := []struct {
			digest string
			want   domain.SignatureStatus
		}{
			{"SHA1", domain.StatusBadSignature},
			{"RIPEMD160", domain.StatusBadSignature},
			{"SHA224", domain.StatusBadSignature},
			{"SHA256", domain.StatusVerified},
			{"SHA384", domain.StatusVerified},
			{"SHA512", domain.StatusVerified},
		}
		for _, tt := range tests {
			t.Run(name+"/"+tt.digest, func(t *testing.T) {
				sig := g.detachSign(fpr, payload, "--digest-algo", tt.digest)
				v := sigverify.GPG{}.Verify(payload, sig, email, now, keys)
				assert.Equal(t, tt.want, v.Status)
				if tt.want != domain.StatusVerified {
					assert.Empty(t, v.KeyID, "a weak signature never names a trusted key")
				}
			})
		}
	}
}

// Several signature packets are refused: the library would verify the first
// one whose issuer it knows, while the clock and the revocation check would
// come from another one.
func TestGPGVerifyRefusesSeveralPackets(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n\nmessage\n")
	unknown := g.generate("unknown <other@example.org>", "ed25519", "never")
	trusted := g.generate("trusted <"+email+">", "ed25519", "never")
	keys := []domain.TrustedKey{trustedGPG(g.public(trusted), email)}
	binary := func(fpr string) []byte {
		block, err := armor.Decode(bytes.NewReader(g.detachSign(fpr, payload)))
		require.NoError(t, err)
		b, err := io.ReadAll(block.Body)
		require.NoError(t, err)
		return b
	}
	var both bytes.Buffer
	w, err := armor.Encode(&both, openpgp.SignatureType, nil)
	require.NoError(t, err)
	_, _ = w.Write(binary(unknown))
	_, _ = w.Write(binary(trusted))
	require.NoError(t, w.Close())

	single := sigverify.GPG{}.Verify(payload, g.detachSign(trusted, payload), email, 0, keys)
	require.Equal(t, domain.StatusVerified, single.Status, "control: one packet verifies")
	v := sigverify.GPG{}.Verify(payload, both.Bytes(), email, 0, keys)
	assert.Equal(t, domain.StatusBadSignature, v.Status)
	assert.Empty(t, v.KeyID)
}

func TestGPGVerifyRevokedKey(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	fpr := g.generate("revoked <"+email+">", "ed25519", "never")
	c := g.commit(fpr, email)
	g.revoke(fpr)
	pub := g.public(fpr) // carries the revocation

	v := sigverify.GPG{}.Verify(c.payload, c.signature, email, c.signedAt, []domain.TrustedKey{trustedGPG(pub, email)})
	assert.Equal(t, domain.StatusRevoked, v.Status)
	assert.Equal(t, fpr, v.Fingerprint)
}

func TestGPGVerifySubkey(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	fpr := g.generate("subkey <"+email+">", "ed25519", "never")
	// A signing subkey; the primary key is certify-only for gpg when asked so.
	g.gpg("", "--passphrase", "", "--quick-add-key", fpr, "ed25519", "sign", "never")
	out := g.gpg("", "--with-colons", "--list-keys", "--with-subkey-fingerprints", fpr)
	var sub string
	var next bool
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "sub:") {
			next = true
		} else if next && strings.HasPrefix(l, "fpr:") {
			sub = strings.Split(l, ":")[9]
			break
		}
	}
	require.NotEmpty(t, sub)
	c := g.commit(sub+"!", email)

	v := sigverify.GPG{}.Verify(c.payload, c.signature, email, c.signedAt, []domain.TrustedKey{trustedGPG(g.public(fpr), email)})
	assert.Equal(t, domain.StatusVerified, v.Status)
	assert.Equal(t, fpr, v.Fingerprint, "the fingerprint is the one of the primary key")
	assert.Equal(t, "gpg-id", v.KeyID)
}

func TestGPGVerifyHostileInput(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	fpr := g.generate("hostile <"+email+">", "ed25519", "never")
	pub := g.public(fpr)
	c := g.commit(fpr, email)
	keys := []domain.TrustedKey{trustedGPG(pub, email)}

	huge := []byte("-----BEGIN PGP SIGNATURE-----\n\n" + strings.Repeat("AAAA\n", 20<<10) + "-----END PGP SIGNATURE-----\n")
	tests := []struct {
		name string
		sig  []byte
	}{
		{"not armored", []byte("hello")},
		{"wrong armor type", []byte(strings.ReplaceAll(string(c.signature), "PGP SIGNATURE", "PGP MESSAGE"))},
		{"empty armor", []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")},
		{"a public key instead", []byte(pub)},
		{"larger than 64 KiB", huge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v domain.CommitVerification
			require.NotPanics(t, func() { v = sigverify.GPG{}.Verify(c.payload, tt.sig, email, c.signedAt, keys) })
			assert.Equal(t, domain.StatusBadSignature, v.Status)
			assert.Equal(t, domain.FormatGPG, v.Format)
		})
	}

	t.Run("every truncation of a valid signature", func(t *testing.T) {
		for i := range c.signature {
			require.NotPanics(t, func() { sigverify.GPG{}.Verify(c.payload, c.signature[:i], email, c.signedAt, keys) })
		}
	})
}

// TestGPGVerifyIgnoresPrivateKeys: a private key stored as trusted (it cannot
// be added through the inspector) does not take part in verification.
func TestGPGVerifyIgnoresPrivateKeys(t *testing.T) {
	const email = "dev@example.org"
	g := newGPGHome(t)
	fpr := g.generate("private <"+email+">", "ed25519", "never")
	c := g.commit(fpr, email)

	v := sigverify.GPG{}.Verify(c.payload, c.signature, email, c.signedAt, []domain.TrustedKey{trustedGPG(g.private(fpr), email)})
	assert.Equal(t, domain.StatusUnknownKey, v.Status)
}
