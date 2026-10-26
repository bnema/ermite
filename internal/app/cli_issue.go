// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

const statePending = "pending"
const stateResolved = "resolved"

func issueStateName(s ermite.IssueState) string {
	switch s {
	case ermite.IssueState_ISSUE_STATE_OPEN:
		return stateOpen
	case ermite.IssueState_ISSUE_STATE_RESOLVED:
		return stateResolved
	case ermite.IssueState_ISSUE_STATE_CLOSED:
		return stateClosed
	case ermite.IssueState_ISSUE_STATE_PENDING:
		return statePending
	case ermite.IssueState_ISSUE_STATE_UNSPECIFIED:
	}
	return "unknown"
}

func parseIssueState(s string) (ermite.IssueState, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case stateOpen:
		return ermite.IssueState_ISSUE_STATE_OPEN, nil
	case stateResolved:
		return ermite.IssueState_ISSUE_STATE_RESOLVED, nil
	case stateClosed:
		return ermite.IssueState_ISSUE_STATE_CLOSED, nil
	case stateAll:
		return ermite.IssueState_ISSUE_STATE_UNSPECIFIED, nil
	}
	return 0, fmt.Errorf("invalid state %q: use %s, %s, %s or %s", s, stateOpen, stateClosed, stateResolved, stateAll)
}

// jsonIssue is the stable JSON shape of an issue.
type jsonIssue struct {
	Repo         string      `json:"repo"`
	URL          string      `json:"url" help:"full web URL"`
	Number       int64       `json:"number"`
	Title        string      `json:"title"`
	Body         string      `json:"body"`
	State        string      `json:"state" help:"open, closed or resolved"`
	Author       string      `json:"author" help:"token label, root for the admin token, owner for the web"`
	CreatedAt    jsonInstant `json:"created_at"`
	UpdatedAt    jsonInstant `json:"updated_at"`
	CommentCount int32       `json:"comment_count"`
	// ClosedByMergeRequest is the merge request whose merge resolved the issue; 0 for none.
	ClosedByMergeRequest int64 `json:"closed_by_merge_request" help:"number of the merge request whose merge resolved the issue; 0 for none"`
}

// issueWebURL is the web page of an issue.
func issueWebURL(baseURL, repo string, number int64) string {
	return itemWebURL(baseURL, repo, "issues", number)
}

func toJSONIssue(i *ermite.Issue, baseURL string) jsonIssue {
	return jsonIssue{
		URL:  issueWebURL(baseURL, i.GetRepo(), i.GetNumber()),
		Repo: i.GetRepo(), Number: i.GetNumber(), Title: i.GetTitle(), Body: i.GetBody(),
		State: issueStateName(i.GetState()), Author: i.GetAuthor(),
		CreatedAt: jsonTime(i.GetCreatedAt()), UpdatedAt: jsonTime(i.GetUpdatedAt()), CommentCount: i.GetCommentCount(),
		ClosedByMergeRequest: i.GetClosedByMergeRequest(),
	}
}

// jsonIssueComment is the stable JSON shape of an issue comment.
type jsonIssueComment struct {
	ID        string      `json:"id"`
	Author    string      `json:"author"`
	Text      string      `json:"text"`
	CreatedAt jsonInstant `json:"created_at"`
}

func toJSONIssueComment(c *ermite.IssueComment) jsonIssueComment {
	return jsonIssueComment{ID: c.GetId(), Author: c.GetAuthor(), Text: c.GetText(), CreatedAt: jsonTime(c.GetCreatedAt())}
}

// jsonIssueDetail is the stable JSON shape of `issue show --json`.
type jsonIssueDetail struct {
	jsonIssue  `help:"the issue fields" see:"ermite issue"`
	Comments   []jsonIssueComment   `json:"comments" help:"always present, possibly empty" see:"ermite issue"`
	References []jsonIssueReference `json:"references" help:"merge requests that name the issue; always present, possibly empty"`
	Events     []jsonIssueEvent     `json:"events" help:"the history, always present, possibly empty"`
}

// jsonIssueReference is a merge request that names the issue.
type jsonIssueReference struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state" help:"open, merged or closed"`
	Closes bool   `json:"closes" help:"true when merging it resolves the issue"`
}

func newIssueCmd(env *cliEnv) *cobra.Command {
	issue := &cobra.Command{
		Use:   "issue",
		Short: "Track issues on a remote server",
		Long: "Issues record a task, a bug or a question about a repository. They share the numbering of merge\n" +
			"requests: the next issue after merge request 1 is number 2. Issues must be enabled for the\n" +
			"repository (`ermite repo edit <repo> --issues owner`). Needs a server with the issues feature (see\n" +
			"`ermite version`). Identify it by repository and number: `ermite issue show proj 3`.\n" +
			jsonFieldsHelp("an issue", jsonIssue{}) + "\n" + jsonFieldsHelp("an issue comment", jsonIssueComment{}),
	}
	issue.AddCommand(
		newIssueCreateCmd(env), newIssueEditCmd(env), newIssueListCmd(env), newIssueShowCmd(env), newIssueCommentCmd(env),
		newIssueCloseCmd(env), newIssueResolveCmd(env), newIssueReopenCmd(env),
	)
	return issue
}

// requireIssues refuses to go on with a server that does not list issues: it
// has no IssueService. A server whose features cannot be read is refused too.
// errNoIssues marks a server that lacks the issues feature.
var errNoIssues = errors.New("issues are not available")

func requireIssues(ctx context.Context, c *client.Client) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), client.FeatureIssues) {
		return fmt.Errorf("%w: the server does not support %s, so it has no issues (upgrade the server, see `ermite version`)", errNoIssues, client.FeatureIssues)
	}
	return nil
}

// issueClient returns a client for a server that supports issues.
func (e *cliEnv) issueClient(cmd *cobra.Command) (*client.Client, error) {
	c, err := e.client(cmd)
	if err != nil {
		return nil, err
	}
	if err := requireIssues(e.ctx(cmd), c); err != nil {
		return nil, err
	}
	return c, nil
}

// parseIssueNumber reads the number of an issue, written 3 or #3.
func parseIssueNumber(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid issue number %q: use a positive number such as 3 or #3", s)
	}
	return n, nil
}

func completeIssueRepoThenNumber(env *cliEnv) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return repoNames(env, cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		case 1:
			c, err := env.completionClient(cmd)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			issues, err := listIssues(env.ctx(cmd), c, args[0], ermite.IssueState_ISSUE_STATE_UNSPECIFIED)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, i := range issues {
				if n := strconv.FormatInt(i.GetNumber(), 10); strings.HasPrefix(n, toComplete) {
					out = append(out, n+"\t"+issueStateName(i.GetState())+", "+clean(i.GetTitle()))
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func listIssues(ctx context.Context, c *client.Client, repo string, state ermite.IssueState) ([]*ermite.Issue, error) {
	return collect(ctx, func(token string) ([]*ermite.Issue, string, error) {
		res, err := c.Issues().ListIssues(ctx, connect.NewRequest(&ermite.ListIssuesRequest{
			Repo: repo, State: state, PageToken: token,
		}))
		if err != nil {
			return nil, "", c.Describe(err)
		}
		return res.Msg.GetIssues(), res.Msg.GetNextPageToken(), nil
	})
}

// outputIssue prints an issue as JSON, or a confirmation followed by its URL.
func outputIssue(cmd *cobra.Command, i *ermite.Issue, asJSON bool, verb, baseURL string) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONIssue(i, baseURL))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s#%d (%s)\n%s\n", verb, clean(i.GetRepo()), i.GetNumber(), clean(i.GetTitle()),
		issueWebURL(baseURL, i.GetRepo(), i.GetNumber()))
	return err
}

func newIssueCreateCmd(env *cliEnv) *cobra.Command {
	var title, bodyFile string
	cmd := &cobra.Command{
		Use:   "create <repo>",
		Short: "Open an issue",
		Long: "Open an issue. The title is one line of at most 120 characters, the body Markdown of at most\n" +
			"10,000 characters; disallowed control characters are stripped (a body may hold line breaks).\n" +
			"--body-file PATH reads the optional body from a file; - reads standard input (1 MiB limit).\n" +
			"Inline body text is not accepted. Prints a confirmation, then the full web URL on its own line.\n" +
			"Needs the issue:write scope." + "\nJSON: an issue (see `ermite issue --help`).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&title, "title", "", "title of the issue (required)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the body from this file, or - for standard input")
	_ = cmd.MarkFlagRequired("title")
	_ = cmd.MarkFlagFilename("body-file")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var text string
		if cmd.Flags().Changed("body-file") {
			value, err := readTextFile(cmd, bodyFile)
			if err != nil {
				return fmt.Errorf("read --body-file: %w", err)
			}
			text = value
		}
		c, err := env.issueClient(cmd)
		if err != nil {
			return err
		}
		res, err := c.Issues().CreateIssue(env.ctx(cmd), connect.NewRequest(&ermite.CreateIssueRequest{
			Repo: args[0], Title: title, Body: text,
		}))
		if err != nil {
			return c.Describe(err)
		}
		return outputIssue(cmd, res.Msg.GetIssue(), *asJSON, "opened", c.URL())
	}
	return cmd
}

func newIssueListCmd(env *cliEnv) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List the issues of a repository",
		Long: "List the issues of a repository, newest first. --state restricts them to open, closed or resolved ones\n" +
			"(default: open; all shows every state)." + "\nJSON: an issue (see `ermite issue --help`)." + "\n--json prints an array of issues.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringVar(&state, "state", stateOpen, "state: open, closed, resolved or all")
	_ = cmd.RegisterFlagCompletionFunc("state", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{stateOpen, stateClosed, stateResolved, stateAll}, cobra.ShellCompDirectiveNoFileComp
	})
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		st, err := parseIssueState(state)
		if err != nil {
			return err
		}
		c, err := env.issueClient(cmd)
		if err != nil {
			return err
		}
		issues, err := listIssues(env.ctx(cmd), c, args[0], st)
		if err != nil {
			return err
		}
		if *asJSON {
			out := make([]jsonIssue, 0, len(issues))
			for _, i := range issues {
				out = append(out, toJSONIssue(i, c.URL()))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if len(issues) == 0 {
			noticef(cmd, "no issues")
			return nil
		}
		rows := make([][]string, 0, len(issues))
		for _, i := range issues {
			rows = append(rows, []string{
				"#" + strconv.FormatInt(i.GetNumber(), 10), issueStateName(i.GetState()), i.GetAuthor(),
				strconv.Itoa(int(i.GetCommentCount())), shortTime(i.GetUpdatedAt(), "-"), i.GetTitle(),
			})
		}
		return table(cmd.OutOrStdout(), []string{"NUMBER", "STATE", "AUTHOR", "COMMENTS", "UPDATED", "TITLE"}, rows)
	}
	return cmd
}

func newIssueShowCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <repo> <number>",
		Short: "Show an issue with its comments",
		Long: "Show an issue: its fields, its body, the merge requests that name it (#N in their title,\n" +
			"description, notes or commits; a closing keyword such as \"Fixes #N\" resolves it on merge) and\n" +
			"its comments and history, oldest first. \"resolved by\" shows the merge request whose merge resolved the issue." +
			"\n" + jsonFieldsHelp("an issue with its history", jsonIssueDetail{}),
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeIssueRepoThenNumber(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseIssueNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.issueClient(cmd)
		if err != nil {
			return err
		}
		res, err := c.Issues().GetIssue(env.ctx(cmd), connect.NewRequest(&ermite.GetIssueRequest{Repo: args[0], Number: n}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			out := jsonIssueDetail{jsonIssue: toJSONIssue(res.Msg.GetIssue(), c.URL()), Comments: []jsonIssueComment{}, Events: toJSONIssueEvents(res.Msg.GetEvents())}
			for _, cm := range res.Msg.GetComments() {
				out.Comments = append(out.Comments, toJSONIssueComment(cm))
			}
			out.References = []jsonIssueReference{}
			for _, r := range res.Msg.GetReferences() {
				out.References = append(out.References, jsonIssueReference{
					Number: r.GetNumber(), Title: r.GetTitle(), State: stateName(r.GetState()), Closes: r.GetCloses(),
				})
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		return printIssueDetail(cmd.OutOrStdout(), res.Msg)
	}
	return cmd
}

func printIssueDetail(w io.Writer, d *ermite.GetIssueResponse) error {
	i := d.GetIssue()
	body := i.GetBody()
	if body == "" {
		body = "-"
	}
	rows := [][2]string{
		{"issue", fmt.Sprintf("%s#%d", i.GetRepo(), i.GetNumber())},
		{"title", i.GetTitle()},
		{"state", issueStateName(i.GetState())},
	}
	if n := i.GetClosedByMergeRequest(); n > 0 {
		rows = append(rows, [2]string{"resolved by", fmt.Sprintf("!%d", n)})
	}
	rows = append(rows,
		[2]string{"author", i.GetAuthor()},
		[2]string{"created", shortTime(i.GetCreatedAt(), "-")},
		[2]string{"updated", shortTime(i.GetUpdatedAt(), "-")},
	)
	err := fields(w, rows)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", indentText(body)); err != nil {
		return err
	}
	if refs := d.GetReferences(); len(refs) > 0 {
		if _, err := fmt.Fprintf(w, "\nmerge requests (%d)\n", len(refs)); err != nil {
			return err
		}
		for _, r := range refs {
			line := fmt.Sprintf("  !%d %-6s %s", r.GetNumber(), stateName(r.GetState()), clean(r.GetTitle()))
			if r.GetCloses() {
				line += " · resolves"
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w, "\ncomments (%d)\n", len(d.GetComments())); err != nil {
		return err
	}
	var timeline []issueTimelineRow
	for _, c := range d.GetComments() {
		c := c
		timeline = append(timeline, issueTimelineRow{at: c.GetCreatedAt(), print: func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "  #%s %s, %s\n%s\n", c.GetId(), clean(c.GetAuthor()), shortTime(c.GetCreatedAt(), "-"), indentText(c.GetText()))
			return err
		}})
	}
	timeline = append(timeline, newIssueTimeline(d.GetEvents(), false)...)
	if err := printIssueTimeline(w, timeline); err != nil {
		return err
	}
	return nil
}

func newIssueCommentCmd(env *cliEnv) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "comment <repo> <number>",
		Short: "Comment on an issue",
		Long: "Add a plain-text comment (at most 10,000 characters; disallowed control characters are stripped).\n" +
			"-m - reads it from standard input. Needs the issue:write scope." + "\nJSON: an issue comment (see `ermite issue --help`).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeIssueRepoThenNumber(env),
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "text of the comment, or - to read standard input (required)")
	cmd.Flags().StringVar(&message, "text", "", "alias of --message")
	_ = cmd.Flags().MarkHidden("text")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseIssueNumber(args[1])
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("message") && !cmd.Flags().Changed("text") {
			return errors.New("the comment is required: give it with -m, or -m - for standard input")
		}
		text, err := readText(cmd, message)
		if err != nil {
			return err
		}
		c, err := env.issueClient(cmd)
		if err != nil {
			return err
		}
		res, err := c.Issues().AddIssueComment(env.ctx(cmd), connect.NewRequest(&ermite.AddIssueCommentRequest{
			Repo: args[0], Number: n, Text: text,
		}))
		if err != nil {
			return c.Describe(err)
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONIssueComment(res.Msg.GetComment()))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "comment %s added to %s#%d\n", res.Msg.GetComment().GetId(), clean(args[0]), n)
		return err
	}
	return cmd
}

func newIssueCloseCmd(env *cliEnv) *cobra.Command {
	return newIssueStateCmd(env, "close", "closed", "Close an issue", "Close an open issue. `ermite issue reopen` opens it again. Needs the issue:write scope.",
		func(ctx context.Context, c *client.Client, repo string, n int64) (*ermite.Issue, error) {
			res, err := c.Issues().CloseIssue(ctx, connect.NewRequest(&ermite.CloseIssueRequest{Repo: repo, Number: n}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetIssue(), nil
		})
}

func newIssueResolveCmd(env *cliEnv) *cobra.Command {
	return newIssueStateCmd(env, "resolve", "resolved", "Resolve an issue", "Mark an open issue as resolved (fixed). `ermite issue reopen` opens it again. Needs the issue:write scope.",
		func(ctx context.Context, c *client.Client, repo string, n int64) (*ermite.Issue, error) {
			if err := requireFeature(ctx, c, "issues.resolve", "cannot resolve issues (issue resolve)"); err != nil {
				return nil, err
			}
			res, err := c.Issues().ResolveIssue(ctx, connect.NewRequest(&ermite.ResolveIssueRequest{Repo: repo, Number: n}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetIssue(), nil
		})
}

func newIssueReopenCmd(env *cliEnv) *cobra.Command {
	return newIssueStateCmd(env, "reopen", "reopened", "Reopen a closed or resolved issue", "Reopen a closed or resolved issue. Needs the issue:write scope.",
		func(ctx context.Context, c *client.Client, repo string, n int64) (*ermite.Issue, error) {
			res, err := c.Issues().ReopenIssue(ctx, connect.NewRequest(&ermite.ReopenIssueRequest{Repo: repo, Number: n}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetIssue(), nil
		})
}

func newIssueStateCmd(env *cliEnv, use, verb, short, long string, do func(context.Context, *client.Client, string, int64) (*ermite.Issue, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:               use + " <repo> <number>",
		Short:             short,
		Long:              long + "\nJSON: an issue (see `ermite issue --help`).",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeIssueRepoThenNumber(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := parseIssueNumber(args[1])
		if err != nil {
			return err
		}
		c, err := env.issueClient(cmd)
		if err != nil {
			return err
		}
		i, err := do(env.ctx(cmd), c, args[0], n)
		if err != nil {
			return c.Describe(err)
		}
		return outputIssue(cmd, i, *asJSON, verb, c.URL())
	}
	return cmd
}
