// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// emptyTreeID is the SHA-1 id of the empty tree, known to Git without the
// object being stored. It is the "parent" of a root commit.
const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// maxDiffLine bounds the text of one diff line; a file with a longer line is
// reported truncated rather than cut, which also bounds the memory of a read.
const maxDiffLine = 64 << 10

// GetCommit returns a commit with its diff against its first parent (the empty
// tree for a root commit), read in one pass of
//
//	git diff-tree -r -M -z --raw --numstat -p <parent> <commit>
//
// which prints the changed files, then their line counts, then the patch.
// Statistics come from the line counts, binary files from their "-" counts.
// The patch is parsed as it streams: nothing beyond the limits is kept, and
// git is stopped once nothing more is needed.
func (g *Git) GetCommit(ctx context.Context, repo domain.RepoName, rev domain.Revision, limits domain.DiffLimits) (domain.CommitDetail, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.CommitDetail{}, err
	}
	var res domain.ResolvedRevision
	err = g.cat.do(ctx, dir, func(c *catConn) (err error) {
		res, err = resolve(c, rev)
		return err
	})
	if err != nil {
		return domain.CommitDetail{}, fmt.Errorf("commit %q of %q: %w", rev, repo, err)
	}
	commit, err := g.lastCommit(ctx, dir, res.CommitID, "")
	if err != nil {
		return domain.CommitDetail{}, fmt.Errorf("commit %q of %q: %w", rev, repo, err)
	}
	base := emptyTreeID
	if len(commit.Parents) > 0 {
		base = commit.Parents[0].String()
	}
	detail := domain.CommitDetail{Commit: commit}
	detail.Stats, detail.Diff, err = g.diffTrees(ctx, dir, base, commit.ID.String(), limits)
	if err != nil {
		return domain.CommitDetail{}, fmt.Errorf("diff of %s in %q: %w", commit.ID, repo, err)
	}
	return detail, nil
}

// diffTrees reads the diff from base to head, two commit ids or tree ids.
func (g *Git) diffTrees(ctx context.Context, dir, base, head string, limits domain.DiffLimits) (domain.DiffStats, domain.Diff, error) {
	var (
		diff  domain.Diff
		stats domain.DiffStats
	)
	args := readArgs(dir, "diff-tree", "-r", "-M", "-z", "--raw", "--numstat", "-p", "-U3",
		"--no-ext-diff", "--no-textconv", "--no-color", "--end-of-options", base, head)
	err := g.pipe(ctx, args, func(r *bufio.Reader) error {
		var err error
		diff, stats, err = readDiff(r, limits)
		return err
	})
	if err != nil {
		return domain.DiffStats{}, domain.Diff{}, err
	}
	return stats, diff, nil
}

// rawEntry is one file of the diff: a `--raw` record and its `--numstat`.
type rawEntry struct {
	file       domain.FileDiff
	typeChange bool
	added, del int
}

// readDiff parses the output of diff-tree --raw --numstat -p.
func readDiff(r *bufio.Reader, limits domain.DiffLimits) (domain.Diff, domain.DiffStats, error) {
	entries, err := readRaw(r)
	if err != nil {
		return domain.Diff{}, domain.DiffStats{}, err
	}
	if err := readNumstat(r, entries); err != nil {
		return domain.Diff{}, domain.DiffStats{}, err
	}
	var stats domain.DiffStats
	stats.FilesChanged = len(entries)
	for i := range entries {
		stats.Additions += entries[i].added
		stats.Deletions += entries[i].del
	}
	var diff domain.Diff
	budget := domain.NewDiffBudget(limits)
	kept, from, to, truncated := budget.Window(len(entries))
	entries = entries[:kept]
	diff.Truncated = truncated

	// The patch holds one section per file, two for a type change, in the
	// order of the entries. Entries without lines to show (binary, pure
	// rename, mode change) have a section without hunks.
	lastRead := -1
	for i := from; i < to; i++ {
		if read, _ := budget.Admit(entries[i].file.Binary, entries[i].file.CommentPath(), entries[i].added+entries[i].del); read {
			lastRead = i
		}
	}
	if lastRead >= 0 {
		// A single NUL separates the numstat from the patch.
		if b, err := r.Peek(1); err == nil && b[0] == 0 {
			_, _ = r.Discard(1)
		}
	}
	pr := &patchReader{lr: newLineReader(r, maxDiffLine), budget: budget}
	diff.Files = make([]domain.FileDiff, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		sections := 1
		if e.typeChange {
			sections = 2
		}
		inWindow := i >= from && i < to
		read, why := false, domain.OmitNone
		if inWindow {
			read, why = pr.budget.Admit(e.file.Binary, e.file.CommentPath(), e.added+e.del)
		}
		switch {
		case !inWindow:
			// Nothing to show.
		case !read:
			e.file.Omitted = why
		default:
			before := pr.budget
			pr.budget.StartFile()
			hunks, _, why, err := pr.file(sections, pr.budget.FileLines())
			if err != nil {
				return domain.Diff{}, domain.DiffStats{}, err
			}
			e.file.Hunks, e.file.Omitted = hunks, why
			if why != domain.OmitNone {
				pr.budget = before // nothing of this file is kept
			}
			sections = 0
		}
		if sections > 0 && i < lastRead {
			if err := pr.skip(sections); err != nil {
				return domain.Diff{}, domain.DiffStats{}, err
			}
		}
		diff.Files = append(diff.Files, e.file)
	}
	if lastRead < len(entries)-1 {
		// What follows has no hunks to read: stop git instead of draining it.
		return diff, stats, errStop
	}
	return diff, stats, nil
}

// readRaw reads the `--raw -z` records, which start with ':'.
func readRaw(r *bufio.Reader) ([]rawEntry, error) {
	var entries []rawEntry
	for {
		b, err := r.Peek(1)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return entries, nil
			}
			return nil, err
		}
		if b[0] != ':' {
			return entries, nil
		}
		meta, err := readToken(r)
		if err != nil {
			return nil, unexpectedEOF(err)
		}
		e, npaths, err := parseRawMeta(meta)
		if err != nil {
			return nil, err
		}
		paths := make([]string, npaths)
		for i := range paths {
			if paths[i], err = readToken(r); err != nil {
				return nil, unexpectedEOF(err)
			}
		}
		switch e.file.Status {
		case domain.FileAdded:
			e.file.NewPath = paths[0]
		case domain.FileDeleted:
			e.file.OldPath = paths[0]
		case domain.FileRenamed, domain.FileCopied:
			e.file.OldPath, e.file.NewPath = paths[0], paths[1]
		default:
			e.file.OldPath, e.file.NewPath = paths[0], paths[0]
		}
		entries = append(entries, e)
	}
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// parseRawMeta parses ":<old mode> <new mode> <old id> <new id> <status>" and
// returns the number of paths that follow.
func parseRawMeta(meta string) (rawEntry, int, error) {
	f := strings.Fields(strings.TrimPrefix(meta, ":"))
	if len(f) != 5 || f[4] == "" {
		return rawEntry{}, 0, fmt.Errorf("git diff-tree: unexpected record %q", truncateText(meta))
	}
	var e rawEntry
	for i, dst := range []*uint32{&e.file.OldMode, &e.file.NewMode} {
		m, err := strconv.ParseUint(f[i], 8, 32)
		if err != nil {
			return rawEntry{}, 0, fmt.Errorf("git diff-tree: mode in %q: %w", truncateText(meta), err)
		}
		*dst = uint32(m)
	}
	for i, dst := range []*domain.ObjectID{&e.file.OldID, &e.file.NewID} {
		if strings.Trim(f[2+i], "0") == "" {
			continue // the null id: no such side
		}
		id, err := domain.ParseObjectID(f[2+i])
		if err != nil {
			return rawEntry{}, 0, fmt.Errorf("git diff-tree: %w", err)
		}
		*dst = id
	}
	npaths := 1
	switch f[4][0] {
	case 'A':
		e.file.Status = domain.FileAdded
	case 'D':
		e.file.Status = domain.FileDeleted
	case 'R', 'C':
		e.file.Status = domain.FileRenamed
		if f[4][0] == 'C' {
			e.file.Status = domain.FileCopied
		}
		npaths = 2
		if len(f[4]) > 1 {
			sim, err := strconv.Atoi(f[4][1:])
			if err != nil {
				return rawEntry{}, 0, fmt.Errorf("git diff-tree: similarity in %q: %w", truncateText(meta), err)
			}
			e.file.Similarity = sim
		}
	case 'T':
		e.file.Status = domain.FileTypeChanged
		e.typeChange = true
	default:
		e.file.Status = domain.FileModified
	}
	return e, npaths, nil
}

// readNumstat reads one `--numstat -z` record per entry. A binary file has
// "-" counts.
func readNumstat(r *bufio.Reader, entries []rawEntry) error {
	for i := range entries {
		tok, err := readToken(r)
		if err != nil {
			return unexpectedEOF(err)
		}
		add, rest, ok1 := strings.Cut(tok, "\t")
		del, path, ok2 := strings.Cut(rest, "\t")
		if !ok1 || !ok2 {
			return fmt.Errorf("git diff-tree: unexpected numstat record %q", truncateText(tok))
		}
		if path == "" { // a rename: the two paths follow
			for range 2 {
				if _, err := readToken(r); err != nil {
					return unexpectedEOF(err)
				}
			}
		}
		if add == "-" && del == "-" {
			entries[i].file.Binary = true
			continue
		}
		if entries[i].added, err = strconv.Atoi(add); err != nil {
			return fmt.Errorf("git diff-tree: numstat %q: %w", truncateText(tok), err)
		}
		if entries[i].del, err = strconv.Atoi(del); err != nil {
			return fmt.Errorf("git diff-tree: numstat %q: %w", truncateText(tok), err)
		}
		entries[i].file.Additions, entries[i].file.Deletions = entries[i].added, entries[i].del
	}
	return nil
}

// patchReader walks the patch section by section, in file order. A section
// starts with "diff --git"; hunk lines start with ' ', '+', '-' or '\', so
// the marker never appears inside one.
type patchReader struct {
	lr      *lineReader
	pending []byte // a "diff --git" line read ahead
	hasPend bool
	eof     bool
	cut     bool // the last line read was cut

	budget domain.DiffBudget
}

const sectionMarker = "diff --git "

// line returns the next line, or ok=false at the end of the patch.
func (p *patchReader) line() (line []byte, ok bool, err error) {
	if p.hasPend {
		p.hasPend = false
		return p.pending, true, nil
	}
	if p.eof {
		return nil, false, nil
	}
	line, cut, err := p.lr.next()
	if errors.Is(err, io.EOF) {
		p.eof = true
		return nil, false, nil
	}
	p.cut = cut
	return line, err == nil, err
}

func (p *patchReader) unread(line []byte) {
	p.pending = append(p.pending[:0], line...)
	p.hasPend = true
}

// enterSection reads up to and including the next "diff --git" line.
func (p *patchReader) enterSection() (bool, error) {
	for {
		line, ok, err := p.line()
		if err != nil || !ok {
			return false, err
		}
		if bytes.HasPrefix(line, []byte(sectionMarker)) {
			return true, nil
		}
	}
}

// skip moves past n sections without keeping anything.
func (p *patchReader) skip(n int) error {
	for range n {
		ok, err := p.enterSection()
		if err != nil || !ok {
			return err
		}
		for {
			line, ok, err := p.line()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if bytes.HasPrefix(line, []byte(sectionMarker)) {
				p.unread(line)
				break
			}
		}
	}
	return nil
}

// file reads the sections of one file and returns its hunks, the number of
// changed lines, and why it was omitted: it exceeded max changed lines or the
// budget, or had a line over maxDiffLine. When omitted, the rest of the file is
// skipped.
func (p *patchReader) file(sections, max int) (hunks []domain.Hunk, lines int, why domain.FileOmission, err error) {
	for done := range sections {
		ok, err := p.enterSection()
		if err != nil {
			return nil, 0, domain.OmitNone, err
		}
		if !ok {
			break
		}
		h, n, why, err := p.section(max - lines)
		if err != nil {
			return nil, 0, domain.OmitNone, err
		}
		hunks = append(hunks, h...)
		lines += n
		if why != domain.OmitNone {
			// Leave the stream at the start of the next file.
			return nil, lines, why, p.skip(sections - done - 1)
		}
	}
	return hunks, lines, domain.OmitNone, nil
}

// section reads the hunks of the current section up to the next marker, which
// stays pending. It gives up (with a reason) after more than max changed lines.
func (p *patchReader) section(max int) (hunks []domain.Hunk, lines int, why domain.FileOmission, err error) {
	var cur *domain.Hunk
	var oldN, newN int
	for {
		line, ok, err := p.line()
		if err != nil {
			return nil, 0, domain.OmitNone, err
		}
		if !ok {
			return hunks, lines, domain.OmitNone, nil
		}
		if bytes.HasPrefix(line, []byte(sectionMarker)) {
			p.unread(line)
			return hunks, lines, domain.OmitNone, nil
		}
		if p.cut {
			return nil, lines, domain.OmitUnavailable, nil
		}
		if bytes.HasPrefix(line, []byte("@@ ")) {
			h, err := parseHunkHeader(string(line))
			if err != nil {
				return nil, 0, domain.OmitNone, err
			}
			hunks = append(hunks, h)
			cur = &hunks[len(hunks)-1]
			oldN, newN = h.OldStart, h.NewStart
			continue
		}
		if cur == nil || len(line) == 0 {
			continue // header lines of the section
		}
		if line[0] == '\\' { // "\ No newline at end of file"
			if n := len(cur.Lines); n > 0 {
				cur.Lines[n-1].NoNewlineAtEOF = true
			}
			continue
		}
		var l domain.Line
		text := strings.ToValidUTF8(string(line[min(1, len(line)):]), "\uFFFD")
		switch line[0] {
		case ' ':
			l = domain.Line{Kind: domain.LineContext, OldNumber: oldN, NewNumber: newN, Text: text}
			oldN++
			newN++
		case '+':
			l = domain.Line{Kind: domain.LineAddition, NewNumber: newN, Text: text}
			newN++
		case '-':
			l = domain.Line{Kind: domain.LineDeletion, OldNumber: oldN, Text: text}
			oldN++
		default:
			continue
		}
		changed := l.Kind == domain.LineAddition || l.Kind == domain.LineDeletion
		if changed {
			lines++
		}
		if why := p.budget.ChargeLine(len(text), changed); why != domain.OmitNone {
			return nil, lines, why, nil
		}
		if lines > max {
			return nil, lines, domain.OmitLarge, nil
		}
		cur.Lines = append(cur.Lines, l)
	}
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@[ header]".
func parseHunkHeader(s string) (domain.Hunk, error) {
	bad := func() (domain.Hunk, error) {
		return domain.Hunk{}, fmt.Errorf("git diff-tree: unexpected hunk header %q", truncateText(s))
	}
	rest, ok := strings.CutPrefix(s, "@@ -")
	if !ok {
		return bad()
	}
	ranges, header, ok := strings.Cut(rest, " @@")
	if !ok {
		return bad()
	}
	oldR, newR, ok := strings.Cut(ranges, " +")
	if !ok {
		return bad()
	}
	var h domain.Hunk
	var err1, err2 error
	h.OldStart, h.OldLines, err1 = parseRange(oldR)
	h.NewStart, h.NewLines, err2 = parseRange(newR)
	if err1 != nil || err2 != nil {
		return bad()
	}
	h.Header = strings.ToValidUTF8(strings.TrimPrefix(header, " "), "\uFFFD")
	return h, nil
}

// parseRange parses "start" or "start,count"; a missing count is 1.
func parseRange(s string) (start, count int, err error) {
	a, b, hasCount := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(b); err != nil {
			return 0, 0, err
		}
	}
	return start, count, nil
}
