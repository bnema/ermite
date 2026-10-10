// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// MergeMode is how a merge request is merged (ADR 0017).
type MergeMode string

// Merge modes.
const (
	// MergeModeFastForward moves the target to the head; no commit is made.
	MergeModeFastForward MergeMode = "fast-forward"
	// MergeModeSquash makes one new signed commit with the tree of the head.
	MergeModeSquash MergeMode = "squash"
)

// MergeModes returns the two modes in their canonical order.
func MergeModes() []MergeMode { return []MergeMode{MergeModeFastForward, MergeModeSquash} }

// ParseMergeMode parses "fast-forward" or "squash".
func ParseMergeMode(s string) (MergeMode, error) {
	m := MergeMode(s)
	if slices.Contains(MergeModes(), m) {
		return m, nil
	}
	return "", fmt.Errorf("%w: merge mode %q (want fast-forward or squash)", ErrInvalid, s)
}

// String returns the mode as text.
func (m MergeMode) String() string { return string(m) }

// RepoSettings are the settings of a repository that govern its merge
// requests.
type RepoSettings struct {
	// AllowedMergeModes is never empty, in canonical order, without
	// duplicates.
	AllowedMergeModes []MergeMode
	// FailingChecksBlock says that a failing check prevents the merge.
	FailingChecksBlock bool
}

// DefaultRepoSettings are the settings of a new repository: both modes, and
// failing checks block the merge.
func DefaultRepoSettings() RepoSettings {
	return RepoSettings{AllowedMergeModes: MergeModes(), FailingChecksBlock: true}
}

// IsZero reports whether s holds no mode, which only the zero value does.
func (s RepoSettings) IsZero() bool { return len(s.AllowedMergeModes) == 0 }

// Allows reports whether the mode may be used.
func (s RepoSettings) Allows(m MergeMode) bool { return slices.Contains(s.AllowedMergeModes, m) }

// NormalizeMergeModes validates a list of modes for the settings: it must not
// be empty or hold an unknown mode. The result is in canonical order, without
// duplicates.
func NormalizeMergeModes(modes []MergeMode) ([]MergeMode, error) {
	if len(modes) == 0 {
		return nil, fmt.Errorf("%w: at least one merge mode is required", ErrInvalid)
	}
	for _, m := range modes {
		if _, err := ParseMergeMode(m.String()); err != nil {
			return nil, err
		}
	}
	var out []MergeMode
	for _, m := range MergeModes() {
		if slices.Contains(modes, m) {
			out = append(out, m)
		}
	}
	return out, nil
}

// MRState is where a merge request is in its life.
type MRState string

// Merge request states.
const (
	// MRStateOpen awaits review.
	MRStateOpen MRState = "open"
	// MRStateMerged is final: the branch was merged.
	MRStateMerged MRState = "merged"
	// MRStateClosed is final: the request was closed without merging.
	MRStateClosed MRState = "closed"
)

// DiffSide is the side of a diff a comment is attached to.
type DiffSide string

// Diff sides. The empty side marks a general comment.
const (
	// SideNone is the side of a general comment.
	SideNone DiffSide = ""
	// SideOld counts lines of the old file.
	SideOld DiffSide = "old"
	// SideNew counts lines of the new file.
	SideNew DiffSide = "new"
)

// MergeRequest asks to merge one branch into another of the same repository.
type MergeRequest struct {
	Repo   RepoName
	Number int64
	Title  MRTitle
	// Description may be empty.
	Description PlainText
	// Source and Target are short branch names.
	Source, Target string
	State          MRState
	// Author is the label of the token that opened it, "root" or "owner".
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// HeadID is the head of the source branch: stored when the request is
	// merged or closed, and filled from Git at every read while it is open
	// (zero when the branch is gone).
	HeadID ObjectID
	// BaseID is the merge base of the target and the head when the request
	// was merged or closed; zero while it is open.
	BaseID ObjectID
	// MergedCommitID is the tip of the target after the merge; zero unless
	// merged.
	MergedCommitID ObjectID
	// MergeMode is the mode used; empty unless merged.
	MergeMode MergeMode
	// CommentSeq is the id of the last comment of the request.
	CommentSeq int64
	// Version is set by the store on read: Update fails with ErrConflict when
	// the stored request changed since. Opaque to everyone else.
	Version uint64
}

// IsOpen reports whether the request awaits review.
func (m MergeRequest) IsOpen() bool { return m.State == MRStateOpen }

// Comment is a note on a merge request, general or on a line of its diff.
type Comment struct {
	// ID is unique within the merge request, in order of creation.
	ID     int64
	Author string
	// Path, Line and Side are empty for a general comment.
	Path string
	Line int
	Side DiffSide
	// CommitID is the head of the request when the comment was written; zero
	// when the source branch was gone.
	CommitID  ObjectID
	Text      PlainText
	Resolved  bool
	CreatedAt time.Time
	// Version is set by the store on read, like MergeRequest.Version.
	Version uint64
}

// IsGeneral reports whether the comment is not attached to a line.
func (c Comment) IsGeneral() bool { return c.Path == "" }

// HasLine reports whether the diff has a line numbered line on side in the
// file path: the new path of a file, or the old path of a deleted one. The new
// side holds the context and addition lines, the old side the context and
// deletion lines. A file the diff truncated has no lines.
func (d Diff) HasLine(path string, side DiffSide, line int) bool {
	if path == "" || line < 1 {
		return false
	}
	for _, f := range d.Files {
		name := f.NewPath
		if f.Status == FileDeleted {
			name = f.OldPath
		}
		if name != path {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch {
				case side == SideNew && l.Kind != LineDeletion && l.NewNumber == line:
					return true
				case side == SideOld && l.Kind != LineAddition && l.OldNumber == line:
					return true
				}
			}
		}
	}
	return false
}

// Identity is a name and an email address, such as the committer of the
// commits ermite creates.
type Identity struct {
	Name, Email string
}

// SquashCommit is the commit a squash merge creates: the tree of the head on
// top of the target.
type SquashCommit struct {
	Tree, Parent      ObjectID
	Author, Committer Signature
	Message           string
}

// MaxSquashCommits bounds the commits a squash lists in its message.
const MaxSquashCommits = 1000

// NewSquashCommit builds the squash commit of a merge request (ADR 0022).
// commits are the commits of the branch that the target lacks, newest first,
// as Git lists them, and not empty. The author is the author of the oldest
// one, with its date; the other distinct authors become Co-authored-by
// trailers, in order of appearance. The message is the title, " (!<number>)",
// a blank line, the summaries of the commits oldest first as a list, and the
// trailers.
func NewSquashCommit(mr MergeRequest, tree, parent ObjectID, commits []Commit, committer Identity, at time.Time) SquashCommit {
	oldest := slices.Clone(commits)
	slices.Reverse(oldest)
	var first Signature
	if len(oldest) > 0 {
		first = oldest[0].Author
	}

	var msg strings.Builder
	fmt.Fprintf(&msg, "%s (!%d)\n", mr.Title.String(), mr.Number)
	if len(oldest) > 0 {
		msg.WriteString("\n")
	}
	var trailers []string
	seen := map[string]bool{strings.ToLower(first.Email): true}
	for _, c := range oldest {
		msg.WriteString("- " + singleLine(c.Summary) + "\n")
		key := strings.ToLower(c.Author.Email)
		if !seen[key] {
			seen[key] = true
			trailers = append(trailers, "Co-authored-by: "+identLine(c.Author.Name, c.Author.Email))
		}
	}
	if len(trailers) > 0 {
		msg.WriteString("\n" + strings.Join(trailers, "\n") + "\n")
	}
	return SquashCommit{
		Tree: tree, Parent: parent,
		Author:    first,
		Committer: Signature{Name: committer.Name, Email: committer.Email, Time: at.UTC()},
		Message:   msg.String(),
	}
}

// Payload returns the text of the commit object without a signature, which is
// what the signature covers.
func (c SquashCommit) Payload() []byte { return c.encode("") }

// Signed returns the text of the commit object with the armored SSH signature
// in its gpgsig header.
func (c SquashCommit) Signed(armored string) []byte { return c.encode(armored) }

func (c SquashCommit) encode(armored string) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree.String() + "\n")
	b.WriteString("parent " + c.Parent.String() + "\n")
	b.WriteString("author " + signatureLine(c.Author) + "\n")
	b.WriteString("committer " + signatureLine(c.Committer) + "\n")
	if armored != "" {
		lines := strings.Split(strings.TrimRight(armored, "\n"), "\n")
		b.WriteString("gpgsig " + lines[0] + "\n")
		for _, l := range lines[1:] {
			b.WriteString(" " + l + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

func signatureLine(s Signature) string {
	return fmt.Sprintf("%s %d +0000", identLine(s.Name, s.Email), s.Time.Unix())
}

// identLine formats "Name <email>", dropping the characters Git does not
// allow in an identity.
func identLine(name, email string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if r == '<' || r == '>' || r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, s))
	}
	return clean(name) + " <" + clean(email) + ">"
}

// singleLine replaces the control characters of a commit summary, so that it
// stays one line of the message.
func singleLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
