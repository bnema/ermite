// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"bytes"
	"net/http"
	"path"
	"strings"
)

// SniffContent sets Binary and MediaType from the first bytes of the content
// (Path must be set). Only the first BinarySniffLen bytes count, so a caller
// may pass the whole content or just its head. Every reader of blobs applies
// this one rule.
func (b *BlobInfo) SniffContent(content []byte) {
	head := content[:min(len(content), BinarySniffLen)]
	b.Binary = bytes.IndexByte(head, 0) >= 0
	b.MediaType = mediaType(b.Path, head, b.Binary)
}

// textTypes maps the extensions whose media type is more precise than what
// the content shows. It is fixed here so that the answer does not depend on the
// MIME tables of the host.
var textTypes = map[string]string{
	".html": "text/html", ".htm": "text/html", ".css": "text/css",
	".js": "text/javascript", ".mjs": "text/javascript",
	".json": "application/json", ".xml": "application/xml",
	".svg": "image/svg+xml", ".md": "text/markdown", ".csv": "text/csv",
}

// mediaType guesses a media type without parameters. What the content shows
// (images, archives, PDF…) wins; for plain text the file name may refine it
// from textTypes, but a name never makes binary content text.
func mediaType(name string, head []byte, binary bool) string {
	sniffed, _, _ := strings.Cut(http.DetectContentType(head), ";")
	switch {
	case binary && strings.HasPrefix(sniffed, "text/"):
		return "application/octet-stream"
	case sniffed != "text/plain" && sniffed != "text/xml" && sniffed != "application/octet-stream":
		return sniffed
	case binary:
		return "application/octet-stream"
	}
	if t, ok := textTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "text/plain"
}

// ValidGitText replaces the bytes of s that are not valid UTF-8 with U+FFFD:
// names, emails and messages of commits may hold any bytes.
func ValidGitText(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// SplitCommitMessage turns the raw message of a commit into Commit.Message
// (valid UTF-8, no trailing line breaks) and Commit.Summary (its first line).
func SplitCommitMessage(raw string) (message, summary string) {
	message = strings.TrimRight(ValidGitText(raw), "\n")
	summary, _, _ = strings.Cut(message, "\n")
	return message, summary
}
