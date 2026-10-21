// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"bytes"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderToString(t *testing.T, c templ.Component) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, c.Render(t.Context(), &b))
	return b.String()
}

// A live banner is the content of a status region that stays in the page: the
// empty slot of the page and the fragment of the stream are the same element
// (same id, role and marker), so that ermiteLive can replace the children and
// leave the region where a screen reader watches it.
func TestLiveSlotsAreStableStatusRegions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		c    templ.Component
		id   string
		text string
	}{
		{"empty commits slot", LiveSlot("commits-head"), "commits-head", ""},
		{"empty mr slot", LiveSlot("mr-notice"), "mr-notice", ""},
		{"mergeability slot, as sent by the stream", MRMergeabilityFragment(MergeabilityView{State: "up_to_date", Label: "Ready to merge", FFOk: true, MergeOk: true}), "mr-mergeability", "Ready to merge"},
		{"commits banner", CommitsBanner("2 new commits — reload", "/x/commits/main"), "commits-head", "2 new commits — reload"},
		{"mr notice", MRNotice("a comment was added — reload", "/x/merge-requests/1"), "mr-notice", "a comment was added — reload"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := renderToString(t, tt.c)
			assert.True(t, strings.HasPrefix(got, `<div id="`+tt.id+`" class="live-slot" role="status" data-slot>`), got)
			if tt.text == "" {
				assert.True(t, strings.HasSuffix(got, "data-slot></div>"), "an empty slot has no child: %s", got)
			} else {
				assert.Contains(t, got, tt.text)
			}
			assert.True(t, strings.HasSuffix(got, "</div>"), got)
		})
	}
}

// The header's refs button is a link to the refs page, which the script turns
// into the dialog; the filters and the buttons that need scripts are marked.
func TestShellDegradesWithoutScripts(t *testing.T) {
	t.Parallel()
	l := Layout{Repo: "demo", Ref: "main", Short: "abc1234", Assets: Assets{}}
	got := renderToString(t, Header(l))
	assert.Contains(t, got, `<a class="btn ref" href="/demo/refs" data-fire="refs"`)
	assert.NotContains(t, got, "hx-on:click", "no handler is inlined in the header")
	assert.Contains(t, got, `class="btn search" data-js`)
	assert.NotContains(t, got, `data-fire="help"`, "the help button is in the footer only")
	assert.Contains(t, renderToString(t, Footer(l)), `class="foot-keys" data-js data-fire="help"`)
}

// Data reaches the scripts through data-* attributes, never spliced into an
// attribute that is evaluated.
func TestDiffControlsPassDataThroughAttributes(t *testing.T) {
	t.Parallel()
	files := renderToString(t, FileList([]DiffFile{{ID: "f1'x", Path: "a.go", Status: "M"}}))
	assert.NotContains(t, files, ":aria-current", "the file in view is marked by ermite.js")
	assert.NotContains(t, files, "f1'x'", "no id is spliced into a script")

	link := renderToString(t, viewLink(DiffView{Both: true, View: "split"}, "split", "Split", "d", "?view=split"))
	assert.Contains(t, link, `data-fire="split"`)
	assert.Contains(t, link, `this.dataset.view`)
	assert.NotContains(t, link, "=== 'split'")
}

// The push signal is intentionally constant-sized: diff work belongs to the
// subsequent windowed page request, not to each subscriber's event handler.
func TestMRPushedHeadSignal(t *testing.T) {
	t.Parallel()
	l := liveStream{s: &Server{}, repo: "demo", view: "mr", n: "1", ref: "feature", target: "main"}
	for _, tt := range []struct {
		ref  string
		want int
	}{{"refs/heads/feature", 1}, {"refs/heads/main", 0}, {"refs/heads/other", 0}} {
		fs, err := l.mrPushed(t.Context(), domain.PushEvent{Updates: []domain.RefUpdate{{Ref: tt.ref}}})
		require.NoError(t, err)
		require.Len(t, fs, tt.want)
		if tt.want > 0 {
			assert.Equal(t, "mr-head", fs[0].event)
			assert.Equal(t, `<div id="mr-head"></div>`, fs[0].html)
			assert.NotContains(t, fs[0].html, "the branch changed")
		}
	}
}

// Release lifecycle events invalidate the release pages without putting notes
// or asset metadata in SSE payloads. Other views need no release fragments.
func TestReleaseLiveUpdates(t *testing.T) {
	for _, ev := range []domain.Event{domain.ReleasePublished{}, domain.ReleaseUpdated{}, domain.ReleaseDeleted{}, domain.ReleaseUnpublished{}} {
		we := ports.WatchEvent{Event: ev}
		require.True(t, liveEvent(we), ev.EventKind())
		for _, view := range []string{"releases", "release", "summary", "refs", "mr"} {
			st := &liveStream{repo: "proj", view: view}
			got, err := st.render(t.Context(), we)
			require.NoError(t, err)
			if view == "release" || view == "releases" {
				assert.Equal(t, gap, got)
			} else {
				assert.Empty(t, got)
			}
		}
	}
}
