// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

func repoWithModes(modes ...domain.MergeMode) ports.RepoView {
	var v ports.RepoView
	v.Settings.AllowedMergeModes = modes
	return v
}

func watchOf(ev domain.Event) ports.WatchEvent { return ports.WatchEvent{Event: ev} }

func TestMergeabilityView(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		m             domain.Mergeability
		label, detail string
		ffOK, mergeOK bool
		note          string
		paths         []string
	}{
		{"up to date", domain.Mergeability{State: domain.MergeabilityUpToDate}, "Ready to merge", "", true, true, "", nil},
		{"behind", domain.Mergeability{State: domain.MergeabilityBehind, Behind: 2}, "Behind main by 2 commits", "merges without conflicts", false, true, "", nil},
		{"behind by one", domain.Mergeability{State: domain.MergeabilityBehind, Behind: 1}, "Behind main by 1 commit", "merges without conflicts", false, true, "", nil},
		{"behind, capped", domain.Mergeability{State: domain.MergeabilityBehind, Behind: 1000, BehindCapped: true}, "Behind main by 1000+ commits", "merges without conflicts", false, true, "", nil},
		{"conflict", domain.Mergeability{State: domain.MergeabilityConflict, ConflictPaths: []string{"a.go"}}, "Conflicts with main", "rebase needed", false, false, "", []string{"a.go"}},
		{"conflict, truncated", domain.Mergeability{State: domain.MergeabilityConflict, ConflictPaths: []string{"a.go"}, ConflictPathsTruncated: true}, "Conflicts with main", "rebase needed", false, false, "more files conflict than are listed.", []string{"a.go"}},
		{"no common history", domain.Mergeability{State: domain.MergeabilityConflict}, "Conflicts with main", "rebase needed", false, false, "no common history.", nil},
		{"checking", domain.Mergeability{State: domain.MergeabilityChecking}, "Checking…", "", false, false, "", nil},
		{"none", domain.Mergeability{}, "Checking…", "", false, false, "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := mergeabilityView(tt.m, "main")
			assert.Equal(t, tt.label, v.Label)
			assert.Equal(t, tt.detail, v.Detail)
			assert.Equal(t, tt.ffOK, v.FFOk)
			assert.Equal(t, tt.mergeOK, v.MergeOk)
			assert.Equal(t, tt.note, v.PathsNote)
			assert.Equal(t, tt.paths, v.Paths)
			assert.Equal(t, !tt.ffOK, v.FFReason != "", "a mode that is not possible says why")
			assert.Equal(t, !tt.mergeOK, v.MergeReason != "")
		})
	}
}

func TestMergeBoxModes(t *testing.T) {
	t.Parallel()
	d := &repoData{repo: repoWithModes(domain.MergeModes()...)}
	s := &Server{}

	behind := s.mergeBox(d, 1, "feat", "abc", true, mergeabilityView(domain.Mergeability{State: domain.MergeabilityBehind, Behind: 1}, "main"))
	require.Len(t, behind.Modes, 3)
	assert.True(t, behind.Modes[0].Disabled, "fast-forward is not possible when behind")
	assert.NotEmpty(t, behind.Modes[0].Reason)
	assert.False(t, behind.Modes[1].Disabled)
	assert.False(t, behind.Modes[2].Disabled)
	assert.Equal(t, 1, behind.Checked(), "the first possible mode is checked")
	assert.True(t, behind.CanMerge())
	assert.True(t, behind.Signed)

	conflict := s.mergeBox(d, 1, "feat", "abc", true, mergeabilityView(domain.Mergeability{State: domain.MergeabilityConflict}, "main"))
	assert.False(t, conflict.CanMerge())
	assert.NotEmpty(t, conflict.MergeTitle(), "the button says why it is disabled")
	assert.Equal(t, 0, conflict.Checked())
	assert.Contains(t, conflict.CLI.Lines[2], "--mode merge")
	assert.NotContains(t, conflict.CLI.Lines[1], "--squash")
}

func TestMergeBoxOnlyFastForwardIsNotSigned(t *testing.T) {
	t.Parallel()
	d := &repoData{repo: repoWithModes(domain.MergeModeFastForward)}
	b := (&Server{}).mergeBox(d, 1, "feat", "abc", true, mergeabilityView(domain.Mergeability{State: domain.MergeabilityUpToDate}, "main"))
	assert.False(t, b.Signed, "no commit is created, nothing to explain")
	assert.Len(t, b.Modes, 1)
}

func TestMergeBoxNamesTheSigningKey(t *testing.T) {
	t.Parallel()
	key := mocks.NewSigningKeyService(t)
	key.EXPECT().Info().Return(domain.SigningKeyInfo{Key: domain.SigningPublicKey{Fingerprint: "SHA256:abc"}})
	d := &repoData{repo: repoWithModes(domain.MergeModes()...)}
	b := (&Server{cfg: Config{SigningKey: key}}).mergeBox(d, 1, "feat", "abc", true, mergeabilityView(domain.Mergeability{}, "main"))
	assert.Equal(t, "SHA256:abc", b.Fingerprint)
}

func TestMergeErrorTravelsInTheContext(t *testing.T) {
	t.Parallel()
	assert.Empty(t, mergeErrorOf(context.Background()))
	assert.Equal(t, "boom", mergeErrorOf(withMergeError(context.Background(), "boom")))
}

func TestMergeabilityLiveEvents(t *testing.T) {
	t.Parallel()
	assert.True(t, liveEvent(watchOf(domain.MergeRequestMergeability{Number: 1})))
	svc := mocks.NewMergeRequestService(t)
	svc.EXPECT().Mergeability(mock.Anything, mock.Anything, mock.Anything, int64(1)).
		Return(domain.Mergeability{State: domain.MergeabilityBehind, Behind: 3}, nil)
	l := liveStream{s: &Server{cfg: Config{MergeRequests: svc}}, repo: "demo", view: "mr", n: "1", target: "main"}
	ctx := context.WithValue(context.Background(), signedInKey{}, struct{}{})

	fs, err := l.repoPage(ctx, watchOf(domain.MergeRequestMergeability{Number: 1}))
	require.NoError(t, err)
	require.Len(t, fs, 1)
	assert.Equal(t, "mr-mergeability", fs[0].event)
	assert.Contains(t, fs[0].html, "Behind main by 3 commits")

	// Another request, or a page that is not one: nothing.
	fs, err = l.repoPage(ctx, watchOf(domain.MergeRequestMergeability{Number: 2}))
	require.NoError(t, err)
	assert.Empty(t, fs)
	// The box is the owner's.
	fs, err = l.repoPage(context.Background(), watchOf(domain.MergeRequestMergeability{Number: 1}))
	require.NoError(t, err)
	assert.Empty(t, fs)
}

func TestMRPanelsAndMenus(t *testing.T) {
	p := MRPage{CommitCount: "500+", CommitsNote: "more commits", Modes: []string{"squash", "merge"}, ChecksBlock: true,
		ChecksSum: ChecksSummary{Total: 2, Label: "1/2", Title: "1/2 failed"}, Box: &MergeBox{Signed: true, Fingerprint: "fingerprint", HeadGone: true}}
	footer := renderToString(t, mrFooter(p))
	assert.Contains(t, footer, `<footer class="mr-footer"><div data-mr-tabs data-js role="group"`)
	assert.NotContains(t, footer, `<details`)
	assert.NotContains(t, footer, `<summary`)
	// Both panels are readable without scripts; JS enhances native buttons.
	assert.Contains(t, footer, `id="mr-commits" data-mr-panel role="group"`)
	assert.Contains(t, footer, `id="mr-checks-panel" data-mr-panel role="group"`)
	assert.NotContains(t, footer, `data-mr-panel hidden`)
	css, err := staticFS.ReadFile("static/css/60-mr.css")
	require.NoError(t, err)
	assert.NotContains(t, string(css), `:not([data-panels])`)
	assert.Contains(t, footer, `<button type="button" id="mr-tab-commits" aria-expanded="true" aria-controls="mr-commits">`)
	assert.Contains(t, footer, `<button type="button" id="mr-tab-checks" aria-expanded="true" aria-controls="mr-checks-panel">`)
	assert.Contains(t, footer, `data-mr-tabs`)
	assert.Contains(t, footer, `id="mr-commits"`)
	assert.Contains(t, footer, `id="mr-checks"`)
	assert.Contains(t, footer, `<table class="mr-commits" aria-label="Commits">`)
	assert.Contains(t, footer, "500+")
	assert.Contains(t, footer, "more commits")
	assert.Contains(t, footer, "1/2 failed")
	rail := renderToString(t, mrRail(p))
	assert.Contains(t, rail, `<kbd aria-hidden="true">l</kbd>`)
	for _, text := range []string{"Checks", "Commits", "Settings", "mr-checks"} {
		assert.NotContains(t, rail, text)
	}
	more := renderToString(t, moreMenu(p))
	for _, text := range []string{"Merge settings", "squash, merge", "failing or unfinished checks block", `id="mr-signing"`, "fingerprint", "ermite signing-key show"} {
		assert.Contains(t, more, text)
	}
	merge := renderToString(t, mergeMenu(p))
	assert.NotContains(t, merge, `<dl`)
	assert.NotContains(t, merge, `mr-signing`)
	assert.Contains(t, merge, "the source branch was deleted")
	bar := renderToString(t, mrBar(MRPage{}))
	assert.Contains(t, bar, `id="mr-ck"`)
	assert.Contains(t, bar, `href="#mr-checks"`)
	assert.Contains(t, bar, "Merge settings")
	assert.NotContains(t, bar, ">Close</button>")
}

func TestMRReadPresentationCommitCount(t *testing.T) {
	commits := func(n int) []domain.Commit { return make([]domain.Commit, n) }
	for _, tc := range []struct {
		name      string
		commits   []domain.Commit
		truncated bool
		count     string
		note      string
	}{
		{"empty", nil, false, "0", ""},
		{"complete", commits(2), false, "2", ""},
		{"truncated", commits(3), true, "3+", "the branch has more commits than the 3 listed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Now: time.Now}}
			p := &MRPage{}
			s.mrReadPresentation(p, "demo", 1, ports.MergeRequestDetail{Commits: tc.commits, CommitsTruncated: tc.truncated}, false, false)
			assert.Equal(t, tc.count, p.CommitCount)
			assert.Equal(t, tc.note, p.CommitsNote)
			assert.Len(t, p.Commits, len(tc.commits))
		})
	}
}

func TestMergeBoxAutoMerge(t *testing.T) {
	t.Parallel()
	mg := mergeabilityView(domain.Mergeability{State: domain.MergeabilityUpToDate}, "main")
	mk := func(allow bool, runs ports.RunService) *MergeBox {
		v := repoWithModes(domain.MergeModes()...)
		v.Settings.AllowAutoMerge = allow
		return (&Server{cfg: Config{Runs: runs}}).mergeBox(&repoData{repo: v}, 1, "feat", "abc", false, mg)
	}
	assert.False(t, mk(false, mocks.NewRunService(t)).CanAutoMerge)
	assert.False(t, mk(true, nil).CanAutoMerge, "no CI, nothing to wait for")
	on := mk(true, mocks.NewRunService(t))
	assert.True(t, on.CanAutoMerge)

	page := MRPage{Target: "main", Box: on}
	html := renderToString(t, mergeMenu(page))
	assert.Contains(t, html, "Merge when ready")
	assert.Contains(t, html, `action="`+on.AutoAction+`"`)
	assert.NotContains(t, html, "Cancel auto-merge")
	// Current mergeability must only gate the immediate form. Every allowed
	// mode remains selectable in the separate auto-merge form.
	for i := range on.Modes {
		on.Modes[i].Disabled = true
	}
	on.CanDeleteBranch = true
	page.Source = "feat"
	html = renderToString(t, mergeMenu(page))
	_, autoForm, found := strings.Cut(html, `action="`+on.AutoAction+`"`)
	require.True(t, found)
	autoForm, _, _ = strings.Cut(autoForm, "</form>")
	assert.NotContains(t, autoForm, "delete_branch")
	assert.NotContains(t, autoForm, "disabled")
	assert.Contains(t, autoForm, `name="head_id"`)
	for _, mode := range on.Modes {
		assert.Contains(t, autoForm, `value="`+mode.Value+`"`)
	}
	page.Box = mk(false, mocks.NewRunService(t))
	assert.NotContains(t, renderToString(t, mergeMenu(page)), "Merge when ready")

	on.AutoMerge = &AutoMergeView{Mode: "squash", EnabledBy: "owner", HeadShort: "abc"}
	html = renderToString(t, mergeMenu(MRPage{Target: "main", Box: on}))
	assert.Contains(t, html, "Auto-merge: squash when ready, enabled by owner")
	assert.Contains(t, html, `action="`+on.CancelAutoAction+`"`)
	assert.Contains(t, html, "Cancel auto-merge")
}

func autoMergeRequest(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("repo", "demo")
	r.SetPathValue("n", "3")
	return r
}

func TestAutoMergeHandlers(t *testing.T) {
	t.Parallel()
	name, err := domain.ParseRepoName("demo")
	require.NoError(t, err)
	ctx := context.Background()

	head, err := domain.ParseObjectID(strings.Repeat("a", domain.ObjectIDLen))
	require.NoError(t, err)
	svc := mocks.NewMergeRequestService(t)
	svc.EXPECT().EnableAutoMerge(mock.Anything, mock.Anything, name, int64(3), ports.EnableAutoMergeParams{Mode: domain.MergeModeSquash, HeadID: head}).
		Return(domain.MergeRequest{}, nil).Once()
	svc.EXPECT().CancelAutoMerge(mock.Anything, mock.Anything, name, int64(3)).Return(domain.MergeRequest{}, nil).Once()
	s := &Server{cfg: Config{MergeRequests: svc}}
	to, _, err := s.mrAutoMerge(ctx, autoMergeRequest("/demo/merge-requests/3/auto-merge", "mode=squash&head_id="+head.String()))
	require.NoError(t, err)
	assert.Equal(t, mrHref("demo", 3), to)
	to, _, err = s.mrCancelAutoMerge(ctx, autoMergeRequest("/demo/merge-requests/3/auto-merge/cancel", ""))
	require.NoError(t, err)
	assert.Equal(t, mrHref("demo", 3), to)

	// A bad mode is refused before the service.
	_, _, err = s.mrAutoMerge(ctx, autoMergeRequest("/x", "mode=rebase"))
	var re *rerender
	require.ErrorAs(t, err, &re)
	assert.Equal(t, http.StatusBadRequest, re.status)

	// A refusal of the use case is shown in the box, like a refused merge.
	svc2 := mocks.NewMergeRequestService(t)
	svc2.EXPECT().EnableAutoMerge(mock.Anything, mock.Anything, name, int64(3), mock.Anything).
		Return(domain.MergeRequest{}, domain.ErrPreconditionFailed).Once()
	svc2.EXPECT().CancelAutoMerge(mock.Anything, mock.Anything, name, int64(3)).
		Return(domain.MergeRequest{}, domain.ErrNotFound).Once()
	s2 := &Server{cfg: Config{MergeRequests: svc2}}
	_, _, err = s2.mrAutoMerge(ctx, autoMergeRequest("/x", "mode=squash&head_id="+head.String()))
	require.ErrorAs(t, err, &re)
	assert.Equal(t, http.StatusConflict, re.status)
	_, _, err = s2.mrCancelAutoMerge(ctx, autoMergeRequest("/x", ""))
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestAutoMergeLiveEvents(t *testing.T) {
	t.Parallel()
	assert.True(t, liveEvent(watchOf(domain.MergeRequestAutoMergeEnabled{})))
	assert.True(t, liveEvent(watchOf(domain.MergeRequestAutoMergeCancelled{})))
}

func TestMergeIgnoreChecks(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bad", "running", "ok", ""} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			box := (&Server{}).mergeBox(&repoData{repo: repoWithModes(domain.MergeModes()...)}, 3, "feat", "abc", true, mergeabilityView(domain.Mergeability{State: domain.MergeabilityUpToDate}, "main"))
			html := renderToString(t, mergeMenu(MRPage{Box: box, ChecksSum: ChecksSummary{Kind: kind}}))
			if kind == "bad" || kind == "running" {
				assert.Contains(t, html, `name="ignore_checks"`)
				assert.Contains(t, html, "Ignore checks")
			} else {
				assert.NotContains(t, html, `name="ignore_checks"`)
			}
			assert.True(t, box.CanMerge())
		})
	}
	name, err := domain.ParseRepoName("demo")
	require.NoError(t, err)
	head, err := domain.ParseObjectID(strings.Repeat("a", domain.ObjectIDLen))
	require.NoError(t, err)
	for _, ignore := range []bool{false, true} {
		svc := mocks.NewMergeRequestService(t)
		svc.EXPECT().Merge(mock.Anything, mock.Anything, name, int64(3), ports.MergeParams{Mode: domain.MergeModeSquash, HeadID: head, IgnoreChecks: ignore}).Return(ports.MergeResult{}, nil).Once()
		body := "mode=squash&head_id=" + head.String()
		if ignore {
			body += "&ignore_checks=on"
		}
		s := &Server{cfg: Config{MergeRequests: svc}}
		to, _, err := s.mrMerge(context.Background(), autoMergeRequest("/demo/merge-requests/3/merge", body))
		require.NoError(t, err)
		assert.Equal(t, mrHref("demo", 3), to)
	}
}

func TestMRLinkedIssuesAndHistory(t *testing.T) {
	t.Parallel()
	r := newIssueRig(t, domain.IssuesOwner, false)
	at := time.Date(2026, 1, 2, 12, 0, 0, 123, time.UTC)
	title, err := domain.ParseIssueTitle("Fix <login>")
	require.NoError(t, err)
	text, err := domain.ParsePlainText("between issue events")
	require.NoError(t, err)
	r.mrs.EXPECT().Get(mock.Anything, mock.Anything, repoName(t, "proj"), int64(12), mock.Anything).Return(ports.MergeRequestDetail{
		MergeRequest: domain.MergeRequest{Repo: repoName(t, "proj"), Number: 12, State: domain.MRStateMerged, Source: "feature", Target: "main"},
		Issues:       []ports.MergeRequestIssue{{Number: 3, Title: title, State: domain.IssueStateResolved, Closes: true}, {Number: 4, Title: title, State: domain.IssueStateOpen}},
		Comments:     []domain.Comment{{ID: 7, Author: "owner", Text: text, CreatedAt: at}},
		IssueEvents: []ports.IssueEventDetail{
			{Event: domain.IssueEvent{ID: 1, Issue: 3, Kind: domain.IssueEventReferenced, Actor: "owner", CreatedAt: at.Add(-time.Second)}},
			{Event: domain.IssueEvent{ID: 2, Issue: 3, Kind: domain.IssueEventReferenced, Closes: true, Actor: "owner", CreatedAt: at}},
			{Event: domain.IssueEvent{ID: 3, Issue: 3, Kind: domain.IssueEventResolved, Actor: "owner", CreatedAt: at.Add(time.Second)}},
			{Event: domain.IssueEvent{ID: 1, Issue: 9, Kind: domain.IssueEventReferenced, Actor: "owner", CreatedAt: at.Add(2 * time.Second)}},
		},
	}, nil)
	rec := r.do("GET", "/proj/merge-requests/12", false, nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, `id="r-issues">Issues`)
	assert.Contains(t, body, `href="/proj/issues/3">#3 Fix &lt;login&gt;</a>`)
	assert.Contains(t, body, `data-state="resolved"`)
	assert.Contains(t, body, " · resolves")
	assert.Contains(t, body, "owner referenced")
	assert.Contains(t, body, "owner will resolve")
	assert.Contains(t, body, "owner resolved")
	assert.Contains(t, body, `href="/proj/issues/9">#9</a>`, "removed links keep their history")
	assert.Less(t, strings.Index(body, `id="e3-1"`), strings.Index(body, `id="c7"`))
	assert.Less(t, strings.Index(body, `id="c7"`), strings.Index(body, `id="e3-2"`))
	assert.Less(t, strings.Index(body, `id="e3-2"`), strings.Index(body, `id="e3-3"`))
}
