// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"strings"

	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// SSHSIG constants (PROTOCOL.sshsig); the namespace is the one Git signs with.
const (
	sshMagic     = "SSHSIG"
	sshVersion   = 1
	sshNamespace = "git"
	sshArmorHead = "-----BEGIN SSH SIGNATURE-----"
	sshArmorTail = "-----END SSH SIGNATURE-----"
)

// SSH verifies SSH signatures (gpg.format=ssh). It ignores the trusted keys
// that are not SSH keys.
type SSH struct{}

var _ ports.SignatureVerifier = SSH{}

// sshSignedData is what the signature covers after the magic.
type sshSignedData struct {
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Hash          string
}

// sshSigBlob is the signature file after the magic.
type sshSigBlob struct {
	Version       uint32
	PublicKey     string
	Namespace     string
	Reserved      string
	HashAlgorithm string
	Signature     string
}

// Verify implements ports.SignatureVerifier.
//
//hexcheck:ignore suspicious-business-logic-in-adapter this is the SSHSIG protocol check the port delegates to the adapter; the trust and email rules stay in domain
func (SSH) Verify(payload, signature []byte, committerEmail string, signedAt int64, keys []domain.TrustedKey) (v domain.CommitVerification) {
	v = domain.CommitVerification{Format: domain.FormatSSH, Status: domain.StatusBadSignature}
	defer func() { // a parser bug must not take the server down
		if recover() != nil {
			v = domain.CommitVerification{Format: domain.FormatSSH, Status: domain.StatusBadSignature, Fingerprint: v.Fingerprint}
		}
	}()
	if len(signature) > maxSignatureBytes {
		return v
	}
	blob, ok := decodeSSHArmor(signature)
	if !ok || !bytes.HasPrefix(blob, []byte(sshMagic)) {
		return v
	}
	var sb sshSigBlob
	if ssh.Unmarshal(blob[len(sshMagic):], &sb) != nil {
		return v
	}
	pk, err := ssh.ParsePublicKey([]byte(sb.PublicKey))
	if err != nil {
		return v
	}
	v.Fingerprint = ssh.FingerprintSHA256(pk)
	if sb.Version != sshVersion || sb.Namespace != sshNamespace {
		return v
	}
	var sum []byte
	switch sb.HashAlgorithm {
	case "sha256":
		h := sha256.Sum256(payload)
		sum = h[:]
	case "sha512":
		h := sha512.Sum512(payload)
		sum = h[:]
	default:
		return v
	}
	var sf struct {
		Format string
		Blob   string
		Rest   []byte `ssh:"rest"`
	}
	if ssh.Unmarshal([]byte(sb.Signature), &sf) != nil {
		return v
	}
	if pk.Type() == ssh.KeyAlgoRSA && sf.Format != ssh.KeyAlgoRSASHA256 && sf.Format != ssh.KeyAlgoRSASHA512 {
		return v // ssh-rsa is SHA-1
	}
	msg := append([]byte(sshMagic), ssh.Marshal(sshSignedData{
		Namespace: sb.Namespace, Reserved: sb.Reserved, HashAlgorithm: sb.HashAlgorithm, Hash: string(sum),
	})...)
	if pk.Verify(msg, &ssh.Signature{Format: sf.Format, Blob: []byte(sf.Blob), Rest: sf.Rest}) != nil {
		return v
	}

	want := pk.Marshal()
	for _, k := range keys {
		if k.Format != domain.FormatSSH {
			continue
		}
		tk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
		if err != nil || !bytes.Equal(tk.Marshal(), want) {
			continue
		}
		return trusted(v, k, committerEmail)
	}
	v.Status = domain.StatusUnknownKey
	return v
}

// decodeSSHArmor returns the binary signature file of an armored SSH
// signature.
func decodeSSHArmor(armored []byte) ([]byte, bool) {
	lines := strings.Split(strings.TrimSpace(string(armored)), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != sshArmorHead || strings.TrimSpace(lines[len(lines)-1]) != sshArmorTail {
		return nil, false
	}
	var b strings.Builder
	for _, l := range lines[1 : len(lines)-1] {
		b.WriteString(strings.TrimSpace(l))
	}
	raw, err := base64.StdEncoding.DecodeString(b.String())
	return raw, err == nil
}
