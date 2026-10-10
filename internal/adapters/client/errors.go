// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"errors"
	"regexp"
	"strings"

	"connectrpc.com/connect"
)

// redirected matches the message Connect gives to a 3xx answer.
var redirected = regexp.MustCompile(`\b30[1-8]\b`)

// maxMessage caps a server message echoed to the terminal.
const maxMessage = 300

// Error is a failed call described for a person: one line, with a hint when
// the code points to a likely cause. It wraps the original error.
type Error struct {
	msg string
	err error
}

// Error implements error.
func (e *Error) Error() string { return e.msg }

// Unwrap returns the original error, a *connect.Error for server answers.
func (e *Error) Unwrap() error { return e.err }

// Describe turns an error of a call into a friendly one-line *Error. Errors
// that are not Connect errors are returned unchanged, nil stays nil.
func (c *Client) Describe(err error) error {
	var ce *connect.Error
	if err == nil || !errors.As(err, &ce) {
		return err
	}
	msg := oneLine(ce.Message())
	with := func(prefix string) string {
		if msg == "" {
			return prefix
		}
		return prefix + ": " + msg
	}
	var text string
	switch ce.Code() {
	case connect.CodeUnauthenticated:
		if c.cfg.Token == "" {
			text = "authentication required: set " + EnvToken + " to a valid token (create one with `ermite token create`)"
		} else {
			text = "not authenticated: " + EnvToken + " is invalid, expired or revoked (create a new one with `ermite token create`)"
		}
	case connect.CodePermissionDenied:
		text = with("permission denied") + " (the token lacks a required scope, see `ermite token list`)"
	case connect.CodeNotFound:
		text = with("not found")
	case connect.CodeAlreadyExists:
		text = with("already exists")
	case connect.CodeInvalidArgument:
		text = with("invalid request")
	case connect.CodeFailedPrecondition:
		text = with("precondition failed")
	case connect.CodeUnavailable:
		text = "cannot reach the server at " + c.cfg.URL
		if msg != "" {
			text += ": " + msg
		}
	case connect.CodeDeadlineExceeded:
		text = "the request to " + c.cfg.URL + " timed out"
	case connect.CodeCanceled:
		text = "canceled"
	case connect.CodeUnimplemented:
		text = "the server does not implement this call: check that " + EnvURL + " points to an ermite server, and its version with `ermite version`"
	case connect.CodeResourceExhausted:
		text = with("too many requests")
	case connect.CodeInternal:
		text = with("server error")
	default:
		text = with("unexpected server response")
		if redirected.MatchString(msg) {
			text += " (the server redirected the request: set " + EnvURL + " to the final address, probably https://)"
		}
	}
	return &Error{msg: text, err: err}
}

// oneLine collapses whitespace and bounds the length of a server message.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxMessage {
		s = strings.ToValidUTF8(s[:maxMessage], "") + "…"
	}
	return s
}
