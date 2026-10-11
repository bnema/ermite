// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pb "bnema.dev/ermite-api/gen/ermite"
)

func ref(kind pb.RefKind, short string) *pb.Ref {
	return &pb.Ref{Name: "refs/x/" + short, ShortName: short, Kind: kind, CommitId: "c-" + short}
}

// AC3: a ref with slashes is found by the longest matching prefix.
func TestResolveRef(t *testing.T) {
	t.Parallel()
	b, tag := pb.RefKind_REF_KIND_BRANCH, pb.RefKind_REF_KIND_TAG
	refs := []*pb.Ref{
		ref(b, "main"), ref(b, "feature"), ref(b, "feature/x"), ref(b, "feature/x/y"), ref(tag, "v1/rc"),
		ref(pb.RefKind_REF_KIND_HEAD, "main"), ref(tag, "main"),
	}
	tests := []struct {
		path, ref, rest string
		kind            pb.RefKind
		ok              bool
	}{
		{"main", "main", "", b, true},
		{"main/src/a.go", "main", "src/a.go", b, true},
		{"feature/x/y/z.txt", "feature/x/y", "z.txt", b, true},
		{"feature/x/z.txt", "feature/x", "z.txt", b, true},
		{"feature/other", "feature", "other", b, true},
		{"/feature/x/", "feature/x", "", b, true},
		{"v1/rc/README", "v1/rc", "README", tag, true},
		{"v1/README", "", "", 0, false},
		{"nope/main", "", "", 0, false},
		{"", "", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			r, rest, ok := resolveRef(refs, tt.path)
			assert.Equal(t, tt.ok, ok)
			if !ok {
				return
			}
			assert.Equal(t, tt.ref, r.GetShortName())
			assert.Equal(t, tt.kind, r.GetKind(), "a branch wins over a tag of the same name")
			assert.Equal(t, tt.rest, rest)
		})
	}
}

func TestRepoHref(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/demo", repoHref("demo"))
	assert.Equal(t, "/demo/tree/feature/x/a%20b/c%23.go", repoHref("demo", "tree", "feature/x", "a b/c#.go"))
	assert.Equal(t, "/demo/blob/main/%3Fq", repoHref("demo", "blob", "main", "", "?q"))
}
