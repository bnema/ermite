// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type MirrorRef struct {
	Name string
	ID   ObjectID
} // ID = Ref.ID (tag object for annotated tags)
// MirrorSnapshot is the last pushed set, persisted; Equal makes the sync a no-op.
type MirrorSnapshot struct {
	URL         string
	Refs        []MirrorRef
	AllBranches bool
	At          time.Time
}

// MirrorRefspecs selects refs/heads/<default> (or every refs/heads/* with allBranches) and
// refs/tags/*, from the refs of RepoReader.ListRefs (HEAD skipped, unborn default skipped).
// Names are plain refs; the caller builds "<ref>:<ref>" (never "+", never a delete).
// Refs whose name is not valid UTF-8 are skipped: they cannot be stored in the snapshot.
// Returned sorted by ref name.
func MirrorRefspecs(refs []Ref, defaultBranch string, allBranches bool) []MirrorRef {
	var out []MirrorRef
	for _, r := range refs {
		if r.ID.IsZero() || r.ID.String() == ZeroOID || !utf8.ValidString(r.Name) {
			continue
		}
		if strings.HasPrefix(r.Name, "refs/tags/") || strings.HasPrefix(r.Name, "refs/heads/") && (allBranches || r.Name == "refs/heads/"+defaultBranch) {
			out = append(out, MirrorRef{Name: r.Name, ID: r.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (s MirrorSnapshot) Equal(url string, refs []MirrorRef, allBranches bool) bool {
	if s.URL != url || s.AllBranches != allBranches || len(s.Refs) != len(refs) {
		return false
	}
	counts := make(map[MirrorRef]int, len(refs))
	for _, r := range s.Refs {
		counts[r]++
	}
	for _, r := range refs {
		if counts[r] == 0 {
			return false
		}
		counts[r]--
	}
	return true
}
