// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// Names of the states, the sides and the merge modes in the output and in the
// flags.
const (
	stateOpen   = "open"
	stateMerged = "merged"
	stateClosed = "closed"
	stateAll    = "all"

	sideOld = "old"
	sideNew = "new"

	modeFastForward = "fast-forward"
	modeSquash      = "squash"
)

func stateName(s ermite.MergeRequestState) string {
	switch s {
	case ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN:
		return stateOpen
	case ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED:
		return stateMerged
	case ermite.MergeRequestState_MERGE_REQUEST_STATE_CLOSED:
		return stateClosed
	}
	return "unknown"
}

func parseState(s string) (ermite.MergeRequestState, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case stateOpen:
		return ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN, nil
	case stateMerged:
		return ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED, nil
	case stateClosed:
		return ermite.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, nil
	case stateAll:
		return ermite.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED, nil
	}
	return 0, fmt.Errorf("invalid state %q: use %s, %s, %s or %s", s, stateOpen, stateMerged, stateClosed, stateAll)
}

func modeName(m ermite.MergeMode) string {
	switch m {
	case ermite.MergeMode_MERGE_MODE_FAST_FORWARD:
		return modeFastForward
	case ermite.MergeMode_MERGE_MODE_SQUASH:
		return modeSquash
	}
	return ""
}

func parseMode(s string) (ermite.MergeMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case modeFastForward, "ff":
		return ermite.MergeMode_MERGE_MODE_FAST_FORWARD, nil
	case modeSquash:
		return ermite.MergeMode_MERGE_MODE_SQUASH, nil
	}
	return 0, fmt.Errorf("invalid merge mode %q: use %s or %s", s, modeFastForward, modeSquash)
}

func sideName(s ermite.DiffSide) string {
	switch s {
	case ermite.DiffSide_DIFF_SIDE_OLD:
		return sideOld
	case ermite.DiffSide_DIFF_SIDE_NEW:
		return sideNew
	}
	return ""
}

func parseSide(s string) (ermite.DiffSide, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case sideOld:
		return ermite.DiffSide_DIFF_SIDE_OLD, nil
	case sideNew:
		return ermite.DiffSide_DIFF_SIDE_NEW, nil
	}
	return 0, fmt.Errorf("invalid side %q: use %s or %s", s, sideOld, sideNew)
}

// jsonMergeRequest is the stable JSON shape of a merge request.
type jsonMergeRequest struct {
	Repo           string  `json:"repo"`
	Number         int64   `json:"number"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	SourceBranch   string  `json:"source_branch"`
	TargetBranch   string  `json:"target_branch"`
	State          string  `json:"state"`
	Author         string  `json:"author"`
	CreatedAt      *string `json:"created_at"`
	UpdatedAt      *string `json:"updated_at"`
	HeadID         string  `json:"head_id"`
	MergedCommitID string  `json:"merged_commit_id"`
	MergeMode      string  `json:"merge_mode"`
}

// jsonMerged is the output of `mr merge --json`.
type jsonMerged struct {
	jsonMergeRequest
	SourceBranchDeleted bool `json:"source_branch_deleted"`
}

func toJSONMergeRequest(m *ermite.MergeRequest) jsonMergeRequest {
	return jsonMergeRequest{
		Repo: m.GetRepo(), Number: m.GetNumber(), Title: m.GetTitle(), Description: m.GetDescription(),
		SourceBranch: m.GetSourceBranch(), TargetBranch: m.GetTargetBranch(), State: stateName(m.GetState()),
		Author: m.GetAuthor(), CreatedAt: jsonTime(m.GetCreatedAt()), UpdatedAt: jsonTime(m.GetUpdatedAt()),
		HeadID: m.GetHeadId(), MergedCommitID: m.GetMergedCommitId(), MergeMode: modeName(m.GetMergeMode()),
	}
}

// jsonComment is the stable JSON shape of a comment.
type jsonComment struct {
	ID        string  `json:"id"`
	Author    string  `json:"author"`
	Path      string  `json:"path"`
	Line      int32   `json:"line"`
	Side      string  `json:"side"`
	CommitID  string  `json:"commit_id"`
	Text      string  `json:"text"`
	Resolved  bool    `json:"resolved"`
	CreatedAt *string `json:"created_at"`
}

func toJSONComment(c *ermite.Comment) jsonComment {
	return jsonComment{
		ID: c.GetId(), Author: c.GetAuthor(), Path: c.GetPath(), Line: c.GetLine(), Side: sideName(c.GetSide()),
		CommitID: c.GetCommitId(), Text: c.GetText(), Resolved: c.GetResolved(), CreatedAt: jsonTime(c.GetCreatedAt()),
	}
}

const jsonMRHelp = `
JSON fields of a merge request (--json): repo, number, title, description, source_branch,
target_branch, state ("open", "merged" or "closed"), author (token label), created_at and
updated_at (RFC 3339 UTC), head_id (the commit the source branch points to; empty when the
branch is gone), merged_commit_id and merge_mode ("fast-forward" or "squash"; both empty
unless merged).`

const jsonCommentHelp = `
JSON fields of a comment: id, author, path, line and side ("old" or "new"; empty path, line 0
and empty side for a general comment), commit_id (the head when it was written), text,
resolved, created_at.`

func newMRCmd(env *cliEnv) *cobra.Command {
	mr := &cobra.Command{
		Use:   "mr",
		Short: "Review and merge branches on a remote server",
		Long: "Merge requests ask to merge one branch of a repository into another. They are about a branch,\n" +
			"not about who wrote it. Needs a server with the merge_requests feature (see `ermite version`).\n" +
			"A merge request is named by its repository and its number: `ermite mr show proj 3`.",
	}
	mr.AddCommand(
		newMRCreateCmd(env), newMRListCmd(env), newMRShowCmd(env), newMRCommentCmd(env),
		newMRResolveCmd(env), newMRMergeCmd(env), newMRCloseCmd(env),
	)
	return mr
}

// parseNumber reads the number of a merge request, written 3 or !3.
func parseNumber(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "!"), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid merge request number %q: use a positive number such as 3 or !3", s)
	}
	return n, nil
}

// readText returns value, or all of standard input when value is "-".
func readText(cmd *cobra.Command, value string) (string, error) {
	if value != "-" {
		return value, nil
	}
	b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	if err != nil {
		return "", fmt.Errorf("read standard input: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func completeMRRepoThenNumber(env *cliEnv) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return repoNames(env, cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		case 1:
			c, err := env.client(cmd, false)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			mrs, err := listMergeRequests(env.ctx(cmd), c, args[0], ermite.MergeRequestState_MERGE_REQUEST_STATE_UNSPECIFIED)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, m := range mrs {
				if n := strconv.FormatInt(m.GetNumber(), 10); strings.HasPrefix(n, toComplete) {
					out = append(out, n+"\t"+stateName(m.GetState())+", "+clean(m.GetTitle()))
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func listMergeRequests(ctx context.Context, c *client.Client, repo string, state ermite.MergeRequestState) ([]*ermite.MergeRequest, error) {
	return collect(ctx, func(token string) ([]*ermite.MergeRequest, string, error) {
		res, err := c.MergeRequests().ListMergeRequests(ctx, connect.NewRequest(&ermite.ListMergeRequestsRequest{
			Repo: repo, State: state, PageToken: token,
		}))
		if err != nil {
			return nil, "", c.Describe(err)
		}
		return res.Msg.GetMergeRequests(), res.Msg.GetNextPageToken(), nil
	})
}

func newMRCreateCmd(env *cliEnv) *cobra.Command {
	var title, description, source, target string
	cmd := &cobra.Command{
		Use:   "create <repo>",
		Short: "Open a merge request",
		Long: "Open a merge request for a branch of the repository. --target defaults to the default branch.\n" +
			"The title is one line of at most 120 characters, the description plain text of at most 8000;\n" +
			"neither may hold control characters (a description may hold line breaks) or bidirectional\n" +
			"override characters. --description - reads it from standard input. Fails if an open merge request\n" +
			"already has the same source and target. Needs the mr:write scope." + jsonMRHelp,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&title, "title", "", "title of the merge request (required)")
	cmd.Flags().StringVar(&description, "description", "", "description, or - to read standard input")
	cmd.Flags().StringVar(&source, "source", "", "branch to merge (required)")
	cmd.Flags().StringVar(&target, "target", "", "branch to merge into (default: the default branch)")
	_ = cmd.MarkFlagRequired("title")
	_ = cmd.MarkFlagRequired("source")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		desc, err := readText(cmd, description)
		if err != nil {
			return err
		}
		req := &ermite.CreateMergeRequestRequest{
			Repo: args[0], Title: title, Description: desc,
			SourceBranch: trimBranch(source), TargetBranch: trimBranch(target),
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().CreateMergeRequest(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "opened")
	}
	return cmd
}

func trimBranch(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "refs/heads/") }

// outputMR prints a merge request as JSON, or as one confirmation line.
func outputMR(cmd *cobra.Command, m *ermite.MergeRequest, asJSON bool, verb string) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONMergeRequest(m))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s!%d (%s into %s)\n", verb, clean(m.GetRepo()), m.GetNumber(),
		clean(m.GetSourceBranch()), clean(m.GetTargetBranch()))
	return err
}

func newMRListCmd(env *cliEnv) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List the merge requests of a repository",
		Long: "List the merge requests of a repository, newest first. --state restricts them to open, merged or\n" +
			"closed ones (default: open; all shows every state)." + jsonMRHelp + "\n--json prints an array of merge requests.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&state, "state", stateOpen, "state: open, merged, closed or all")
	_ = cmd.RegisterFlagCompletionFunc("state", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{stateOpen, stateMerged, stateClosed, stateAll}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		st, err := parseState(state)
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		mrs, err := listMergeRequests(env.ctx(cmd), c, args[0], st)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonMergeRequest, 0, len(mrs))
			for _, m := range mrs {
				out = append(out, toJSONMergeRequest(m))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(mrs) == 0 {
			noticef(cmd, "no merge requests")
			return nil
		}
		rows := make([][]string, 0, len(mrs))
		for _, m := range mrs {
			rows = append(rows, []string{
				"!" + strconv.FormatInt(m.GetNumber(), 10), stateName(m.GetState()),
				m.GetSourceBranch() + " -> " + m.GetTargetBranch(), m.GetAuthor(),
				shortTime(m.GetUpdatedAt(), "-"), m.GetTitle(),
			})
		}
		return table(cmd.OutOrStdout(), []string{"NUMBER", "STATE", "BRANCHES", "AUTHOR", "UPDATED", "TITLE"}, rows)
	}
	return cmd
}

// jsonStats are the totals of a diff.
type jsonStats struct {
	FilesChanged int32 `json:"files_changed"`
	Additions    int32 `json:"additions"`
	Deletions    int32 `json:"deletions"`
}

type jsonLine struct {
	Kind      string `json:"kind"`
	OldNumber int32  `json:"old_number"`
	NewNumber int32  `json:"new_number"`
	Text      string `json:"text"`
}

type jsonHunk struct {
	Header   string     `json:"header"`
	OldStart int32      `json:"old_start"`
	OldLines int32      `json:"old_lines"`
	NewStart int32      `json:"new_start"`
	NewLines int32      `json:"new_lines"`
	Lines    []jsonLine `json:"lines"`
}

type jsonFile struct {
	OldPath   string     `json:"old_path"`
	NewPath   string     `json:"new_path"`
	Status    string     `json:"status"`
	Additions int32      `json:"additions"`
	Deletions int32      `json:"deletions"`
	Binary    bool       `json:"binary"`
	Truncated bool       `json:"truncated"`
	Hunks     []jsonHunk `json:"hunks,omitempty"`
}

type jsonCommit struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Summary string `json:"summary"`
}

// jsonMRDetail is the stable JSON shape of `mr show --json`.
type jsonMRDetail struct {
	jsonMergeRequest
	Commits          []jsonCommit  `json:"commits"`
	CommitsTruncated bool          `json:"commits_truncated"`
	DiffBaseID       string        `json:"diff_base_id"`
	Stats            jsonStats     `json:"stats"`
	DiffTruncated    bool          `json:"diff_truncated"`
	Files            []jsonFile    `json:"files"`
	Comments         []jsonComment `json:"comments"`
	LastReadID       string        `json:"last_read_id"`
	LastReadLost     bool          `json:"last_read_lost"`
}

func fileStatusName(s ermite.FileStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "FILE_STATUS_"))
}

func lineKindName(k ermite.LineKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "LINE_KIND_"))
}

func toJSONDetail(d *ermite.GetMergeRequestResponse, withDiff bool) jsonMRDetail {
	out := jsonMRDetail{
		jsonMergeRequest: toJSONMergeRequest(d.GetMergeRequest()),
		Commits:          []jsonCommit{}, CommitsTruncated: d.GetCommitsTruncated(),
		DiffBaseID: d.GetDiffBaseId(),
		Stats: jsonStats{
			FilesChanged: d.GetStats().GetFilesChanged(), Additions: d.GetStats().GetAdditions(), Deletions: d.GetStats().GetDeletions(),
		},
		DiffTruncated: d.GetDiff().GetTruncated(),
		Files:         []jsonFile{}, Comments: []jsonComment{},
		LastReadID: d.GetLastReadId(), LastReadLost: d.GetLastReadLost(),
	}
	for _, c := range d.GetCommits() {
		out.Commits = append(out.Commits, jsonCommit{ID: c.GetId(), Author: c.GetAuthor().GetName(), Summary: c.GetSummary()})
	}
	for _, f := range d.GetDiff().GetFiles() {
		jf := jsonFile{
			OldPath: f.GetOldPath(), NewPath: f.GetNewPath(), Status: fileStatusName(f.GetStatus()),
			Additions: f.GetAdditions(), Deletions: f.GetDeletions(), Binary: f.GetBinary(), Truncated: f.GetTruncated(),
		}
		if withDiff {
			for _, h := range f.GetHunks() {
				jh := jsonHunk{
					Header: h.GetHeader(), OldStart: h.GetOldStart(), OldLines: h.GetOldLines(),
					NewStart: h.GetNewStart(), NewLines: h.GetNewLines(), Lines: []jsonLine{},
				}
				for _, l := range h.GetLines() {
					jh.Lines = append(jh.Lines, jsonLine{
						Kind: lineKindName(l.GetKind()), OldNumber: l.GetOldNumber(), NewNumber: l.GetNewNumber(), Text: l.GetText(),
					})
				}
				jf.Hunks = append(jf.Hunks, jh)
			}
		}
		out.Files = append(out.Files, jf)
	}
	for _, c := range d.GetComments() {
		out.Comments = append(out.Comments, toJSONComment(c))
	}
	return out
}

func newMRShowCmd(env *cliEnv) *cobra.Command {
	var sinceLastRead, withDiff, markRead bool
	cmd := &cobra.Command{
		Use:   "show <repo> <number>",
		Short: "Show a merge request with its commits, comments and diff",
		Long: "Show a merge request: its fields, the commits of the branch that the target lacks, the changed\n" +
			"files and the comments. --diff adds the diff, with the line numbers to use in `mr comment`.\n" +
			"--since-last-read shows the diff since the head recorded by --mark-read (or by the web) instead of\n" +
			"the whole change; after a force-push it still works when the recorded commit exists, else the\n" +
			"whole change is shown and the output says so. --mark-read records the head shown as read; it needs\n" +
			"the mr:write scope." + jsonMRHelp + jsonCommentHelp + "\n" +
			"--json adds to the merge request fields: commits (id, author, summary), commits_truncated,\n" +
			"diff_base_id (the commit the diff starts from), stats (files_changed, additions, deletions),\n" +
			"diff_truncated, files (old_path, new_path, status, additions, deletions, binary, truncated and,\n" +
			"with --diff, hunks of lines with kind, old_number, new_number and text), comments, last_read_id\n" +
			"and last_read_lost.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().BoolVar(&withDiff, "diff", false, "include the diff")
	cmd.Flags().BoolVar(&sinceLastRead, "since-last-read", false, "diff since the head last marked read")
	cmd.Flags().BoolVar(&markRead, "mark-read", false, "record the head shown as read (needs mr:write)")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		res, err := c.MergeRequests().GetMergeRequest(ctx, connect.NewRequest(&ermite.GetMergeRequestRequest{
			Repo: args[0], Number: n, SinceLastRead: sinceLastRead,
		}))
		if err != nil {
			return c.Describe(err)
		}
		d := res.Msg
		if *asJSON {
			err = writeJSON(cmd.OutOrStdout(), toJSONDetail(d, withDiff))
		} else {
			err = printMRDetail(cmd.OutOrStdout(), d, withDiff, sinceLastRead)
		}
		if err != nil {
			return err
		}
		if markRead && d.GetMergeRequest().GetHeadId() != "" {
			_, err = c.MergeRequests().MarkRead(ctx, connect.NewRequest(&ermite.MarkReadRequest{
				Repo: args[0], Number: n, HeadId: d.GetMergeRequest().GetHeadId(),
			}))
			if err != nil {
				return c.Describe(err)
			}
		}
		if d.GetLastReadLost() {
			noticef(cmd, "the head you last read is gone (force-push?): showing the whole change")
		}
		return nil
	}
	return cmd
}

func printMRDetail(w io.Writer, d *ermite.GetMergeRequestResponse, withDiff, since bool) error {
	m := d.GetMergeRequest()
	desc := m.GetDescription()
	if desc == "" {
		desc = "-"
	}
	pairs := [][2]string{
		{"merge request", fmt.Sprintf("%s!%d", m.GetRepo(), m.GetNumber())},
		{"title", m.GetTitle()},
		{"state", stateName(m.GetState())},
		{"branches", m.GetSourceBranch() + " -> " + m.GetTargetBranch()},
		{"author", m.GetAuthor()},
		{"created", shortTime(m.GetCreatedAt(), "-")},
		{"updated", shortTime(m.GetUpdatedAt(), "-")},
		{"head", orDash(m.GetHeadId())},
	}
	if m.GetState() == ermite.MergeRequestState_MERGE_REQUEST_STATE_MERGED {
		pairs = append(pairs, [2]string{"merged", orDash(m.GetMergedCommitId()) + " (" + modeName(m.GetMergeMode()) + ")"})
	}
	if err := fields(w, pairs); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", indentText(desc))

	_, _ = fmt.Fprintf(w, "\ncommits (%d%s)\n", len(d.GetCommits()), plus(d.GetCommitsTruncated()))
	for _, c := range d.GetCommits() {
		_, _ = fmt.Fprintf(w, "  %s  %s  %s\n", shortID(c.GetId()), clean(c.GetAuthor().GetName()), clean(c.GetSummary()))
	}

	st := d.GetStats()
	what := "changes"
	if since && !d.GetLastReadLost() && d.GetLastReadId() != "" {
		what = "changes since " + shortID(d.GetLastReadId())
	}
	_, _ = fmt.Fprintf(w, "\n%s: %d file(s), +%d -%d, from %s\n", what, st.GetFilesChanged(), st.GetAdditions(), st.GetDeletions(), orDash(shortID(d.GetDiffBaseId())))
	for _, f := range d.GetDiff().GetFiles() {
		path := f.GetNewPath()
		if path == "" {
			path = f.GetOldPath()
		}
		note := ""
		if f.GetTruncated() {
			note = " (too large, not shown)"
		}
		_, _ = fmt.Fprintf(w, "  %-8s +%d -%d  %s%s\n", fileStatusName(f.GetStatus()), f.GetAdditions(), f.GetDeletions(), clean(path), note)
	}
	if d.GetDiff().GetTruncated() {
		_, _ = fmt.Fprintln(w, "  (the diff is truncated)")
	}

	_, _ = fmt.Fprintf(w, "\ncomments (%d)\n", len(d.GetComments()))
	for _, c := range d.GetComments() {
		where := "general"
		if c.GetPath() != "" {
			where = fmt.Sprintf("%s:%d (%s)", clean(c.GetPath()), c.GetLine(), sideName(c.GetSide()))
		}
		mark := ""
		if c.GetResolved() {
			mark = " [resolved]"
		}
		_, _ = fmt.Fprintf(w, "  #%s %s, %s, %s%s\n%s\n", c.GetId(), clean(c.GetAuthor()), shortTime(c.GetCreatedAt(), "-"), where, mark, indentText(c.GetText()))
	}
	if withDiff {
		printDiff(w, d.GetDiff())
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plus(truncated bool) string {
	if truncated {
		return "+"
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// indentText indents a multi-line text by four spaces, making each line safe
// for a terminal.
func indentText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + client.Clean(l)
	}
	return strings.Join(lines, "\n")
}

// printDiff prints the diff with a gutter of the old and new line numbers,
// which are the numbers `mr comment --line` takes with --side old or new.
func printDiff(w io.Writer, d *ermite.Diff) {
	_, _ = fmt.Fprintln(w, "\ndiff (old and new line numbers on the left)")
	num := func(n int32) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(int(n))
	}
	for _, f := range d.GetFiles() {
		_, _ = fmt.Fprintf(w, "\n--- %s\n+++ %s\n", clean(orDash(f.GetOldPath())), clean(orDash(f.GetNewPath())))
		if f.GetBinary() {
			_, _ = fmt.Fprintln(w, "(binary file)")
		}
		if f.GetTruncated() {
			_, _ = fmt.Fprintln(w, "(too large, not shown)")
		}
		for _, h := range f.GetHunks() {
			_, _ = fmt.Fprintf(w, "@@ -%d,%d +%d,%d @@ %s\n", h.GetOldStart(), h.GetOldLines(), h.GetNewStart(), h.GetNewLines(), clean(h.GetHeader()))
			for _, l := range h.GetLines() {
				sign := map[ermite.LineKind]string{
					ermite.LineKind_LINE_KIND_ADDITION: "+", ermite.LineKind_LINE_KIND_DELETION: "-",
				}[l.GetKind()]
				if sign == "" {
					sign = " "
				}
				_, _ = fmt.Fprintf(w, "%5s %5s %s%s\n", num(l.GetOldNumber()), num(l.GetNewNumber()), sign, clean(l.GetText()))
			}
		}
	}
}

func newMRCommentCmd(env *cliEnv) *cobra.Command {
	var message, path, side string
	var line int
	cmd := &cobra.Command{
		Use:   "comment <repo> <number>",
		Short: "Comment on a merge request",
		Long: "Add a plain-text comment (at most 8000 characters, no control characters except line breaks).\n" +
			"-m - reads it from standard input. Without --path it is a general comment. With --path, --line and\n" +
			"--side (default new) it is attached to a line of the current diff: the path as in `mr show --diff`\n" +
			"(the new path, the old one for a deleted file), the line number on the old or new side; a line\n" +
			"that is not in the diff is refused. Needs the mr:write scope." + jsonCommentHelp,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "text of the comment, or - to read standard input (required)")
	cmd.Flags().StringVar(&path, "path", "", "file the comment is attached to")
	cmd.Flags().IntVar(&line, "line", 0, "1-based line number on --side")
	cmd.Flags().StringVar(&side, "side", sideNew, "side the line counts on: old or new")
	_ = cmd.MarkFlagRequired("message")
	_ = cmd.RegisterFlagCompletionFunc("side", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{sideOld, sideNew}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		text, err := readText(cmd, message)
		if err != nil {
			return err
		}
		req := &ermite.AddCommentRequest{Repo: args[0], Number: n, Text: text}
		switch {
		case path != "":
			if line < 1 {
				return errors.New("--line is required with --path: give the line number")
			}
			if req.Side, err = parseSide(side); err != nil {
				return err
			}
			req.Path, req.Line = path, int32(line) //nolint:gosec // a line number typed by a person
		case line != 0 || cmd.Flags().Changed("side"):
			return errors.New("--line and --side need --path")
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().AddComment(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONComment(res.Msg.GetComment()))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "comment %s added to %s!%d\n", res.Msg.GetComment().GetId(), clean(args[0]), n)
		return err
	}
	return cmd
}

func newMRResolveCmd(env *cliEnv) *cobra.Command {
	var undo bool
	cmd := &cobra.Command{
		Use:   "resolve <repo> <number> <comment-id>",
		Short: "Mark a comment resolved",
		Long: "Mark a comment of a merge request resolved, or unresolved with --undo. Doing it twice is not an\n" +
			"error. Needs the mr:write scope." + jsonCommentHelp,
		Args:              cobra.ExactArgs(3),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().BoolVar(&undo, "undo", false, "mark the comment unresolved instead")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		id := strings.TrimPrefix(strings.TrimSpace(args[2]), "#")
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().ResolveComment(env.ctx(cmd), connect.NewRequest(&ermite.ResolveCommentRequest{
			Repo: args[0], Number: n, CommentId: id, Resolved: !undo,
		}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONComment(res.Msg.GetComment()))
		}
		verb := "resolved"
		if undo {
			verb = "reopened"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "comment %s of %s!%d %s\n", clean(id), clean(args[0]), n, verb)
		return err
	}
	return cmd
}

func newMRMergeCmd(env *cliEnv) *cobra.Command {
	var squash, deleteBranch bool
	var head string
	cmd := &cobra.Command{
		Use:   "merge <repo> <number>",
		Short: "Merge a merge request",
		Long: "Merge an open merge request. The default is a fast-forward: the target branch moves to the head\n" +
			"and no commit is created. --squash makes one new commit with the tree of the head and the target\n" +
			"as only parent, signed by the server's key (see `ermite signing-key show`). The target must be an\n" +
			"ancestor of the head, else rebase the branch and push it again; the repository settings may allow\n" +
			"only one of the modes.\n\n" +
			"The server merges only if the source branch still points to the head you reviewed, so --head is\n" +
			"required: give the full id of the head that `ermite mr show <repo> <number>` printed when you\n" +
			"reviewed the change. If the branch moved since, the merge is refused: review again. Needs the\n" +
			"mr:merge scope.\n\n" +
			"--delete-branch deletes the source branch after the merge. The merge is refused when the branch\n" +
			"is the default branch or used by another open merge request. If the deletion fails after the\n" +
			"merge, the merge stands, the branch is kept and a warning goes to standard error." + jsonMRHelp +
			"\n--json adds source_branch_deleted (true when the branch was deleted).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().BoolVar(&squash, "squash", false, "squash the branch into one signed commit instead of a fast-forward")
	cmd.Flags().StringVar(&head, "head", "", "full id of the head you reviewed, from `ermite mr show` (required)")
	cmd.Flags().BoolVar(&deleteBranch, "delete-branch", false, "delete the source branch after the merge")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		head = strings.TrimSpace(head)
		if head == "" {
			return fmt.Errorf("--head is required: run `ermite mr show %s %d` and give the head id it prints, "+
				"so that only the commit you reviewed is merged", args[0], n)
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		mode := ermite.MergeMode_MERGE_MODE_FAST_FORWARD
		if squash {
			mode = ermite.MergeMode_MERGE_MODE_SQUASH
		}
		res, err := c.MergeRequests().MergeMergeRequest(ctx, connect.NewRequest(&ermite.MergeMergeRequestRequest{
			Repo: args[0], Number: n, Mode: mode, HeadId: head, DeleteSourceBranch: deleteBranch,
		}))
		if err != nil {
			return c.Describe(err)
		}
		m := res.Msg.GetMergeRequest()
		deleted := res.Msg.GetSourceBranchDeleted()
		if deleteBranch && !deleted {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ermite: warning: the branch %s was kept (it moved after the merge, or the server cannot delete it)\n",
				clean(m.GetSourceBranch()))
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), jsonMerged{jsonMergeRequest: toJSONMergeRequest(m), SourceBranchDeleted: deleted})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged %s!%d into %s (%s) at %s\n", clean(m.GetRepo()), m.GetNumber(),
			clean(m.GetTargetBranch()), modeName(m.GetMergeMode()), shortID(m.GetMergedCommitId()))
		if err == nil && deleted {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted branch %s\n", clean(m.GetSourceBranch()))
		}
		return err
	}
	return cmd
}

func newMRCloseCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close <repo> <number>",
		Short: "Close a merge request without merging it",
		Long: "Close an open merge request without merging it. A closed merge request cannot be reopened: open a\n" +
			"new one. Needs the mr:write scope." + jsonMRHelp,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd, true)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().CloseMergeRequest(env.ctx(cmd), connect.NewRequest(&ermite.CloseMergeRequestRequest{Repo: args[0], Number: n}))
		if err != nil {
			return c.Describe(err)
		}
		return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "closed")
	}
	return cmd
}
