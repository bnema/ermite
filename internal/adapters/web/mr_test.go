// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
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
	assert.Contains(t, footer, `<footer class="mr-footer"><nav data-mr-tabs`)
	assert.NotContains(t, footer, `<details`)
	assert.NotContains(t, footer, `<summary`)
	// No hidden attribute: the CSS shows the panel of the URL fragment until
	// mr.js takes over, so the tabs work as anchors without scripts.
	assert.Contains(t, footer, `id="mr-commits" data-mr-panel role="region"`)
	assert.Contains(t, footer, `id="mr-checks-panel" data-mr-panel role="region"`)
	assert.NotContains(t, footer, `data-mr-panel hidden`)
	css, err := staticFS.ReadFile("static/css/60-mr.css")
	require.NoError(t, err)
	assert.Contains(t, string(css), `&:not([data-panels]) [data-mr-panel]:not(:target, :has(:target)) { display: none; }`)
	assert.Contains(t, footer, `data-mr-tabs`)
	assert.Contains(t, footer, `id="mr-commits"`)
	assert.Contains(t, footer, `id="mr-checks"`)
	assert.Contains(t, footer, `<table class="mr-commits">`)
	assert.Contains(t, footer, "500+")
	assert.Contains(t, footer, "more commits")
	assert.Contains(t, footer, "1/2 failed")
	rail := renderToString(t, mrRail(p))
	assert.Contains(t, rail, "g f")
	for _, text := range []string{"Checks", "Commits", "Settings", "mr-checks"} {
		assert.NotContains(t, rail, text)
	}
	more := renderToString(t, moreMenu(p))
	for _, text := range []string{"Merge settings", "squash, merge", "failing ones block", `id="mr-signing"`, "fingerprint", "ermite signing-key show"} {
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
