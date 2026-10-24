// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"slices"
	"strings"
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
	// Topics are the normalized topics (see NormalizeTopics); nil for none.
	Topics     []string
	Visibility Visibility
	CreatedAt  time.Time
	PushedAt   time.Time // zero until the first push
	ActiveAt   time.Time // latest push, merge request or issue activity
	// Settings are the merge settings; the stores always fill them in, with
	// the defaults for a repository that never stored any.
	Settings RepoSettings
	Archived bool
}

// Limits of the topics of a repository.
const (
	// MaxTopics is the largest number of topics of a repository.
	MaxTopics = 20
	// MaxTopicLen is the longest topic, in characters.
	MaxTopicLen = 50
)

// NormalizeTopics lowercases, validates, deduplicates and sorts topics. A
// topic is 1 to MaxTopicLen characters of a-z, 0-9 and "-", starting with a
// letter or a digit; there are at most MaxTopics of them once deduplicated.
// No topic gives nil. A broken rule gives an error matching ErrInvalid.
func NormalizeTopics(topics []string) ([]string, error) {
	var out []string
	for _, t := range topics {
		t = strings.ToLower(t)
		if !validTopic(t) {
			return nil, fmt.Errorf("%w: topic %q must be 1 to %d characters of a-z, 0-9 and -, starting with a letter or a digit", ErrInvalid, t, MaxTopicLen)
		}
		out = append(out, t)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxTopics {
		return nil, fmt.Errorf("%w: %d topics, at most %d", ErrInvalid, len(out), MaxTopics)
	}
	return out, nil
}

func validTopic(t string) bool {
	if t == "" || len(t) > MaxTopicLen {
		return false
	}
	for i := range len(t) {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// EffectiveSettings returns the repository settings, using defaults for an unset record.
func (r Repo) EffectiveSettings() RepoSettings {
	if r.Settings.IsZero() {
		return DefaultRepoSettings()
	}
	return r.Settings
}
