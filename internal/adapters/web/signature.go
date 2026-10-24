// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bnema.dev/ermite/internal/domain"
)

// SignatureBadge is the pill of a signed commit. Label is the verdict, Proto
// the protocol ("SSH", "GPG", "X.509", empty when unknown), Class the style
// (sig-ok, sig-warn, sig-bad, sig-none) and Title the tooltip that says why.
// Reason says why without naming the key, for a page that shows the key next
// to it.
type SignatureBadge struct {
	Label, Proto, Class, Title, Reason string
}

// Short is the protocol and the verdict, "SSH Verified": the text of the
// icon of a list, for screen readers.
func (b *SignatureBadge) Short() string {
	if b.Proto == "" {
		return b.Label
	}
	return b.Proto + " " + b.Label
}

// sigIconClass is the shield of a verdict: checked when verified, crossed
// when the signature is bad, with an alert mark otherwise.
func sigIconClass(class string) string {
	switch class {
	case "sig-ok":
		return "i-shield-check"
	case "sig-bad":
		return "i-shield-x"
	}
	return "i-shield-alert"
}

// signatureProto is the display name of a signature format.
func signatureProto(f domain.SignatureFormat) string {
	switch f {
	case domain.FormatSSH:
		return "SSH"
	case domain.FormatGPG:
		return "GPG"
	case domain.FormatX509:
		return "X.509"
	}
	return ""
}

// signatureBadge is the badge of a verification, nil when there is nothing to
// show: the commit was not checked, or (unless long, the commit page) it is
// not signed.
func signatureBadge(v domain.CommitVerification, long bool) *SignatureBadge {
	switch v.Status {
	case domain.StatusUnchecked:
		return nil
	case domain.StatusUnsigned:
		if !long {
			return nil
		}
		return &SignatureBadge{Label: "unsigned", Class: "sig-none", Title: "This commit is not signed", Reason: "Not signed"}
	}
	b := &SignatureBadge{Proto: signatureProto(v.Format)}
	key := keyPhrase(v)
	switch v.Status {
	case domain.StatusVerified:
		b.Label, b.Class = "Verified", "sig-ok"
		b.Title = "Signed with " + key + " — verified for the committer email"
		b.Reason = "Verified for the committer email"
	case domain.StatusUnknownKey:
		b.Label, b.Class = "Unverified", "sig-warn"
		b.Title = unknownKeyTitle(v)
		b.Reason = "The key is unknown"
	case domain.StatusEmailMismatch:
		b.Label, b.Class = "Unverified", "sig-warn"
		b.Title = "Signed with " + key + " — this key is not trusted for the committer email"
		b.Reason = "This key is not trusted for the committer email"
	case domain.StatusExpired:
		b.Label, b.Class = "Unverified", "sig-warn"
		b.Title = "Signed with " + key + " — the key or the signature expired"
		b.Reason = "The key or the signature expired"
	case domain.StatusRevoked:
		b.Label, b.Class = "Unverified", "sig-warn"
		b.Title = "Signed with " + key + " — the key is revoked"
		b.Reason = "The key is revoked"
	case domain.StatusBadSignature:
		b.Label, b.Class = "Bad signature", "sig-bad"
		b.Reason = "The signature does not match the commit"
	case domain.StatusUnsupported:
		b.Label, b.Class = "Unverified", "sig-warn"
		if proto := signatureProto(v.Format); proto != "" {
			b.Reason = proto + " signatures are not checked"
		} else {
			b.Reason = "This kind of signature is not checked"
		}
	default:
		return nil
	}
	if b.Title == "" {
		b.Title = b.Reason
	}
	return b
}

// keyPhrase names the signing key for a tooltip: "SSH key SHA256:… (title)".
func keyPhrase(v domain.CommitVerification) string {
	s := "a key"
	if p := signatureProto(v.Format); p != "" {
		s = p + " key"
	}
	if v.Fingerprint != "" {
		s += " " + v.Fingerprint
	}
	if v.KeyTitle != "" {
		s += " (" + visibleText(v.KeyTitle) + ")"
	}
	return s
}

// unknownKeyTitle is the tooltip of a signature by a key that is not trusted.
func unknownKeyTitle(v domain.CommitVerification) string {
	s := "Signed with an unknown "
	if p := signatureProto(v.Format); p != "" {
		s += p + " "
	}
	s += "key"
	if v.Fingerprint != "" {
		s += " " + v.Fingerprint
	}
	return s
}
