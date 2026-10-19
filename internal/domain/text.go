// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits of the plain text of a merge request (ADR 0017), in characters.
const (
	// MaxTitleLen is the longest merge request title.
	MaxTitleLen = 120
	// MaxTextLen is the longest description or comment.
	MaxTextLen = 8000
)

// ErrInvalidText means a text breaks the rules of ParseMRTitle or
// ParsePlainText.
var ErrInvalidText = errors.New("invalid text")

// TextError says why a text is invalid. It matches ErrInvalidText.
type TextError struct {
	// Reason completes the sentence "the text ...", for example "is not valid
	// UTF-8".
	Reason string
}

// Error implements error.
func (e *TextError) Error() string { return "text " + e.Reason }

// Unwrap makes errors.Is(err, ErrInvalidText) true.
func (e *TextError) Unwrap() error { return ErrInvalidText }

// MRTitle is a validated merge request title: one line of plain text, at most
// MaxTitleLen characters. Only ParseMRTitle builds a non-zero value.
type MRTitle struct{ s string }

// ParseMRTitle validates s: not blank, valid UTF-8, one line, at most
// MaxTitleLen characters, no control character and no bidirectional override
// or isolate character.
func ParseMRTitle(s string) (MRTitle, error) {
	if reason := textProblem(s, false, MaxTitleLen); reason != "" {
		return MRTitle{}, &TextError{Reason: reason}
	}
	if isBlank(s) {
		return MRTitle{}, &TextError{Reason: "is empty"}
	}
	return MRTitle{s: s}, nil
}

// String returns the title.
func (t MRTitle) String() string { return t.s }

// IsZero reports whether t is the zero value.
func (t MRTitle) IsZero() bool { return t.s == "" }

// PlainText is a validated multi-line plain text, such as a description or a
// comment: valid UTF-8, at most MaxTextLen characters, line breaks allowed,
// no other control character, no U+2028 or U+2029, no bidirectional mark,
// override or isolate character.
// The zero value is the empty text.
type PlainText struct{ s string }

// ParsePlainText validates s. The empty text is valid: a caller that needs
// text checks IsBlank.
func ParsePlainText(s string) (PlainText, error) {
	if reason := textProblem(s, true, MaxTextLen); reason != "" {
		return PlainText{}, &TextError{Reason: reason}
	}
	return PlainText{s: s}, nil
}

// String returns the text.
func (t PlainText) String() string { return t.s }

// IsBlank reports whether the text is empty or only white space.
func (t PlainText) IsBlank() bool { return isBlank(t.s) }

func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// textProblem returns why s breaks the rules, or "".
func textProblem(s string, multiline bool, maxLen int) string {
	if !utf8.ValidString(s) {
		return "is not valid UTF-8"
	}
	if n := utf8.RuneCountInString(s); n > maxLen {
		return fmt.Sprintf("is longer than %d characters (%d)", maxLen, n)
	}
	for _, r := range s {
		switch {
		case r == '\n':
			if !multiline {
				return "must be one line"
			}
		case unicode.IsControl(r):
			return fmt.Sprintf("contains the control character U+%04X", r)
		case r == '\u2028' || r == '\u2029':
			return fmt.Sprintf("contains the line or paragraph separator U+%04X", r)
		case isBidiControl(r):
			return fmt.Sprintf("contains the bidirectional override or isolate character U+%04X", r)
		}
	}
	return ""
}

// isBidiControl reports whether r is a bidirectional mark, embedding, override
// or isolate character (U+200E, U+200F, U+061C, U+202A to U+202E, U+2066 to
// U+2069), which can make text read differently from how it is stored.
func isBidiControl(r rune) bool {
	return r == 0x200e || r == 0x200f || r == 0x061c ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// FieldViolation names a request field that breaks a rule.
type FieldViolation struct {
	// Field is the name of the field in the API contract, for example
	// "title".
	Field string
	// Reason says what is wrong with it.
	Reason string
}

// InvalidFieldsError is a request rejected whole because of its fields. It
// matches ErrInvalid.
type InvalidFieldsError struct {
	Fields []FieldViolation
}

// Error implements error.
func (e *InvalidFieldsError) Error() string {
	msg := "invalid argument"
	for i, f := range e.Fields {
		sep := "; "
		if i == 0 {
			sep = ": "
		}
		msg += sep + f.Field + " " + f.Reason
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrInvalid) true.
func (e *InvalidFieldsError) Unwrap() error { return ErrInvalid }

// Violations collects the violations of a request.
type Violations struct{ list []FieldViolation }

// Add records that field breaks a rule.
func (v *Violations) Add(field, reason string) {
	v.list = append(v.list, FieldViolation{Field: field, Reason: reason})
}

// AddErr records an error of a parser for field: the reason of a *TextError,
// else the text of the error.
func (v *Violations) AddErr(field string, err error) {
	var te *TextError
	if errors.As(err, &te) {
		v.Add(field, te.Reason)
		return
	}
	v.Add(field, err.Error())
}

// Err returns nil without violations, else an *InvalidFieldsError.
func (v *Violations) Err() error {
	if len(v.list) == 0 {
		return nil
	}
	return &InvalidFieldsError{Fields: append([]FieldViolation(nil), v.list...)}
}

// Limits of the text of an issue, in code points.
const (
	// MaxIssueTitleLen is the longest issue title.
	MaxIssueTitleLen = 120
	// MaxIssueBodyLen is the longest issue body or issue comment.
	MaxIssueBodyLen = 10000
	// MaxAuthorLabelLen is the longest author label.
	MaxAuthorLabelLen = 40
)

// IssueTitle is a validated issue title: one line, 1 to MaxIssueTitleLen code
// points, normalized. Only ParseIssueTitle builds a non-zero value.
type IssueTitle struct{ s string }

// String returns the title.
func (t IssueTitle) String() string { return t.s }

// IsZero reports whether t is the zero value.
func (t IssueTitle) IsZero() bool { return t.s == "" }

// IssueBody is a validated issue body or issue comment: at most
// MaxIssueBodyLen code points, normalized, line breaks and tabs allowed. The
// zero value is the empty text.
type IssueBody struct{ s string }

// String returns the text.
func (b IssueBody) String() string { return b.s }

// IsBlank reports whether the text is empty or only white space.
func (b IssueBody) IsBlank() bool { return isBlank(b.s) }

// ParseIssueTitle cleans s (see cleanText) and validates it: one line, not
// blank, at most MaxIssueTitleLen code points. A line break is refused, not
// removed, so that two lines are never glued into one.
func ParseIssueTitle(s string) (IssueTitle, error) {
	if !utf8.ValidString(s) {
		return IssueTitle{}, &TextError{Reason: "is not valid UTF-8"}
	}
	if strings.ContainsAny(s, "\r\n\u2028\u2029") {
		return IssueTitle{}, &TextError{Reason: "must be one line"}
	}
	c := strings.TrimSpace(cleanText(s, false))
	if c == "" {
		return IssueTitle{}, &TextError{Reason: "is empty"}
	}
	if n := utf8.RuneCountInString(c); n > MaxIssueTitleLen {
		return IssueTitle{}, &TextError{Reason: fmt.Sprintf("is longer than %d characters (%d)", MaxIssueTitleLen, n)}
	}
	return IssueTitle{s: c}, nil
}

// ParseIssueBody cleans s (see cleanText, line breaks and tabs are kept) and
// validates it: at most MaxIssueBodyLen code points. The empty text is valid.
func ParseIssueBody(s string) (IssueBody, error) {
	if !utf8.ValidString(s) {
		return IssueBody{}, &TextError{Reason: "is not valid UTF-8"}
	}
	c := cleanText(s, true)
	if n := utf8.RuneCountInString(c); n > MaxIssueBodyLen {
		return IssueBody{}, &TextError{Reason: fmt.Sprintf("is longer than %d characters (%d)", MaxIssueBodyLen, n)}
	}
	return IssueBody{s: c}, nil
}

// ParseAuthorLabel cleans s like a title (one line) and validates it: at most
// MaxAuthorLabelLen code points. The empty label is valid.
func ParseAuthorLabel(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", &TextError{Reason: "is not valid UTF-8"}
	}
	if strings.ContainsAny(s, "\r\n\u2028\u2029") {
		return "", &TextError{Reason: "must be one line"}
	}
	c := strings.TrimSpace(cleanText(s, false))
	if n := utf8.RuneCountInString(c); n > MaxAuthorLabelLen {
		return "", &TextError{Reason: fmt.Sprintf("is longer than %d characters (%d)", MaxAuthorLabelLen, n)}
	}
	return c, nil
}

// cleanText normalizes s to NFC and removes the control characters (C0, C1 and
// DEL; with multiline, line feeds and tabs stay) and the bidirectional marks,
// embeddings, overrides and isolates. s must be valid UTF-8.
func cleanText(s string, multiline bool) string {
	s = norm.NFC.String(s)
	if multiline {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case multiline && (r == '\n' || r == '\t'):
			return r
		case unicode.IsControl(r), isBidiControl(r), r == '\u2028' || r == '\u2029':
			return -1
		}
		return r
	}, s)
}
