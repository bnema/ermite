// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// journalPruneEvery is how often Record also deletes the expired entries.
const journalPruneEvery = time.Hour

// Journal records the activity of the repositories for the owner's inbox.
//
// The use cases call it where they publish their events, before publishing:
// the event bus drops events for a subscriber that falls behind (EventGap),
// which would lose entries for good, whereas a call cannot be dropped. The
// write is one ermite-kv batch; a failure is logged and never fails the
// action. Expired entries are pruned by Record itself, at most once per
// journalPruneEvery, so the journal needs no loop of its own.
//
// The pushes do not reach the owner topic; OnPush publishes
// domain.JournalUpdated for the inbox when it wrote an entry.
type Journal struct {
	store   ports.JournalStore
	git     ports.GitRepos
	commits ports.MergeGit
	bus     ports.EventBus
	clock   ports.Clock

	mu       sync.Mutex
	prunedAt time.Time
}

var _ ports.JournalRecorder = (*Journal)(nil)

// NewJournal builds the recorder. git gives the default branch of a
// repository and commits counts the commits of a push.
func NewJournal(store ports.JournalStore, git ports.GitRepos, commits ports.MergeGit, bus ports.EventBus, clock ports.Clock) *Journal {
	return &Journal{store: store, git: git, commits: commits, bus: bus, clock: clock}
}

// Record appends the entry, then prunes when it is time.
func (j *Journal) Record(ctx context.Context, e domain.JournalEntry) {
	j.append(ctx, e)
}

// append is Record that tells whether the entry was written.
func (j *Journal) append(ctx context.Context, e domain.JournalEntry) bool {
	log := zerowrap.FromCtx(ctx)
	now := j.clock.Now().UTC()
	if e.At.IsZero() {
		e.At = now
	}
	err := j.store.Append(ctx, e)
	if err != nil {
		log.Error().Err(err).Str("repo", e.Repo.String()).Str("kind", string(e.Kind)).Msg("record journal entry")
	}
	j.pruneIfDue(ctx, now)
	return err == nil
}

func (j *Journal) pruneIfDue(ctx context.Context, now time.Time) {
	j.mu.Lock()
	due := now.Sub(j.prunedAt) >= journalPruneEvery
	if due {
		j.prunedAt = now
	}
	j.mu.Unlock()
	if !due {
		return
	}
	log := zerowrap.FromCtx(ctx)
	n, err := j.store.Prune(ctx, now.Add(-domain.JournalRetention))
	if err != nil {
		log.Error().Err(err).Msg("prune journal")
		return
	}
	if n > 0 {
		log.Debug().Int("entries", n).Msg("journal pruned")
	}
}

// OnPush records the tags a push created or deleted and what it brought to
// the default branch. Other branches are not recorded.
func (j *Journal) OnPush(ctx context.Context, actor string, repo domain.RepoName, updates []domain.RefUpdate) {
	log := zerowrap.FromCtx(ctx)
	now := j.clock.Now().UTC()
	branch, read := "", false
	wrote := false
	defer func() {
		if wrote {
			j.bus.Publish(ctx, domain.JournalUpdated{Repo: repo, At: now})
		}
	}()
	for _, u := range updates {
		if tag, ok := strings.CutPrefix(u.Ref, "refs/tags/"); ok {
			e := domain.JournalEntry{At: now, Repo: repo, Kind: domain.JournalTagCreated, Actor: actor, Ref: tag}
			switch {
			case u.IsDelete():
				e.Kind = domain.JournalTagDeleted
			case !u.IsCreate():
				continue // a moved tag is neither
			default:
				e.Commit, _ = domain.ParseObjectID(u.New)
			}
			wrote = j.append(ctx, e) || wrote
			continue
		}
		name, ok := strings.CutPrefix(u.Ref, "refs/heads/")
		if !ok || u.IsDelete() {
			continue
		}
		if !read {
			b, err := j.git.HeadBranch(ctx, repo)
			if err != nil {
				if !errors.Is(err, domain.ErrNotFound) {
					log.Error().Err(err).Str("repo", repo.String()).Msg("journal: read the default branch")
				}
				return
			}
			branch, read = b, true
		}
		if name != branch {
			continue
		}
		wrote = j.recordDefaultPush(ctx, now, actor, repo, name, u) || wrote
	}
}

// recordDefaultPush counts the commits from the old head to the new one (all of
// the history of a new branch), at most domain.MaxJournalCommits.
func (j *Journal) recordDefaultPush(ctx context.Context, now time.Time, actor string, repo domain.RepoName, branch string, u domain.RefUpdate) bool {
	log := zerowrap.FromCtx(ctx)
	head, err := domain.ParseObjectID(u.New)
	if err != nil {
		return false
	}
	var old domain.ObjectID
	if !u.IsCreate() {
		if old, err = domain.ParseObjectID(u.Old); err != nil {
			return false
		}
	}
	e := domain.JournalEntry{At: now, Repo: repo, Kind: domain.JournalDefaultBranchPush, Actor: actor, Ref: branch, Commit: head}
	commits, more, err := j.commits.ListRange(ctx, repo, old, head, domain.MaxJournalCommits)
	switch {
	case err != nil:
		log.Error().Err(err).Str("repo", repo.String()).Msg("journal: count the pushed commits")
	case len(commits) == 0:
		return false // a rewind: the push brought nothing
	default:
		e.Commits, e.More, e.Summary = len(commits), more, commits[0].Summary
	}
	return j.append(ctx, e)
}

// journalMR is the entry of a merge request event.
func journalMR(kind domain.JournalKind, actor string, mr domain.MergeRequest, at time.Time) domain.JournalEntry {
	e := domain.JournalEntry{At: at, Repo: mr.Repo, Kind: kind, Actor: actor, Number: mr.Number, Title: mr.Title.String(), Ref: mr.Target}
	if kind == domain.JournalMergeRequestMerged {
		e.Commit = mr.MergedCommitID
	}
	return e
}

// journalIssue is the entry of an issue event.
func journalIssue(kind domain.JournalKind, actor string, issue domain.Issue, at time.Time) domain.JournalEntry {
	return domain.JournalEntry{At: at, Repo: issue.Repo, Kind: kind, Actor: actor, Number: issue.Number, Title: issue.Title.String()}
}

// record is Record on an optional recorder.
func record(ctx context.Context, j ports.JournalRecorder, e domain.JournalEntry) {
	if j != nil {
		j.Record(ctx, e)
	}
}
