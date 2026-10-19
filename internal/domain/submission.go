// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrSubmissionRefused is matched by every refusal of an anonymous issue
// submission, whatever the class. Unknown or closed repositories answer
// ErrNotFound instead, and a text that breaks the rules a *TextError.
var ErrSubmissionRefused = errors.New("submission refused")

var (
	// ErrSubmissionRateLimited means the client submitted too often; the
	// error is a *SubmissionRetryError that says when to retry.
	ErrSubmissionRateLimited = fmt.Errorf("%w: rate limited", ErrSubmissionRefused)
	// ErrSubmissionsPaused means too many submissions await moderation (or
	// the server cannot track more challenges): nobody may submit for now.
	ErrSubmissionsPaused = fmt.Errorf("%w: submissions paused", ErrSubmissionRefused)
	// ErrSubmissionInvalid covers every bot, token, proof-of-work, timing and
	// honeypot failure. It is deliberately one error: it must not tell a bot
	// which check it failed.
	ErrSubmissionInvalid = fmt.Errorf("%w: invalid submission", ErrSubmissionRefused)
)

// SubmissionRetryError is the ErrSubmissionRateLimited of a client that may
// try again after the given delay.
type SubmissionRetryError struct {
	After time.Duration
}

// Error implements error.
func (e *SubmissionRetryError) Error() string {
	return fmt.Sprintf("%v: retry after %s", ErrSubmissionRateLimited, e.After.Round(time.Second))
}

// Unwrap makes errors.Is match ErrSubmissionRateLimited and
// ErrSubmissionRefused.
func (e *SubmissionRetryError) Unwrap() error { return ErrSubmissionRateLimited }
