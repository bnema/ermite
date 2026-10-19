// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of the public issue form (ADR 0031).
const (
	// MaxPendingIssues is the most issues awaiting moderation, over every
	// repository. Past it submissions are refused.
	MaxPendingIssues = 200
	// SubmitPerHour and SubmitPerDay are the submissions one client may make
	// in a sliding hour and a sliding day.
	SubmitPerHour = 3
	SubmitPerDay  = 10
	// BanDuration is how long a rejected submitter stays banned.
	BanDuration = 30 * 24 * time.Hour
	// MaxPendingAuthorLen is the longest display name of a submitter, in
	// code points.
	MaxPendingAuthorLen = 40
	// PendingIssueIDLen is the length of a pending issue id: 16 random bytes
	// in unpadded lowercase base32.
	PendingIssueIDLen = 26
	// IPHashLen is the size of the HMAC of a client key.
	IPHashLen = 32
)

var pendingIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// PendingIssueID identifies an issue awaiting moderation. It is random and
// opaque, not a number: a pending issue has none yet.
type PendingIssueID string

// NewPendingIssueID returns a fresh random id.
func NewPendingIssueID() (PendingIssueID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate pending issue id: %w", err)
	}
	return PendingIssueID(strings.ToLower(pendingIDEncoding.EncodeToString(b[:]))), nil
}

// ParsePendingIssueID accepts exactly what NewPendingIssueID produces: 26
// characters of a-z and 2-7. Anything else gives an error wrapping ErrInvalid.
func ParsePendingIssueID(s string) (PendingIssueID, error) {
	if len(s) != PendingIssueIDLen {
		return "", fmt.Errorf("%w: pending issue id", ErrInvalid)
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return "", fmt.Errorf("%w: pending issue id", ErrInvalid)
		}
	}
	// 26 base32 characters carry 130 bits; the last one must not set the two
	// spare bits, or the id would not round-trip.
	raw, err := pendingIDEncoding.DecodeString(strings.ToUpper(s))
	if err != nil || len(raw) != 16 || strings.ToLower(pendingIDEncoding.EncodeToString(raw)) != s {
		return "", fmt.Errorf("%w: pending issue id", ErrInvalid)
	}
	return PendingIssueID(s), nil
}

// String returns the id as text.
func (id PendingIssueID) String() string { return string(id) }

// ParsePendingAuthor validates the optional display name of a submitter: valid
// UTF-8, one line, at most MaxPendingAuthorLen code points, normalized.
// The empty name is valid.
func ParsePendingAuthor(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", &TextError{Reason: "is not valid UTF-8"}
	}
	c := cleanText(s, false)
	if n := utf8.RuneCountInString(c); n > MaxPendingAuthorLen {
		return "", &TextError{Reason: fmt.Sprintf("is longer than %d characters (%d)", MaxPendingAuthorLen, n)}
	}
	return c, nil
}

// AnonymousAuthor labels an approved issue whose submitter gave no name.
const AnonymousAuthor = "anonymous"

// ApprovedIssue is the open, external issue a pending submission becomes when
// the owner approves it at now. The store sets the number. A submitter who gave
// no name is AnonymousAuthor; the issue keeps the submission time as its
// creation time.
func ApprovedIssue(p PendingIssue, now time.Time) Issue {
	author := p.Author
	if author == "" {
		author = AnonymousAuthor
	}
	return Issue{
		Repo: p.Repo, Title: p.Title, Body: p.Body, State: IssueStateOpen,
		Author: author, CreatedAt: p.CreatedAt, UpdatedAt: now, External: true,
	}
}

// PendingIssue is an issue submitted through the public form, awaiting the
// owner's decision. It has no number: approval gives it one.
type PendingIssue struct {
	ID    PendingIssueID
	Repo  RepoName
	Title IssueTitle
	Body  IssueBody
	// Author is the display name the submitter gave; it may be empty and is
	// not verified.
	Author    string
	CreatedAt time.Time
	// IPHash is the HMAC-SHA256 of the client key, IPHashLen bytes. It lets
	// the owner ban a submitter without the server keeping an address.
	IPHash []byte
	// Version is set by the store on read; opaque to everyone else.
	Version uint64
}
