// SPDX-License-Identifier: AGPL-3.0-or-later

package sigverify

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Inspector parses the public keys to trust. The zero value is ready.
type Inspector struct{}

var _ ports.KeyInspector = Inspector{}

// Inspect implements ports.KeyInspector.
func (Inspector) Inspect(format domain.SignatureFormat, publicKey string) (id, canonical string, err error) {
	if len(publicKey) > domain.MaxTrustedKeyBytes {
		return "", "", fmt.Errorf("%w: public key larger than %d bytes", domain.ErrInvalid, domain.MaxTrustedKeyBytes)
	}
	defer func() { // a parser bug must not take the server down
		if recover() != nil {
			id, canonical, err = "", "", fmt.Errorf("%w: unreadable public key", domain.ErrInvalid)
		}
	}()
	switch format {
	case domain.FormatSSH:
		return inspectSSH(publicKey)
	case domain.FormatGPG:
		return inspectGPG(publicKey)
	}
	return "", "", fmt.Errorf("%w: key format %q, want ssh or gpg", domain.ErrInvalid, format)
}

func inspectSSH(publicKey string) (id, canonical string, err error) {
	pk, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return "", "", fmt.Errorf("%w: not an SSH public key (want one authorized_keys line)", domain.ErrInvalid)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return "", "", fmt.Errorf("%w: give exactly one SSH public key", domain.ErrInvalid)
	}
	if _, isCert := pk.(*ssh.Certificate); isCert {
		return "", "", fmt.Errorf("%w: SSH certificates cannot be trusted, give the public key", domain.ErrInvalid)
	}
	return ssh.FingerprintSHA256(pk), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), nil
}

func inspectGPG(publicKey string) (id, canonical string, err error) {
	if strings.Count(publicKey, "-----BEGIN PGP") != 1 {
		return "", "", fmt.Errorf("%w: give exactly one armored OpenPGP public key block", domain.ErrInvalid)
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(publicKey))
	if err != nil {
		return "", "", fmt.Errorf("%w: not an armored OpenPGP public key", domain.ErrInvalid)
	}
	if len(ring) != 1 {
		return "", "", fmt.Errorf("%w: give exactly one OpenPGP key, got %d", domain.ErrInvalid, len(ring))
	}
	e := ring[0]
	if hasPrivateKey(e) {
		return "", "", fmt.Errorf("%w: this is a private key; export the public key instead", domain.ErrInvalid)
	}
	var out bytes.Buffer
	w, err := armor.Encode(&out, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", "", errors.New("armor the public key")
	}
	if err := e.Serialize(w); err != nil {
		return "", "", fmt.Errorf("%w: unreadable OpenPGP key", domain.ErrInvalid)
	}
	if err := w.Close(); err != nil {
		return "", "", errors.New("armor the public key")
	}
	return fingerprintHex(e), out.String(), nil
}

func hasPrivateKey(e *openpgp.Entity) bool {
	if e.PrivateKey != nil {
		return true
	}
	for _, s := range e.Subkeys {
		if s.PrivateKey != nil {
			return true
		}
	}
	return false
}

// fingerprintHex is the primary key fingerprint of e in upper-case hex.
func fingerprintHex(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}
