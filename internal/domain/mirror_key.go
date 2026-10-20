// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/url"
	"time"
)

const MirrorKeyPath = "mirror/key" // KV key and secretbox AAD of the sealed seed
type MirrorPublicKey struct {
	AuthorizedKey string // "ssh-ed25519 AAAA… ermite-mirror@<host>"
	Fingerprint   string // "SHA256:<base64 no padding>"
	CreatedAt     time.Time
}

func MirrorKeyComment(publicURL string) string {
	host := "localhost"
	if u, e := url.Parse(publicURL); e == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return "ermite-mirror@" + host
}

// MirrorPublicKeyOf formats an ed25519 public key (wire encoding + base64 done with stdlib).
func MirrorPublicKeyOf(pub ed25519.PublicKey, host string, createdAt time.Time) MirrorPublicKey {
	wire := make([]byte, 4+len("ssh-ed25519")+4+len(pub))
	binary.BigEndian.PutUint32(wire, uint32(len("ssh-ed25519")))
	copy(wire[4:], "ssh-ed25519")
	offset := 4 + len("ssh-ed25519")
	binary.BigEndian.PutUint32(wire[offset:], uint32(len(pub)))
	copy(wire[offset+4:], pub)
	sum := sha256.Sum256(wire)
	return MirrorPublicKey{AuthorizedKey: "ssh-ed25519 " + base64.StdEncoding.EncodeToString(wire) + " " + MirrorKeyComment("https://"+host), Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), CreatedAt: createdAt}
}
