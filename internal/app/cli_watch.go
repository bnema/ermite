// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// jsonEvent is the stable JSON shape of one event: one object per line with
// `ermite watch --json`. Fields not meaningful for a type are omitted.
type jsonEvent struct {
	ID         string          `json:"id"`
	Time       *string         `json:"time"`
	Type       string          `json:"type"`
	Repo       string          `json:"repo,omitempty"`
	Repository *jsonRepo       `json:"repository,omitempty"`
	RefUpdates []jsonRefUpdate `json:"ref_updates,omitempty"`
	// MergeRequest is set for the merge_request_* events, Number and Comment
	// for comment_added.
	MergeRequest *jsonMergeRequest `json:"merge_request,omitempty"`
	Number       int64             `json:"number,omitempty"`
	Comment      *jsonComment      `json:"comment,omitempty"`
	// Mergeability is set for a merge_request_mergeability event.
	Mergeability *jsonMergeability `json:"mergeability,omitempty"`
	// State and FailureClass are set for a mirror_failed event.
	State        string `json:"state,omitempty"`
	FailureClass string `json:"failure_class,omitempty"`
	// Auto is set for merge_request_merged: auto-merge made the merge.
	Auto bool `json:"auto,omitempty"`
	// Reason is set for merge_request_auto_merge_cancelled.
	Reason        string   `json:"reason,omitempty"`
	ChangedFields []string `json:"changed_fields,omitempty"`
}

type jsonRefUpdate struct {
	Ref   string `json:"ref"`
	OldID string `json:"old_id"`
	NewID string `json:"new_id"`
}

// Event types in the output.
const (
	eventRepoCreated = "repo_created"
	eventRepoUpdated = "repo_updated"
	eventRepoDeleted = "repo_deleted"
	eventPushed      = "pushed"
	eventGap         = "gap"

	eventIssueEdited     = "issue_edited"
	eventMREdited        = "merge_request_edited"
	eventMROpened        = "merge_request_opened"
	eventMRMerged        = "merge_request_merged"
	eventMRClosed        = "merge_request_closed"
	eventMRAutoEnabled   = "merge_request_auto_merge_enabled"
	eventMRAutoCancelled = "merge_request_auto_merge_cancelled"
	eventCommentAdded    = "comment_added"
	eventCommentEdited   = "comment_edited"
	eventCommentDeleted  = "comment_deleted"
	eventCommentResolved = "comment_resolved"
	eventMRMergeability  = "merge_request_mergeability"
	eventMirrorFailed    = "mirror_failed"
	eventMirrorRecovered = "mirror_recovered"
)

// toJSONEvent maps an event; ok is false for kinds this client does not know,
// which the contract says to ignore.
func toJSONEvent(ev *ermite.Event, baseURL string) (jsonEvent, bool) {
	out := jsonEvent{ID: ev.GetId(), Time: jsonTime(ev.GetTime())}
	switch k := ev.GetKind().(type) {
	case *ermite.Event_RepoCreated:
		r := toJSONRepo(k.RepoCreated.GetRepo())
		out.Type, out.Repo, out.Repository = eventRepoCreated, r.Name, &r
	case *ermite.Event_RepoUpdated:
		r := toJSONRepo(k.RepoUpdated.GetRepo())
		out.Type, out.Repo, out.Repository = eventRepoUpdated, r.Name, &r
	case *ermite.Event_RepoDeleted:
		out.Type, out.Repo = eventRepoDeleted, k.RepoDeleted.GetRepo()
	case *ermite.Event_Pushed:
		out.Type, out.Repo = eventPushed, k.Pushed.GetRepo()
		out.RefUpdates = []jsonRefUpdate{}
		for _, u := range k.Pushed.GetRefUpdates() {
			out.RefUpdates = append(out.RefUpdates, jsonRefUpdate{Ref: u.GetRef(), OldID: u.GetOldId(), NewID: u.GetNewId()})
		}
	case *ermite.Event_MergeRequestOpened:
		m := toJSONMergeRequest(k.MergeRequestOpened.GetMergeRequest(), baseURL)
		out.Type, out.Repo, out.MergeRequest = eventMROpened, m.Repo, &m
	case *ermite.Event_MergeRequestMerged:
		m := toJSONMergeRequest(k.MergeRequestMerged.GetMergeRequest(), baseURL)
		out.Type, out.Repo, out.MergeRequest = eventMRMerged, m.Repo, &m
		out.Auto = k.MergeRequestMerged.GetAuto()
	case *ermite.Event_MergeRequestAutoMergeEnabled:
		m := toJSONMergeRequest(k.MergeRequestAutoMergeEnabled.GetMergeRequest(), baseURL)
		out.Type, out.Repo, out.MergeRequest = eventMRAutoEnabled, m.Repo, &m
	case *ermite.Event_MergeRequestAutoMergeCancelled:
		m := toJSONMergeRequest(k.MergeRequestAutoMergeCancelled.GetMergeRequest(), baseURL)
		out.Type, out.Repo, out.MergeRequest = eventMRAutoCancelled, m.Repo, &m
		out.Reason = k.MergeRequestAutoMergeCancelled.GetReason()
	case *ermite.Event_MergeRequestClosed:
		m := toJSONMergeRequest(k.MergeRequestClosed.GetMergeRequest(), baseURL)
		out.Type, out.Repo, out.MergeRequest = eventMRClosed, m.Repo, &m
	case *ermite.Event_IssueEdited:
		item := k.IssueEdited.GetIssue()
		out.Type, out.Repo, out.Number, out.ChangedFields = eventIssueEdited, item.GetRepo(), item.GetNumber(), k.IssueEdited.GetChangedFields()
	case *ermite.Event_MergeRequestEdited:
		item := k.MergeRequestEdited.GetMergeRequest()
		out.Type, out.Repo, out.Number, out.ChangedFields = eventMREdited, item.GetRepo(), item.GetNumber(), k.MergeRequestEdited.GetChangedFields()
	case *ermite.Event_CommentAdded:
		c := toJSONComment(k.CommentAdded.GetComment())
		out.Type, out.Repo, out.Number, out.Comment = eventCommentAdded, k.CommentAdded.GetRepo(), k.CommentAdded.GetNumber(), &c
	case *ermite.Event_CommentEdited:
		c := toJSONComment(k.CommentEdited.GetComment())
		out.Type, out.Repo, out.Number, out.Comment = eventCommentEdited, k.CommentEdited.GetRepo(), k.CommentEdited.GetNumber(), &c
	case *ermite.Event_CommentDeleted:
		c := toJSONComment(k.CommentDeleted.GetComment())
		out.Type, out.Repo, out.Number, out.Comment = eventCommentDeleted, k.CommentDeleted.GetRepo(), k.CommentDeleted.GetNumber(), &c
	case *ermite.Event_CommentResolved:
		c := toJSONComment(k.CommentResolved.GetComment())
		out.Type, out.Repo, out.Number, out.Comment = eventCommentResolved, k.CommentResolved.GetRepo(), k.CommentResolved.GetNumber(), &c
	case *ermite.Event_MergeRequestMergeability:
		out.Type, out.Repo, out.Number = eventMRMergeability, k.MergeRequestMergeability.GetRepo(), k.MergeRequestMergeability.GetNumber()
		out.Mergeability = toJSONMergeability(k.MergeRequestMergeability.GetMergeability())
	case *ermite.Event_MirrorFailed:
		out.Type, out.Repo = eventMirrorFailed, k.MirrorFailed.GetRepo()
		out.State, out.FailureClass = mirrorStateName(k.MirrorFailed.GetState()), k.MirrorFailed.GetFailureClass()
	case *ermite.Event_MirrorRecovered:
		out.Type, out.Repo = eventMirrorRecovered, k.MirrorRecovered.GetRepo()
	case *ermite.Event_Gap:
		out.Type = eventGap
	default:
		return jsonEvent{}, false
	}
	return out, true
}

// eventLine is the human line of an event: "<time>  <type>  <details>".
func eventLine(e jsonEvent) string {
	ts := "-"
	if e.Time != nil {
		ts = *e.Time
	}
	var detail string
	switch e.Type {
	case eventRepoCreated:
		detail = e.Repo + " (" + e.Repository.Visibility + ")"
	case eventRepoUpdated:
		detail = e.Repo + " (" + e.Repository.Visibility + ", default branch " + e.Repository.DefaultBranch + ")"
	case eventRepoDeleted:
		detail = e.Repo
	case eventPushed:
		parts := make([]string, 0, len(e.RefUpdates))
		for _, u := range e.RefUpdates {
			parts = append(parts, refUpdateText(u))
		}
		detail = e.Repo + " " + strings.Join(parts, ", ")
	case eventMROpened, eventMRMerged, eventMRClosed:
		detail = fmt.Sprintf("%s!%d %s -> %s", e.Repo, e.MergeRequest.Number, e.MergeRequest.SourceBranch, e.MergeRequest.TargetBranch)
		if e.Auto {
			detail += " (auto-merge)"
		}
	case eventMRAutoEnabled:
		detail = fmt.Sprintf("%s!%d %s when ready", e.Repo, e.MergeRequest.Number, e.MergeRequest.AutoMerge.Mode)
	case eventMRAutoCancelled:
		detail = fmt.Sprintf("%s!%d: %s", e.Repo, e.MergeRequest.Number, e.Reason)
	case eventIssueEdited:
		detail = fmt.Sprintf("%s#%d %s", e.Repo, e.Number, strings.Join(e.ChangedFields, ", "))
	case eventMREdited:
		detail = fmt.Sprintf("%s!%d %s", e.Repo, e.Number, strings.Join(e.ChangedFields, ", "))
	case eventCommentAdded, eventCommentEdited, eventCommentDeleted, eventCommentResolved:
		detail = fmt.Sprintf("%s!%d by %s", e.Repo, e.Number, e.Comment.Author)
	case eventMRMergeability:
		detail = fmt.Sprintf("%s!%d mergeability: %s", e.Repo, e.Number, mergeabilityEventText(e.Mergeability))
	case eventMirrorFailed:
		detail = e.Repo + " " + e.State + " (" + e.FailureClass + ")"
	case eventMirrorRecovered:
		detail = e.Repo
	case eventGap:
		detail = "events were lost, reload what you display"
	}
	return clean(ts + "  " + e.Type + "  " + detail)
}

// mergeabilityEventText is the short form of a mergeability for a watch line:
// "up_to_date", "behind by 2", "conflict (a, b)", "checking".
func mergeabilityEventText(m *jsonMergeability) string {
	if m == nil {
		return "unknown"
	}
	switch m.State {
	case "behind":
		return fmt.Sprintf("behind by %d%s", m.Behind, plus(m.BehindCapped))
	case "conflict":
		if len(m.ConflictPaths) == 0 {
			return "conflict (no common history)"
		}
		more := ""
		if m.ConflictPathsTruncated {
			more = ", ..."
		}
		return "conflict (" + strings.Join(m.ConflictPaths, ", ") + more + ")"
	}
	return m.State
}

func refUpdateText(u jsonRefUpdate) string {
	short := func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	switch {
	case u.OldID == "":
		return u.Ref + " created at " + short(u.NewID)
	case u.NewID == "":
		return u.Ref + " deleted (was " + short(u.OldID) + ")"
	default:
		return u.Ref + " " + short(u.OldID) + ".." + short(u.NewID)
	}
}

func newWatchCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch [repo]",
		Short: "Print repository events as they happen",
		Long: "Print one line per event for all visible repositories, or only [repo], until interrupted.\n" +
			"Events cover repository creation/update/deletion, pushes, merge requests opened/merged\n" +
			"(manually or automatically)/closed, auto-merge enabled/cancelled, and notes added/edited/\n" +
			"deleted/resolved. The command reconnects with increasing delay after a disconnect; it resumes after the\n" +
			"last printed event. If it expired on the server, the command prints a gap event: reload your display.\n" +
			"Connection notices go to stderr. The command skips unknown event types.\n" +
			"\n" +
			"--json prints NDJSON (one object per line): id, time (RFC 3339 UTC), type\n" +
			"(repo_created, repo_updated, repo_deleted, pushed, merge_request_opened,\n" +
			"merge_request_merged, merge_request_closed, merge_request_auto_merge_enabled,\n" +
			"merge_request_auto_merge_cancelled (with reason), issue_edited, merge_request_edited,\n" +
			"comment_added, comment_edited, comment_deleted, comment_resolved, gap).\n" +
			"Type-specific fields:\n" +
			"  repo: name; repository: fields from `repo show --json`.\n" +
			"  ref_updates: array of ref, old_id, new_id; empty id means creation/deletion.\n" +
			"  merge_request: fields from `ermite mr --help`; merge_request_merged also has\n" +
			"    auto (true for auto-merge).\n" +
			"  Comment events: number (merge request number), comment (see `ermite mr --help`).\n" +
			"  Edit events: repo, number (issue or merge request), changed_fields\n" +
			"    (title/body for issue_edited; title/description for merge_request_edited).",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		topics := []string{"home"}
		if len(args) == 1 {
			topics = []string{"repo:" + args[0]}
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(env.ctx(cmd), os.Interrupt, syscall.SIGTERM)
		defer stop()
		out := cmd.OutOrStdout()
		opts := client.WatchOptions{
			Topics:   topics,
			Backoff:  env.backoff,
			Sleep:    env.sleep,
			OnNotice: func(msg string) { noticef(cmd, "%s", clean(msg)) },
		}
		return c.Watch(ctx, opts, func(ev *ermite.Event) error {
			e, ok := toJSONEvent(ev, c.URL())
			if !ok {
				return nil
			}
			if *asJSON {
				return writeNDJSON(out, e)
			}
			_, err := fmt.Fprintln(out, eventLine(e))
			return err
		})
	}
	return cmd
}
