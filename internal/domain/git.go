// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Errors of the read side. Callers match them with errors.Is; the rpc adapter
// maps all of them but the not found ones to invalid_argument.
var (
	// ErrInvalidObjectID means a string is not a full SHA-1 object id.
	ErrInvalidObjectID = errors.New("invalid object id")
	// ErrInvalidRevision means a revision breaks the rules of ParseRevision.
	ErrInvalidRevision = errors.New("invalid revision")
	// ErrInvalidPath means a path inside a repository breaks the rules of
	// ParseTreePath.
	ErrInvalidPath = errors.New("invalid path")
	// ErrInvalidPageToken means a page token was not produced by the server
	// for this kind of listing.
	ErrInvalidPageToken = errors.New("invalid page token")
	// ErrInvalidPageSize means a page size is negative.
	ErrInvalidPageSize = errors.New("invalid page size")
	// ErrInvalidRefKind means a ref kind is not one of the known kinds.
	ErrInvalidRefKind = errors.New("invalid ref kind")
	// ErrNotDirectory means a path that must name a directory names something else.
	ErrNotDirectory = errors.New("not a directory")
	// ErrNotFile means a path that must name a file names something else.
	ErrNotFile = errors.New("not a file")
)

// ObjectIDLen is the length of an object id in hexadecimal characters.
// ermite creates SHA-1 repositories only.
const ObjectIDLen = 40

// ObjectID is a full, lowercase, hexadecimal SHA-1 object id. Only
// ParseObjectID builds a non-zero value; the zero value means "no object".
type ObjectID struct{ s string }

// ParseObjectID validates s as a full object id.
func ParseObjectID(s string) (ObjectID, error) {
	if len(s) != ObjectIDLen || !isLowerHex(s) {
		return ObjectID{}, fmt.Errorf("%w: %q", ErrInvalidObjectID, s)
	}
	return ObjectID{s: s}, nil
}

// String returns the id, or "" for the zero value.
func (id ObjectID) String() string { return id.s }

// IsZero reports whether id is the zero value.
func (id ObjectID) IsZero() bool { return id.s == "" }

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

// MinAbbrevLen is the shortest abbreviation of an object id accepted as a
// revision.
const MinAbbrevLen = 7

// maxRevisionLen bounds a revision, which is a ref name or an object id.
const maxRevisionLen = 512

// Revision names a commit: a branch, a tag, a full ref name ("refs/heads/x"),
// an object id or an abbreviation of at least MinAbbrevLen characters. The
// zero value is HEAD. Parsing checks the syntax only, with the rules of
// `git check-ref-format`; whether the revision exists is the reader's concern.
type Revision struct{ s string }

// ParseRevision validates s. The empty string is HEAD.
func ParseRevision(s string) (Revision, error) {
	if s == "" {
		return Revision{}, nil
	}
	if reason := refNameProblem(s); reason != "" {
		return Revision{}, fmt.Errorf("%w: %q: %s", ErrInvalidRevision, s, reason)
	}
	return Revision{s: s}, nil
}

// RevisionOf returns the revision that designates exactly the commit id.
func RevisionOf(id ObjectID) Revision { return Revision(id) }

// String returns the revision text, or "" for HEAD.
func (r Revision) String() string { return r.s }

// IsHead reports whether r is the zero value, which designates HEAD.
func (r Revision) IsHead() bool { return r.s == "" }

// IsObjectID reports whether r is a full object id.
func (r Revision) IsObjectID() bool { return len(r.s) == ObjectIDLen && isLowerHex(r.s) }

// IsAbbrevID reports whether r is a hexadecimal abbreviation of an object id
// (MinAbbrevLen to ObjectIDLen-1 characters).
func (r Revision) IsAbbrevID() bool {
	return len(r.s) >= MinAbbrevLen && len(r.s) < ObjectIDLen && isLowerHex(r.s)
}

// refNameProblem returns why s cannot be a ref name or revision, or "".
func refNameProblem(s string) string {
	switch {
	case len(s) > maxRevisionLen:
		return "too long"
	case !utf8.ValidString(s):
		return "not valid UTF-8"
	case strings.HasPrefix(s, "-"):
		return "starts with a dash"
	case strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/"):
		return "starts or ends with a slash"
	case strings.HasSuffix(s, "."):
		return "ends with a dot"
	case s == "@":
		return "is @"
	case strings.Contains(s, ".."), strings.Contains(s, "@{"), strings.Contains(s, "//"):
		return "contains a forbidden sequence"
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*[\\", c) {
			return "contains a forbidden character"
		}
	}
	for part := range strings.SplitSeq(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return "has a component that starts with a dot or ends with .lock"
		}
	}
	return ""
}

// maxTreePathLen bounds a path inside a repository, as Linux does.
const maxTreePathLen = 4096

// TreePath is a slash-separated path from the root of a repository, without
// leading or trailing slash. The zero value is the root. Only ParseTreePath
// builds a non-root value.
type TreePath struct{ s string }

// ParseTreePath validates s. It accepts one trailing slash, which it drops,
// and the empty string (the root). Empty, "." and ".." components, NUL and
// control characters, and text that is not valid UTF-8 are rejected.
func ParseTreePath(s string) (TreePath, error) {
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return TreePath{}, nil
	}
	if len(s) > maxTreePathLen || !utf8.ValidString(s) {
		return TreePath{}, fmt.Errorf("%w: %q: too long or not valid UTF-8", ErrInvalidPath, truncate(s))
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return TreePath{}, fmt.Errorf("%w: %q: control character", ErrInvalidPath, truncate(s))
		}
	}
	for part := range strings.SplitSeq(s, "/") {
		if part == "" || part == "." || part == ".." {
			return TreePath{}, fmt.Errorf("%w: %q: empty, . or .. component", ErrInvalidPath, truncate(s))
		}
	}
	return TreePath{s: s}, nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// String returns the path, or "" for the root.
func (p TreePath) String() string { return p.s }

// IsRoot reports whether p is the root of the repository.
func (p TreePath) IsRoot() bool { return p.s == "" }

// Join returns the path of the entry name inside the directory p. name must
// be a single component.
func (p TreePath) Join(name string) string {
	if p.s == "" {
		return name
	}
	return p.s + "/" + name
}

// RefKind is the kind of a Git ref.
type RefKind string

// Ref kinds, in the order refs are listed.
const (
	// RefKindHead is the symbolic ref HEAD.
	RefKindHead RefKind = "head"
	// RefKindBranch is a branch (refs/heads/*).
	RefKindBranch RefKind = "branch"
	// RefKindTag is a tag (refs/tags/*), lightweight or annotated.
	RefKindTag RefKind = "tag"
)

// Ref is a branch, a tag or HEAD.
type Ref struct {
	// Name is the full name: "refs/heads/main", "refs/tags/v1" or "HEAD".
	Name string
	// ShortName is Name without "refs/heads/" or "refs/tags/". For HEAD it is
	// the short name of the default branch, empty if that branch is unborn.
	ShortName string
	Kind      RefKind
	// ID is the object the ref points to (the tag object of an annotated
	// tag). Zero for HEAD when the default branch is unborn.
	ID ObjectID
	// CommitID is the commit the ref designates once tags are followed. Zero
	// for a tag that does not lead to a commit and for an unborn HEAD.
	CommitID ObjectID
}

// ResolvedRevision is the result of resolving a Revision.
type ResolvedRevision struct {
	// CommitID is the commit the revision designates, tags followed.
	CommitID ObjectID
	// Ref is the full name of the ref the revision matched; empty when the
	// revision was an object id or HEAD.
	Ref string
}

// RefPath is a URL path split into a revision and the path inside it. It is
// the answer to "feature/x/dir/file": the ref feature/x and the path dir/file.
type RefPath struct {
	// Name is the matched ref's short name, or the object id text.
	Name string
	// Ref is the matched ref's full name; empty when Name is an object id.
	Ref string
	// CommitID is the commit Name designates.
	CommitID ObjectID
	// Rest is what follows Name in the path, without leading slash.
	Rest TreePath
}

// EntryKind is the kind of an entry of a Git tree.
type EntryKind string

// Entry kinds.
const (
	// EntryFile is a regular file (mode 100644).
	EntryFile EntryKind = "file"
	// EntryExecutable is an executable file (mode 100755).
	EntryExecutable EntryKind = "executable"
	// EntryDirectory is a directory (mode 040000).
	EntryDirectory EntryKind = "directory"
	// EntrySymlink is a symbolic link (mode 120000).
	EntrySymlink EntryKind = "symlink"
	// EntrySubmodule is a submodule commit (mode 160000).
	EntrySubmodule EntryKind = "submodule"
)

// File mode bits as Git stores them.
const (
	modeTypeMask  = 0o170000
	modeDirectory = 0o040000
	modeSymlink   = 0o120000
	modeRegular   = 0o100000
	modeSubmodule = 0o160000
)

// EntryKindOf returns the kind of an entry from its Git mode, or "" if the
// mode is not a Git mode.
func EntryKindOf(mode uint32) EntryKind {
	switch mode & modeTypeMask {
	case modeDirectory:
		return EntryDirectory
	case modeSymlink:
		return EntrySymlink
	case modeSubmodule:
		return EntrySubmodule
	case modeRegular:
		if mode&0o111 != 0 {
			return EntryExecutable
		}
		return EntryFile
	}
	return ""
}

// TreeEntry is one entry of a directory.
type TreeEntry struct {
	// Name is the name in its directory; Path is the path from the root.
	Name, Path string
	Kind       EntryKind
	Mode       uint32
	// ID is the blob, tree or submodule commit.
	ID ObjectID
	// Size is the size of a file or symlink; 0 for directories and submodules.
	Size int64
	// LastCommit is the latest commit that changed the entry; nil unless
	// asked for, and for the entries the reader left out (see
	// Tree.LastCommitsOmitted).
	LastCommit *Commit
}

// Tree is a listed directory.
type Tree struct {
	// CommitID is the commit the revision resolved to.
	CommitID ObjectID
	// ID is the tree object that was listed.
	ID ObjectID
	// Entries are the directories first, then the other entries, each group
	// ordered by name.
	Entries []TreeEntry
	// LastCommitsOmitted is true when last commits were asked for and the
	// reader gave only the first entries (MaxLastCommitEntries) one.
	LastCommitsOmitted bool
}

// MaxLastCommitEntries bounds the entries of one directory that get a last
// commit: each costs a history lookup.
const MaxLastCommitEntries = 200

// BlobInfo describes a file at a revision.
type BlobInfo struct {
	ID   ObjectID
	Path string
	Size int64
	// Binary follows Git's rule: a NUL byte in the first 8,000 bytes.
	Binary bool
	// MediaType comes from the file name and the first bytes, without
	// parameters; "application/octet-stream" when unknown.
	MediaType string
	Mode      uint32
}

// BinarySniffLen is how many leading bytes decide whether a blob is binary.
const BinarySniffLen = 8000

// Signature is who did something in Git, and when.
type Signature struct {
	Name, Email string
	Time        time.Time
}

// Commit is the metadata of a commit.
type Commit struct {
	ID        ObjectID
	Parents   []ObjectID
	Author    Signature
	Committer Signature
	// Summary is the first line of Message.
	Summary string
	// Message is the whole message without trailing line breaks, converted to
	// valid UTF-8.
	Message string
}

// FileStatus tells what a commit did to a file.
type FileStatus string

// File statuses.
const (
	FileAdded       FileStatus = "added"
	FileDeleted     FileStatus = "deleted"
	FileModified    FileStatus = "modified"
	FileRenamed     FileStatus = "renamed"
	FileCopied      FileStatus = "copied"
	FileTypeChanged FileStatus = "type_changed"
)

// LineKind tells how a line of a hunk relates to the two sides of a diff.
type LineKind string

// Line kinds.
const (
	LineContext  LineKind = "context"
	LineAddition LineKind = "addition"
	LineDeletion LineKind = "deletion"
)

// Line is one line of a hunk.
type Line struct {
	Kind LineKind
	// OldNumber is 0 for an addition; NewNumber is 0 for a deletion.
	OldNumber, NewNumber int
	// Text has no line terminator and is valid UTF-8.
	Text string
	// NoNewlineAtEOF is set on the last line of a side that has no final line
	// break.
	NoNewlineAtEOF bool
}

// Hunk is a contiguous changed region with its context.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	// Header is the text after the second "@@", often the enclosing function.
	Header string
	Lines  []Line
}

// FileDiff is the change of one file.
type FileDiff struct {
	// OldPath is empty for an added file, NewPath for a deleted one.
	OldPath, NewPath string
	Status           FileStatus
	// OldMode and NewMode are 0 for an added and a deleted file.
	OldMode, NewMode uint32
	// OldID and NewID are the blob ids, zero for an added and a deleted file.
	OldID, NewID ObjectID
	// Binary is Git's numstat "-": no hunks, no additions or deletions.
	Binary               bool
	Additions, Deletions int
	// Similarity is a percentage for a renamed or copied file, 0 otherwise.
	Similarity int
	// Truncated means the file exceeded a diff limit: Hunks is empty although
	// the file changed. Additions and Deletions stay accurate.
	Truncated bool
	Hunks     []Hunk
}

// Diff is the structured difference a commit introduces against its first
// parent, or against the empty tree for a root commit.
type Diff struct {
	// Files are ordered as Git reports them (by path). Files past
	// DiffLimits.MaxFiles are left out.
	Files []FileDiff
	// Truncated is true when files were left out or a listed file is truncated.
	Truncated bool
}

// DiffStats are the totals over every changed file, including the ones the
// diff leaves out or truncates. Binary files count 0 lines.
type DiffStats struct {
	FilesChanged, Additions, Deletions int
}

// CommitDetail is a commit with its statistics and diff.
type CommitDetail struct {
	Commit Commit
	Stats  DiffStats
	Diff   Diff
}

// DiffLimits bound the size of a Diff. Lines are hunk lines, context included.
type DiffLimits struct {
	// MaxFiles is the number of files kept.
	MaxFiles int
	// MaxFileLines is the most lines one file may have; a file over it is
	// truncated.
	MaxFileLines int
	// MaxTotalLines is the most lines of all the files. A file is truncated
	// when it does not fit in what remains; a smaller one after it may still
	// fit.
	MaxTotalLines int
	// MaxTotalBytes is the most text, in bytes, of all the lines. It bounds
	// the memory of a diff whose lines are very long; files are truncated
	// like for MaxTotalLines.
	MaxTotalBytes int
}

// DefaultDiffLimits are the limits of the API contract: 300 files, 2,000
// lines per file, 20,000 lines in total, plus 8 MiB of text.
func DefaultDiffLimits() DiffLimits {
	return DiffLimits{MaxFiles: 300, MaxFileLines: 2000, MaxTotalLines: 20000, MaxTotalBytes: 8 << 20}
}
