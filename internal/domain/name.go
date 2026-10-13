// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"strings"
)

// MaxRepoNameLen is the longest repository name, in characters.
const MaxRepoNameLen = 100

// gitSuffix is the suffix of a Git URL and of a repository directory.
const gitSuffix = ".git"

// reservedNames cannot name a repository: they are routes of the server.
var reservedNames = map[string]bool{
	"-":           true,
	"robots.txt":  true,
	"favicon.ico": true,
}

// RepoName is a validated repository name. Only ParseRepoName and
// ParseRepoPath build a non-zero value; the zero value is invalid.
type RepoName struct{ s string }

// String returns the name.
func (n RepoName) String() string { return n.s }

// IsZero reports whether n is the invalid zero value.
func (n RepoName) IsZero() bool { return n.s == "" }

// ParseRepoName validates s as a repository name. It does not strip any
// ".git" suffix: a name ending in ".git" is invalid.
//
// The error matches ErrInvalidName and is an *InvalidNameError.
func ParseRepoName(s string) (RepoName, error) {
	reason := nameProblem(s)
	if reason == "" {
		return RepoName{s: s}, nil
	}
	e := &InvalidNameError{Name: s, Reason: reason}
	if lower := strings.ToLower(s); lower != s && nameProblem(lower) == "" {
		e.Suggestion = lower
	}
	return RepoName{}, e
}

// ParseRepoPath parses the repository segment of a URL path. It strips one
// trailing ".git" before validating, so "tools.git" names "tools".
func ParseRepoPath(segment string) (RepoName, error) {
	return ParseRepoName(strings.TrimSuffix(segment, gitSuffix))
}

// InvalidNameError describes why a repository name is invalid. It matches
// ErrInvalidName.
type InvalidNameError struct {
	Name       string
	Reason     string
	Suggestion string // a valid alternative, empty when there is none
}

// Error implements error.
func (e *InvalidNameError) Error() string {
	msg := fmt.Sprintf("invalid repository name %q: %s", e.Name, e.Reason)
	if e.Suggestion != "" {
		msg += fmt.Sprintf(" (try %q)", e.Suggestion)
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrInvalidName) true.
func (e *InvalidNameError) Unwrap() error { return ErrInvalidName }

// nameProblem returns why s is not a valid name, or "" when it is valid.
func nameProblem(s string) string {
	if s == "" {
		return "name is empty"
	}
	if len(s) > MaxRepoNameLen {
		return fmt.Sprintf("name is longer than %d characters", MaxRepoNameLen)
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			return "uppercase letters are not allowed"
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Sprintf("character %q is not allowed (use a-z, 0-9, '.', '_', '-')", r)
		}
	}
	switch {
	case strings.HasPrefix(s, ".") || strings.HasSuffix(s, "."):
		return "name must not start or end with '.'"
	case strings.Contains(s, ".."):
		return "name must not contain '..'"
	case strings.HasSuffix(s, gitSuffix):
		return "name must not end in '.git'"
	case reservedNames[s]:
		return "name is reserved"
	}
	return ""
}

// OwnerMatches reports whether name is the owner of the forge, as GitHub
// matches the owner of a repository: without regard to case. It is the one
// rule for the owner segment of the Git URLs, of the REST subset and of the
// references of actions. configured is ERMITE_OWNER, which is lower case (it
// follows the rules of repository names); an empty one matches nothing.
func OwnerMatches(configured, name string) bool {
	return configured != "" && strings.EqualFold(configured, name)
}
