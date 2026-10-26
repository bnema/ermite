// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
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
	modeMerge       = "merge"
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
	case ermite.MergeMode_MERGE_MODE_MERGE_COMMIT:
		return modeMerge
	}
	return ""
}

func parseMode(s string) (ermite.MergeMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case modeFastForward, "ff":
		return ermite.MergeMode_MERGE_MODE_FAST_FORWARD, nil
	case modeSquash:
		return ermite.MergeMode_MERGE_MODE_SQUASH, nil
	case modeMerge:
		return ermite.MergeMode_MERGE_MODE_MERGE_COMMIT, nil
	}
	return 0, fmt.Errorf("invalid merge mode %q: use %s, %s or %s", s, modeFastForward, modeSquash, modeMerge)
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
	URL            string  `json:"url"`
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
	// AutoMerge is the pending auto-merge, null when there is none.
	AutoMerge *jsonAutoMerge `json:"auto_merge"`
	// AutoMerged is true when the server merged the request by auto-merge.
	AutoMerged bool `json:"auto_merged"`
}

// jsonAutoMerge is the stable JSON shape of a pending auto-merge.
type jsonAutoMerge struct {
	Mode      string  `json:"mode"`
	HeadID    string  `json:"head_id"`
	EnabledBy string  `json:"enabled_by"`
	EnabledAt *string `json:"enabled_at"`
}

// toJSONAutoMerge maps an auto-merge; nil when there is none.
func toJSONAutoMerge(a *ermite.AutoMerge) *jsonAutoMerge {
	if a == nil {
		return nil
	}
	return &jsonAutoMerge{Mode: modeName(a.GetMode()), HeadID: a.GetHeadId(), EnabledBy: a.GetEnabledBy(), EnabledAt: jsonTime(a.GetEnabledAt())}
}

// jsonMergeability is the stable JSON shape of the mergeability of an open
// merge request.
type jsonMergeability struct {
	// State is "checking", "up_to_date", "behind" or "conflict".
	State string `json:"state"`
	// Behind counts the commits of the target the head lacks (behind only);
	// BehindCapped says it is a lower bound.
	Behind        int32    `json:"behind"`
	BehindCapped  bool     `json:"behind_capped"`
	ConflictPaths []string `json:"conflict_paths"`
	// ConflictPathsTruncated says that more paths conflict than listed.
	ConflictPathsTruncated bool   `json:"conflict_paths_truncated"`
	TargetID               string `json:"target_id"`
	HeadID                 string `json:"head_id"`
}

func mergeabilityName(s ermite.MergeabilityState) string {
	switch s {
	case ermite.MergeabilityState_MERGEABILITY_STATE_CHECKING:
		return "checking"
	case ermite.MergeabilityState_MERGEABILITY_STATE_UP_TO_DATE:
		return "up_to_date"
	case ermite.MergeabilityState_MERGEABILITY_STATE_BEHIND:
		return "behind"
	case ermite.MergeabilityState_MERGEABILITY_STATE_CONFLICT:
		return "conflict"
	}
	return ""
}

// toJSONMergeability maps a mergeability; nil when the server sent none.
func toJSONMergeability(m *ermite.Mergeability) *jsonMergeability {
	if m == nil {
		return nil
	}
	return &jsonMergeability{
		State: mergeabilityName(m.GetState()), Behind: m.GetBehind(), BehindCapped: m.GetBehindCapped(),
		ConflictPaths: append([]string{}, m.GetConflictPaths()...), ConflictPathsTruncated: m.GetConflictPathsTruncated(),
		TargetID: m.GetTargetId(), HeadID: m.GetHeadId(),
	}
}

// mergeabilityText is the human form of a mergeability: "up to date",
// "behind by 2", "conflict (a, b, ...)", "checking".
func mergeabilityText(m *jsonMergeability) string {
	switch m.State {
	case "up_to_date":
		return "up to date with the target, can be fast-forwarded"
	case "behind":
		return fmt.Sprintf("behind by %d%s, merges without conflicts (squash or merge commit; fast-forward needs a rebase)", m.Behind, plus(m.BehindCapped))
	case "conflict":
		if len(m.ConflictPaths) == 0 {
			return "conflict (no common history with the target), rebase needed"
		}
		more := ""
		if m.ConflictPathsTruncated {
			more = ", ..."
		}
		return "conflict (" + strings.Join(m.ConflictPaths, ", ") + more + "), rebase needed"
	case "checking":
		return "checking"
	}
	return m.State
}

// jsonMerged is the output of `mr merge --json`.
type jsonMerged struct {
	jsonMergeRequest
	SourceBranchDeleted bool `json:"source_branch_deleted"`
}

func toJSONMergeRequest(m *ermite.MergeRequest, baseURL string) jsonMergeRequest {
	return jsonMergeRequest{
		URL:  mrWebURL(baseURL, m.GetRepo(), m.GetNumber()),
		Repo: m.GetRepo(), Number: m.GetNumber(), Title: m.GetTitle(), Description: m.GetDescription(),
		SourceBranch: m.GetSourceBranch(), TargetBranch: m.GetTargetBranch(), State: stateName(m.GetState()),
		Author: m.GetAuthor(), CreatedAt: jsonTime(m.GetCreatedAt()), UpdatedAt: jsonTime(m.GetUpdatedAt()),
		HeadID: m.GetHeadId(), MergedCommitID: m.GetMergedCommitId(), MergeMode: modeName(m.GetMergeMode()),
		AutoMerge: toJSONAutoMerge(m.GetAutoMerge()), AutoMerged: m.GetAutoMerged(),
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
	ReplyTo   string  `json:"reply_to"`
	StartLine int32   `json:"start_line"`
	Verdict   string  `json:"verdict"`
	EditedAt  *string `json:"edited_at"`
	Deleted   bool    `json:"deleted"`
}

func verdictName(v ermite.Verdict) string {
	switch v {
	case ermite.Verdict_VERDICT_APPROVE:
		return "approve"
	case ermite.Verdict_VERDICT_CHANGES_REQUESTED:
		return "changes_requested"
	}
	return ""
}

func toJSONComment(c *ermite.Comment) jsonComment {
	return jsonComment{
		ID: c.GetId(), Author: c.GetAuthor(), Path: c.GetPath(), Line: c.GetLine(), Side: sideName(c.GetSide()),
		CommitID: c.GetCommitId(), Text: c.GetText(), Resolved: c.GetResolved(), CreatedAt: jsonTime(c.GetCreatedAt()),
		ReplyTo: c.GetReplyTo(), StartLine: c.GetStartLine(), Verdict: verdictName(c.GetVerdict()), EditedAt: jsonTime(c.GetEditedAt()), Deleted: c.GetDeleted(),
	}
}

const jsonMRHelp = `
JSON fields of a merge request (--json): repo, url (full web URL), number, title, description, source_branch,
target_branch, state ("open", "merged" or "closed"), author (token label), created_at and
updated_at (RFC 3339 UTC), head_id (the commit the source branch points to; empty when the
branch is gone), merged_commit_id and merge_mode ("fast-forward", "squash" or "merge"; both empty
unless merged), auto_merge (null unless auto-merge is pending: mode, head_id, enabled_by,
enabled_at) and auto_merged (true when the server merged the request by auto-merge).`

const jsonMergeabilityHelp = `
mr show --json adds mergeability, null unless the request is open: state ("checking", "up_to_date",
"behind" or "conflict"), behind and behind_capped (commits of the target the head lacks; behind
only), conflict_paths and conflict_paths_truncated (conflict only), target_id and head_id (the
commits the result is for).`

const jsonCommentHelp = `
JSON fields of a comment: id, author, path, line and side ("old" or "new"; empty path, line 0
and empty side for a general comment), commit_id (the head when it was written), text,
resolved, created_at, reply_to (empty for roots), start_line (0 for a single line),
verdict ("", "approve", "changes_requested"), edited_at (nullable RFC 3339), deleted.`

func newMRCmd(env *cliEnv) *cobra.Command {
	mr := &cobra.Command{
		Use:   "mr",
		Short: "Review and merge branches on a remote server",
		Long: "Merge requests ask to merge one branch of a repository into another. They are about a branch,\n" +
			"not about who wrote it. Needs a server with the merge_requests feature (see `ermite version`).\n" +
			"Identify it by repository and number: `ermite mr show proj 3`." + jsonMRHelp + jsonCommentHelp,
	}
	mr.AddCommand(
		newMRCreateCmd(env), newMREditCmd(env), newMRListCmd(env), newMRShowCmd(env), newMRCommentCmd(env),
		newMRResolveCmd(env), newMREditCommentCmd(env), newMRDeleteCommentCmd(env), newMRMergeCmd(env), newMRCloseCmd(env), newMRReadyCmd(env),
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
			c, err := env.completionClient(cmd)
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
	var title, descriptionFile, source, target string
	cmd := &cobra.Command{
		Use:   "create <repo>",
		Short: "Open a merge request",
		Long: "Open a merge request for a branch of the repository. --target defaults to the default branch.\n" +
			"The title is one line of at most 120 characters, the description plain text of at most 8000;\n" +
			"neither may hold control characters (a description may hold line breaks) or bidirectional\n" +
			"override characters. --description-file PATH reads the optional description from a file; - reads\n" +
			"standard input (1 MiB limit). Fails if an open merge request already has the same source and target.\n" +
			"Prints a confirmation, then the full web URL on its own line.\n" +
			"Needs the mr:write scope." + "\nJSON: a merge request (see `ermite mr --help`).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&title, "title", "", "title of the merge request (required)")
	cmd.Flags().StringVar(&descriptionFile, "description-file", "", "read the description from this file, or - for standard input")
	_ = cmd.MarkFlagFilename("description-file")
	cmd.Flags().StringVar(&source, "source", "", "branch to merge (required)")
	cmd.Flags().StringVar(&target, "target", "", "branch to merge into (default: the default branch)")
	_ = cmd.MarkFlagRequired("title")
	_ = cmd.MarkFlagRequired("source")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var desc string
		if cmd.Flags().Changed("description-file") {
			value, err := readTextFile(cmd, descriptionFile)
			if err != nil {
				return fmt.Errorf("read --description-file: %w", err)
			}
			desc = value
		}
		req := &ermite.CreateMergeRequestRequest{
			Repo: args[0], Title: title, Description: desc,
			SourceBranch: trimBranch(source), TargetBranch: trimBranch(target),
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().CreateMergeRequest(env.ctx(cmd), connect.NewRequest(req))
		if err != nil {
			return c.Describe(err)
		}
		return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "opened", c.URL())
	}
	return cmd
}

func trimBranch(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "refs/heads/") }

// mrWebURL builds the web route with the repository as a single path segment.
// Repository names currently forbid slashes, but escape all path delimiters defensively.
func mrWebURL(baseURL, repo string, number int64) string {
	return itemWebURL(baseURL, repo, "merge-requests", number)
}

// itemWebURL builds <base>/<repo>/<kind>/<number>.
func itemWebURL(baseURL, repo, kind string, number int64) string {
	return strings.TrimRight(baseURL, "/") + "/" + url.PathEscape(repo) + "/" + kind + "/" + strconv.FormatInt(number, 10)
}

// outputMR prints a merge request as JSON, or a confirmation followed by its URL.
func outputMR(cmd *cobra.Command, m *ermite.MergeRequest, asJSON bool, verb, baseURL string) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONMergeRequest(m, baseURL))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s!%d (%s into %s)\n%s\n", verb, clean(m.GetRepo()), m.GetNumber(),
		clean(m.GetSourceBranch()), clean(m.GetTargetBranch()), mrWebURL(baseURL, m.GetRepo(), m.GetNumber()))
	return err
}

func newMRListCmd(env *cliEnv) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List the merge requests of a repository",
		Long: "List the merge requests of a repository, newest first. --state restricts them to open, merged or\n" +
			"closed ones (default: open; all shows every state)." + "\nJSON: a merge request (see `ermite mr --help`)." + "\n--json prints an array of merge requests.",
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
		c, err := env.client(cmd)
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
				out = append(out, toJSONMergeRequest(m, c.URL()))
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
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	Status    string `json:"status"`
	Additions int32  `json:"additions"`
	Deletions int32  `json:"deletions"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	// Omitted is generated, large, budget or unavailable; absent when the hunks are not omitted.
	Omitted string     `json:"omitted,omitempty"`
	Hunks   []jsonHunk `json:"hunks,omitempty"`
}

type jsonCommit struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Summary string `json:"summary"`
}

// jsonMRDetail is the stable JSON shape of `mr show --json`.
type jsonMRDetail struct {
	Issues      []jsonMRIssue    `json:"issues"`
	IssueEvents []jsonIssueEvent `json:"issue_events"`
	jsonMergeRequest
	// Mergeability is null unless the request is open and the server computes it.
	Mergeability     *jsonMergeability   `json:"mergeability"`
	Commits          []jsonCommit        `json:"commits"`
	CommitsTruncated bool                `json:"commits_truncated"`
	DiffBaseID       string              `json:"diff_base_id"`
	Stats            jsonStats           `json:"stats"`
	DiffTruncated    bool                `json:"diff_truncated"`
	Files            []jsonFile          `json:"files"`
	Comments         []jsonComment       `json:"comments"`
	Verdicts         []jsonAuthorVerdict `json:"verdicts"`
	LastReadID       string              `json:"last_read_id"`
	LastReadLost     bool                `json:"last_read_lost"`
}

type jsonMRIssue struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Closes bool   `json:"closes"`
}

type jsonAuthorVerdict struct {
	Author    string  `json:"author"`
	Verdict   string  `json:"verdict"`
	CommentID string  `json:"comment_id"`
	CommitID  string  `json:"commit_id"`
	CreatedAt *string `json:"created_at"`
	Stale     bool    `json:"stale"`
}

func fileStatusName(s ermite.FileStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "FILE_STATUS_"))
}

// fileOmissionName is the lowercase reason of an omitted file, "" for none.
func fileOmissionName(o ermite.FileOmission) string {
	if o == ermite.FileOmission_FILE_OMISSION_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(o.String(), "FILE_OMISSION_"))
}

func lineKindName(k ermite.LineKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "LINE_KIND_"))
}

func toJSONDetail(d *ermite.GetMergeRequestResponse, withDiff bool, baseURL string) jsonMRDetail {
	out := jsonMRDetail{
		jsonMergeRequest: toJSONMergeRequest(d.GetMergeRequest(), baseURL),
		Issues:           []jsonMRIssue{}, IssueEvents: toJSONIssueEvents(d.GetIssueEvents()),
		Mergeability: toJSONMergeability(d.GetMergeRequest().GetMergeability()),
		Commits:      []jsonCommit{}, CommitsTruncated: d.GetCommitsTruncated(),
		DiffBaseID: d.GetDiffBaseId(),
		Stats: jsonStats{
			FilesChanged: d.GetStats().GetFilesChanged(), Additions: d.GetStats().GetAdditions(), Deletions: d.GetStats().GetDeletions(),
		},
		DiffTruncated: d.GetDiff().GetTruncated(),
		Files:         []jsonFile{}, Comments: []jsonComment{}, Verdicts: []jsonAuthorVerdict{},
		LastReadID: d.GetLastReadId(), LastReadLost: d.GetLastReadLost(),
	}
	for _, i := range d.GetIssues() {
		out.Issues = append(out.Issues, jsonMRIssue{Number: i.GetNumber(), Title: i.GetTitle(), State: mrIssueStateName(i.GetState()), Closes: i.GetCloses()})
	}
	for _, c := range d.GetCommits() {
		out.Commits = append(out.Commits, jsonCommit{ID: c.GetId(), Author: c.GetAuthor().GetName(), Summary: c.GetSummary()})
	}
	for _, f := range d.GetDiff().GetFiles() {
		jf := jsonFile{
			OldPath: f.GetOldPath(), NewPath: f.GetNewPath(), Status: fileStatusName(f.GetStatus()),
			Additions: f.GetAdditions(), Deletions: f.GetDeletions(), Binary: f.GetBinary(), Truncated: f.GetTruncated(),
			Omitted: fileOmissionName(f.GetOmitted()),
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
	for _, v := range d.GetVerdicts() {
		out.Verdicts = append(out.Verdicts, jsonAuthorVerdict{Author: v.GetAuthor(), Verdict: verdictName(v.GetVerdict()), CommentID: v.GetCommentId(), CommitID: v.GetCommitId(), CreatedAt: jsonTime(v.GetCreatedAt()), Stale: v.GetStale()})
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
			"the mr:write scope. The mergeability line says whether the branch merges with its target now: up to\n" +
			"date (can be fast-forwarded), behind by N commits (merges without conflicts, squash or merge commit),\n" +
			"or in conflict with the paths that conflict (rebase needed)." + "\nJSON: a merge request (see `ermite mr --help`)." + jsonMergeabilityHelp + "\nJSON: a comment (see `ermite mr --help`)." + jsonIssueEventHelp + "\n" +
			"--json adds issues (number, title, state: open/closed/resolved, closes), issue_events and commits (id, author, summary), commits_truncated,\n" +
			"diff_base_id (the commit the diff starts from), stats (files_changed, additions, deletions),\n" +
			"diff_truncated, files (old_path, new_path, status, additions, deletions, binary, truncated, omitted (generated, large, budget or unavailable; absent when the hunks are\n" +
			"not omitted) and, with --diff, hunks of lines with kind, old_number, new_number and text), comments, last_read_id\n" +
			"and last_read_lost; verdicts (author, verdict, comment_id, commit_id, created_at, stale).",
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
		c, err := env.client(cmd)
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
			err = writeJSON(cmd.OutOrStdout(), toJSONDetail(d, withDiff, c.URL()))
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
		merged := orDash(m.GetMergedCommitId()) + " (" + modeName(m.GetMergeMode()) + ")"
		pairs = append(pairs, [2]string{"merged", merged})
		if m.GetAutoMerged() {
			pairs = append(pairs, [2]string{"auto-merged", "yes, by the server once the checks passed"})
		}
	}
	if am := m.GetAutoMerge(); am != nil && m.GetState() == ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN {
		pairs = append(pairs, [2]string{"auto-merge", modeName(am.GetMode()) + " when ready (enabled by " + am.GetEnabledBy() + ")"})
	}
	if mg := toJSONMergeability(m.GetMergeability()); mg != nil && m.GetState() == ermite.MergeRequestState_MERGE_REQUEST_STATE_OPEN {
		pairs = append(pairs, [2]string{"mergeability", mergeabilityText(mg)})
	}
	if len(d.GetVerdicts()) > 0 {
		var verdicts []string
		for _, v := range d.GetVerdicts() {
			var label string
			switch v.GetVerdict() {
			case ermite.Verdict_VERDICT_APPROVE:
				label = "approved"
			case ermite.Verdict_VERDICT_CHANGES_REQUESTED:
				label = "changes requested"
			default:
				label = fmt.Sprintf("verdict %d", v.GetVerdict())
			}
			if v.GetStale() {
				label += " (stale)"
			}
			verdicts = append(verdicts, v.GetAuthor()+" "+label)
		}
		pairs = append(pairs, [2]string{"verdicts", strings.Join(verdicts, ", ")})
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

	if len(d.GetIssues()) > 0 {
		_, _ = fmt.Fprintf(w, "\nissues (%d)\n", len(d.GetIssues()))
		for _, i := range d.GetIssues() {
			marker := ""
			if i.GetCloses() {
				marker = " · resolves"
			}
			_, _ = fmt.Fprintf(w, "  #%d %s %s%s\n", i.GetNumber(), mrIssueStateName(i.GetState()), clean(i.GetTitle()), marker)
		}
	}
	_, _ = fmt.Fprintf(w, "\ncomments (%d)\n", len(d.GetComments()))
	var timeline []issueTimelineRow
	for _, root := range d.GetComments() {
		if root.GetReplyTo() != "" {
			continue
		}
		thread := []*ermite.Comment{root}
		for _, reply := range d.GetComments() {
			if reply.GetReplyTo() == root.GetId() {
				thread = append(thread, reply)
			}
		}
		timeline = append(timeline, issueTimelineRow{at: root.GetCreatedAt(), print: func(w io.Writer) error {
			for _, c := range thread {
				where := "general"
				if c.GetPath() != "" {
					where = fmt.Sprintf("%s:%d (%s)", clean(c.GetPath()), c.GetLine(), sideName(c.GetSide()))
				}
				if c.GetStartLine() > 0 {
					where = fmt.Sprintf("%s:lines %d-%d (%s)", clean(c.GetPath()), c.GetStartLine(), c.GetLine(), sideName(c.GetSide()))
				}
				mark := ""
				if c.GetEditedAt() != nil {
					mark += " (edited)"
				}
				if c.GetDeleted() {
					mark += " [deleted]"
				}
				if c.GetVerdict() != ermite.Verdict_VERDICT_UNSPECIFIED {
					mark += " [" + verdictName(c.GetVerdict()) + "]"
				}
				if c.GetResolved() {
					mark += " [resolved]"
				}
				prefix := "  "
				body := indentText(c.GetText())
				if c.GetReplyTo() != "" {
					prefix += "    "
					body = "    " + strings.ReplaceAll(body, "\n", "\n    ")
				}
				_, err := fmt.Fprintf(w, "%s#%s %s, %s, %s%s\n%s\n", prefix, c.GetId(), clean(c.GetAuthor()), shortTime(c.GetCreatedAt(), "-"), where, mark, body)
				if err != nil {
					return err
				}
			}
			return nil
		}})
	}
	timeline = append(timeline, newIssueTimeline(d.GetIssueEvents(), true)...)
	if err := printIssueTimeline(w, timeline); err != nil {
		return err
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
	var message, path, side, line, reply string
	var approve, requestChanges bool
	cmd := &cobra.Command{
		Use:   "comment <repo> <number>",
		Short: "Comment on a merge request",
		Long: "Add a Markdown note (at most 8000 characters); ^N refers to a note in this merge request.\n" +
			"--reply replies to a root note. --line accepts N or A-B. --approve and --request-changes\n" +
			"set a verdict (needs mr:review) and allow an empty message.\n" +
			"-m - reads it from standard input. Without --path it is a general comment. With --path, --line and\n" +
			"--side (default new) it is attached to a line of the current diff: the path as in `mr show --diff`\n" +
			"(the new path, the old one for a deleted file), the line number on the old or new side; a line\n" +
			"that is not in the diff is refused. Needs the mr:write scope." + "\nJSON: a comment (see `ermite mr --help`).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Markdown text, or - to read standard input (required unless setting a verdict)")
	cmd.Flags().StringVar(&path, "path", "", "file the comment is attached to")
	cmd.Flags().StringVar(&line, "line", "", "1-based line N or range A-B on --side")
	cmd.Flags().StringVar(&reply, "reply", "", "reply to root note (^12, #12 or 12)")
	cmd.Flags().BoolVar(&approve, "approve", false, "approve (needs mr:review)")
	cmd.Flags().BoolVar(&requestChanges, "request-changes", false, "request changes (needs mr:review)")
	cmd.Flags().StringVar(&side, "side", sideNew, "side the line counts on: old or new")
	cmd.MarkFlagsMutuallyExclusive("approve", "request-changes")
	for _, flag := range []string{"path", "line", "side", "approve", "request-changes"} {
		cmd.MarkFlagsMutuallyExclusive("reply", flag)
	}
	// A verdict note may omit its message.
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
		if !cmd.Flags().Changed("message") && !approve && !requestChanges {
			return errors.New("--message is required unless --approve or --request-changes is set")
		}
		req := &ermite.AddCommentRequest{Repo: args[0], Number: n, Text: text}
		if approve {
			req.Verdict = ermite.Verdict_VERDICT_APPROVE
		}
		if requestChanges {
			req.Verdict = ermite.Verdict_VERDICT_CHANGES_REQUESTED
		}
		if cmd.Flags().Changed("reply") {
			req.ReplyTo, err = parseNoteID(reply)
			if err != nil {
				return err
			}
		}
		switch {
		case path != "":
			if line == "" {
				return errors.New("--line is required with --path: give the line number")
			}
			req.StartLine, req.Line, err = parseLineRange(line)
			if err != nil {
				return err
			}
			if req.Side, err = parseSide(side); err != nil {
				return err
			}
			req.Path = path
		case cmd.Flags().Changed("line") || cmd.Flags().Changed("side"):
			return errors.New("--line and --side need --path")
		}
		c, err := env.client(cmd)
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

func parseNoteID(s string) (string, error) {
	id := strings.TrimLeft(strings.TrimSpace(s), "^#")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 1 {
		return "", fmt.Errorf("invalid note id %q: use ^12, #12 or 12", s)
	}
	return strconv.FormatInt(n, 10), nil
}

func parseLineRange(s string) (start, end int32, err error) {
	parts := strings.Split(s, "-")
	invalid := fmt.Errorf("invalid --line %q: use N or A-B with 1 <= A < B", s)
	if len(parts) < 1 || len(parts) > 2 {
		return 0, 0, invalid
	}
	parse := func(v string) (int32, error) {
		n, e := strconv.ParseInt(v, 10, 32)
		if e != nil || n < 1 {
			return 0, invalid
		}
		return int32(n), nil
	}
	end, err = parse(parts[len(parts)-1])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 2 {
		start, err = parse(parts[0])
		if err != nil || start >= end {
			return 0, 0, invalid
		}
	}
	return start, end, nil
}

func newMREditCommentCmd(env *cliEnv) *cobra.Command   { return newMRNoteMutationCmd(env, true) }
func newMRDeleteCommentCmd(env *cliEnv) *cobra.Command { return newMRNoteMutationCmd(env, false) }

func newMRNoteMutationCmd(env *cliEnv, edit bool) *cobra.Command {
	verb, short := "delete", "Delete your own merge request note"
	if edit {
		verb, short = "edit", "Edit your own merge request note"
	}
	var message string
	cmd := &cobra.Command{Use: verb + "-comment <repo> <number> <comment-id>", Short: short, Long: "Edit or delete your own note on an open merge request. Needs mr:write. Verdicts are unchanged.\nJSON: a comment (see `ermite mr --help`).", Args: cobra.ExactArgs(3), ValidArgsFunction: completeMRRepoThenNumber(env)}
	if edit {
		cmd.Flags().StringVarP(&message, "message", "m", "", "Markdown text, or - to read standard input (required)")
		_ = cmd.MarkFlagRequired("message")
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		id, err := parseNoteID(args[2])
		if err != nil {
			return err
		}
		text, err := readText(cmd, message)
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		var note *ermite.Comment
		if edit {
			res, e := c.MergeRequests().EditComment(env.ctx(cmd), connect.NewRequest(&ermite.EditCommentRequest{Repo: args[0], Number: n, CommentId: id, Text: text}))
			if e != nil {
				return c.Describe(e)
			}
			note = res.Msg.GetComment()
		} else {
			res, e := c.MergeRequests().DeleteComment(env.ctx(cmd), connect.NewRequest(&ermite.DeleteCommentRequest{Repo: args[0], Number: n, CommentId: id}))
			if e != nil {
				return c.Describe(e)
			}
			note = res.Msg.GetComment()
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONComment(note))
		}
		past := "deleted"
		if edit {
			past = "edited"
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "comment %s of %s!%d %s\n", clean(id), clean(args[0]), n, past)
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
			"error. Needs the mr:write scope." + "\nJSON: a comment (see `ermite mr --help`).",
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
		c, err := env.client(cmd)
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
	var mode string
	var deleteBranch bool
	var head string
	var auto, cancelAuto, ignoreChecks bool
	cmd := &cobra.Command{
		Use:   "merge <repo> <number>",
		Short: "Merge a merge request",
		Long: "Merge an open merge request. Needs mr:merge. --mode defaults to fast-forward;\n" +
			"repository settings may restrict modes (see `ermite repo edit --merge-modes`):\n" +
			"  fast-forward  the target moves to the head; no commit is created. The target must be an\n" +
			"                ancestor of the head; otherwise rebase and push again or use another mode.\n" +
			"  squash        one new commit with the merged head/target tree and the target tip as only\n" +
			"                parent. The branch must merge without conflicts.\n" +
			"  merge         a merge commit with the target tip and the head as parents. The branch must\n" +
			"                merge without conflicts.\n" +
			"Squash and merge commits use the server's signing key, generated on first start;\n" +
			"`ermite signing-key show` prints it. The server never rebases. `ermite mr show` reports\n" +
			"mergeability (up to date, behind, conflict); merging rechecks it and explains refusals.\n" +
			"\n" +
			"--head requires the full reviewed head id from `ermite mr show <repo> <number>`.\n" +
			"The server refuses the merge if the source branch moved: review again.\n" +
			"Checks must have no failures or queued/running jobs; no checks is allowed.\n" +
			"--ignore-checks bypasses this; needs mr:force-merge (cli preset, not agent preset).\n" +
			"\n" +
			"The command returns once the target moves. --delete-branch deletes the source afterward, in the\n" +
			"background. The server refuses the merge if that branch is default or used by another open merge\n" +
			"request. If deletion fails later (the branch moved, say), the merge stands and the branch stays.\n" +
			"\n" +
			"--auto schedules --mode instead of merging now. Needs mr:merge, server CI and\n" +
			"`ermite repo edit --allow-auto-merge` on the repository. The server waits for CI to settle with at\n" +
			"least one successful head check and every check allowing merge. A source push, settings\n" +
			"change or permission change cancels it. --head must still match the source branch.\n" +
			"--cancel-auto cancels pending auto-merge." + "\nJSON: a merge request (see `ermite mr --help`)." +
			"\n--json adds source_branch_deleted (true when deletion was requested; deletion follows the merge).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	cmd.Flags().StringVar(&mode, "mode", modeFastForward, "merge mode: fast-forward, squash or merge (a merge commit)")
	cmd.Flags().StringVar(&head, "head", "", "full reviewed head id from ermite mr show (required)")
	cmd.Flags().BoolVar(&deleteBranch, "delete-branch", false, "delete the source branch after the merge")
	cmd.Flags().BoolVar(&auto, "auto", false, "auto-merge when checks pass (needs repo edit --allow-auto-merge)")
	cmd.Flags().BoolVar(&cancelAuto, "cancel-auto", false, "cancel the pending auto-merge")
	cmd.Flags().BoolVar(&ignoreChecks, "ignore-checks", false, "merge despite failing or unfinished checks (needs mr:force-merge)")
	cmd.MarkFlagsMutuallyExclusive("ignore-checks", "auto")
	cmd.MarkFlagsMutuallyExclusive("ignore-checks", "cancel-auto")
	cmd.MarkFlagsMutuallyExclusive("auto", "cancel-auto")
	cmd.MarkFlagsMutuallyExclusive("cancel-auto", "delete-branch")
	cmd.MarkFlagsMutuallyExclusive("cancel-auto", "head")
	cmd.MarkFlagsMutuallyExclusive("auto", "delete-branch")
	_ = cmd.RegisterFlagCompletionFunc("mode", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{modeFastForward, modeSquash, modeMerge}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		if cancelAuto {
			return runMRCancelAuto(env, cmd, args[0], n, *asJSON)
		}
		pbMode, err := parseMode(mode)
		if err != nil {
			return err
		}
		head = strings.TrimSpace(head)
		if head == "" {
			return fmt.Errorf("--head is required: run `ermite mr show %s %d` and give the head id it prints, "+
				"so that only the commit you reviewed is merged", args[0], n)
		}
		if auto {
			return runMREnableAuto(env, cmd, args[0], n, pbMode, head, *asJSON)
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		if ignoreChecks {
			if err := requireFeature(ctx, c, client.FeatureIgnoreChecks, "cannot bypass checks (--ignore-checks)"); err != nil {
				return err
			}
		}
		if pbMode == ermite.MergeMode_MERGE_MODE_MERGE_COMMIT {
			if err := requireFeature(ctx, c, client.FeatureMergeability, "has no merge commit mode (--mode merge)"); err != nil {
				return err
			}
		}
		res, err := c.MergeRequests().MergeMergeRequest(ctx, connect.NewRequest(&ermite.MergeMergeRequestRequest{
			Repo: args[0], Number: n, Mode: pbMode, HeadId: head, DeleteSourceBranch: deleteBranch, IgnoreChecks: ignoreChecks,
		}))
		if err != nil {
			return c.Describe(err)
		}
		m := res.Msg.GetMergeRequest()
		deleted := res.Msg.GetSourceBranchDeleted()
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), jsonMerged{jsonMergeRequest: toJSONMergeRequest(m, c.URL()), SourceBranchDeleted: deleted})
		}
		out := cmd.OutOrStdout()
		_, err = fmt.Fprintf(out, "merged %s!%d into %s (%s) at %s\n", clean(m.GetRepo()), m.GetNumber(),
			clean(m.GetTargetBranch()), modeName(m.GetMergeMode()), shortID(m.GetMergedCommitId()))
		if err == nil && pbMode != ermite.MergeMode_MERGE_MODE_FAST_FORWARD {
			_, err = fmt.Fprintln(out, signedNote(ctx, c))
		}
		if err == nil && deleted {
			_, err = fmt.Fprintf(out, "deleting branch %s (done by the server after the merge)\n", clean(m.GetSourceBranch()))
		}
		return err
	}
	return cmd
}

// runMREnableAuto is `mr merge --auto`: it asks the server to merge once ready.
func runMREnableAuto(env *cliEnv, cmd *cobra.Command, repo string, n int64, mode ermite.MergeMode, head string, asJSON bool) error {
	c, err := env.client(cmd)
	if err != nil {
		return err
	}
	ctx := env.ctx(cmd)
	if err := requireFeature(ctx, c, client.FeatureAutoMerge, "cannot merge by itself (--auto)"); err != nil {
		return err
	}
	res, err := c.MergeRequests().EnableAutoMerge(ctx, connect.NewRequest(&ermite.EnableAutoMergeRequest{Repo: repo, Number: n, Mode: mode, HeadId: head}))
	if err != nil {
		return c.Describe(err)
	}
	m := res.Msg.GetMergeRequest()
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONMergeRequest(m, c.URL()))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "auto-merge enabled on %s!%d: %s into %s when ready (enabled by %s)\n", clean(m.GetRepo()), m.GetNumber(),
		modeName(m.GetAutoMerge().GetMode()), clean(m.GetTargetBranch()), clean(m.GetAutoMerge().GetEnabledBy()))
	return err
}

// runMRCancelAuto is `mr merge --cancel-auto`.
func runMRCancelAuto(env *cliEnv, cmd *cobra.Command, repo string, n int64, asJSON bool) error {
	c, err := env.client(cmd)
	if err != nil {
		return err
	}
	ctx := env.ctx(cmd)
	if err := requireFeature(ctx, c, client.FeatureAutoMerge, "has no auto-merge to cancel (--cancel-auto)"); err != nil {
		return err
	}
	res, err := c.MergeRequests().CancelAutoMerge(ctx, connect.NewRequest(&ermite.CancelAutoMergeRequest{Repo: repo, Number: n}))
	if err != nil {
		return c.Describe(err)
	}
	m := res.Msg.GetMergeRequest()
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONMergeRequest(m, c.URL()))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "auto-merge cancelled on %s!%d\n", clean(m.GetRepo()), m.GetNumber())
	return err
}

// signedNote says that a squash or merge commit is signed by the server's
// key, with its fingerprint when the server tells it.
func signedNote(ctx context.Context, c *client.Client) string {
	const how = "; `ermite signing-key show` prints the key to verify it"
	res, err := c.Server().GetSigningKey(ctx, connect.NewRequest(&ermite.GetSigningKeyRequest{}))
	if err != nil || res.Msg.GetFingerprint() == "" {
		return "the commit is signed by the server's key" + how
	}
	return "the commit is signed by the server's key " + clean(res.Msg.GetFingerprint()) + how
}

// requireFeature refuses to go on with a server that does not list the
// feature. A server whose features cannot be read is refused too.
func requireFeature(ctx context.Context, c *client.Client, feature, what string) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), feature) {
		return fmt.Errorf("the server does not support %s, so it %s (upgrade the server, see `ermite version`)", feature, what)
	}
	return nil
}

func newMRCloseCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close <repo> <number>",
		Short: "Close a merge request without merging it",
		Long: "Close an open merge request without merging it. A closed merge request cannot be reopened: open a\n" +
			"new one. Prints a confirmation, then the full web URL on its own line. Needs the mr:write scope." + "\nJSON: a merge request (see `ermite mr --help`).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := c.MergeRequests().CloseMergeRequest(env.ctx(cmd), connect.NewRequest(&ermite.CloseMergeRequestRequest{Repo: args[0], Number: n}))
		if err != nil {
			return c.Describe(err)
		}
		return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "closed", c.URL())
	}
	return cmd
}

func newMRReadyCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "ready <repo> <number>",
		Short:             "Mark a draft merge request ready for review",
		Long:              "Remove the WIP: or [WIP] prefix from the title so the merge request can be merged.\nFails if it is not open or not a draft. Needs the mr:write scope.\nPrints a confirmation, then the full web URL on its own line." + "\nJSON: a merge request (see `ermite mr --help`).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMRRepoThenNumber(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		if err := requireFeature(ctx, c, client.FeatureMarkReady, "cannot mark drafts ready (mr ready)"); err != nil {
			return err
		}
		res, err := c.MergeRequests().MarkReady(ctx, connect.NewRequest(&ermite.MarkReadyRequest{Repo: args[0], Number: n}))
		if err != nil {
			return c.Describe(err)
		}
		return outputMR(cmd, res.Msg.GetMergeRequest(), *asJSON, "marked ready", c.URL())
	}
	return cmd
}
