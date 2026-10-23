// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCommitResolverCacheAndCap(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "demo"), mock.Anything).Return(domain.ResolvedRevision{}, domain.ErrNotFound).Times(maxCommitLookups)
	srv := &Server{cfg: Config{Browse: browse}}
	resolve := srv.commitResolver(t.Context(), "demo")
	for i := 0; i < maxCommitLookups+10; i++ {
		sha := fmt.Sprintf("%040x", i+1)
		_, _, ok := resolve(sha)
		assert.False(t, ok)
		_, _, ok = resolve(sha)
		assert.False(t, ok)
	}
}

func TestCommitResolverCanonicalLink(t *testing.T) {
	t.Parallel()
	browse := mocks.NewBrowseService(t)
	full := "f25742a" + strings.Repeat("b", 33)
	id, err := domain.ParseObjectID(full)
	require.NoError(t, err)
	browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "demo"), mock.Anything).Return(domain.ResolvedRevision{CommitID: id}, nil).Once()
	srv := &Server{cfg: Config{Browse: browse}}
	resolve := srv.commitResolver(t.Context(), "demo")
	for _, sha := range []string{"f25742a", "F25742A"} {
		href, label, ok := resolve(sha)
		assert.True(t, ok)
		assert.Equal(t, full[:7], label)
		assert.Equal(t, "/demo/commit/"+full, href)
	}
}

func TestCommitResolverRejectsDifferentTarget(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"", "refs/heads/deadbeef", "refs/tags/deadbeef"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			browse := mocks.NewBrowseService(t)
			id, err := domain.ParseObjectID(strings.Repeat("a", 40))
			require.NoError(t, err)
			browse.EXPECT().ResolveRevision(mock.Anything, domain.Anonymous(), repoName(t, "demo"), mock.Anything).Return(domain.ResolvedRevision{CommitID: id, Ref: ref}, nil).Once()
			srv := &Server{cfg: Config{Browse: browse}}
			res := srv.referenceResolver(t.Context(), "demo")
			// Empty Ref simulates an annotated tag object peeled to another commit.
			// Repeat rendering to verify rejected resolutions are cached as misses.
			for range 2 {
				h, err := newMarkdown().renderNote([]byte("deadbeef"), "demo", "main", "r-", res)
				require.NoError(t, err)
				assert.Equal(t, "<p>deadbeef</p>\n", string(h))
			}
		})
	}
}
