// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
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
	b, tag := domain.RefKindBranch, domain.RefKindTag
	name, err := domain.ParseRepoName("demo")
	require.NoError(t, err)
	d := &repoData{
		repo:     ports.RepoView{Repo: domain.Repo{Name: name}, DefaultBranch: "main"},
		branches: []domain.Ref{ref(b, "main"), ref(b, "side")},
		tags:     []domain.Ref{ref(tag, "v1")},
	}
	d.head = &d.branches[0]
	// The list of the other repositories is not reachable: the layout does
	// without it.
	repos := mocks.NewRepoService(t)
	repos.EXPECT().ListPage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RepoPage{}, errors.New("no list"))
	s, err := New(Config{
		Repos: repos, Browse: mocks.NewBrowseService(t),
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
		{"", []string{"main"}, nil, "main", short(commitOf("main").String())},
		{"v1", nil, []string{"v1"}, "v1", short(commitOf("v1").String())},
		{"side", []string{"side"}, nil, "side", short(commitOf("side").String())},
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
