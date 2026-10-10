// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"context"
	"strconv"

	"connectrpc.com/connect"
	"github.com/a-h/templ"

	pb "bnema.dev/ermite-api/gen/ermite"
)

// liveStream turns the events of Watch into the fragments one page shows.
type liveStream struct {
	s    *Server
	repo string // "" on the home page
	view string // "summary", "refs" or "commits"
	ref  string // commits: the branch listed
	head string // commits: the commit listed first
	n    string // mr: the number of the merge request shown
}

// renderFragment renders a component to the data of an event.
func renderFragment(ctx context.Context, event string, c templ.Component) (fragment, error) {
	var b bytes.Buffer
	if err := c.Render(ctx, &b); err != nil {
		return fragment{}, err
	}
	return fragment{event: event, html: b.String()}, nil
}

var gap = []fragment{{event: "gap", html: "gap"}}

// render returns the fragments for an event, none when it changes nothing on
// the page.
func (l *liveStream) render(ctx context.Context, ev *pb.Event) ([]fragment, error) {
	if ev == nil {
		return nil, nil
	}
	if ev.GetGap() != nil {
		return gap, nil
	}
	if l.repo == "" {
		return l.home(ctx, ev)
	}
	return l.repoPage(ctx, ev)
}

// home upserts the row of the repository an event is about. The row is read
// again from the API as the visitor of the stream, so a repository that was
// deleted or that the visitor can no longer see (public to private) removes
// the row, and the stream never carries more than the list shows.
func (l *liveStream) home(ctx context.Context, ev *pb.Event) ([]fragment, error) {
	var name string
	fresh := false
	switch k := ev.GetKind().(type) {
	case *pb.Event_RepoDeleted:
		return l.gone(ctx, k.RepoDeleted.GetRepo())
	case *pb.Event_RepoCreated:
		name = k.RepoCreated.GetRepo().GetName()
	case *pb.Event_RepoUpdated:
		name = k.RepoUpdated.GetRepo().GetName()
	case *pb.Event_Pushed:
		name, fresh = k.Pushed.GetRepo(), true
	default:
		return nil, nil
	}
	res, err := l.s.cfg.Repos.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: name}))
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound:
		return l.gone(ctx, name)
	case err != nil:
		return nil, err
	}
	row := homeRow(res.Msg.GetRepo(), l.s.cfg.Now(), false)
	row.Fresh = fresh
	f, err := renderFragment(ctx, "home-list", HomeRowView(row))
	return []fragment{f}, err
}

func (l *liveStream) gone(ctx context.Context, name string) ([]fragment, error) {
	f, err := renderFragment(ctx, "home-list", RepoGone("repo-"+name))
	return []fragment{f}, err
}

// repoPage handles the events of the topic of one repository.
func (l *liveStream) repoPage(ctx context.Context, ev *pb.Event) ([]fragment, error) {
	switch k := ev.GetKind().(type) {
	case *pb.Event_RepoDeleted, *pb.Event_RepoCreated:
		// Gone, or a new repository under the same name: whatever the page
		// shows is not true any more.
		return gap, nil
	case *pb.Event_RepoUpdated:
		_, err := l.s.cfg.Repos.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: l.repo}))
		switch {
		case connect.CodeOf(err) == connect.CodeNotFound:
			return gap, nil // no longer visible
		case err != nil:
			return nil, err
		case l.view == "summary", l.view == "refs":
			return gap, nil // the description, the default branch
		}
		return nil, nil
	case *pb.Event_MergeRequestOpened:
		return l.mrNotice(ctx, k.MergeRequestOpened.GetMergeRequest().GetNumber(), "a merge request was opened — reload")
	case *pb.Event_MergeRequestMerged:
		return l.mrNotice(ctx, k.MergeRequestMerged.GetMergeRequest().GetNumber(), "this merge request was merged — reload")
	case *pb.Event_MergeRequestClosed:
		return l.mrNotice(ctx, k.MergeRequestClosed.GetMergeRequest().GetNumber(), "this merge request was closed — reload")
	case *pb.Event_CommentAdded:
		return l.mrNotice(ctx, k.CommentAdded.GetNumber(), "a comment was added — reload")
	case *pb.Event_Pushed:
		switch l.view {
		case "mr":
			return l.mrPushed(ctx, k.Pushed)
		case "mrs":
			return nil, nil
		}
		switch l.view {
		case "refs":
			return l.refs(ctx)
		case "commits":
			return l.banner(ctx, k.Pushed)
		}
		return l.lastPush(ctx)
	}
	return nil, nil
}

// lastPush is the "last push" fact of the summary.
func (l *liveStream) lastPush(ctx context.Context) ([]fragment, error) {
	res, err := l.s.cfg.Repos.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: l.repo}))
	if err != nil {
		return nil, err
	}
	f, err := renderFragment(ctx, "repo-last-push", FactValue(lastPushFact(res.Msg.GetRepo(), l.s.cfg.Now())))
	return []fragment{f}, err
}

// refs is the table of the refs page and the facts of its rail.
func (l *liveStream) refs(ctx context.Context) ([]fragment, error) {
	d, err := l.s.loadRepo(ctx, l.repo)
	if err != nil {
		return nil, err
	}
	p := l.s.refsPage(ctx, d)
	body, err := renderFragment(ctx, "refs-list", RefsBody(p))
	if err != nil {
		return nil, err
	}
	kv, err := renderFragment(ctx, "refs-list", RefsKV(p))
	return []fragment{body, kv}, err
}

// banner tells the commits page how many commits its branch got since the
// head it lists.
func (l *liveStream) banner(ctx context.Context, p *pb.Pushed) ([]fragment, error) {
	if l.ref == "" || !isFullID(l.head) {
		return nil, nil
	}
	tip, touched := "", false
	for _, u := range p.GetRefUpdates() {
		if u.GetRef() == "refs/heads/"+l.ref {
			tip, touched = u.GetNewId(), true
		}
	}
	switch {
	case !touched:
		return nil, nil
	case tip == "":
		return gap, nil // the branch was deleted
	}
	text := ""
	if tip != l.head {
		res, err := l.s.cfg.Git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{
			Repo: l.repo, Revision: tip, PageSize: pageSize,
		}))
		if err != nil {
			return nil, err
		}
		// The count is approximate for a history with merges: the log is
		// ordered by date, not as "head..tip", so commits that are not new may
		// be counted before the head is met. And one page of the log being
		// all that is read, "100+" cannot be told from a rewritten branch
		// whose old head is further back: the API has no "is ancestor".
		n, found := 0, false
		for _, c := range res.Msg.GetCommits() {
			if c.GetId() == l.head {
				found = true
				break
			}
			n++
		}
		switch {
		case found:
			text = newCommits(strconv.Itoa(n), n)
		case res.Msg.GetNextPageToken() != "":
			text = newCommits(strconv.Itoa(n)+"+", 2)
		default:
			text = "the branch was rewritten — reload"
		}
	}
	c := CommitsSlot()
	if text != "" {
		c = CommitsBanner(text, repoHref(l.repo, "commits", l.ref))
	}
	f, err := renderFragment(ctx, "commits-head", c)
	return []fragment{f}, err
}

// newCommits is the text of the banner: "1 new commit — reload".
func newCommits(count string, n int) string {
	if n == 1 {
		return count + " new commit — reload"
	}
	return count + " new commits — reload"
}

// isFullID reports whether s is a full commit id.
func isFullID(s string) bool { return len(s) == 40 && isCommitID(s) }

// mrNotice tells a merge request page that something happened to the request
// it shows, or the list that its content changed. Only a fixed sentence is sent:
// the text of a comment or a title never leaves through the stream.
func (l *liveStream) mrNotice(ctx context.Context, number int64, text string) ([]fragment, error) {
	switch l.view {
	case "mrs":
		text = "merge requests changed — reload"
		return l.notice(ctx, text, mrBase(l.repo))
	case "mr":
		if strconv.FormatInt(number, 10) != l.n {
			return nil, nil
		}
		return l.notice(ctx, text, mrHref(l.repo, number))
	}
	return nil, nil
}

// mrPushed announces a push to the source branch of the merge request shown.
func (l *liveStream) mrPushed(ctx context.Context, p *pb.Pushed) ([]fragment, error) {
	if l.ref == "" {
		return nil, nil
	}
	for _, u := range p.GetRefUpdates() {
		if u.GetRef() == "refs/heads/"+l.ref {
			n, err := strconv.ParseInt(l.n, 10, 64)
			if err != nil {
				return nil, nil
			}
			return l.notice(ctx, "the branch changed — reload", mrHref(l.repo, n))
		}
	}
	return nil, nil
}

func (l *liveStream) notice(ctx context.Context, text, href string) ([]fragment, error) {
	f, err := renderFragment(ctx, "mr-notice", MRNotice(text, href))
	return []fragment{f}, err
}
