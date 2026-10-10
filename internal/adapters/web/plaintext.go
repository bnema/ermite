// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"net/url"
	"regexp"
	"strings"
)

// TextPart is a run of plain text, or a link found in it.
type TextPart struct {
	Text string
	// Href is the target of a link, "" for plain text. Only http and https
	// URLs are ever linked.
	Href string
}

// reURL finds the candidates for a link in plain text.
var reURL = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// linkTrail is what ends a sentence rather than an address.
const linkTrail = ".,;:!?)]}"

// plainParts splits a user text (a description, a comment) into runs: the text
// stays text, escaped by the template with its line breaks kept, and the
// http(s) URLs in it become links (ADR 0017: "text is plain everywhere"). The
// invisible characters of the text are shown as markers.
func plainParts(s string) []TextPart {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	var out []TextPart
	last := 0
	for _, loc := range reURL.FindAllStringIndex(s, -1) {
		raw := s[loc[0]:loc[1]]
		// A closing bracket belongs to the address only when it has an opening
		// one in it: "(see https://x.test/a)" ends before the ")".
		end := len(raw)
		for end > 0 && strings.ContainsRune(linkTrail, rune(raw[end-1])) {
			if raw[end-1] == ')' && strings.Count(raw[:end], "(") >= strings.Count(raw[:end], ")") {
				break
			}
			end--
		}
		raw = raw[:end]
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			continue
		}
		if loc[0] > last {
			out = append(out, TextPart{Text: visibleText(s[last:loc[0]])})
		}
		out = append(out, TextPart{Text: visibleText(raw), Href: u.String()})
		last = loc[0] + len(raw)
	}
	if last < len(s) {
		out = append(out, TextPart{Text: visibleText(s[last:])})
	}
	return out
}
