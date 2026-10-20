// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"slices"
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
	if why := treePathProblem(s); why != "" {
		return TreePath{}, fmt.Errorf("%w: %q: %s", ErrInvalidPath, truncate(s), why)
	}
	return TreePath{s: s}, nil
}

// IsAddressablePath reports whether s is a non-root path that ParseTreePath
// accepts as it is (no trailing slash). It allocates nothing.
func IsAddressablePath(s string) bool {
	return s != "" && treePathProblem(s) == ""
}

// IsAddressablePathBytes is IsAddressablePath for a path held in bytes; it
// spares the copy into a string.
func IsAddressablePathBytes(b []byte) bool {
	return len(b) > 0 && len(b) <= maxTreePathLen && utf8.Valid(b) && pathShapeProblem(b) == ""
}

// treePathProblem returns why s cannot be a non-root tree path, or "".
func treePathProblem(s string) string {
	if len(s) > maxTreePathLen || !utf8.ValidString(s) {
		return "too long or not valid UTF-8"
	}
	return pathShapeProblem(s)
}

// pathShapeProblem checks the control characters and the components of a path
// that is valid UTF-8 and short enough. The bytes of a multi-byte character
// are all 0x80 or more, so it looks at bytes.
func pathShapeProblem[S ~string | ~[]byte](s S) string {
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '/' {
			if c := s[i]; c < 0x20 || c == 0x7f {
				return "control character"
			}
			continue
		}
		n := i - start
		if n == 0 || (n == 1 && s[start] == '.') || (n == 2 && s[start] == '.' && s[start+1] == '.') {
			return "empty, . or .. component"
		}
		start = i + 1
	}
	return ""
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

// TagInfo is a tag and when it was made: the tagger date of an annotated
// tag, the committer date of the commit of a lightweight one.
type TagInfo struct {
	Name string // short name, without refs/tags/
	At   time.Time
	// CommitID is the commit the tag leads to; zero for a tag of a tree or
	// a blob.
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
	// Truncated is true when the directory has more than MaxTreeEntries
	// entries: Entries holds the first ones in the documented order.
	Truncated bool
}

// MaxTreeEntries bounds the entries of a listed directory.
const MaxTreeEntries = 5000

// MaxPathEntries bounds the paths ListPaths returns.
const MaxPathEntries = 100_000

// MaxPathBytes bounds the total bytes of the paths ListPaths returns.
const MaxPathBytes = 8 << 20

// PathList is the recursive list of the file paths of a revision.
type PathList struct {
	// CommitID is the commit the revision resolved to.
	CommitID ObjectID
	// TreeID is the root tree of that commit.
	TreeID ObjectID
	// Paths are the paths of the files and symbolic links, in tree order (the
	// order of `git ls-tree -r`).
	Paths []string
	// Truncated is true when more paths exist than the ones in Paths.
	Truncated bool

	size int // bytes of Paths, kept by Add
}

// Add lists p, the next path of a walk in tree order, within the bounds of
// ListPaths: at most maxPaths paths and maxBytes bytes of paths. A path that
// is not addressable (IsAddressablePath) is left out. When a bound is reached
// p is not listed, Truncated is set, and Add returns false: the walk can stop.
// Otherwise it returns true. The bytes of p are kept, not copied.
func (l *PathList) Add(p string, maxPaths, maxBytes int) bool {
	if !IsAddressablePath(p) {
		return true
	}
	if len(l.Paths) >= maxPaths || l.size+len(p) > maxBytes {
		l.Truncated = true
		return false
	}
	l.size += len(p)
	l.Paths = append(l.Paths, p)
	return true
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
	// Verification is the signature state (ADR 0030). The readers leave it
	// zero (unchecked); the browse use case fills it.
	Verification CommitVerification
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
	// Omitted tells why the hunks were not kept: Hunks is empty although the
	// file changed; Additions and Deletions stay accurate. A file outside the
	// window is OmitNone with no Hunks.
	Omitted FileOmission
	Hunks   []Hunk
}

// CommentPath is the path a comment designates for f, as Diff.HasLine reads it:
// the old path of a deleted file, the new path otherwise.
func (f FileDiff) CommentPath() string {
	if f.Status == FileDeleted {
		return f.OldPath
	}
	return f.NewPath
}

// FileOmission tells why a changed file has no hunks.
type FileOmission uint8

// File omissions.
const (
	OmitNone        FileOmission = iota
	OmitGenerated                // SkipGenerated and IsGeneratedPath: hunks not read
	OmitLarge                    // changed lines > MaxFileLines
	OmitBudget                   // did not fit in the remaining MaxTotalLines
	OmitUnavailable              // retained text would exceed MaxTotalBytes
)

// Collapsed reports a file the web view folds and can load on its own.
func (o FileOmission) Collapsed() bool {
	return o == OmitGenerated || o == OmitLarge || o == OmitBudget
}

// Diff is the structured difference a commit introduces against its first
// parent, or against the empty tree for a root commit.
type Diff struct {
	// Files are ordered as Git reports them (by path). Files past
	// DiffLimits.MaxFiles (when it is not 0) are left out.
	Files []FileDiff
	// Truncated is true when files past DiffLimits.MaxFiles were left out. It
	// says nothing of the listed files: see FileDiff.Omitted.
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

// DiffLimits bound the size of a Diff. Line limits count additions plus
// deletions, not context. A file that does not fit is listed without hunks and
// with an Omitted reason.
type DiffLimits struct {
	// MaxFiles is the number of files kept; 0 means no limit on the number of
	// files (every changed file is listed).
	MaxFiles int
	// MaxFileLines is the most changed lines one file may have; a file over it is
	// OmitLarge. 0 means no limit.
	MaxFileLines int
	// MaxTotalLines is the most changed lines of all the files. A file is
	// OmitBudget when it does not fit in what remains; a smaller one after it
	// may still fit. 0 means no limit.
	MaxTotalLines int
	// MaxTotalBytes is the most text, in bytes, of all the lines, including context. It bounds
	// the memory of a diff whose lines are very long; a file whose
	// text does not fit is OmitUnavailable.
	MaxTotalBytes int
	// From and To select the files whose hunks are read: those with an index in
	// [From, To), counted from 0 in the order of Diff.Files. The other files
	// are listed (paths, status, modes, ids, Binary, Additions, Deletions,
	// Similarity) without hunks; they are OmitNone and use none of
	// MaxTotalLines and MaxTotalBytes. To == 0 means no end, so the zero values
	// select every file. MaxFileLines applies to each file of the window.
	From, To int
	// MaxAloneLines is the most changed lines a file may have to be read at all;
	// a file over it is OmitUnavailable (it cannot be shown even alone), unlike
	// OmitLarge, which a read of that file alone can load. 0 means no limit.
	MaxAloneLines int
	// SkipGenerated omits the files of the window that IsGeneratedPath
	// recognizes (OmitGenerated), without reading their hunks.
	SkipGenerated bool
}

// MaxOneFileLines is the most changed lines of a file read alone on the web
// (OneFileDiffLimits). A file over it is OmitUnavailable, so a short-lines
// file within the byte guard cannot produce millions of rows.
const MaxOneFileLines = 50000

// OneFileDiffLimits reads the hunks of file i only, with no MaxFileLines cap;
// MaxAloneLines (MaxOneFileLines) and MaxTotalBytes are the guards. i must be at least 0: From -1 with To 0
// would mean the whole diff, so a negative i reads no file at all.
func OneFileDiffLimits(i int) DiffLimits {
	if i < 0 {
		return DiffLimits{From: 1, To: 1, MaxAloneLines: MaxOneFileLines, MaxTotalBytes: DefaultDiffLimits().MaxTotalBytes}
	}
	return DiffLimits{From: i, To: i + 1, MaxAloneLines: MaxOneFileLines, MaxTotalBytes: DefaultDiffLimits().MaxTotalBytes}
}

// DefaultDiffLimits are the limits of the API contract: 300 files, 2,000
// changed lines per file, 20,000 changed lines in total, plus 8 MiB of text
// including context. Generated files are not skipped.
func DefaultDiffLimits() DiffLimits {
	return DiffLimits{MaxFiles: 300, MaxFileLines: 2000, MaxTotalLines: 20000, MaxTotalBytes: 8 << 20}
}

// ParseBranchName validates s as the short name of a branch ("feat/x"), with
// the syntax rules of ParseRevision, and rejects a full ref name, which would
// not be short.
func ParseBranchName(s string) (string, error) {
	switch {
	case s == "":
		return "", fmt.Errorf("%w: empty branch name", ErrInvalidRevision)
	case strings.HasPrefix(s, "refs/"):
		return "", fmt.Errorf("%w: %q: give the short name of the branch", ErrInvalidRevision, s)
	}
	if reason := refNameProblem(s); reason != "" {
		return "", fmt.Errorf("%w: %q: %s", ErrInvalidRevision, s, reason)
	}
	return s, nil
}

// MaxRefPathTries bounds the number of leading segments of a path tried as a
// ref name by SplitRefPath.
const MaxRefPathTries = 32

// SplitRefPath splits a path such as "feature/x/dir/file" into the longest
// leading part that resolve accepts as a revision and the rest. resolve
// reports domain.ErrNotFound for a name that is not a revision; any other
// error stops the search. At most MaxRefPathTries leading segments are tried.
// It returns ErrNotFound if nothing matches.
func SplitRefPath(path string, resolve func(Revision) (ResolvedRevision, error)) (RefPath, error) {
	path = strings.Trim(path, "/")
	segs := strings.Split(path, "/")
	if path == "" || slices.Contains(segs, "") {
		return RefPath{}, fmt.Errorf("path %q: %w", path, ErrNotFound)
	}
	for n := min(len(segs), MaxRefPathTries); n >= 1; n-- {
		name := strings.Join(segs[:n], "/")
		rev, err := ParseRevision(name)
		if err != nil {
			continue
		}
		res, err := resolve(rev)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return RefPath{}, err
		}
		rest, err := ParseTreePath(strings.Join(segs[n:], "/"))
		if err != nil {
			return RefPath{}, err
		}
		return RefPath{Name: name, Ref: res.Ref, CommitID: res.CommitID, Rest: rest}, nil
	}
	return RefPath{}, fmt.Errorf("path %q: %w", path, ErrNotFound)
}

// SortTreeEntries orders the entries of a directory as the port promises:
// directories first, then the other entries, each group by name.
func SortTreeEntries(entries []TreeEntry) {
	slices.SortStableFunc(entries, func(a, b TreeEntry) int {
		ad, bd := a.Kind == EntryDirectory, b.Kind == EntryDirectory
		switch {
		case ad && !bd:
			return -1
		case !ad && bd:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// BoundTree keeps the first MaxTreeEntries entries of t and sets Truncated
// when there were more.
func (t *Tree) BoundTree() {
	if len(t.Entries) > MaxTreeEntries {
		t.Entries, t.Truncated = t.Entries[:MaxTreeEntries], true
	}
}

// CommitPageFiles is how many files of a commit get their hunks read with its
// page; the files past it are collapsed (see Diff.CollapseFrom) and load on
// their own.
const CommitPageFiles = 300

// CollapseFrom marks the files from index n on that have lines to show (not
// binary, not already omitted, with changes) as OmitBudget: listed, not read.
func (d *Diff) CollapseFrom(n int) {
	for i := max(n, 0); i < len(d.Files); i++ {
		if f := &d.Files[i]; !f.Binary && f.Omitted == OmitNone && f.Additions+f.Deletions > 0 {
			f.Omitted = OmitBudget
		}
	}
}
