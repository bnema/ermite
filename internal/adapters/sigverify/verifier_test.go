// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/adapters/sigverify"
	"bnema.dev/ermite/internal/domain"
)

func TestVerifierDispatch(t *testing.T) {
	const email = "dev@example.org"
	priv, sshPub := sshKey(t, "ed25519")
	sshC := sshCommit(t, priv, email)
	g := newGPGHome(t)
	fpr := g.generate("dispatch <"+email+">", "ed25519", "never")
	gpgC := g.commit(fpr, email)
	keys := []domain.TrustedKey{trustedSSH(sshPub, email), trustedGPG(g.public(fpr), email)}

	tests := []struct {
		name       string
		payload    []byte
		signature  []byte
		wantFormat domain.SignatureFormat
		wantStatus domain.SignatureStatus
	}{
		{"ssh", sshC.payload, sshC.signature, domain.FormatSSH, domain.StatusVerified},
		{"gpg", gpgC.payload, gpgC.signature, domain.FormatGPG, domain.StatusVerified},
		{"ssh signature of another commit", append(sshC.payload[:len(sshC.payload):len(sshC.payload)], 0), sshC.signature, domain.FormatSSH, domain.StatusBadSignature},
		{"x509", sshC.payload, []byte("-----BEGIN SIGNED MESSAGE-----\nMIIB\n-----END SIGNED MESSAGE-----\n"), domain.FormatX509, domain.StatusUnsupported},
		{"unknown armor", sshC.payload, []byte("-----BEGIN SOMETHING-----\nAAAA\n-----END SOMETHING-----\n"), domain.FormatNone, domain.StatusBadSignature},
		{"empty", sshC.payload, nil, domain.FormatNone, domain.StatusBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := sigverify.Verifier{}.Verify(tt.payload, tt.signature, email, sshC.signedAt, keys)
			assert.Equal(t, tt.wantFormat, v.Format)
			assert.Equal(t, tt.wantStatus, v.Status)
		})
	}
}
