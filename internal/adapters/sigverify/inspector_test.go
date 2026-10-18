// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/sigverify"
	"bnema.dev/ermite/internal/domain"
)

func TestInspectSSH(t *testing.T) {
	priv, pub := sshKey(t, "ed25519")
	_, rsaPub := sshKey(t, "rsa", "-b", "2048")
	privText, err := os.ReadFile(priv)
	require.NoError(t, err)
	canonical := strings.Join(strings.Fields(pub)[:2], " ")

	id, got, err := sigverify.Inspector{}.Inspect(domain.FormatSSH, pub)
	require.NoError(t, err)
	assert.Equal(t, fingerprint(t, pub), id)
	assert.Equal(t, canonical, got, "the comment is dropped")

	id2, got2, err := sigverify.Inspector{}.Inspect(domain.FormatSSH, "  "+canonical+"\n\n")
	require.NoError(t, err)
	assert.Equal(t, id, id2)
	assert.Equal(t, canonical, got2)

	_, got, err = sigverify.Inspector{}.Inspect(domain.FormatSSH, rsaPub)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, "ssh-rsa "))

	tests := []struct {
		name, key string
	}{
		{"empty", ""},
		{"garbage", "hello world"},
		{"private key", string(privText)},
		{"two keys", pub + "\n" + rsaPub + "\n"},
		{"truncated", canonical[:len(canonical)-10]},
		{"larger than the limit", strings.Repeat("a", domain.MaxTrustedKeyBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := sigverify.Inspector{}.Inspect(domain.FormatSSH, tt.key)
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.NotContains(t, err.Error(), "PRIVATE KEY")
		})
	}
}

func TestInspectGPG(t *testing.T) {
	g := newGPGHome(t)
	fpr := g.generate("inspect <dev@example.org>", "ed25519", "never")
	pub := g.public(fpr)
	other := g.generate("two <two@example.org>", "ed25519", "never")

	id, canonical, err := sigverify.Inspector{}.Inspect(domain.FormatGPG, pub)
	require.NoError(t, err)
	assert.Equal(t, strings.ToUpper(fpr), id)
	assert.True(t, strings.HasPrefix(canonical, "-----BEGIN PGP PUBLIC KEY BLOCK-----"))

	// The canonical form is stable and verifies signatures like the original.
	id2, canonical2, err := sigverify.Inspector{}.Inspect(domain.FormatGPG, canonical)
	require.NoError(t, err)
	assert.Equal(t, id, id2)
	assert.Equal(t, canonical, canonical2)
	c := g.commit(fpr, "dev@example.org")
	v := sigverify.GPG{}.Verify(c.payload, c.signature, "dev@example.org", c.signedAt, []domain.TrustedKey{trustedGPG(canonical, "dev@example.org")})
	assert.Equal(t, domain.StatusVerified, v.Status)

	tests := []struct {
		name, key string
	}{
		{"empty", ""},
		{"garbage", "hello world"},
		{"private key", g.private(fpr)},
		{"two keys in one block", g.public(fpr, other)},
		{"two blocks", pub + "\n" + g.public(other)},
		{"a signature", string(c.signature)},
		{"an ssh key", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA"},
		{"larger than the limit", strings.Repeat("a", domain.MaxTrustedKeyBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := sigverify.Inspector{}.Inspect(domain.FormatGPG, tt.key)
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.NotContains(t, err.Error(), "PRIVATE KEY")
		})
	}
}

func TestInspectUnknownFormat(t *testing.T) {
	for _, f := range []domain.SignatureFormat{domain.FormatNone, domain.FormatX509, "pgp"} {
		_, _, err := sigverify.Inspector{}.Inspect(f, "x")
		require.ErrorIs(t, err, domain.ErrInvalid)
	}
}
