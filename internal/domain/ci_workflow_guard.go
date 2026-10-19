// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "fmt"

// ObjectType is the type of a peeled Git object.
type ObjectType string

// Peeled object types accepted by the push tree reader.
const (
	ObjectCommit ObjectType = "commit"
	ObjectTree   ObjectType = "tree"
	ObjectBlob   ObjectType = "blob"
)

// PushWalkBounds bounds the walk over the commits of a push.
type PushWalkBounds struct{ Commits, IDs int }

// Bounds fail closed and count repeated parent IDs too.
const (
	MaxPushedCommits = 10000
	MaxPushedIDs     = 4 * MaxPushedCommits
)

// DefaultPushWalkBounds returns the job-token push walk limits.
func DefaultPushWalkBounds() PushWalkBounds { return PushWalkBounds{MaxPushedCommits, MaxPushedIDs} }

// PushedEnd describes a peeled ref endpoint.
type PushedEnd struct{ Type ObjectType }

// Subtree is the kind and identity of an entry at a tree path.
type Subtree struct {
	Kind EntryKind
	ID   ObjectID
}

// PushedCommit is a newly reachable commit with its ordered parents.
type PushedCommit struct {
	ID      ObjectID
	Parents []ObjectID
}

// HeadKey identifies the default branch head in PushSubtrees.Subtrees.
const HeadKey = "HEAD"

// PushSubtrees contains endpoint types and entries keyed by original object ID
// (or HeadKey), and newly reachable commits in rev-list order. Absent entries
// mean there is no subtree at the requested path.
type PushSubtrees struct {
	Ends     map[string]PushedEnd
	Commits  []PushedCommit
	Subtrees map[string]Subtree
}

// WorkflowTreeChange is the first forbidden workflow-tree replacement.
// A zero Old denotes no workflow directory.
type WorkflowTreeChange struct{ Old, New ObjectID }

// WorkflowChangeOf applies the job-token workflow policy. Every new commit must
// keep the workflows of at least one parent; then each ref must keep its old
// workflows (HEAD's for a new ref). Disappearance and ref deletion are allowed.
// New commits are judged before ref ends, in their supplied order. Missing ends
// are a reader contract breach and fail closed with ErrInvalid.
func WorkflowChangeOf(updates []RefUpdate, t PushSubtrees) (WorkflowTreeChange, bool, error) {
	for _, up := range updates {
		if up.IsDelete() {
			continue
		}
		ids := []string{up.New}
		if !up.IsCreate() {
			ids = append(ids, up.Old)
		}
		for _, id := range ids {
			if _, ok := t.Ends[id]; !ok {
				return WorkflowTreeChange{}, false, fmt.Errorf("%w: missing push end %s", ErrInvalid, id)
			}
		}
	}
	dirOf := func(key string) ObjectID {
		s := t.Subtrees[key]
		if s.Kind != EntryDirectory {
			return ObjectID{}
		}
		return s.ID
	}
	for _, c := range t.Commits {
		cur := dirOf(c.ID.String())
		if cur.IsZero() {
			continue
		}
		var first ObjectID
		same := false
		for i, p := range c.Parents {
			w := dirOf(p.String())
			if i == 0 {
				first = w
			}
			if w == cur {
				same = true
			}
		}
		if !same {
			return WorkflowTreeChange{first, cur}, true, nil
		}
	}
	for _, up := range updates {
		if up.IsDelete() || t.Ends[up.New].Type == ObjectBlob {
			continue
		}
		cur := dirOf(up.New)
		if cur.IsZero() {
			continue
		}
		var base ObjectID
		if up.IsCreate() {
			base = dirOf(HeadKey)
		} else if t.Ends[up.Old].Type != ObjectBlob {
			base = dirOf(up.Old)
		}
		if base != cur {
			return WorkflowTreeChange{base, cur}, true, nil
		}
	}
	return WorkflowTreeChange{}, false, nil
}
