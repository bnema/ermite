// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// acceptedHashes are the digest algorithms of the signatures GPG accepts. The
// library's own reject lists (packet.Config.RejectMessageHashAlgorithms) are
// not applied to detached signatures, so the allowlist is enforced explicitly:
// a signature over MD5, SHA-1, RIPEMD-160 or SHA-224 is a bad signature.
var acceptedHashes = []crypto.Hash{crypto.SHA256, crypto.SHA384, crypto.SHA512, crypto.SHA3_256, crypto.SHA3_512}

// GPG verifies OpenPGP detached signatures. It ignores the trusted keys that
// are not GPG keys.
//
// Time policy: the committer date is not used. Key, subkey and signature
// expiry are judged at the creation time of the signature, which the
// signature authenticates, so a key that has expired since still verifies
// what it signed while it was valid. Revocation is judged now: a signature by
// a key that is revoked today (key, signing subkey or primary user ID) is
// reported as revoked, whenever it was made.
type GPG struct{}

var _ ports.SignatureVerifier = GPG{}

// Verify implements ports.SignatureVerifier. signedAt is ignored: see the time
// policy of GPG.
func (GPG) Verify(payload, signature []byte, committerEmail string, _ int64, keys []domain.TrustedKey) (v domain.CommitVerification) {
	v = domain.CommitVerification{Format: domain.FormatGPG, Status: domain.StatusBadSignature}
	defer func() { // a parser bug must not take the server down
		if recover() != nil {
			v = domain.CommitVerification{Format: domain.FormatGPG, Status: domain.StatusBadSignature, Fingerprint: v.Fingerprint}
		}
	}()
	if len(signature) > maxSignatureBytes {
		return v
	}
	block, err := armor.Decode(bytes.NewReader(signature))
	if err != nil || block.Type != openpgp.SignatureType {
		return v
	}
	body, err := io.ReadAll(io.LimitReader(block.Body, maxSignatureBytes))
	if err != nil {
		return v
	}
	sig, ok := onlySignature(body)
	if !ok {
		return v
	}
	issuer := *sig.IssuerKeyId
	v.Fingerprint = fmt.Sprintf("%016X", issuer)

	ring, byFingerprint := gpgKeyring(keys)
	created := sig.CreationTime
	cfg := &packet.Config{Time: func() time.Time { return created }}
	_, signer, err := openpgp.VerifyDetachedSignatureAndHash(ring, bytes.NewReader(payload), bytes.NewReader(body), acceptedHashes, cfg)
	if signer == nil && !errors.Is(err, pgperrors.ErrUnknownIssuer) {
		// The check failed before naming the signer: look it up for the fingerprint.
		if known := ring.KeysById(issuer); len(known) > 0 {
			signer = known[0].Entity
		}
	}
	if signer != nil {
		v.Fingerprint = fingerprintHex(signer)
	}

	if signer != nil && (err == nil || errors.Is(err, pgperrors.ErrKeyExpired) || errors.Is(err, pgperrors.ErrSignatureExpired)) &&
		revokedNow(ring.KeysById(issuer), signer) {
		// The library judged revocation at the signature date: a revocation
		// published since is only seen with the current time.
		v.Status = domain.StatusRevoked
		return v
	}

	switch {
	case err == nil:
	case errors.Is(err, pgperrors.ErrUnknownIssuer):
		v.Status = domain.StatusUnknownKey
		return v
	case errors.Is(err, pgperrors.ErrKeyExpired), errors.Is(err, pgperrors.ErrSignatureExpired):
		v.Status = domain.StatusExpired
		return v
	case errors.Is(err, pgperrors.ErrKeyRevoked):
		v.Status = domain.StatusRevoked
		return v
	default:
		return v
	}
	if key, ok := byFingerprint[v.Fingerprint]; ok {
		return trusted(v, key, committerEmail)
	}
	v.Status = domain.StatusUnknownKey
	return v
}

// onlySignature returns the signature packet of body when body holds exactly
// one packet, a signature with an issuer key id and a creation time. One
// packet only: the library verifies the first packet whose issuer it knows,
// so with several the clock and the revocation check could be taken from a
// packet other than the one that verified.
func onlySignature(body []byte) (*packet.Signature, bool) {
	r := bytes.NewReader(body)
	p, err := packet.Read(r)
	if err != nil {
		return nil, false
	}
	if _, err := packet.Read(r); !errors.Is(err, io.EOF) {
		return nil, false
	}
	sig, ok := p.(*packet.Signature)
	if !ok || sig.IssuerKeyId == nil || sig.CreationTime.IsZero() {
		return nil, false
	}
	return sig, true
}

// revokedNow reports whether the entity, the signing key among keys (the
// primary key or a subkey) or the primary user ID is revoked at the current
// time.
func revokedNow(keys []openpgp.Key, signer *openpgp.Entity) bool {
	now := time.Now()
	if signer.Revoked(now) {
		return true
	}
	if _, id := signer.PrimarySelfSignature(); id != nil && id.Revoked(now) {
		return true
	}
	for _, k := range keys {
		if k.Entity == signer && k.Revoked(now) {
			return true
		}
	}
	return false
}

// gpgKeyring builds the keyring of the trusted GPG keys, and indexes the
// trusted keys by the primary fingerprint. Unreadable keys and private keys
// are skipped.
func gpgKeyring(keys []domain.TrustedKey) (openpgp.EntityList, map[string]domain.TrustedKey) {
	var ring openpgp.EntityList
	byFingerprint := make(map[string]domain.TrustedKey, len(keys))
	for _, k := range keys {
		if k.Format != domain.FormatGPG || len(k.PublicKey) > domain.MaxTrustedKeyBytes {
			continue
		}
		entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(k.PublicKey))
		if err != nil {
			continue
		}
		for _, e := range entities {
			if hasPrivateKey(e) {
				continue
			}
			ring = append(ring, e)
			byFingerprint[fingerprintHex(e)] = k
		}
	}
	return ring, byFingerprint
}
