// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// smallReads hands out at most 32 KiB per read, like io.Copy's buffer.
type smallReads struct{ r io.Reader }

func (s smallReads) Read(p []byte) (int, error) { return s.r.Read(p[:min(len(p), 32<<10)]) }

func reserveFixture(t *testing.T, limits ports.ArtifactLimits) (*CI, *mocks.CIArtifactStore, domain.CIArtifact) {
	t.Helper()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	a := domain.CIArtifact{ID: 7, Repo: repo, RunID: 1, State: domain.CIArtifactPending}
	store := mocks.NewCIArtifactStore(t)
	store.EXPECT().ArtifactUsage(mock.Anything, repo).Return(0, 1, nil).Maybe()
	store.EXPECT().GetArtifact(mock.Anything, a.ID).Return(a, nil).Maybe()
	return &CI{cfg: CIConfig{Artifacts: store, ArtifactLimits: limits}}, store, a
}

// A large block takes one reservation per MiB, not one per read.
func TestReservationsPerMiB(t *testing.T) {
	c, store, a := reserveFixture(t, ports.ArtifactLimits{Quota: 1 << 30, MaxSize: 1 << 30})
	rr := &reservingReader{ctx: t.Context(), c: c, a: a, r: smallReads{bytes.NewReader(make([]byte, 8<<20))}}
	n, err := io.Copy(io.Discard, rr)
	require.NoError(t, err)
	assert.EqualValues(t, 8<<20, n)
	calls := 0
	for _, call := range store.Calls {
		if call.Method == "ArtifactUsage" {
			calls++
		}
	}
	assert.LessOrEqual(t, calls, 8, "one reservation per MiB for 256 reads")
	assert.EqualValues(t, 8<<20, rr.reserved, "never more reserved than needed at the end of a whole MiB")

	c.release(a, rr.reserved)
	assert.Zero(t, c.reservations.of(a.Repo).total, "all given back")
}

// Near the limit a read reserves just what it needs: an upload that fits is
// never refused because of the reservation ahead.
func TestReservationsNearTheLimit(t *testing.T) {
	const limit = 1<<20 + 100
	c, _, a := reserveFixture(t, ports.ArtifactLimits{Quota: 1 << 30, MaxSize: limit})
	rr := &reservingReader{ctx: t.Context(), c: c, a: a, r: iotest.HalfReader(bytes.NewReader(make([]byte, limit)))}
	_, err := io.Copy(io.Discard, rr)
	require.NoError(t, err)
	assert.LessOrEqual(t, rr.reserved, int64(limit))

	// One byte more than the limit: the read that crosses it fails and is not
	// passed on.
	c.release(a, rr.reserved)
	rr = &reservingReader{ctx: t.Context(), c: c, a: a, r: bytes.NewReader(make([]byte, limit+1))}
	n, err := io.Copy(io.Discard, rr)
	require.ErrorIs(t, err, domain.ErrResourceExhausted)
	assert.LessOrEqual(t, n, int64(limit))
}

// Repositories do not share a lock or a quota.
func TestReservationsArePerRepository(t *testing.T) {
	c, _, a := reserveFixture(t, ports.ArtifactLimits{Quota: 1 << 20, MaxSize: 1 << 20})
	require.NoError(t, c.reserve(t.Context(), a, 1<<20))
	assert.ErrorIs(t, c.reserve(t.Context(), a, 1), domain.ErrResourceExhausted)

	other, err := domain.ParseRepoName("other")
	require.NoError(t, err)
	b := domain.CIArtifact{ID: 8, Repo: other, RunID: 2, State: domain.CIArtifactPending}
	store := c.cfg.Artifacts.(*mocks.CIArtifactStore)
	store.EXPECT().ArtifactUsage(mock.Anything, other).Return(0, 1, nil)
	store.EXPECT().GetArtifact(mock.Anything, b.ID).Return(b, nil)
	require.NoError(t, c.reserve(t.Context(), b, 1<<20), "the quota of another repository is its own")
	assert.NotSame(t, c.reservations.of(a.Repo), c.reservations.of(b.Repo))
}
