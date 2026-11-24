// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "time"

// ReleaseLevel tells how urgently a repository ahead of its nearest tag needs
// a release.
type ReleaseLevel string

const (
	ReleaseDue   ReleaseLevel = "due"
	ReleaseSoon  ReleaseLevel = "soon"
	ReleaseLater ReleaseLevel = "later"
)

const (
	releaseDueCommits  = 10
	releaseSoonCommits = 3
	// releaseOldTagAge is the age past which a tag counts as old.
	releaseOldTagAge = 30 * 24 * time.Hour
)

// ReleaseUrgency rates the commits since the nearest tag: due from ten
// commits, from three on an old tag, or when the count hit its limit; soon
// from three commits or on an old tag; later otherwise. A tag with no name or
// date is never old.
func ReleaseUrgency(d TagDistance, now time.Time) ReleaseLevel {
	old := d.Tag.Name != "" && !d.Tag.At.IsZero() && now.Sub(d.Tag.At) > releaseOldTagAge
	switch {
	case d.More || d.Commits >= releaseDueCommits || (old && d.Commits >= releaseSoonCommits):
		return ReleaseDue
	case d.Commits >= releaseSoonCommits || old:
		return ReleaseSoon
	default:
		return ReleaseLater
	}
}
