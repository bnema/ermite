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
)
