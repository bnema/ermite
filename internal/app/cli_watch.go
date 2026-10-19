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

	eventMROpened        = "merge_request_opened"
	eventMRMerged        = "merge_request_merged"
	eventMRClosed        = "merge_request_closed"
	eventCommentAdded    = "comment_added"
	eventCommentEdited   = "comment_edited"
	eventCommentDeleted  = "comment_deleted"
	eventCommentResolved = "comment_resolved"
)

// toJSONEvent maps an event; ok is false for kinds this client does not know,
// which the contract says to ignore.
func toJSONEvent(ev *ermite.Event) (jsonEvent, bool) {
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
		m := toJSONMergeRequest(k.MergeRequestOpened.GetMergeRequest())
		out.Type, out.Repo, out.MergeRequest = eventMROpened, m.Repo, &m
	case *ermite.Event_MergeRequestMerged:
		m := toJSONMergeRequest(k.MergeRequestMerged.GetMergeRequest())
		out.Type, out.Repo, out.MergeRequest = eventMRMerged, m.Repo, &m
	case *ermite.Event_MergeRequestClosed:
		m := toJSONMergeRequest(k.MergeRequestClosed.GetMergeRequest())
		out.Type, out.Repo, out.MergeRequest = eventMRClosed, m.Repo, &m
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
	case eventCommentAdded, eventCommentEdited, eventCommentDeleted, eventCommentResolved:
		detail = fmt.Sprintf("%s!%d by %s", e.Repo, e.Number, e.Comment.Author)
	case eventGap:
		detail = "events were lost, reload what you display"
	}
	return clean(ts + "  " + e.Type + "  " + detail)
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
		Long: "Print one line per event (repository created, updated or deleted, refs pushed, merge requests\n" +
			"opened, merged or closed, notes added, edited, deleted or resolved) for every\n" +
			"repository you can see, or only for [repo]. It runs until interrupted. When the connection drops it\n" +
			"reconnects with a growing delay and resumes after the last event it printed; if the server no longer\n" +
			"has that event it prints a gap event: reload what you display. Connection notices go to standard\n" +
			"error.\n\n" +
			"--json prints one JSON object per line (NDJSON). Fields: id, time (RFC 3339 UTC), type\n" +
			"(repo_created, repo_updated, repo_deleted, pushed, merge_request_opened, merge_request_merged,\n" +
			"merge_request_closed, comment_added, comment_edited, comment_deleted, comment_resolved or gap)\n" +
			"and, depending on the type, repo (name),\n" +
			"repository (the repository fields of `repo show --json`), ref_updates (array of ref, old_id,\n" +
			"new_id; an empty id means the ref was created or deleted), merge_request (the fields of\n" +
			"`mr show --json`) and, for comment events, number (of the merge request) and comment. Unknown\n" +
			"event types are skipped.",
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
			e, ok := toJSONEvent(ev)
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
