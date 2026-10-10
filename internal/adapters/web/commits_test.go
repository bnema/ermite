// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pb "bnema.dev/ermite-api/gen/ermite"
)

func TestIsCommitID(t *testing.T) {
	for _, s := range []string{"abcdef0", "0123456789abcdef0123456789abcdef01234567"} {
		assert.True(t, isCommitID(s), s)
	}
	for _, s := range []string{"", "abcdef", "ABCDEF0", "main", "abcdefg", "0123456789abcdef0123456789abcdef012345678"} {
		assert.False(t, isCommitID(s), s)
	}
}

func TestSelectRev(t *testing.T) {
	b, tag := pb.RefKind_REF_KIND_BRANCH, pb.RefKind_REF_KIND_TAG
	d := &repoData{branches: []*pb.Ref{ref(b, "main"), ref(b, "feature/x")}, tags: []*pb.Ref{ref(tag, "v1")}}
	d.head = d.branches[0]
	tests := []struct {
		in       string
		want     revSel
		wantOK   bool
		describe string
	}{
		{"", revSel{rev: "c-main", name: "main"}, true, "head"},
		{"main", revSel{rev: "c-main", name: "main"}, true, "branch"},
		{"main/docs/a.md", revSel{rev: "c-main", name: "main", path: "docs/a.md"}, true, "path"},
		{"feature/x/src", revSel{rev: "c-feature/x", name: "feature/x", path: "src"}, true, "ref with a slash"},
		{"v1", revSel{rev: "c-v1", name: "v1"}, true, "tag"},
		{"abcdef0123/x.go", revSel{rev: "abcdef0123", name: "abcdef0123", path: "x.go", detached: true}, true, "commit id"},
		{"nope", revSel{}, false, "unknown"},
		{"feature", revSel{}, false, "a prefix of a ref is not a ref"},
	}
	for _, tt := range tests {
		got, ok := d.selectRev(tt.in)
		assert.Equal(t, tt.wantOK, ok, tt.describe)
		if ok {
			assert.Equal(t, tt.want, got, tt.describe)
		}
	}
	_, ok := (&repoData{}).selectRev("")
	assert.False(t, ok, "an empty repository has no history")
}
