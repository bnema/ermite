// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
)

// Every value of the domain maps to a real value of the contract, never to
// UNSPECIFIED, and the mappers that have an inverse round-trip. A value added
// to the domain and forgotten here would reach clients as "not set".
func TestEnumMappersCoverEveryDomainValue(t *testing.T) {
	t.Run("visibility", func(t *testing.T) {
		for _, v := range []domain.Visibility{domain.VisibilityPublic, domain.VisibilityPrivate} {
			got := visibilityToPB(v)
			assert.NotEqual(t, pb.Visibility_VISIBILITY_UNSPECIFIED, got, v)
			back, err := visibilityFromPB(got)
			require.NoError(t, err, v)
			assert.Equal(t, v, back)
		}
		assert.Equal(t, pb.Visibility_VISIBILITY_UNSPECIFIED, visibilityToPB(""))
		_, err := visibilityFromPB(pb.Visibility(99))
		assert.ErrorIs(t, err, domain.ErrInvalidVisibility)
	})

	t.Run("scope", func(t *testing.T) {
		for _, s := range domain.AllScopes() {
			got, ok := scopeToPB[s]
			require.True(t, ok, "scope %s has no counterpart in the contract", s)
			assert.NotEqual(t, pb.Scope_SCOPE_UNSPECIFIED, got, s)
			back, err := scopeFromPB(got)
			require.NoError(t, err, s)
			assert.Equal(t, s, back)
		}
		_, err := scopeFromPB(pb.Scope_SCOPE_UNSPECIFIED)
		assert.ErrorIs(t, err, domain.ErrInvalidScope)
		// Every scope of the contract is a scope of the domain.
		for v := range pb.Scope_name {
			if pb.Scope(v) == pb.Scope_SCOPE_UNSPECIFIED {
				continue
			}
			_, err := scopeFromPB(pb.Scope(v))
			assert.NoError(t, err, pb.Scope(v))
		}
	})

	t.Run("ref kind", func(t *testing.T) {
		for _, k := range []domain.RefKind{domain.RefKindHead, domain.RefKindBranch, domain.RefKindTag} {
			got := refKindToPB(k)
			assert.NotEqual(t, pb.RefKind_REF_KIND_UNSPECIFIED, got, k)
			back, err := refKindFromPB(got)
			require.NoError(t, err, k)
			assert.Equal(t, k, back)
		}
		_, err := refKindFromPB(pb.RefKind_REF_KIND_UNSPECIFIED)
		assert.ErrorIs(t, err, domain.ErrInvalidRefKind)
	})

	t.Run("entry kind", func(t *testing.T) {
		seen := map[pb.EntryKind]domain.EntryKind{}
		for _, k := range []domain.EntryKind{
			domain.EntryFile, domain.EntryExecutable, domain.EntryDirectory, domain.EntrySymlink, domain.EntrySubmodule,
		} {
			got := entryKindToPB(k)
			assert.NotEqual(t, pb.EntryKind_ENTRY_KIND_UNSPECIFIED, got, k)
			assert.NotContains(t, seen, got, "%s and %s share a value", k, seen[got])
			seen[got] = k
		}
	})

	t.Run("file status", func(t *testing.T) {
		seen := map[pb.FileStatus]domain.FileStatus{}
		for _, s := range []domain.FileStatus{
			domain.FileAdded, domain.FileDeleted, domain.FileModified, domain.FileRenamed, domain.FileCopied, domain.FileTypeChanged,
		} {
			got := fileStatusToPB(s)
			assert.NotEqual(t, pb.FileStatus_FILE_STATUS_UNSPECIFIED, got, s)
			assert.NotContains(t, seen, got, "%s and %s share a value", s, seen[got])
			seen[got] = s
		}
	})

	t.Run("line kind", func(t *testing.T) {
		seen := map[pb.LineKind]domain.LineKind{}
		for _, k := range []domain.LineKind{domain.LineContext, domain.LineAddition, domain.LineDeletion} {
			got := lineKindToPB(k)
			assert.NotEqual(t, pb.LineKind_LINE_KIND_UNSPECIFIED, got, k)
			assert.NotContains(t, seen, got, "%s and %s share a value", k, seen[got])
			seen[got] = k
		}
	})

	// Every value of the contract except UNSPECIFIED is produced by some value
	// of the domain: nothing in the contract is unreachable.
	t.Run("the contract has no value the domain cannot produce", func(t *testing.T) {
		produced := func(names map[int32]string, from func(v int32) bool) {
			for v, n := range names {
				if v == 0 {
					continue
				}
				assert.True(t, from(v), "%s is never produced", n)
			}
		}
		produced(pb.EntryKind_name, func(v int32) bool {
			for _, k := range []domain.EntryKind{domain.EntryFile, domain.EntryExecutable, domain.EntryDirectory, domain.EntrySymlink, domain.EntrySubmodule} {
				if entryKindToPB(k) == pb.EntryKind(v) {
					return true
				}
			}
			return false
		})
		produced(pb.FileStatus_name, func(v int32) bool {
			for _, k := range []domain.FileStatus{domain.FileAdded, domain.FileDeleted, domain.FileModified, domain.FileRenamed, domain.FileCopied, domain.FileTypeChanged} {
				if fileStatusToPB(k) == pb.FileStatus(v) {
					return true
				}
			}
			return false
		})
		produced(pb.LineKind_name, func(v int32) bool {
			for _, k := range []domain.LineKind{domain.LineContext, domain.LineAddition, domain.LineDeletion} {
				if lineKindToPB(k) == pb.LineKind(v) {
					return true
				}
			}
			return false
		})
	})
}
