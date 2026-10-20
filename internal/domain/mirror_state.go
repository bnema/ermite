// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"strings"
	"time"
)

type MirrorState string

const (
	MirrorDisabled MirrorState = "disabled"
	MirrorOK       MirrorState = "ok"
	MirrorFailing  MirrorState = "failing"
	MirrorPaused   MirrorState = "paused"
)

type MirrorFailureClass string

const (
	MirrorClassNone           MirrorFailureClass = ""
	MirrorClassDiverged       MirrorFailureClass = "diverged"         // non-fast-forward / fetch-first -> paused
	MirrorClassAuthDenied     MirrorFailureClass = "auth_denied"      // "Permission denied (publickey)" -> paused
	MirrorClassRemoteNotFound MirrorFailureClass = "remote_not_found" // "Repository not found", "does not appear to be a git repository" -> paused
	MirrorClassHostKey        MirrorFailureClass = "host_key"         // known_hosts mismatch -> paused
	MirrorClassTransient      MirrorFailureClass = "transient"        // timeout, network, exit without a known marker -> backoff
	MirrorClassInternal       MirrorFailureClass = "internal"         // no key, no secrets key, local git error -> backoff
)

// MirrorError is what the transport returns: Class decides the transition, Detail (capped,
// URL-redacted stderr tail) is shown to admins only. Unwrap() yields ErrUnavailable for
// transient, ErrPreconditionFailed for the paused classes.
type MirrorError struct {
	Class  MirrorFailureClass
	Detail string
}

// MirrorStatus is the persisted per-repository state.
type MirrorStatus struct {
	Repo                RepoName
	State               MirrorState
	Class               MirrorFailureClass
	Reason              string // MirrorReason(...) output, human
	Detail              string // last error detail (admin only)
	ConsecutiveFailures int
	LastAttemptAt       time.Time
	LastSuccessAt       time.Time
	NextRetryAt         time.Time // zero unless Failing
	PausedAt            time.Time
	UpdatedAt           time.Time
}
type MirrorOutcome struct {
	Err  *MirrorError
	NoOp bool
} // Err nil == success
type MirrorTransition struct {
	Status      MirrorStatus
	FailedEvent bool // State moved into Failing or Paused from OK/Disabled: once per incident
	Recovered   bool // State moved from Failing/Paused to OK
}

const (
	MirrorBackoffBase = 5 * time.Minute
	MirrorBackoffCap  = 6 * time.Hour
	MirrorPauseAfter  = 5 // consecutive transient failures
)

func (e *MirrorError) Error() string { return fmt.Sprintf("mirror %s: %s", e.Class, e.Detail) }
func mirrorFatal(c MirrorFailureClass) bool {
	return c == MirrorClassDiverged || c == MirrorClassAuthDenied || c == MirrorClassRemoteNotFound || c == MirrorClassHostKey
}
func (e *MirrorError) Unwrap() error {
	if mirrorFatal(e.Class) {
		return ErrPreconditionFailed
	}
	return ErrUnavailable
}

// ClassifyMirrorOutput maps git/ssh stderr to a class (table of substrings, tested).
func ClassifyMirrorOutput(stderr string, exitErr error, timedOut bool) MirrorFailureClass {
	if timedOut {
		return MirrorClassTransient
	}
	s := strings.ToLower(stderr)
	for _, entry := range []struct {
		class   MirrorFailureClass
		markers []string
	}{
		{MirrorClassHostKey, []string{"host key verification failed", "remote host identification has changed", "host key has changed"}},
		{MirrorClassAuthDenied, []string{"permission denied (publickey)", "authentication failed"}},
		{MirrorClassRemoteNotFound, []string{"repository not found", "does not appear to be a git repository"}},
		{MirrorClassDiverged, []string{"non-fast-forward", "fetch first", "fetch-first"}},
	} {
		for _, m := range entry.markers {
			if strings.Contains(s, m) {
				return entry.class
			}
		}
	}
	if exitErr != nil {
		return MirrorClassTransient
	}
	return MirrorClassNone
}
func MirrorBackoff(n int) time.Duration {
	d := MirrorBackoffBase
	for i := 1; i < n && d < MirrorBackoffCap; i++ {
		d *= 2
	}
	return min(d, MirrorBackoffCap)
}

// MirrorDisable/MirrorReset are the config-change transitions: reset lifts Paused/Failing to OK
// with counters cleared (config changed); disable -> Disabled. sync --force does not reset: the
// attempt is judged against the previous status so a recovery still reports Recovered.
func MirrorReset(p MirrorStatus, now time.Time) MirrorStatus {
	p.State = MirrorOK
	p.Class = MirrorClassNone
	p.Reason = ""
	p.Detail = ""
	p.ConsecutiveFailures = 0
	p.NextRetryAt = time.Time{}
	p.PausedAt = time.Time{}
	p.UpdatedAt = now
	return p
}
func MirrorDisable(p MirrorStatus, now time.Time) MirrorStatus {
	p = MirrorReset(p, now)
	p.State = MirrorDisabled
	return p
}

// ApplyMirrorOutcome is the single transition function (pure):
//
//	success  -> OK, counters cleared, LastSuccessAt=now (NoOp also sets LastSuccessAt).
//	fatal class (Diverged/AuthDenied/RemoteNotFound/HostKey) -> Paused at once.
//	Transient/Internal -> Failing, n++, NextRetryAt=now+MirrorBackoff(n); n>=MirrorPauseAfter -> Paused.
//	A Paused status receiving an outcome was synced with force: it is judged like any other.
func ApplyMirrorOutcome(prev MirrorStatus, out MirrorOutcome, now time.Time) MirrorTransition {
	p := MirrorReset(prev, now)
	p.LastAttemptAt = now
	if out.Err == nil {
		p.LastSuccessAt = now
		return MirrorTransition{Status: p, Recovered: prev.State == MirrorFailing || prev.State == MirrorPaused}
	}
	p.Class = out.Err.Class
	p.Detail = out.Err.Detail
	p.ConsecutiveFailures = prev.ConsecutiveFailures + 1
	p.State = MirrorFailing
	if mirrorFatal(p.Class) || p.ConsecutiveFailures >= MirrorPauseAfter {
		p.State = MirrorPaused
		p.PausedAt = now
		if prev.State == MirrorPaused && !prev.PausedAt.IsZero() {
			p.PausedAt = prev.PausedAt
		}
	} else {
		p.NextRetryAt = now.Add(MirrorBackoff(p.ConsecutiveFailures))
	}
	p.Reason = MirrorReason(p)
	return MirrorTransition{Status: p, FailedEvent: prev.State != MirrorFailing && prev.State != MirrorPaused}
}

// MirrorReason is the human sentence of a status, e.g.
//
//	diverged: "the remote has commits this repository does not: push is never forced; fix the remote and run `ermite mirror sync <repo> --force`"
//	auth_denied: "the remote refused the mirror key: add the output of `ermite mirror key show` to the remote"
//	remote_not_found: "the remote repository does not exist or is not reachable with this key"
//	host_key: "the host key of the remote changed: check <data dir>/mirror_known_hosts"
//	transient: "the last %d attempts failed (%s); next retry %s"
//	paused after transient: "paused after %d consecutive failures; run `ermite mirror sync <repo> --force`"
func MirrorReason(s MirrorStatus) string {
	if s.State != MirrorFailing && s.State != MirrorPaused {
		return ""
	}
	switch s.Class {
	case MirrorClassDiverged:
		return "the remote has commits this repository does not: push is never forced; fix the remote and run `ermite mirror sync <repo> --force`"
	case MirrorClassAuthDenied:
		return "the remote refused the mirror key: add the output of `ermite mirror key show` to the remote"
	case MirrorClassRemoteNotFound:
		return "the remote repository does not exist or is not reachable with this key"
	case MirrorClassHostKey:
		return "the host key of the remote changed: check <data dir>/mirror_known_hosts"
	}
	if s.State == MirrorPaused {
		return fmt.Sprintf("paused after %d consecutive failures; run `ermite mirror sync <repo> --force`", s.ConsecutiveFailures)
	}
	return fmt.Sprintf("the last %d attempts failed (%s); next retry %s", s.ConsecutiveFailures, s.Class, s.NextRetryAt.UTC().Format(time.RFC3339))
}
