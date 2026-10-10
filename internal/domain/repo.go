// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"time"
)

// Visibility is the access level of a repository.
type Visibility string

// Visibility values.
const (
	// VisibilityPrivate hides the repository from anonymous callers.
	VisibilityPrivate Visibility = "private"
	// VisibilityPublic lets anonymous callers read the repository.
	VisibilityPublic Visibility = "public"
)

// ParseVisibility parses "public" or "private".
func ParseVisibility(s string) (Visibility, error) {
	switch v := Visibility(s); v {
	case VisibilityPublic, VisibilityPrivate:
		return v, nil
	}
	return "", fmt.Errorf("%w: %q (want public or private)", ErrInvalidVisibility, s)
}

// String returns the visibility as text.
func (v Visibility) String() string { return string(v) }

// Repo is the metadata ermite owns about a repository. Git-owned state (refs,
// HEAD) stays in Git.
type Repo struct {
	Name        RepoName
	Description string
	Visibility  Visibility
	CreatedAt   time.Time
	PushedAt    time.Time // zero until the first push
}
