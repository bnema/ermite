// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"strings"
	"unicode"
)

// invisible reports whether r is a character that changes how code reads
// without being seen: the bidirectional controls and the other format
// characters (Cf: zero-width, joiners, tags, BOM…), the control characters,
// the line and paragraph separators, and the blank Hangul fillers. A tab is
// not one, nor is the line break of a commit message.
func invisible(r rune) bool {
	switch r {
	case '\t', '\n':
		return false
	case 0x115F, 0x1160, 0x3164, 0xFFA0: // Hangul fillers render blank
		return true
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// marker is the visible form of an invisible character.
func marker(r rune) string { return fmt.Sprintf("<U+%04X>", r) }

// who is the name shown for the author of a merge request or a comment, or the
// actor of a run. ermite has one user: the owner's web session, root and every
// stored token act for the owner, so they show the owner's name (ERMITE_OWNER),
// not the label of the token, which names a device ("laptop"). Only CI is
// someone else, shown as stored: a job token ("ci job") and the workflows a job
// dispatched ("github-actions[bot]").
func (s *Server) who(stored string) string {
	if strings.HasPrefix(stored, "ci ") || strings.HasSuffix(stored, "[bot]") {
		return visibleText(stored)
	}
	if s.cfg.Owner != "" {
		return s.cfg.Owner
	}
	return "owner"
}

// visibleText is s with every invisible character replaced by its marker,
// for the places that show text as a plain string: paths, names, subjects.
func visibleText(s string) string {
	if !strings.ContainsFunc(s, invisible) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if invisible(r) {
			b.WriteString(marker(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
