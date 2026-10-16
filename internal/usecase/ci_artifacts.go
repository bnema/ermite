// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The artifacts of the runs (artifacts v4, CI-4 D3). The runner translates the
// toolkit's protocol and calls JobArtifactService with the job token; owners
// use ArtifactService. The records are in CIConfig.Artifacts, the contents in
// CIConfig.ArtifactFiles, named by ids only.
//
// Every write of the artifacts of a repository goes through its usage record
// (ports.CIArtifactWrite): a decision is made on the records read after it and
// written under its version, or made again. So the quota, the unique names of
// a run and the states of an upload hold with concurrent calls, and the bytes
// of a write are counted before it is accepted. The files are written before
// the record that refers to them and removed after the record that dropped
// them: a crash leaves at most files that no record names, which the sweep
// removes, never a record without its file.

var (
	_ ports.JobArtifactService = (*CI)(nil)
	_ ports.ArtifactService    = (*CI)(nil)
)

const (
	// artifactTries bounds the retries of a write of the artifacts of a
	// repository that other writes keep changing.
	artifactTries = 256
	// commitTries bounds the assemblies of one commit whose blocks change
	// meanwhile.
	commitTries = 5
)

var (
	// errNoArtifactWrite ends updateArtifacts without a write.
	errNoArtifactWrite = errors.New("no write")
	// errArtifactChanged makes a commit start again.
	errArtifactChanged = errors.New("artifact changed")
)

func (c *CI) artifactsOn() bool { return c.cfg.Artifacts != nil && c.cfg.ArtifactFiles != nil }

func artifactRef(a domain.CIArtifact) ports.CIArtifactRef {
	return ports.CIArtifactRef{RunID: a.RunID, ID: a.ID}
}

// artifactJob authenticates the job token of an artifact call: the job of
// jobID, which has not ended.
func (c *CI) artifactJob(ctx context.Context, secret, jobID string) (domain.CIJob, error) {
	if !c.artifactsOn() {
		return domain.CIJob{}, fmt.Errorf("%w: artifacts are off on this server", domain.ErrPreconditionFailed)
	}
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return domain.CIJob{}, err
	}
	if j.Ended() {
		return domain.CIJob{}, endedError(j)
	}
	return j, nil
}

// updateArtifacts reads the usage of the repository, lets fn fill the write
// from the records it reads then, and applies it; on a conflict it starts
// again. fn returns errNoArtifactWrite to write nothing.
func (c *CI) updateArtifacts(ctx context.Context, repo domain.RepoName, fn func(used int64, w *ports.CIArtifactWrite) error) error {
	for range artifactTries {
		used, version, err := c.cfg.Artifacts.ArtifactUsage(ctx, repo)
		if err != nil {
			return err
		}
		w := ports.CIArtifactWrite{Repo: repo, Used: used, UsageVersion: version}
		if err := fn(used, &w); err != nil {
			return err
		}
		err = c.cfg.Artifacts.ApplyArtifacts(ctx, w)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: the artifacts of %s keep changing, try again", domain.ErrUnavailable, repo)
}

// --- limits ---

// sizeText writes a limit for a message: in GiB or MiB when it is a whole
// number of them.
func sizeText(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return strconv.FormatInt(n>>30, 10) + " GiB"
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

func (c *CI) errArtifactTooLarge() error {
	return fmt.Errorf("%w: the artifact would exceed the size limit of one artifact (%s)", domain.ErrResourceExhausted, sizeText(c.cfg.ArtifactLimits.MaxSize))
}

func (c *CI) errQuota(repo domain.RepoName) error {
	return fmt.Errorf("%w: the artifacts of %s would exceed their quota (%s)", domain.ErrResourceExhausted, repo, sizeText(c.cfg.ArtifactLimits.Quota))
}

// --- reservations ---

// artifactReservations are the bytes being written and not charged yet:
// blocks streaming in, the repeats of a commit being assembled. A write
// reserves its bytes before they reach the disk, under the lock of its
// repository and against the charges stored and the other reservations, and
// releases them once its record is written or refused. So concurrent writes
// never put more on disk than the limits leave, and the write of the record
// checks them again. The size of one artifact and the quota are both limits of
// one repository: repositories never wait for one another.
type artifactReservations struct {
	mu    sync.Mutex
	repos map[domain.RepoName]*repoReservations
}

// repoReservations are the reservations of one repository. An entry stays
// once made (a few words per repository that ever uploaded); its maps empty.
type repoReservations struct {
	mu    sync.Mutex
	total int64
	art   map[int64]int64
}

func (r *artifactReservations) of(repo domain.RepoName) *repoReservations {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repos == nil {
		r.repos = map[domain.RepoName]*repoReservations{}
	}
	rr := r.repos[repo]
	if rr == nil {
		rr = &repoReservations{art: map[int64]int64{}}
		r.repos[repo] = rr
	}
	return rr
}

// reserve takes n bytes for artifact a, or returns the limit it would cross.
func (c *CI) reserve(ctx context.Context, a domain.CIArtifact, n int64) error {
	r := c.reservations.of(a.Repo)
	r.mu.Lock()
	defer r.mu.Unlock()
	used, _, err := c.cfg.Artifacts.ArtifactUsage(ctx, a.Repo)
	if err != nil {
		return err
	}
	cur, err := c.cfg.Artifacts.GetArtifact(ctx, a.ID)
	if err != nil {
		return err
	}
	limits := c.cfg.ArtifactLimits
	switch {
	case cur.Charged+r.art[a.ID]+n > limits.MaxSize:
		return c.errArtifactTooLarge()
	case used+r.total+n > limits.Quota:
		return c.errQuota(a.Repo)
	}
	r.total += n
	r.art[a.ID] += n
	return nil
}

// release gives back n bytes reserved for a.
func (c *CI) release(a domain.CIArtifact, n int64) {
	if n == 0 {
		return
	}
	r := c.reservations.of(a.Repo)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total = max(r.total-n, 0)
	if r.art[a.ID] -= n; r.art[a.ID] <= 0 {
		delete(r.art, a.ID)
	}
}

// reserveAhead is how much a reservingReader reserves at once, so that a
// large block takes one reservation per MiB, not one per read.
const reserveAhead = domain.CIArtifactChunk

// reservingReader passes on the data of r, each read covered by a
// reservation first: a read that the limits refuse fails without being passed
// on, so a block is never written beyond them. It reserves reserveAhead at a
// time, or just what a read needs when the limits leave less; reserved is all
// it holds (written or not), released whole after the record is written.
type reservingReader struct {
	ctx      context.Context
	c        *CI
	a        domain.CIArtifact
	r        io.Reader
	reserved int64
	left     int64 // reserved and not yet passed on
}

func (rr *reservingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if need := int64(n) - rr.left; need > 0 {
		got := max(need, reserveAhead)
		if rerr := rr.c.reserve(rr.ctx, rr.a, got); rerr != nil {
			if got == need {
				return 0, rerr
			}
			got = need
			if rerr := rr.c.reserve(rr.ctx, rr.a, got); rerr != nil {
				return 0, rerr
			}
		}
		rr.reserved += got
		rr.left += got
	}
	rr.left -= int64(n)
	return n, err
}

// --- locks of the commits ---

// lockArtifact serializes the commits of an artifact: two commits must not
// assemble its content at once. It returns the unlock function.
func (c *CI) lockArtifact(id int64) func() {
	c.mu.Lock()
	if c.artifactLocks == nil {
		c.artifactLocks = map[int64]*artifactLock{}
	}
	l := c.artifactLocks[id]
	if l == nil {
		l = &artifactLock{}
		c.artifactLocks[id] = l
	}
	l.users++
	c.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		c.mu.Lock()
		if l.users--; l.users == 0 {
			delete(c.artifactLocks, id)
		}
		c.mu.Unlock()
	}
}

type artifactLock struct {
	sync.Mutex
	users int
}

// --- removal ---

// removeArtifacts deletes the artifacts of a run that pick chooses, judged on
// the records as they are when the write is made, refunds them, then removes
// their files. It returns the deleted ones.
func (c *CI) removeArtifacts(ctx context.Context, repo domain.RepoName, runID int64, pick func(domain.CIArtifact) bool) ([]domain.CIArtifact, error) {
	var removed []domain.CIArtifact
	err := c.updateArtifacts(ctx, repo, func(used int64, w *ports.CIArtifactWrite) error {
		arts, err := c.cfg.Artifacts.RunArtifacts(ctx, repo, runID)
		if err != nil {
			return err
		}
		removed = removed[:0]
		for _, a := range arts {
			if pick(a) {
				removed = append(removed, a)
				w.Used -= a.Charged
			}
		}
		if len(removed) == 0 {
			return errNoArtifactWrite
		}
		w.Delete = removed
		w.Used = max(w.Used, 0)
		return nil
	})
	if errors.Is(err, errNoArtifactWrite) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.removeArtifactFiles(ctx, removed)
	return removed, nil
}

// removeArtifactFiles removes the files of deleted artifacts. A failure is
// logged: the sweep removes the files that no record names.
func (c *CI) removeArtifactFiles(ctx context.Context, arts []domain.CIArtifact) {
	for _, a := range arts {
		if err := c.cfg.ArtifactFiles.Remove(ctx, artifactRef(a)); err != nil {
			ctxLog(ctx).Warn().Err(err).Int64("artifact_id", a.ID).Msg("ci: remove the files of an artifact; the sweep will")
		}
	}
}

// dropIfEnded discards the uploads of the job's assignment when the job ended
// or was given again while the call ran: the cleanup of the end may have run
// before the call wrote its record.
func (c *CI) dropIfEnded(ctx context.Context, j domain.CIJob) {
	cur, err := c.cfg.Store.GetJob(ctx, j.RunID, j.RunAttempt, j.ID)
	if err == nil && !cur.Ended() && cur.TokenHash == j.TokenHash {
		return
	}
	c.dropPending(ctx, j, func(a domain.CIArtifact) bool { return a.Assignment == j.TokenHash })
}

// dropPending discards the unfinished uploads of a job, of any of its
// assignments, that pick chooses.
func (c *CI) dropPending(ctx context.Context, j domain.CIJob, pick func(domain.CIArtifact) bool) {
	removed, err := c.removeArtifacts(ctx, j.Repo, j.RunID, func(a domain.CIArtifact) bool {
		return !a.Final() && a.UploadedBy(j) && pick(a)
	})
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		ctxLog(ctx).Error().Err(err).Str("job_id", j.ID).Msg("ci: discard the unfinished artifacts of a job; the sweep will")
		return
	}
	if len(removed) > 0 {
		ctxLog(ctx).Info().Str("job_id", j.ID).Int64("run_id", j.RunID).Int("artifacts", len(removed)).Msg("ci: unfinished artifacts discarded")
	}
}

// artifactsJobEnded discards the unfinished uploads of a job that ended. It is
// called after the end is written.
func (c *CI) artifactsJobEnded(ctx context.Context, j domain.CIJob) {
	if !c.artifactsOn() {
		return
	}
	c.dropPending(ctx, j, func(domain.CIArtifact) bool { return true })
}

// --- JobArtifactService ---

// CreateJobArtifact begins an upload: a pending artifact with a new id. A
// pending artifact of the same job and name is discarded.
func (c *CI) CreateJobArtifact(ctx context.Context, secret string, params ports.CreateJobArtifactParams) (ports.CreatedArtifact, error) {
	j, err := c.artifactJob(ctx, secret, params.JobID)
	if err != nil {
		return ports.CreatedArtifact{}, err
	}
	if err := domain.ParseArtifactName(params.Name); err != nil {
		return ports.CreatedArtifact{}, err
	}
	mediaType, err := domain.ParseArtifactMediaType(params.MediaType)
	if err != nil {
		return ports.CreatedArtifact{}, err
	}
	now := c.now()
	id, err := c.cfg.Artifacts.NextArtifactID(ctx)
	if err != nil {
		return ports.CreatedArtifact{}, err
	}
	a := domain.CIArtifact{
		ID: id, Repo: j.Repo, RunID: j.RunID, Attempt: j.RunAttempt, JobID: j.ID, Assignment: j.TokenHash,
		Name: params.Name, MediaType: mediaType, State: domain.CIArtifactPending,
		CreatedAt: now, ExpiresAt: domain.ArtifactExpiry(params.ExpiresAt, now, c.cfg.ArtifactLimits.Retention),
	}
	var discarded []domain.CIArtifact
	err = c.updateArtifacts(ctx, j.Repo, func(used int64, w *ports.CIArtifactWrite) error {
		if used >= c.cfg.ArtifactLimits.Quota {
			return c.errQuota(j.Repo)
		}
		arts, err := c.cfg.Artifacts.RunArtifacts(ctx, j.Repo, j.RunID)
		if err != nil {
			return err
		}
		discarded = discarded[:0]
		pending := 0
		for _, o := range arts {
			switch {
			case o.Name != a.Name:
				if !o.Final() && o.UploadedBy(j) {
					pending++
				}
			case o.Visible(now) && o.Attempt == j.RunAttempt:
				return fmt.Errorf("%w: an artifact with this name already exists on the workflow run", domain.ErrAlreadyExists)
			case !o.Final() && o.UploadedBy(j):
				discarded = append(discarded, o) // a new create is a new upload
				w.Used -= o.Charged
			}
		}
		switch {
		case pending >= domain.CIArtifactMaxPendingPerJob:
			return fmt.Errorf("%w: a job has at most %d unfinished artifact uploads", domain.ErrResourceExhausted, domain.CIArtifactMaxPendingPerJob)
		case len(arts)-len(discarded) >= domain.CIArtifactMaxPerRun:
			return fmt.Errorf("%w: a run has at most %d artifacts", domain.ErrResourceExhausted, domain.CIArtifactMaxPerRun)
		}
		w.Put, w.Delete = []domain.CIArtifact{a}, discarded
		return nil
	})
	if err != nil {
		return ports.CreatedArtifact{}, err
	}
	c.removeArtifactFiles(ctx, discarded)
	log := zerowrap.FromCtx(ctx)
	log.Info().Int64("artifact_id", id).Int64("run_id", j.RunID).Str("job_id", j.ID).Msg("artifact upload begun")
	c.dropIfEnded(ctx, j)
	return ports.CreatedArtifact{ID: id, ExpiresAt: a.ExpiresAt}, nil
}

// pendingArtifact returns an unfinished upload of the job's assignment, or
// domain.ErrNotFound.
func (c *CI) pendingArtifact(ctx context.Context, j domain.CIJob, id int64) (domain.CIArtifact, error) {
	a, err := c.cfg.Artifacts.GetArtifact(ctx, id)
	if err != nil {
		return domain.CIArtifact{}, err
	}
	if a.Repo != j.Repo || !a.PendingOf(j) {
		return domain.CIArtifact{}, fmt.Errorf("%w: artifact %d is not an upload of this job", domain.ErrNotFound, id)
	}
	return a, nil
}

func errCommitted(id int64) error {
	return fmt.Errorf("%w: artifact %d is already committed", domain.ErrPreconditionFailed, id)
}

func newBlockFile(blockID []byte) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(blockID) + "." + hex.EncodeToString(raw[:])
}

// UploadJobArtifactBlock stores a block: the data goes to a new file, counted
// against the limits as it comes, then the record names it in place of the
// block of the same id, if any.
func (c *CI) UploadJobArtifactBlock(ctx context.Context, secret, jobID string, artifactID int64, blockID []byte, data io.Reader) (int64, error) {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return 0, err
	}
	if err := domain.CheckArtifactBlockID(blockID); err != nil {
		return 0, err
	}
	a, err := c.pendingArtifact(ctx, j, artifactID)
	if err != nil {
		return 0, err
	}
	if a.State != domain.CIArtifactPending {
		return 0, errCommitted(a.ID)
	}
	// Every byte is reserved before it is written, against the charges and
	// the other writes in flight (a block it replaces counts until the record
	// drops it): nothing beyond the limits ever reaches the disk.
	src := &reservingReader{ctx: ctx, c: c, a: a, r: data}
	defer func() { c.release(a, src.reserved) }()
	limits := c.cfg.ArtifactLimits
	ref, file := artifactRef(a), newBlockFile(blockID)
	size, err := c.cfg.ArtifactFiles.PutBlock(ctx, ref, file, src)
	if err != nil {
		return 0, err
	}
	var old *ports.CIArtifactBlock
	err = c.updateArtifacts(ctx, a.Repo, func(used int64, w *ports.CIArtifactWrite) error {
		cur, err := c.pendingArtifact(ctx, j, a.ID)
		if err != nil {
			return err
		}
		if cur.State != domain.CIArtifactPending {
			return errCommitted(cur.ID)
		}
		blocks, err := c.cfg.Artifacts.ArtifactBlocks(ctx, cur.ID)
		if err != nil {
			return err
		}
		old = nil
		for i := range blocks {
			if bytes.Equal(blocks[i].ID, blockID) {
				old = &blocks[i]
			}
		}
		var replaced int64
		if old != nil {
			replaced = old.Size
		} else if len(blocks) >= domain.CIArtifactMaxBlocks {
			return fmt.Errorf("%w: an artifact has at most %d blocks", domain.ErrInvalid, domain.CIArtifactMaxBlocks)
		}
		cur.Charged += size - replaced
		w.Used += size - replaced
		switch {
		case cur.Charged > limits.MaxSize:
			return c.errArtifactTooLarge()
		case w.Used > limits.Quota:
			return c.errQuota(cur.Repo)
		}
		w.Put = []domain.CIArtifact{cur}
		w.PutBlocks = []ports.CIArtifactBlock{{ArtifactID: cur.ID, ID: blockID, Size: size, File: file}}
		return nil
	})
	if err != nil {
		if rerr := c.cfg.ArtifactFiles.RemoveBlock(ctx, ref, file); rerr != nil {
			ctxLog(ctx).Warn().Err(rerr).Int64("artifact_id", a.ID).Msg("ci: remove a refused block; the sweep will")
		}
		return 0, err
	}
	if old != nil {
		if err := c.cfg.ArtifactFiles.RemoveBlock(ctx, ref, old.File); err != nil {
			ctxLog(ctx).Warn().Err(err).Int64("artifact_id", a.ID).Msg("ci: remove a replaced block")
		}
	}
	c.dropIfEnded(ctx, j)
	return size, nil
}

// blockListDigest identifies a block list: each id with its length.
func blockListDigest(ids [][]byte) []byte {
	h := sha256.New()
	var n [2]byte
	for _, id := range ids {
		binary.BigEndian.PutUint16(n[:], uint16(len(id))) //nolint:gosec // at most 64
		h.Write(n[:])
		h.Write(id)
	}
	return h.Sum(nil)
}

// CommitJobArtifact assembles the content of a pending artifact from its
// blocks, then records it and drops the blocks. The size counts against the
// limits with the repeats of the list.
func (c *CI) CommitJobArtifact(ctx context.Context, secret, jobID string, artifactID int64, blockIDs [][]byte) (int64, string, error) {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return 0, "", err
	}
	if len(blockIDs) > domain.CIArtifactMaxBlocks {
		return 0, "", fmt.Errorf("%w: a commit names at most %d blocks", domain.ErrInvalid, domain.CIArtifactMaxBlocks)
	}
	for _, id := range blockIDs {
		if err := domain.CheckArtifactBlockID(id); err != nil {
			return 0, "", err
		}
	}
	list := blockListDigest(blockIDs)
	// A retry of the runner waits here for the commit in progress and gets
	// its result. The commit goes on when its caller leaves: the runner gives
	// up on a long assembly and asks again.
	unlock := c.lockArtifact(artifactID)
	defer unlock()
	ctx = context.WithoutCancel(ctx)
	limits := c.cfg.ArtifactLimits
	for range commitTries {
		a, err := c.cfg.Artifacts.GetArtifact(ctx, artifactID)
		if err != nil {
			return 0, "", err
		}
		if a.Repo != j.Repo || !a.UploadedBy(j) || a.Assignment != j.TokenHash {
			return 0, "", fmt.Errorf("%w: artifact %d is not an upload of this job", domain.ErrNotFound, artifactID)
		}
		if a.State != domain.CIArtifactPending {
			if bytes.Equal(a.ListDigest, list) {
				return a.Size, a.Digest, nil // a repeat
			}
			return 0, "", fmt.Errorf("%w: artifact %d is committed with another block list", domain.ErrPreconditionFailed, a.ID)
		}
		blocks, err := c.cfg.Artifacts.ArtifactBlocks(ctx, a.ID)
		if err != nil {
			return 0, "", err
		}
		byID := make(map[string]ports.CIArtifactBlock, len(blocks))
		for _, b := range blocks {
			byID[string(b.ID)] = b
		}
		files := make([]string, 0, len(blockIDs))
		var total int64
		for _, id := range blockIDs {
			b, ok := byID[string(id)]
			if !ok {
				return 0, "", fmt.Errorf("%w: block %x was never uploaded", domain.ErrInvalid, id)
			}
			files = append(files, b.File)
			total += b.Size
		}
		if total > limits.MaxSize {
			return 0, "", c.errArtifactTooLarge()
		}
		// The repeats of the list make the content larger than the blocks
		// charged: that excess is reserved before the content is written.
		excess := max(total-a.Charged, 0)
		if excess > 0 {
			if err := c.reserve(ctx, a, excess); err != nil {
				return 0, "", err
			}
		}
		size, digest, err := c.cfg.ArtifactFiles.Assemble(ctx, artifactRef(a), files)
		if err != nil {
			c.release(a, excess)
		}
		if errors.Is(err, domain.ErrNotFound) {
			continue // a block was replaced meanwhile
		}
		if err != nil {
			return 0, "", err
		}
		err = c.updateArtifacts(ctx, a.Repo, func(used int64, w *ports.CIArtifactWrite) error {
			cur, err := c.cfg.Artifacts.GetArtifact(ctx, a.ID)
			if err != nil {
				return err
			}
			if cur.Version != a.Version {
				return errArtifactChanged
			}
			w.Used += size - cur.Charged
			if w.Used > limits.Quota {
				return c.errQuota(cur.Repo)
			}
			cur.State, cur.Size, cur.Digest, cur.ListDigest, cur.Charged = domain.CIArtifactCommitted, size, digest, list, size
			w.Put, w.DropBlocks = []domain.CIArtifact{cur}, []int64{cur.ID}
			return nil
		})
		c.release(a, excess)
		if errors.Is(err, errArtifactChanged) {
			continue
		}
		if errors.Is(err, domain.ErrNotFound) {
			c.removeArtifactFiles(ctx, []domain.CIArtifact{a}) // discarded meanwhile
		}
		if err != nil {
			return 0, "", err
		}
		if err := c.cfg.ArtifactFiles.RemoveBlocks(ctx, artifactRef(a)); err != nil {
			ctxLog(ctx).Warn().Err(err).Int64("artifact_id", a.ID).Msg("ci: remove the blocks of a committed artifact")
		}
		c.dropIfEnded(ctx, j)
		return size, digest, nil
	}
	return 0, "", fmt.Errorf("%w: the blocks of artifact %d keep changing, try again", domain.ErrUnavailable, artifactID)
}

// FinalizeJobArtifact makes the committed upload of the job with that name
// visible. An artifact of that name from an earlier attempt is deleted in the
// same write; one finalized in this attempt is already_exists.
func (c *CI) FinalizeJobArtifact(ctx context.Context, secret, jobID, name string, size int64, digest string) (domain.CIArtifact, error) {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return domain.CIArtifact{}, err
	}
	if err := domain.ParseArtifactName(name); err != nil {
		return domain.CIArtifact{}, err
	}
	digest, err = domain.ParseArtifactDigest(digest)
	if err != nil {
		return domain.CIArtifact{}, err
	}
	now := c.now()
	var out domain.CIArtifact
	var replaced []domain.CIArtifact
	err = c.updateArtifacts(ctx, j.Repo, func(used int64, w *ports.CIArtifactWrite) error {
		arts, err := c.cfg.Artifacts.RunArtifacts(ctx, j.Repo, j.RunID)
		if err != nil {
			return err
		}
		var mine, done *domain.CIArtifact
		var finals []domain.CIArtifact
		for i, a := range arts {
			switch {
			case a.Name != name:
			case a.PendingOf(j):
				if mine == nil || a.ID > mine.ID {
					mine = &arts[i]
				}
			case a.Final():
				finals = append(finals, a)
				if a.UploadedBy(j) && a.Visible(now) {
					done = &arts[i]
				}
			}
		}
		switch {
		case mine == nil && done != nil:
			out = *done // a repeat
			return errNoArtifactWrite
		case mine == nil:
			return fmt.Errorf("%w: this job has no upload of an artifact named %q", domain.ErrNotFound, name)
		case mine.State != domain.CIArtifactCommitted:
			return fmt.Errorf("%w: artifact %q is not committed", domain.ErrPreconditionFailed, name)
		case digest != "" && digest != mine.Digest:
			return fmt.Errorf("%w: the digest %s is not the digest of the content (%s)", domain.ErrInvalid, digest, mine.Digest)
		}
		replaced = replaced[:0]
		for _, f := range finals {
			if f.Visible(now) && f.Attempt == j.RunAttempt {
				return fmt.Errorf("%w: an artifact with this name already exists on the workflow run", domain.ErrAlreadyExists)
			}
			replaced = append(replaced, f) // an earlier attempt's, or expired
			w.Used -= f.Charged
		}
		out = *mine
		out.State = domain.CIArtifactFinal
		w.Put, w.Delete = []domain.CIArtifact{out}, replaced
		w.Used = max(w.Used, 0)
		return nil
	})
	if errors.Is(err, errNoArtifactWrite) {
		return out, nil
	}
	if err != nil {
		return domain.CIArtifact{}, err
	}
	c.removeArtifactFiles(ctx, replaced)
	log := zerowrap.FromCtx(ctx)
	ev := log.Info()
	if size != out.Size {
		ev = ev.Int64("uploader_size", size)
	}
	ev.Int64("artifact_id", out.ID).Int64("run_id", out.RunID).Str("job_id", j.ID).Int64("size", out.Size).Int("replaced", len(replaced)).Msg("artifact finalized")
	return out, nil
}

// ListJobArtifacts lists the visible artifacts of the job's run.
func (c *CI) ListJobArtifacts(ctx context.Context, secret, jobID, name string, artifactID int64) ([]domain.CIArtifact, error) {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return nil, err
	}
	arts, err := c.visibleArtifacts(ctx, j.Repo, j.RunID)
	if err != nil {
		return nil, err
	}
	out := arts[:0]
	for _, a := range arts {
		if (name == "" || a.Name == name) && (artifactID == 0 || a.ID == artifactID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (c *CI) visibleArtifacts(ctx context.Context, repo domain.RepoName, runID int64) ([]domain.CIArtifact, error) {
	arts, err := c.cfg.Artifacts.RunArtifacts(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	now := c.now()
	out := make([]domain.CIArtifact, 0, len(arts))
	for _, a := range arts {
		if a.Visible(now) {
			out = append(out, a)
		}
	}
	return out, nil
}

// DownloadJobArtifact streams a visible artifact of the job's run.
func (c *CI) DownloadJobArtifact(ctx context.Context, secret, jobID string, artifactID, offset int64, send func(ports.ArtifactChunk) error) error {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return err
	}
	a, err := c.cfg.Artifacts.GetArtifact(ctx, artifactID)
	if err != nil {
		return err
	}
	if a.Repo != j.Repo || a.RunID != j.RunID || !a.Visible(c.now()) {
		return fmt.Errorf("%w: artifact %d is not an artifact of this run", domain.ErrNotFound, artifactID)
	}
	return c.streamArtifact(ctx, a, offset, send)
}

// streamArtifact sends the artifact, then its content from offset.
func (c *CI) streamArtifact(ctx context.Context, a domain.CIArtifact, offset int64, send func(ports.ArtifactChunk) error) error {
	if offset < 0 || offset > a.Size {
		return fmt.Errorf("%w: offset %d is beyond the size %d", domain.ErrOutOfRange, offset, a.Size)
	}
	f, err := c.cfg.ArtifactFiles.Open(ctx, artifactRef(a), offset)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := send(ports.ArtifactChunk{Artifact: &a}); err != nil {
		return err
	}
	left := a.Size - offset
	buf := make([]byte, min(left, domain.CIArtifactChunk))
	for left > 0 {
		n, err := io.ReadFull(f, buf[:min(left, int64(len(buf)))])
		if err != nil {
			return fmt.Errorf("read artifact %d: %w", a.ID, err)
		}
		left -= int64(n)
		if err := send(ports.ArtifactChunk{Data: buf[:n]}); err != nil {
			return err
		}
	}
	return nil
}

// DeleteJobArtifact deletes the visible artifact of that name of the job's
// run.
func (c *CI) DeleteJobArtifact(ctx context.Context, secret, jobID, name string) (int64, error) {
	j, err := c.artifactJob(ctx, secret, jobID)
	if err != nil {
		return 0, err
	}
	if err := domain.ParseArtifactName(name); err != nil {
		return 0, err
	}
	now := c.now()
	removed, err := c.removeArtifacts(ctx, j.Repo, j.RunID, func(a domain.CIArtifact) bool {
		return a.Name == name && a.Visible(now)
	})
	if err != nil {
		return 0, err
	}
	if len(removed) == 0 {
		return 0, fmt.Errorf("%w: no artifact named %q on this run", domain.ErrNotFound, name)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Int64("artifact_id", removed[0].ID).Int64("run_id", j.RunID).Str("job_id", j.ID).Msg("artifact deleted by a job")
	return removed[0].ID, nil
}

// --- ArtifactService ---

// artifactRepo is the access rule of reading artifacts: a signed-in caller
// with repo:read, even on a public repository (GitHub serves no artifact
// anonymously).
func (c *CI) artifactRepo(ctx context.Context, p domain.Principal, name domain.RepoName) (domain.Repo, error) {
	if !c.artifactsOn() {
		return domain.Repo{}, fmt.Errorf("%w: artifacts are off on this server", domain.ErrPreconditionFailed)
	}
	repo, err := c.readableRepo(ctx, p, name)
	if err != nil {
		return domain.Repo{}, err
	}
	if err := scopeAccess(p, repo, domain.ScopeRepoRead); err != nil {
		return domain.Repo{}, err
	}
	return repo, nil
}

// ListArtifacts lists the visible artifacts of a run with the space of the
// repository.
func (c *CI) ListArtifacts(ctx context.Context, p domain.Principal, name domain.RepoName, runID int64) (ports.ArtifactList, error) {
	if _, err := c.artifactRepo(ctx, p, name); err != nil {
		return ports.ArtifactList{}, err
	}
	if _, err := c.runOf(ctx, name, runID); err != nil {
		return ports.ArtifactList{}, err
	}
	arts, err := c.visibleArtifacts(ctx, name, runID)
	if err != nil {
		return ports.ArtifactList{}, err
	}
	used, _, err := c.cfg.Artifacts.ArtifactUsage(ctx, name)
	if err != nil {
		return ports.ArtifactList{}, err
	}
	return ports.ArtifactList{Artifacts: arts, Used: used, Quota: c.cfg.ArtifactLimits.Quota}, nil
}

// ownerArtifact returns a visible artifact of the repository.
func (c *CI) ownerArtifact(ctx context.Context, name domain.RepoName, id int64) (domain.CIArtifact, error) {
	a, err := c.cfg.Artifacts.GetArtifact(ctx, id)
	if err != nil {
		return domain.CIArtifact{}, err
	}
	if a.Repo != name || !a.Visible(c.now()) {
		return domain.CIArtifact{}, fmt.Errorf("%w: artifact %d is not an artifact of %s", domain.ErrNotFound, id, name)
	}
	return a, nil
}

// DownloadArtifact streams a visible artifact of the repository.
func (c *CI) DownloadArtifact(ctx context.Context, p domain.Principal, name domain.RepoName, artifactID, offset int64, send func(ports.ArtifactChunk) error) error {
	if _, err := c.artifactRepo(ctx, p, name); err != nil {
		return err
	}
	a, err := c.ownerArtifact(ctx, name, artifactID)
	if err != nil {
		return err
	}
	return c.streamArtifact(ctx, a, offset, send)
}

// DeleteArtifact deletes an artifact. It needs ci:write on the repository and,
// like the other writes on a run, the branch of the run within the prefixes of
// a limited token.
func (c *CI) DeleteArtifact(ctx context.Context, p domain.Principal, name domain.RepoName, artifactID int64) error {
	if !c.artifactsOn() {
		return fmt.Errorf("%w: artifacts are off on this server", domain.ErrPreconditionFailed)
	}
	if _, err := c.writableRepo(ctx, p, name); err != nil {
		return err
	}
	a, err := c.ownerArtifact(ctx, name, artifactID)
	if err != nil {
		return err
	}
	run, err := c.runOf(ctx, name, a.RunID)
	if err != nil {
		return err
	}
	if err := runWriteAccess(p, run); err != nil {
		return err
	}
	removed, err := c.removeArtifacts(ctx, name, a.RunID, func(o domain.CIArtifact) bool { return o.ID == a.ID && o.Final() })
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		return fmt.Errorf("%w: artifact %d is not an artifact of %s", domain.ErrNotFound, artifactID, name)
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Int64("artifact_id", a.ID).Int64("run_id", a.RunID).Str("repo", name.String()).
		Str("principal", p.Label()).Str("token_id", p.TokenID()).Msg("artifact deleted")
	return nil
}

// --- sweep ---

// sweepArtifacts deletes the expired artifacts, those of runs that no longer
// exist, and the unfinished uploads of jobs that ended, were given again or
// began more than domain.CIArtifactPendingMax ago; then the blocks and files
// that no record names (a crash between a file and its record, a repository
// deleted while the server was down). Every failure is logged and the sweep
// goes on.
func (c *CI) sweepArtifacts(ctx context.Context) {
	if !c.artifactsOn() {
		return
	}
	log := ctxLog(ctx)
	all, err := c.cfg.Artifacts.AllArtifacts(ctx)
	if err != nil {
		log.Error().Err(err).Msg("ci: list the artifacts")
		return
	}
	type runKey struct {
		repo domain.RepoName
		run  int64
	}
	runs := map[runKey]bool{}
	for _, a := range all {
		runs[runKey{a.Repo, a.RunID}] = true
	}
	removed := 0
	for k := range runs {
		gone := false
		if _, err := c.cfg.Store.GetRunSource(ctx, k.run); errors.Is(err, domain.ErrNotFound) {
			gone = true
		}
		now := c.now()
		arts, err := c.removeArtifacts(ctx, k.repo, k.run, func(a domain.CIArtifact) bool {
			return gone || c.artifactDead(ctx, a, now)
		})
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			log.Error().Err(err).Int64("run_id", k.run).Msg("ci: remove the expired artifacts of a run")
		}
		removed += len(arts)
	}
	if removed > 0 {
		log.Info().Int("artifacts", removed).Msg("ci: expired artifacts removed")
	}
	c.sweepArtifactOrphans(ctx)
}

// artifactDead reports whether the sweep deletes an artifact of a run that
// exists.
func (c *CI) artifactDead(ctx context.Context, a domain.CIArtifact, now time.Time) bool {
	if a.Final() {
		return !now.Before(a.ExpiresAt)
	}
	if now.Sub(a.CreatedAt) >= domain.CIArtifactPendingMax {
		return true
	}
	j, err := c.cfg.Store.GetJob(ctx, a.RunID, a.Attempt, a.JobID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return true
	case err != nil:
		return false
	}
	return j.Ended() || j.TokenHash != a.Assignment
}

// sweepArtifactOrphans removes the blocks and files that no record names.
func (c *CI) sweepArtifactOrphans(ctx context.Context) {
	log := ctxLog(ctx)
	if ids, err := c.cfg.Artifacts.BlockArtifactIDs(ctx); err != nil {
		log.Error().Err(err).Msg("ci: list the staged artifact blocks")
	} else {
		for _, id := range ids {
			if _, err := c.cfg.Artifacts.GetArtifact(ctx, id); !errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err := c.cfg.Artifacts.DeleteBlocks(ctx, id); err != nil {
				log.Error().Err(err).Int64("artifact_id", id).Msg("ci: remove the blocks of an artifact that no longer exists")
			}
		}
	}
	c.removeOrphans(ctx, "artifacts", "artifact directories", c.cfg.ArtifactFiles.RunIDs, c.cfg.ArtifactFiles.RemoveRuns)
	runIDs, err := c.cfg.ArtifactFiles.RunIDs(ctx)
	if err != nil {
		log.Error().Err(err).Msg("ci: list the artifact directories")
		return
	}
	for _, run := range runIDs {
		ids, err := c.cfg.ArtifactFiles.ArtifactIDs(ctx, run)
		if err != nil {
			log.Error().Err(err).Int64("run_id", run).Msg("ci: list the artifact files of a run")
			continue
		}
		for _, id := range ids {
			a, err := c.cfg.Artifacts.GetArtifact(ctx, id)
			if err == nil && a.RunID == run {
				continue
			}
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				continue
			}
			ref := ports.CIArtifactRef{RunID: run, ID: id}
			if err := c.cfg.ArtifactFiles.Remove(ctx, ref); err != nil {
				log.Error().Err(err).Int64("artifact_id", id).Msg("ci: remove the files of an artifact that no longer exists")
			}
		}
	}
}
