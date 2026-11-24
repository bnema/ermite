// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"testing"
	"time"
)

func TestReleaseUrgency(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                   string
		commits, days          int
		more, unknown, unnamed bool
		want                   ReleaseLevel
	}{
		{name: "ten", commits: 10, want: ReleaseDue},
		{name: "nine", commits: 9, want: ReleaseSoon},
		{name: "three-old", commits: 3, days: 31, want: ReleaseDue},
		{name: "two-old", commits: 2, days: 31, want: ReleaseSoon},
		{name: "exact-thirty", commits: 3, days: 30, want: ReleaseSoon},
		{name: "three", commits: 3, want: ReleaseSoon},
		{name: "two", commits: 2, want: ReleaseLater},
		{name: "capped", commits: 1, more: true, want: ReleaseDue},
		{name: "unknown", commits: 2, unknown: true, want: ReleaseLater},
		{name: "unnamed", commits: 2, days: 31, unnamed: true, want: ReleaseLater},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag := TagInfo{Name: "v1", At: now.Add(-time.Duration(tc.days) * 24 * time.Hour)}
			if tc.unknown {
				tag.At = time.Time{}
			}
			if tc.unnamed {
				tag.Name = ""
			}
			if got := ReleaseUrgency(TagDistance{Commits: tc.commits, More: tc.more, Tag: tag}, now); got != tc.want {
				t.Errorf("ReleaseUrgency = %q, want %q", got, tc.want)
			}
		})
	}
}
