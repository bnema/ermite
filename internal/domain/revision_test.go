// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func TestRevisionCandidates(t *testing.T) {
	full := strings.Repeat("a", 40)
	ref := func(s string) domain.RevisionCandidate {
		return domain.RevisionCandidate{Kind: domain.RevisionCandidateRef, Name: s}
	}
	branches := func(s string) []domain.RevisionCandidate {
		return []domain.RevisionCandidate{ref("refs/heads/" + s), ref("refs/tags/" + s)}
	}
	for _, tt := range []struct {
		name, text string
		want       []domain.RevisionCandidate
	}{
		{"zero HEAD", "", []domain.RevisionCandidate{{Kind: domain.RevisionCandidateHead, Name: "HEAD"}}},
		{"explicit HEAD", "HEAD", []domain.RevisionCandidate{{Kind: domain.RevisionCandidateHead, Name: "HEAD"}}},
		{"full object first", full, append([]domain.RevisionCandidate{{Kind: domain.RevisionCandidateObject, Name: full}}, branches(full)...)},
		{"full branch ref last", "refs/heads/main", append(branches("refs/heads/main"), ref("refs/heads/main"))},
		{"full tag ref last", "refs/tags/v1", append(branches("refs/tags/v1"), ref("refs/tags/v1"))},
		{"other full ref last", "refs/custom/x", append(branches("refs/custom/x"), ref("refs/custom/x"))},
		{"minimum abbreviation last", "abc1234", append(branches("abc1234"), domain.RevisionCandidate{Kind: domain.RevisionCandidateAbbrevID, Name: "abc1234"})},
		{"maximum abbreviation last", full[:39], append(branches(full[:39]), domain.RevisionCandidate{Kind: domain.RevisionCandidateAbbrevID, Name: full[:39]})},
		{"short hex is ref", "abc123", branches("abc123")},
		{"uppercase hex is ref", "ABC1234", branches("ABC1234")},
		{"ordinary name", "main", branches("main")},
		{"slash name", "feature/x", branches("feature/x")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := domain.ParseRevision(tt.text)
			require.NoError(t, err)
			assert.Equal(t, tt.want, r.Candidates())
		})
	}
}
