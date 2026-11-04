// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"context"
	"time"

	"bnema.dev/ermite/internal/domain"
)

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
	// NPMVersions returns non-deprecated versions newer than current with publication times.
	NPMVersions(ctx context.Context, name, current string) ([]domain.DepVersion, error)
	// GoMajorPaths discovers up to five consecutive newer module-path majors.
	GoMajorPaths(ctx context.Context, module string) ([]string, error)
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

// DepsHeaderBytes bounds the prefix of a DepsInput read with Header set.
const DepsHeaderBytes = 4 << 10

// DepsInput is one file an ecosystem reads at a commit.
type DepsInput struct {
	Path domain.TreePath
	// Optional inputs may be absent at the commit; absent ones are left out of
	// the files map.
	Optional bool
	// Header reads at most DepsHeaderBytes of a file of any size (lockfile
	// versions, presence). The content may still be complete when the same
	// path is read in full for another reason.
	Header bool
}

// DepsSkip names a dependency of the ecosystem that Detect skipped, with the
// version the manifest holds now.
type DepsSkip struct{ Directory, Name, Current string }

// DepsEcosystem detects and applies updates in one dependency ecosystem.
type DepsEcosystem interface {
	Name() domain.DepsEcosystem
	// Manifests lists the files Detect reads, chosen from a possibly
	// truncated listing.
	Manifests(paths domain.PathList) []DepsInput
	// Inputs lists the files RunnerDetails and Validate read for ups, beyond
	// the changed report paths (nil before a report), without a full listing.
	Inputs(ups []domain.DepUpdate, changed []domain.TreePath) []DepsInput
	// Detect returns the candidates and the dependencies whose versions it
	// could not look up this time (a registry failure, a spent budget): their
	// stored updates are kept, not dropped. An error fails the whole check.
	Detect(ctx context.Context, files map[domain.TreePath][]byte, reg DepsRegistry) ([]domain.DepCandidate, []DepsSkip, error)
	ReportAllowed(ups []domain.DepUpdate, p domain.TreePath) bool
	ApplyInServer() bool
	// Bump purely edits manifests, without tidy or lockfile changes.
	Bump(files map[domain.TreePath][]byte, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error)
	// Apply is only called when ApplyInServer is true.
	Apply(files map[domain.TreePath][]byte, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error)
	// Validate checks after (runner report or server Apply) against before and
	// returns accepted side effects, ordered by directory and module name.
	Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.DepSideEffect, error)
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

// DepsRunParams describes the CI run of one dependency job.
type DepsRunParams struct {
	Repo domain.RepoName
	// Ref and Sha are the default branch and its tip, the commit the runner
	// applies the updates to.
	Ref, Sha string
	// Title names the run (the subject of Bernard's commit).
	Title string
	Job   domain.CIDepsJob
	// Label is the runner label that serves the job.
	Label string
	// Timeout is the time budget of the job once a runner has it, rounded up to
	// the minute (at least one).
	Timeout time.Duration
}

// DepsRunner starts and cancels the CI runs of dependency jobs. *usecase.CI
// implements it for Bernard.
type DepsRunner interface {
	// StartDepsRun creates a run of one job, ready for a runner of the label.
	// domain.ErrNotFound: the repository is gone.
	StartDepsRun(ctx context.Context, p DepsRunParams) (domain.CIRun, error)
	// CancelDepsRun cancels the run of a dependency job, without a principal
	// (a server-side decision); a run that has completed is left alone.
	CancelDepsRun(ctx context.Context, repo domain.RepoName, runID int64) error
	// GetDepsRun returns the run, without a principal; domain.ErrNotFound when
	// it is gone. (Not GetRun: CI's RunService.GetRun takes a principal.)
	GetDepsRun(ctx context.Context, repo domain.RepoName, id int64) (domain.CIRun, error)
}

// DepsRunObserver checks dependency reports before completion and is told
// when the job of a dependency run ended.
// *usecase.Bernard implements it; CI.SetDepsObserver installs it.
type DepsRunObserver interface {
	// CheckDepsReport checks a successful report before the job ends, outside
	// the run lock. The asynchronous path re-checks on purpose, under the
	// repository lock, and stays authoritative.
	CheckDepsReport(ctx context.Context, run domain.CIRun, job domain.CIJob, report DepsReport) error
	// DepsJobEnded is called once per ended dependency job, after the write that
	// ended it and outside the lock of the run. report is the runner's report;
	// it is nil when the server ended the job (cancel, timeout, lost runner,
	// queue timeout) or when the runner sent none.
	DepsJobEnded(ctx context.Context, run domain.CIRun, job domain.CIJob, report *DepsReport)
}

// RunnerLister lists the registered runners with their labels and kinds.
// CIStore implements it.
type RunnerLister interface {
	// ListRunners returns every runner ordered by name.
	ListRunners(ctx context.Context) ([]domain.CIRunner, error)
}

// DepsJobPreparer derives ecosystem-specific runner inputs at the job commit.
// Ecosystems without additional inputs need not implement it.
type DepsJobPreparer interface {
	RunnerDetails(files map[domain.TreePath][]byte, ups []domain.DepUpdate) ([]domain.CIDepsUpdate, string, error)
}
