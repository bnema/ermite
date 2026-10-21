// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"io"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// CIArtifactBlock is a staged block of a pending artifact.
type CIArtifactBlock struct {
	ArtifactID int64
	// ID is the uploader's block id (1 to 64 bytes).
	ID   []byte
	Size int64
	// File is the name of the block's file in CIArtifactFiles.
	File string
}

// CIArtifactWrite is a change of the artifacts of one repository, written in
// one atomic batch. The batch requires the usage record of the repository at
// UsageVersion (0: none stored) and stores Used in it: every write of the
// artifacts of a repository goes through that record, so two writes never both
// succeed on the same state, and a write fails with domain.ErrConflict when
// anything of the repository's artifacts changed since UsageVersion was read.
//
// Put stores records: one with Version 0 must not exist yet (the batch then
// also requires the repository, domain.ErrNotFound once it is deleted), any
// other must be at its Version. Delete removes records at their Version with
// their staged blocks. PutBlocks stores blocks (replacing one of the same id),
// DropBlocks removes all the blocks of the artifacts named.
type CIArtifactWrite struct {
	Repo         domain.RepoName
	Used         int64
	UsageVersion uint64
	Put          []domain.CIArtifact
	Delete       []domain.CIArtifact
	PutBlocks    []CIArtifactBlock
	DropBlocks   []int64
}

// CIArtifactStore keeps the records of the CI artifacts, never their contents.
// *kvstore.CIArtifacts implements it; the records go with their repository.
type CIArtifactStore interface {
	// NextArtifactID takes the next artifact id: global, increasing, never
	// reused.
	NextArtifactID(ctx context.Context) (int64, error)
	// ArtifactUsage returns the bytes charged to the artifacts of the
	// repository and the version of the record (0, 0 when there is none).
	ArtifactUsage(ctx context.Context, repo domain.RepoName) (used int64, version uint64, err error)
	// GetArtifact returns an artifact by id, or domain.ErrNotFound.
	GetArtifact(ctx context.Context, id int64) (domain.CIArtifact, error)
	// RunArtifacts returns the artifacts of a run in every state, by id.
	RunArtifacts(ctx context.Context, repo domain.RepoName, runID int64) ([]domain.CIArtifact, error)
	// AllArtifacts returns every artifact, for the sweep.
	AllArtifacts(ctx context.Context) ([]domain.CIArtifact, error)
	// ArtifactBlocks returns the staged blocks of an artifact.
	ArtifactBlocks(ctx context.Context, id int64) ([]CIArtifactBlock, error)
	// BlockArtifactIDs returns the artifacts that have staged blocks, for the
	// sweep of blocks left by a deleted artifact.
	BlockArtifactIDs(ctx context.Context) ([]int64, error)
	// ApplyArtifacts writes w in one batch.
	ApplyArtifacts(ctx context.Context, w CIArtifactWrite) error
	// DeleteBlocks removes the staged blocks of an artifact that no longer
	// exists (left by a repository deleted during an upload).
	DeleteBlocks(ctx context.Context, id int64) error
}

// CIArtifactRef names the files of one artifact.
type CIArtifactRef struct {
	RunID int64
	ID    int64
}

// CIArtifactFiles reads and removes the artifacts recorded before the blob
// store (ADR 0034); removed once they have all expired. *ciartifacts.Store
// implements it. Files are named by ids only.
type CIArtifactFiles interface {
	// Open returns the content from offset, or domain.ErrNotFound.
	Open(ctx context.Context, ref CIArtifactRef, offset int64) (io.ReadCloser, error)
	// Remove deletes the content and the blocks of the artifact.
	Remove(ctx context.Context, ref CIArtifactRef) error
	// RemoveRuns deletes the files of the runs.
	RemoveRuns(ctx context.Context, runIDs []int64) error
	// RunIDs lists the runs that have a directory, ArtifactIDs the artifacts
	// that have files in one, for the orphan sweep.
	RunIDs(ctx context.Context) ([]int64, error)
	ArtifactIDs(ctx context.Context, runID int64) ([]int64, error)
}

// ArtifactLimits are the settings of the artifacts of the server.
type ArtifactLimits struct {
	// Retention is the default and longest retention.
	Retention time.Duration
	// Quota is the space of the artifacts of one repository, MaxSize the size
	// of one artifact.
	Quota   int64
	MaxSize int64
}

// CreatedArtifact is the answer of CreateJobArtifact.
type CreatedArtifact struct {
	ID        int64
	ExpiresAt time.Time
}

// CreateJobArtifactParams begin an upload.
type CreateJobArtifactParams struct {
	JobID     string
	Name      string
	MediaType string
	// ExpiresAt is the expiry asked for; zero for the default.
	ExpiresAt time.Time
}

// ArtifactChunk is one message of a download: the artifact in the first one,
// then data.
type ArtifactChunk struct {
	Artifact *domain.CIArtifact
	Data     []byte
}

// JobArtifactService is the artifacts side of the runner protocol. The secret
// is the job token; jobID must be its job (domain.ErrForbidden otherwise), the
// job must not have ended (a *domain.JobEndedError), and the run is always the
// job's. *usecase.CI implements it.
type JobArtifactService interface {
	CreateJobArtifact(ctx context.Context, secret string, params CreateJobArtifactParams) (CreatedArtifact, error)
	// UploadJobArtifactBlock stores data as a block of a pending artifact of
	// the job. An error of data is returned as it is.
	UploadJobArtifactBlock(ctx context.Context, secret, jobID string, artifactID int64, blockID []byte, data io.Reader) (int64, error)
	CommitJobArtifact(ctx context.Context, secret, jobID string, artifactID int64, blockIDs [][]byte) (size int64, digest string, err error)
	FinalizeJobArtifact(ctx context.Context, secret, jobID, name string, size int64, digest string) (domain.CIArtifact, error)
	ListJobArtifacts(ctx context.Context, secret, jobID, name string, artifactID int64) ([]domain.CIArtifact, error)
	// DownloadJobArtifact calls send with the artifact, then with the content
	// from offset in chunks of at most domain.CIArtifactChunk bytes.
	DownloadJobArtifact(ctx context.Context, secret, jobID string, artifactID, offset int64, send func(ArtifactChunk) error) error
	DeleteJobArtifact(ctx context.Context, secret, jobID, name string) (int64, error)
}

// ArtifactList are the artifacts of a run with the space of the repository.
type ArtifactList struct {
	Artifacts []domain.CIArtifact
	Used      int64
	Quota     int64
}

// ArtifactService is the owner side of the artifacts. Listing and downloading
// need a signed-in caller with repo:read, even on a public repository;
// deleting needs ci:write. *usecase.CI implements it.
type ArtifactService interface {
	ListArtifacts(ctx context.Context, p domain.Principal, repo domain.RepoName, runID int64) (ArtifactList, error)
	// DownloadArtifact is DownloadJobArtifact for an owner.
	DownloadArtifact(ctx context.Context, p domain.Principal, repo domain.RepoName, artifactID, offset int64, send func(ArtifactChunk) error) error
	DeleteArtifact(ctx context.Context, p domain.Principal, repo domain.RepoName, artifactID int64) error
}
