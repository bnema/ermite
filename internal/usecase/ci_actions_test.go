// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/ports"
)

func TestParseActionRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		uses                         string
		owner, repo, path, ref, fail string
	}{
		{uses: "actions/checkout@v4", owner: "actions", repo: "checkout", ref: "v4"},
		{uses: "actions/cache/save@v4", owner: "actions", repo: "cache", path: "save", ref: "v4"},
		{uses: "o/r/a/b/c@feature/x", owner: "o", repo: "r", path: "a/b/c", ref: "feature/x"},
		{uses: "o\\r\\p@v1", owner: "o", repo: "r", path: "p", ref: "v1"}, // GitHub splits on both separators
		{uses: "o//r@v1", owner: "o", repo: "r", ref: "v1"},               // and ignores empty segments
		{uses: "noslash@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'noslash@v1'"},
		{uses: "o/r", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r'"},
		{uses: "o/r@", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r@'"},
		{uses: "@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual '@v1'"},
		{uses: "o/r@a@b", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r@a@b'"},
		{uses: "o/r/../x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r/../x@v1'"},
		{uses: "o/r/./x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'o/r/./x@v1'"},
		{uses: "a/...@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/...@v1'"},
		{uses: "../..@" + strings.Repeat("a", 40), fail: "Expected format {org}/{repo}[/path]@ref. Actual '../..@" + strings.Repeat("a", 40) + "'"},
		{uses: "a%2f/b@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a%2f/b@v1'"},
		{uses: "a/b?x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/b?x@v1'"},
		{uses: "a/b#x@v1", fail: "Expected format {org}/{repo}[/path]@ref. Actual 'a/b#x@v1'"},
		{uses: "-a/_b.c@v1", owner: "-a", repo: "_b.c", ref: "v1"},
		{uses: "", fail: "Expected format {org}/{repo}[/path]@ref. Actual ''"},
	}
	for _, tt := range tests {
		t.Run(tt.uses, func(t *testing.T) {
			t.Parallel()
			a := parseActionRef(tt.uses)
			assert.Equal(t, tt.fail, a.err)
			if tt.fail == "" {
				assert.Equal(t, []string{tt.owner, tt.repo, tt.path, tt.ref}, []string{a.owner, a.repo, a.path, a.ref})
			}
		})
	}
	assert.Equal(t, parseActionRef("Actions/Checkout@v4").key(), parseActionRef("actions/checkout@v4").key(), "names are case-insensitive")
	assert.NotEqual(t, parseActionRef("a/b@V4").key(), parseActionRef("a/b@v4").key(), "refs are not")
}

func TestPickRemoteRef(t *testing.T) {
	t.Parallel()
	refs := []ports.RemoteRef{
		{Name: "refs/heads/v1", ID: "b1"},
		{Name: "refs/tags/v1", ID: "t1"},
		{Name: "refs/tags/v2", ID: "tobj", Peeled: "c2"},
		{Name: "refs/heads/main", ID: "m"},
	}
	assert.Equal(t, "t1", pickRemoteRef(refs, "v1"), "a tag before a branch")
	assert.Equal(t, "c2", pickRemoteRef(refs, "v2"), "the peeled commit of an annotated tag")
	assert.Equal(t, "m", pickRemoteRef(refs, "main"))
	assert.Empty(t, pickRemoteRef(refs, "refs/heads/main"), "short names only")
	assert.Empty(t, pickRemoteRef(refs, "nope"))
	assert.Empty(t, pickRemoteRef(nil, "v1"))
}
