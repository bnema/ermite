// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

// TestSplitCommitSignature pins SplitCommitSignature to what `git
// verify-commit` hands to gpg.program in a SHA-1 repository: the cases were
// captured with a fake gpg.program that dumps its stdin (the payload) and the
// signature file (git 2.56, objects made with `git hash-object --literally`).
func TestSplitCommitSignature(t *testing.T) {
	const (
		hdr = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n"
		one = "-----BEGIN PGP SIGNATURE-----\n\nONE\n-----END PGP SIGNATURE-----\n"
		two = "-----BEGIN PGP SIGNATURE-----\n\nTWO\n-----END PGP SIGNATURE-----\n"
	)
	// sigHdr renders a signature as a header: name, then each line, the
	// continuation lines starting with a space.
	sigHdr := func(name, body string) string {
		return name + " -----BEGIN PGP SIGNATURE-----\n \n " + body + "\n -----END PGP SIGNATURE-----\n"
	}
	tests := []struct {
		name        string
		raw         string
		wantPayload string
		wantSig     string
		unsigned    bool
	}{
		{"single signature", hdr + sigHdr("gpgsig", "ONE") + "\nmsg\n", hdr + "\nmsg\n", one, false},
		{"repeated gpgsig are concatenated, both removed", hdr + sigHdr("gpgsig", "ONE") + sigHdr("gpgsig", "TWO") + "\nmsg\n", hdr + "\nmsg\n", one + two, false},
		{"gpgsig and gpgsig-sha256: the sha256 one is removed, not used", hdr + sigHdr("gpgsig", "ONE") + sigHdr("gpgsig-sha256", "TWO") + "\nmsg\n", hdr + "\nmsg\n", one, false},
		{"gpgsig-sha256 first", hdr + sigHdr("gpgsig-sha256", "TWO") + sigHdr("gpgsig", "ONE") + "\nmsg\n", hdr + "\nmsg\n", one, false},
		{"gpgsig-sha256 only is unsigned", hdr + sigHdr("gpgsig-sha256", "TWO") + "\nmsg\n", "", "", true},
		{"gpgsig-looking line in the message is untouched", hdr + "\nmsg\n" + sigHdr("gpgsig", "MSG"), "", "", true},
		{"gpgsig in the message of a signed commit is untouched", hdr + sigHdr("gpgsig", "ONE") + "\nmsg\n" + sigHdr("gpgsig", "MSG"), hdr + "\nmsg\n" + sigHdr("gpgsig", "MSG"), one, false},
		{"CRLF in the signature is kept", hdr + "gpgsig -----BEGIN PGP SIGNATURE-----\r\n \r\n ONE\r\n -----END PGP SIGNATURE-----\r\n" + "\nmsg\n", hdr + "\nmsg\n", "-----BEGIN PGP SIGNATURE-----\r\n\r\nONE\r\n-----END PGP SIGNATURE-----\r\n", false},
		{"CRLF message", hdr + sigHdr("gpgsig", "ONE") + "\r\nmsg\r\n", hdr + "\r\nmsg\r\n", one, false},
		{"missing final newline of the message", hdr + sigHdr("gpgsig", "ONE") + "\nmsg", hdr + "\nmsg", one, false},
		{"missing final newline of the signature: none is added", hdr + sigHdr("gpgsig", "ONE")[:len(sigHdr("gpgsig", "ONE"))-1], hdr, one[:len(one)-1], false},
		{"other headers with continuation lines are kept", hdr + sigHdr("gpgsig", "ONE") + "other x\n cont\n\nmsg\n", hdr + "other x\n cont\n\nmsg\n", one, false},
		{"gpgsig between other headers", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" + sigHdr("gpgsig", "ONE") + "author A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nmsg\n", hdr + "\nmsg\n", one, false},
		{"headers that start with gpgsig are removed with their continuation lines", hdr + sigHdr("gpgsig", "ONE") + "gpgsigx foo\n c1\ngpgsig-foo bar\n\nmsg\n", hdr + "\nmsg\n", one, false},
		{"a gpgsigx block does not extend the signature", hdr + "gpgsig A\n B\ngpgsigx foo\n C\ngpgsig D\n\nmsg\n", hdr + "\nmsg\n", "A\nB\nD\n", false},
		{"a gpgsig-sha256 block between two gpgsig", hdr + sigHdr("gpgsig", "ONE") + "gpgsig-sha256 mid\n m1\n" + sigHdr("gpgsig", "TWO") + "\nmsg\n", hdr + "\nmsg\n", one + two, false},
		{"a bare gpgsig line (no value) alone is unsigned", hdr + "gpgsig\n c1\n\nmsg\n", "", "", true},
		{"a bare gpgsig line next to a signature is removed", hdr + sigHdr("gpgsig", "ONE") + "gpgsig\n c1\n\nmsg\n", hdr + "\nmsg\n", one, false},
		{"header names are case-sensitive", hdr + sigHdr("gpgsig", "ONE") + "GPGSIG foo\n\nmsg\n", hdr + "GPGSIG foo\n\nmsg\n", one, false},
		{"continuation line of a plain header is kept", hdr + sigHdr("gpgsig", "ONE") + "x y\n z\n\nmsg\n", hdr + "x y\n z\n\nmsg\n", one, false},
		{"empty value", hdr + "gpgsig \n\nmsg\n", hdr + "\nmsg\n", "\n", false},
		{"unsigned", hdr + "\nmsg\n", "", "", true},
		{"empty", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, sig, ok := domain.SplitCommitSignature([]byte(tt.raw))
			assert.Equal(t, !tt.unsigned, ok)
			if tt.unsigned {
				assert.Nil(t, payload)
				assert.Nil(t, sig)
				return
			}
			assert.Equal(t, tt.wantPayload, string(payload))
			assert.Equal(t, tt.wantSig, string(sig))
		})
	}
}
