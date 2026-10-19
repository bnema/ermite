// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import "strings"

// RevisionCandidateKind identifies the lookup needed to resolve a revision.
type RevisionCandidateKind uint8

const (
	RevisionCandidateHead RevisionCandidateKind = iota
	RevisionCandidateObject
	RevisionCandidateRef
	RevisionCandidateAbbrevID
)

// RevisionCandidate is one lookup to try when resolving a revision.
// Name is HEAD, an object id, a full ref name, or an abbreviated object id.
type RevisionCandidate struct {
	Kind RevisionCandidateKind
	Name string
}

// Candidates returns the resolution order. HEAD is special; full object ids
// are objects before refs, so a ref cannot redirect an existing object link.
// Otherwise branches beat tags. Full ref names and abbreviated ids are tried
// as written only after the branch and tag forms.
func (r Revision) Candidates() []RevisionCandidate {
	s := r.String()
	if r.IsHead() || s == "HEAD" {
		return []RevisionCandidate{{Kind: RevisionCandidateHead, Name: "HEAD"}}
	}
	refs := []RevisionCandidate{
		{Kind: RevisionCandidateRef, Name: "refs/heads/" + s},
		{Kind: RevisionCandidateRef, Name: "refs/tags/" + s},
	}
	switch {
	case r.IsObjectID():
		return append([]RevisionCandidate{{Kind: RevisionCandidateObject, Name: s}}, refs...)
	case strings.HasPrefix(s, "refs/"):
		return append(refs, RevisionCandidate{Kind: RevisionCandidateRef, Name: s})
	case r.IsAbbrevID():
		return append(refs, RevisionCandidate{Kind: RevisionCandidateAbbrevID, Name: s})
	}
	return refs
}

// ResolvedRef returns the full ref name for a ref candidate, or empty otherwise.
func (c RevisionCandidate) ResolvedRef() string {
	if c.Kind == RevisionCandidateRef {
		return c.Name
	}
	return ""
}
