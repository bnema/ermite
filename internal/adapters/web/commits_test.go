// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

func TestIsCommitID(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"abcdef0", "0123456789abcdef0123456789abcdef01234567"} {
		assert.True(t, isCommitID(s), s)
	}
	for _, s := range []string{"", "abcdef", "ABCDEF0", "main", "abcdefg", "0123456789abcdef0123456789abcdef012345678"} {
		assert.False(t, isCommitID(s), s)
	}
}

// The ref switcher marks the ref the page shows, whatever the page: a tag, a
// branch other than the default one, or none for a commit.
func TestPinRefMarksCurrent(t *testing.T) {
	t.Parallel()
	b, tag := pb.RefKind_REF_KIND_BRANCH, pb.RefKind_REF_KIND_TAG
	d := &repoData{
		repo:     &pb.Repo{Name: "demo", DefaultBranch: "main"},
		branches: []*pb.Ref{ref(b, "main"), ref(b, "side")},
		tags:     []*pb.Ref{ref(tag, "v1")},
	}
	d.head = d.branches[0]
	s, err := New(Config{
		Repos: failing{err: errors.New("no list")}, Git: ermiteconnect.NewGitServiceClient(http.DefaultClient, "http://unused.invalid"),
		PublicURL: "https://forge.example", SourceURL: "https://forge.example/ermite", Version: "v1",
	})
	require.NoError(t, err)
	current := func(items []RefItem) (names []string) {
		for _, it := range items {
			if it.Current {
				names = append(names, it.Name)
			}
		}
		return names
	}
	for _, tt := range []struct {
		rest        string
		heads, tags []string
		ref, short  string
	}{
		{"", []string{"main"}, nil, "main", "c-main"},
		{"v1", nil, []string{"v1"}, "v1", "c-v1"},
		{"side", []string{"side"}, nil, "side", "c-side"},
		{"abcdef0123", nil, nil, "detached", "abcdef0"},
	} {
		tg, ok := d.locate(tt.rest)
		require.True(t, ok, tt.rest)
		l := s.repoLayout(t.Context(), d, "commits")
		pinRef(&l, tg)
		assert.Equal(t, tt.heads, current(l.Heads), tt.rest)
		assert.Equal(t, tt.tags, current(l.Tags), tt.rest)
		assert.Equal(t, tt.ref, l.Ref, tt.rest)
		assert.Equal(t, tt.short, l.Short, tt.rest)
	}
}
