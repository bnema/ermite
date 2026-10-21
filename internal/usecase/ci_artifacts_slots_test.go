// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// gateReader signals its first Read on started, then blocks until release is
// closed or ctx ends; it then returns data (or err when set).
type gateReader struct {
	ctx     context.Context
	started chan struct{}
	release chan struct{}
	data    []byte
	err     error
	once    sync.Once
	reads   int
	r       *bytes.Reader
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.reads++
	g.once.Do(func() {
		if g.started != nil {
			close(g.started)
		}
	})
	if g.release != nil {
		select {
		case <-g.release:
		case <-g.ctx.Done():
			return 0, g.ctx.Err()
		}
	}
	if g.err != nil {
		return 0, g.err
	}
	if g.r == nil {
		g.r = bytes.NewReader(g.data)
	}
	return g.r.Read(p)
}

type slotFixture struct {
	c      *CI
	secret string
	job    domain.CIJob
	puts   *int
}

func newSlotFixture(t *testing.T, slots int) *slotFixture {
	t.Helper()
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	secret := domain.JobTokenPrefix + strings.Repeat("a", domain.CITokenRandomLen)
	hash := domain.HashTokenSecret(secret)
	j := domain.CIJob{ID: "1-0-0", RunID: 1, RunAttempt: 1, Repo: repo, TokenHash: hash, Phase: domain.CIPhaseRunning}
	a := domain.CIArtifact{ID: 7, Repo: repo, RunID: 1, Attempt: 1, JobID: j.ID, Assignment: hash, State: domain.CIArtifactPending}

	store := mocks.NewCIStore(t)
	store.EXPECT().FindJobByToken(mock.Anything, hash, mock.Anything).Return(j, nil).Maybe()
	store.EXPECT().GetJob(mock.Anything, j.RunID, j.RunAttempt, j.ID).Return(j, nil).Maybe()
	arts := mocks.NewCIArtifactStore(t)
	arts.EXPECT().ArtifactUsage(mock.Anything, repo).Return(0, 1, nil).Maybe()
	arts.EXPECT().GetArtifact(mock.Anything, a.ID).Return(a, nil).Maybe()
	arts.EXPECT().ArtifactBlocks(mock.Anything, a.ID).Return(nil, nil).Maybe()
	arts.EXPECT().ApplyArtifacts(mock.Anything, mock.Anything).Return(nil).Maybe()
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().Return(time.Unix(1000, 0)).Maybe()
	blobs, puts, _ := artifactBlobMock(t, false)
	c := &CI{
		cfg: CIConfig{
			Store: store, Artifacts: arts, Blobs: blobs, Clock: clock,
			ArtifactLimits: ports.ArtifactLimits{Quota: 1 << 30, MaxSize: 1 << 30},
		},
		uploadSlots: make(chan struct{}, slots),
	}
	return &slotFixture{c: c, secret: secret, job: j, puts: puts}
}

func (f *slotFixture) upload(ctx context.Context, r io.Reader) (int64, error) {
	return f.c.UploadJobArtifactBlock(ctx, f.secret, f.job.ID, 7, []byte("blk"), r)
}

func TestArtifactUploadSlotsBound(t *testing.T) {
	for _, mode := range []string{"success", "body error", "ctx cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newSlotFixture(t, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hold := &gateReader{ctx: ctx, started: make(chan struct{}), release: make(chan struct{}), data: []byte("held")}
			if mode == "body error" {
				hold.err = errors.New("connection reset")
			}
			type result struct {
				n   int64
				err error
			}
			done := make(chan result, 1)
			go func() { n, err := f.upload(ctx, hold); done <- result{n, err} }()
			<-hold.started

			// The slot is taken: the next upload is refused at once, before
			// reading its body, and nothing is reserved or stored.
			refused := &gateReader{ctx: t.Context(), data: []byte("x")}
			_, err := f.upload(t.Context(), refused)
			require.ErrorIs(t, err, domain.ErrResourceExhausted)
			require.Zero(t, refused.reads)
			require.Zero(t, *f.puts)
			require.Zero(t, f.c.reservations.of(f.job.Repo).total)

			switch mode {
			case "ctx cancel":
				cancel()
			default:
				close(hold.release)
			}
			res := <-done
			if mode == "success" {
				require.NoError(t, res.err)
				require.EqualValues(t, 4, res.n)
			} else {
				require.Error(t, res.err)
			}
			require.Empty(t, f.c.uploadSlots, "the slot is released")

			n, err := f.upload(t.Context(), &gateReader{ctx: t.Context(), data: []byte("again")})
			require.NoError(t, err)
			require.EqualValues(t, 5, n)
			require.Empty(t, f.c.uploadSlots)
		})
	}
}

func TestArtifactAssemblyTakesASlot(t *testing.T) {
	b, _, _ := artifactBlobMock(t, false)
	c := &CI{cfg: CIConfig{Blobs: b}, uploadSlots: make(chan struct{}, 1)}
	a := domain.CIArtifact{RunID: 1, ID: 2}
	_, err := b.Put(t.Context(), a.BlockKey("a"), strings.NewReader("hi"), 2)
	require.NoError(t, err)

	// A full set of slots makes the assembly wait, not fail; it gives up
	// only when its context ends.
	c.uploadSlots <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = c.assembleBlobs(ctx, a, []string{"a"}, 2)
	require.ErrorIs(t, err, context.Canceled)

	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, _, err := c.assembleBlobs(t.Context(), a, []string{"a"}, 2)
		done <- result{n, err}
	}()
	<-c.uploadSlots // free the slot: the waiting assembly proceeds
	res := <-done
	require.NoError(t, res.err)
	require.EqualValues(t, 2, res.n)
	require.Empty(t, c.uploadSlots)
}
