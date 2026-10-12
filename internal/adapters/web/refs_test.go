// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"crypto/sha1" // #nosec G505 -- ids for tests
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/domain"
)

// commitOf is a commit id derived from a name, so that two refs of one name
// share their commit.
func commitOf(name string) domain.ObjectID {
	sum := sha1.Sum([]byte(name)) // #nosec G401 -- an id for a test, not a hash for security
	id, err := domain.ParseObjectID(hex.EncodeToString(sum[:]))
	if err != nil {
		panic(err)
	}
	return id
}

func ref(kind domain.RefKind, short string) domain.Ref {
	id := commitOf(short)
	return domain.Ref{Name: "refs/x/" + short, ShortName: short, Kind: kind, ID: id, CommitID: id}
}

// AC3: a ref with slashes is found by the longest matching prefix.
func TestResolveRef(t *testing.T) {
	t.Parallel()
	b, tag := domain.RefKindBranch, domain.RefKindTag
	refs := []domain.Ref{
		ref(b, "main"), ref(b, "feature"), ref(b, "feature/x"), ref(b, "feature/x/y"), ref(tag, "v1/rc"),
		ref(domain.RefKindHead, "main"), ref(tag, "main"),
	}
	tests := []struct {
		path, ref, rest string
		kind            domain.RefKind
		ok              bool
	}{
		{"main", "main", "", b, true},
		{"main/src/a.go", "main", "src/a.go", b, true},
		{"feature/x/y/z.txt", "feature/x/y", "z.txt", b, true},
		{"feature/x/z.txt", "feature/x", "z.txt", b, true},
		{"feature/other", "feature", "other", b, true},
		{"/feature/x/", "feature/x", "", b, true},
		{"v1/rc/README", "v1/rc", "README", tag, true},
		{"v1/README", "", "", "", false},
		{"nope/main", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			r, rest, ok := resolveRef(refs, tt.path)
			assert.Equal(t, tt.ok, ok)
			if !ok {
				return
			}
			assert.Equal(t, tt.ref, r.ShortName)
			assert.Equal(t, tt.kind, r.Kind, "a branch wins over a tag of the same name")
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
