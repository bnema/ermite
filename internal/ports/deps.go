// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// DepsJob asks a runner to apply exact dependency versions.
type DepsJob struct {
	Updates   []domain.DepRef
	GoVersion string
}

// DepsReport contains the changed files or the runner's application error.
// Files remain a slice so duplicate paths and the original file count can be
// checked by the use case without information being lost in conversion.
type DepsReport struct {
	Files []DepsFile
	Error string
}

// DepsFile is one complete changed manifest or checksum file.
type DepsFile struct {
	Path    string
	Content []byte
}

// DepsRegistry resolves versions and malicious-package advisories for one check.
type DepsRegistry interface {
	// GoVersions returns the versions of module newer than current, each with
	// its publish time. domain.ErrNotFound = unresolvable: skip the module.
	GoVersions(ctx context.Context, module, current string) ([]domain.DepVersion, error)
	// GoDirective returns the go directive of module@version's go.mod, empty
	// when it has none. An error means unknown: the caller does not propose
	// the version.
	GoDirective(ctx context.Context, module, version string) (string, error)
	// ActionTags returns the v* tags of github.com/owner/repo with their peeled
	// commit. domain.ErrNotFound = missing repository: skip.
	ActionTags(ctx context.Context, owner, repo string) ([]domain.DepTag, error)
	// Malicious reports an OSV MAL- advisory for that exact version. An error
	// means unknown: the caller does not propose the version (fail closed).
	Malicious(ctx context.Context, eco domain.DepsEcosystem, name, version string) (bool, error)
}

// DepsEcosystem detects and applies updates in one dependency ecosystem.
type DepsEcosystem interface {
	Name() domain.DepsEcosystem
	Manifests(paths domain.PathList) []domain.TreePath
	Detect(ctx context.Context, files map[domain.TreePath][]byte, reg DepsRegistry) ([]domain.DepCandidate, error)
	ApplyInServer() bool
	// Apply is only called when ApplyInServer is true.
	Apply(files map[domain.TreePath][]byte, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error)
	// Validate checks after (runner report or server Apply) against before.
	Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) error
}

// DepsStore keeps repository records and ignores, and global first-seen times.
type DepsStore interface {
	// Get returns the record, or a zero RepoDeps{Repo: repo} (no error) when absent.
	Get(ctx context.Context, repo domain.RepoName) (domain.RepoDeps, error)
	// List returns every stored record, ordered by repository.
	List(ctx context.Context) ([]domain.RepoDeps, error)
	// Mutate applies fn under optimistic concurrency, like RepoMetaStore.Mutate:
	// fn is pure and may run several times; changed=false writes nothing.
	// It creates the record when absent.
	Mutate(ctx context.Context, repo domain.RepoName,
		fn func(*domain.RepoDeps) (changed bool, err error)) (domain.RepoDeps, error)
	// Ignore records refs as ignored for repo (idempotent).
	Ignore(ctx context.Context, repo domain.RepoName, refs []domain.DepRef) error
	IsIgnored(ctx context.Context, repo domain.RepoName, ref domain.DepRef) (bool, error)
	// FirstSeen returns when version@sha of name was first seen, recording at
	// when there is no record yet (put-if-absent). Global, not per repository.
	FirstSeen(ctx context.Context, eco domain.DepsEcosystem, name, version, sha string, at time.Time) (time.Time, error)
}

// DepsAssignment is a deps job handed to a runner.
type DepsAssignment struct {
	// JobID starts with "deps-".
	JobID string
	Repo  domain.RepoName
	Sha   string
	Job   DepsJob
	// Deadline is when the job times out; the job's repository token never
	// outlives it by more than domain.RepoTokenGrace.
	Deadline time.Time
}

// DepsJobs is the queue of dependency jobs that CI hands to runners (ids start
// with "deps-"). CI authenticates the runner and passes its id; a job that is
// unknown, or not assigned to that runner, is domain.ErrNotFound.
// *usecase.Bernard implements it.
type DepsJobs interface {
	// Next hands a pending job to the runner, or ok=false when there is none.
	Next(ctx context.Context, runnerID string) (a DepsAssignment, ok bool, err error)
	// Update is UpdateJob: steps and annotations carry nothing for a deps job.
	Update(ctx context.Context, runnerID, jobID string, steps []StepUpdate, annotations []domain.CIAnnotation) error
	// AppendLog keeps the tail of the log and returns the next offset; an
	// offset above the lines received is a *ErrCILogGap.
	AppendLog(ctx context.Context, runnerID, jobID string, offset int64, lines []domain.CILogLine) (next int64, err error)
	// Heartbeat tells that the runner is alive; a job past its deadline is
	// failed and domain.ErrNotFound.
	Heartbeat(ctx context.Context, runnerID, jobID string) error
	// Complete ends the job with the runner's report (nil when it sent none).
	Complete(ctx context.Context, runnerID, jobID string, report *DepsReport, conclusion domain.CIConclusion) error
}
