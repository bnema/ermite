// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "errors"

// Sentinel errors of the domain. Callers match them with errors.Is.
var (
	// ErrNotFound means the entity does not exist or is hidden from the caller.
	ErrNotFound = errors.New("not found")
	// ErrInvalidName means a repository name breaks the naming rules.
	ErrInvalidName = errors.New("invalid repository name")
	// ErrInvalidVisibility means a visibility value is neither public nor private.
	ErrInvalidVisibility = errors.New("invalid visibility")
	// ErrInvalidScope means a scope is not one of the known scopes.
	ErrInvalidScope = errors.New("invalid scope")
	// ErrInvalidToken means the settings of a token are not acceptable.
	ErrInvalidToken = errors.New("invalid token")
	// ErrAlreadyExists means the entity to create already exists.
	ErrAlreadyExists = errors.New("already exists")
	// ErrUnauthenticated means the operation needs credentials the caller lacks.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden means the caller is known but may not do this.
	ErrForbidden = errors.New("forbidden")
	// ErrRejected means a push was refused by policy.
	ErrRejected = errors.New("rejected")
	// ErrConflict means a concurrent change prevented the operation.
	ErrConflict = errors.New("conflict")
	// ErrInvalid means an argument of a use case is not acceptable and no
	// more specific error says why: an empty update, an unknown topic, a
	// description that breaks the rules.
	ErrInvalid = errors.New("invalid argument")
	// ErrPreconditionFailed means the state of the forge does not allow the
	// operation, for example a default branch that does not exist.
	ErrPreconditionFailed = errors.New("failed precondition")
	// ErrResourceExhausted means a limit of the server is reached, for example
	// the number of event watchers; the caller may retry later.
	ErrResourceExhausted = errors.New("resource exhausted")
)

// invalidErrors are the errors that mean a request was malformed.
var invalidErrors = []error{
	ErrInvalid, ErrInvalidName, ErrInvalidVisibility,
	ErrInvalidScope, ErrInvalidToken,
	ErrInvalidObjectID, ErrInvalidRevision, ErrInvalidPath,
	ErrInvalidPageToken, ErrInvalidPageSize, ErrInvalidRefKind,
	ErrNotDirectory, ErrNotFile, ErrInvalidText,
}

// IsInvalid reports whether err means that the request was malformed: an
// invalid name, revision, path, page token, text, and the like. The adapters
// answer all of them alike (invalid_argument on the API, the 404 page on the
// web).
func IsInvalid(err error) bool {
	for _, invalid := range invalidErrors {
		if errors.Is(err, invalid) {
			return true
		}
	}
	return false
}
