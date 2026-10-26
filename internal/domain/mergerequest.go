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
	// MergeModeMerge makes one new signed commit with two parents: the target
	// tip and the head.
	MergeModeMerge MergeMode = "merge"
)

// MergeModes returns the modes in their canonical order.
func MergeModes() []MergeMode {
	return []MergeMode{MergeModeFastForward, MergeModeSquash, MergeModeMerge}
}

// ParseMergeMode parses "fast-forward", "squash" or "merge".
func ParseMergeMode(s string) (MergeMode, error) {
	m := MergeMode(s)
	if slices.Contains(MergeModes(), m) {
		return m, nil
	}
	return "", fmt.Errorf("%w: merge mode %q (want fast-forward, squash or merge)", ErrInvalid, s)
}

// String returns the mode as text.
func (m MergeMode) String() string { return string(m) }

// RepoSettings are the settings of a repository that govern its merge
// requests.
type RepoSettings struct {
	// AllowedMergeModes is never empty, in canonical order, without
	// duplicates.
	AllowedMergeModes []MergeMode
	// Issues says who may write issues; the empty mode means IssuesOff.
	Issues IssuesMode
	// AllowAutoMerge says that merge requests may be set to merge themselves
	// once their checks pass. Off by default.
	AllowAutoMerge bool
}

// DefaultRepoSettings are the settings of a new repository: every merge mode,
// with issues and auto-merge disabled.
func DefaultRepoSettings() RepoSettings {
	return RepoSettings{AllowedMergeModes: MergeModes(), Issues: IssuesOff}
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
	// AutoMerge is the pending auto-merge; nil when off.
	AutoMerge *AutoMerge
	// AutoMerged says that the request was merged by auto-merge.
	AutoMerged bool
	// Version is set by the store on read: Update fails with ErrConflict when
	// the stored request changed since. Opaque to everyone else.
	Version uint64
}

// AutoMerge is the pending auto-merge of an open merge request.
type AutoMerge struct {
	// Mode is the merge mode chosen when auto-merge was enabled.
	Mode MergeMode
	// HeadID is the head it was enabled for; a new head cancels it.
	HeadID ObjectID
	// EnabledBy is who enabled it; the merge is made on their behalf.
	EnabledBy CIPusher
	// EnablerLabel is the display name of the enabler.
	EnablerLabel string
	// EnabledAt is when auto-merge was enabled.
	EnabledAt time.Time
}

// draftPrefixes mark a draft at the start of a title, compared without regard
// to case, as Forgejo and Gitea do.
var draftPrefixes = []string{"wip:", "[wip]"}

// IsDraft reports whether the title marks the request as a draft: it starts
// with "WIP:" or "[WIP]". A draft cannot be merged.
func (mr MergeRequest) IsDraft() bool {
	_, ok := draftRest(mr.Title.String())
	return ok
}

// ReadyTitle is the title without its draft prefixes (all of them, so that
// "WIP: [WIP] x" is no longer a draft), and false when the title has none or
// nothing would be left of it.
func ReadyTitle(t MRTitle) (MRTitle, bool) {
	rest, ok := draftRest(t.String())
	if !ok {
		return MRTitle{}, false
	}
	for {
		next, more := draftRest(rest)
		if !more {
			break
		}
		rest = next
	}
	ready, err := ParseMRTitle(rest)
	if err != nil {
		return MRTitle{}, false
	}
	return ready, true
}

// draftRest is the title after its draft prefix and the spaces that follow.
func draftRest(title string) (string, bool) {
	t := strings.TrimLeft(title, " ")
	for _, p := range draftPrefixes {
		if len(t) >= len(p) && strings.EqualFold(t[:len(p)], p) {
			return strings.TrimLeft(t[len(p):], " "), true
		}
	}
	return "", false
}

// IsOpen reports whether the request awaits review.
func (m MergeRequest) IsOpen() bool { return m.State == MRStateOpen }

// Comment is a note on a merge request, general or on a line of its diff.
type Comment struct {
	ReplyTo   int64
	StartLine int
	Verdict   Verdict
	EditedAt  time.Time
	Deleted   bool
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

// ServerCommit is a commit the server creates: a squash or a merge commit.
type ServerCommit struct {
	Tree              ObjectID
	Parents           []ObjectID
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
func NewSquashCommit(mr MergeRequest, tree, parent ObjectID, commits []Commit, committer Identity, at time.Time) ServerCommit {
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
	return ServerCommit{
		Tree: tree, Parents: []ObjectID{parent},
		Author:    first,
		Committer: Signature{Name: committer.Name, Email: committer.Email, Time: at.UTC()},
		Message:   msg.String(),
	}
}

// BotCommitSubject returns the subject line of Bernard's commit, which is also
// the title of its merge request.
func BotCommitSubject(n int) string {
	if n == 1 {
		return "build(deps): update 1 dependency"
	}
	return fmt.Sprintf("build(deps): update %d dependencies", n)
}

// BotCommitBody returns the list of updates, one line each.
func BotCommitBody(ups []DepUpdate) string {
	var b strings.Builder
	for _, u := range ups {
		fmt.Fprintf(&b, "- %s %s \u2192 %s", asciiLine(u.Name), asciiLine(u.Current), asciiLine(u.Version))
		if u.Directory != "" {
			fmt.Fprintf(&b, " (%s)", asciiLine(u.Directory))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// BotMRBody returns the description of Bernard's merge request: the list of
// updates, then the changes Go made beyond them. Every field comes from a
// runner report or a go.mod, so each is cut to one printable ASCII line and
// cannot close the code span that holds a module name.
func BotMRBody(ups []DepUpdate, effects []DepSideEffect) string {
	var b strings.Builder
	b.WriteString(BotCommitBody(ups))
	if len(effects) == 0 {
		return b.String()
	}
	line := func(s string) string { return strings.ReplaceAll(asciiLine(s), "`", "'") }
	b.WriteString("\n## Also changed by Go\n")
	for _, e := range effects {
		fmt.Fprintf(&b, "- `%s` ", line(e.Name))
		if e.To == "" {
			b.WriteString("removed, no longer needed")
		} else {
			fmt.Fprintf(&b, "%s \u2192 %s", line(e.From), line(e.To))
		}
		if e.Directory != "" {
			fmt.Fprintf(&b, " (%s)", line(e.Directory))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// NewBotCommit builds Bernard's commit of dependency updates on parent. The
// author is Bernard, with an address on the domain of the committer, which is
// the instance identity, unless author sets them; the message lists the
// updates.
func NewBotCommit(tree, parent ObjectID, ups []DepUpdate, author, committer Identity, at time.Time) ServerCommit {
	host := "localhost"
	if i := strings.LastIndex(committer.Email, "@"); i >= 0 && i+1 < len(committer.Email) {
		host = asciiLine(committer.Email[i+1:])
	}
	name, email := "Bernard", "bernard@"+host
	if author.Name != "" {
		name = asciiLine(author.Name)
	}
	if author.Email != "" {
		email = asciiLine(author.Email)
	}
	return ServerCommit{
		Tree: tree, Parents: []ObjectID{parent},
		Author:    Signature{Name: name, Email: email, Time: at.UTC()},
		Committer: Signature{Name: committer.Name, Email: committer.Email, Time: at.UTC()},
		Message:   BotCommitSubject(len(ups)) + "\n\n" + BotCommitBody(ups),
	}
}

// asciiLine keeps printable ASCII and replaces anything else, newlines
// included, with '?'.
func asciiLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// NewMergeCommit builds the merge commit of a merge request: the tree of the
// merge, the target tip and the head as parents, and the server identity as
// author and committer.
func NewMergeCommit(mr MergeRequest, tree, target, head ObjectID, committer Identity, at time.Time) ServerCommit {
	sig := Signature{Name: committer.Name, Email: committer.Email, Time: at.UTC()}
	return ServerCommit{
		Tree: tree, Parents: []ObjectID{target, head},
		Author: sig, Committer: sig,
		Message: fmt.Sprintf("Merge branch '%s' into %s (!%d)\n\n%s\n",
			singleLine(mr.Source), singleLine(mr.Target), mr.Number, singleLine(mr.Title.String())),
	}
}

// Payload returns the text of the commit object without a signature, which is
// what the signature covers.
func (c ServerCommit) Payload() []byte { return c.encode("") }

// Signed returns the text of the commit object with the armored SSH signature
// in its gpgsig header.
func (c ServerCommit) Signed(armored string) []byte { return c.encode(armored) }

func (c ServerCommit) encode(armored string) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree.String() + "\n")
	for _, p := range c.Parents {
		b.WriteString("parent " + p.String() + "\n")
	}
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

// Verdict is an immutable review decision on a root note.
type Verdict uint8

// Review verdicts.
const (
	VerdictNone Verdict = iota
	VerdictApprove
	VerdictChangesRequested
)

// String returns the wire name of a verdict.
func (v Verdict) String() string {
	switch v {
	case VerdictApprove:
		return "approve"
	case VerdictChangesRequested:
		return "changes_requested"
	}
	return ""
}

// ParseVerdict parses a review decision.
func ParseVerdict(s string) (Verdict, error) {
	switch s {
	case "":
		return VerdictNone, nil
	case "approve":
		return VerdictApprove, nil
	case "changes_requested":
		return VerdictChangesRequested, nil
	}
	return VerdictNone, fmt.Errorf("%w: verdict %q", ErrInvalid, s)
}

// IsReply reports whether this note replies to a root.
func (c Comment) IsReply() bool { return c.ReplyTo != 0 }

// IsRange reports whether this note anchors a range.
func (c Comment) IsRange() bool { return c.StartLine != 0 }

// AuthorVerdict is an author's latest review decision.
type AuthorVerdict struct {
	Author    string
	Verdict   Verdict
	CommentID int64
	CommitID  ObjectID
	At        time.Time
	Stale     bool
}

// CurrentVerdicts returns the latest verdict per author, including deleted notes,
// in note ID order. Only approvals become stale when the head changes.
func CurrentVerdicts(comments []Comment, head ObjectID) []AuthorVerdict {
	latest := make(map[string]Comment)
	for _, c := range comments {
		if c.Verdict != VerdictNone && c.ID > latest[c.Author].ID {
			latest[c.Author] = c
		}
	}
	out := make([]AuthorVerdict, 0, len(latest))
	for _, c := range latest {
		out = append(out, AuthorVerdict{Author: c.Author, Verdict: c.Verdict, CommentID: c.ID, CommitID: c.CommitID, At: c.CreatedAt, Stale: c.Verdict == VerdictApprove && (head == (ObjectID{}) || c.CommitID != head)})
	}
	slices.SortFunc(out, func(a, b AuthorVerdict) int {
		if a.CommentID < b.CommentID {
			return -1
		}
		if a.CommentID > b.CommentID {
			return 1
		}
		return 0
	})
	return out
}

// Limits of a stored Mergeability.
const (
	// MaxBehindCount caps the commits counted as behind.
	MaxBehindCount = 1000
	// MaxConflictPaths caps the conflicted paths listed.
	MaxConflictPaths = 20
)

// MergeabilityState is the outcome of a mergeability check.
type MergeabilityState string

// Mergeability states.
const (
	// MergeabilityChecking means there is no up-to-date result yet; it is never
	// stored.
	MergeabilityChecking MergeabilityState = "checking"
	// MergeabilityUpToDate means the target is an ancestor of the head.
	MergeabilityUpToDate MergeabilityState = "up_to_date"
	// MergeabilityBehind means the head lacks commits of the target but merges
	// cleanly.
	MergeabilityBehind MergeabilityState = "behind"
	// MergeabilityConflict means the head does not merge cleanly.
	MergeabilityConflict MergeabilityState = "conflict"
)

// Mergeability is whether an open merge request merges with its target, for
// given tips.
type Mergeability struct {
	Repo   RepoName
	Number int64
	State  MergeabilityState
	// TargetID and HeadID are the tips the result is for; zero for a derived
	// "checking".
	TargetID, HeadID ObjectID
	// Behind counts the commits of the target not in the head (behind only),
	// capped at MaxBehindCount.
	Behind       int
	BehindCapped bool
	// ConflictPaths holds at most MaxConflictPaths paths (conflict only).
	ConflictPaths          []string
	ConflictPathsTruncated bool
	CheckedAt              time.Time
	// Version is set by the store on read, like MergeRequest.Version.
	Version uint64
}

// Allows reports whether the mode may be used given the state, and if not,
// why.
func (m Mergeability) Allows(mode MergeMode) (bool, string) {
	switch mode {
	case MergeModeFastForward:
		if m.State == MergeabilityUpToDate {
			return true, ""
		}
		if m.State == MergeabilityBehind {
			return false, "behind the target: rebase or use squash/merge"
		}
	case MergeModeSquash, MergeModeMerge:
		if m.State != MergeabilityConflict && m.State != MergeabilityChecking && m.State != "" {
			return true, ""
		}
	}
	switch m.State {
	case MergeabilityConflict:
		return false, "conflicts with the target"
	case MergeabilityChecking, "":
		return false, "mergeability is being checked"
	}
	return false, "mode not allowed"
}

// IsFor reports whether the result is for these tips, that is not stale.
func (m Mergeability) IsFor(target, head ObjectID) bool {
	return m.State != "" && m.State != MergeabilityChecking && m.TargetID == target && m.HeadID == head
}

// MergeTreeResult is the outcome of merging two commits without a commit.
type MergeTreeResult struct {
	// Tree is the merged tree; zero when not clean.
	Tree          ObjectID
	Clean         bool
	ConflictPaths []string
	Truncated     bool
}

// Bounded returns r with at most MaxConflictPaths conflicted paths, Truncated
// set when some were dropped. Adapters return the raw result; the bound is
// the domain's.
func (r MergeTreeResult) Bounded() MergeTreeResult {
	kept := r.ConflictPaths
	if len(kept) > MaxConflictPaths {
		kept = kept[:MaxConflictPaths]
		r.Truncated = true
	}
	if kept != nil {
		// Exact-sized copies: neither the backing array nor the git output the
		// paths were cut from stays referenced.
		out := make([]string, len(kept))
		for i, p := range kept {
			out[i] = strings.Clone(p)
		}
		r.ConflictPaths = out
	}
	return r
}

// BehindLimit is the limit to give a count of commits behind so that
// BoundBehind can tell whether MaxBehindCount was exceeded.
const BehindLimit = MaxBehindCount + 1

// BoundBehind caps a raw count of commits behind (counted up to BehindLimit)
// at MaxBehindCount; capped is true when the count was over it.
func BoundBehind(n int) (count int, capped bool) {
	if n > MaxBehindCount {
		return MaxBehindCount, true
	}
	return n, false
}

// IssuesEnabled reports whether the repository exposes issue streams and pages.
func (s RepoSettings) IssuesEnabled() bool { return s.Issues.OrDefault() != IssuesOff }

// ShowsIssueSubmissionReceipt reports whether submissions await public moderation.
func (s RepoSettings) ShowsIssueSubmissionReceipt() bool { return s.Issues.OrDefault() == IssuesPublic }

// AcceptsAnonymousIssues reports whether anyone may submit an issue.
func (s RepoSettings) AcceptsAnonymousIssues() bool { return s.Issues.OrDefault() == IssuesPublic }
