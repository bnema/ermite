// SPDX-License-Identifier: AGPL-3.0-or-later

// Package kvstore implements the metadata ports over ermite-kv. Values are
// the protobuf messages of proto/ermite/storage.
package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// SchemaVersion is the storage schema this build reads and writes. Version 2
// adds branch prefixes to tokens: a build that does not know them would treat
// a branch-limited token as unrestricted, so it must refuse the data
// (ErrSchemaTooNew) instead. Version 3 adds the records of CI-3 that an older
// build would misread: the call and scoped jobs of reusable workflows (it would
// run the jobs of a called workflow as jobs of the caller's file, and plan the
// call itself), the head commit of the runs of merge requests
// (CIRun.head_sha: it would attach their checks to the merge commit, where
// nothing looks for them), and the index of the open merge requests by source
// branch (mropen/: an older build would not keep it up to date; the upgrade
// builds it once, see BuildOpenIndex). Version 4 adds the issues (is/, isc/,
// isopen/, numbered from the same mrseq/ sequence as the merge requests, and
// RepoSettings.issues): an older build would not delete them with their
// repository, nor keep the numbers apart, and it would misread the issue:write
// scope, so it must refuse the data. It also adds the pending issues of the
// public form (ispend/<repo>/<id>, removed with their repository), the bans of
// their submitters (isban/<hex hash>) and the form's server key
// (meta/issuekey). The upgrade also adds mr:review and issue:write (each only
// if missing) to the stored tokens that held all nine original scopes with no
// repository list and no branch prefix, so that they keep the owner rights (see
// upgradeTokens). Version 5 grants mr:force-merge to unrestricted tokens
// holding all eleven previous scopes, preserving their owner rights. Version 6
// adds releases and release:write; older builds cannot clean up their metadata
// or preserve the owner rights of upgraded tokens (see upgradeReleaseTokens).
// Version 7 adds the latest-release index; older writers would leave it stale.
// Version 8 adds resolved issues and history (isev/, mrev/), and rewrites
// merge-closed issues as resolved; older builds cannot decode state 4.
// Version 9 adds archived repositories (an older build would accept writes to
// them) and the Bernard records dpend/ (snooze, job and merge request state
// included) and dign/ (an older build would not remove them with their
// repository), plus the global dseen/.
const SchemaVersion = 9

const schemaKey = "meta/schema"

// ErrSchemaTooNew means the data was written by a newer ermite.
var ErrSchemaTooNew = errors.New("storage schema is newer than this ermite supports")

// Store is the ermite-kv backed metadata store.
type Store struct {
	db    *kv.DB
	locks *numberLocks // shared by the merge request and issue stores
}

var _ ports.RepoMetaStore = (*Store)(nil)

// Open opens (or creates) the database in dir and checks the storage schema:
// a missing version is written as SchemaVersion, a lower one is upgraded to it,
// a higher one stops with ErrSchemaTooNew. dir must exist.
func Open(ctx context.Context, dir string) (*Store, error) {
	db, err := kv.Open(ctx, dir, kv.Options{})
	if err != nil {
		return nil, fmt.Errorf("open ermite-kv in %s: %w", dir, err)
	}
	s := &Store{db: db, locks: new(numberLocks)}
	if err := s.checkSchema(ctx); err != nil {
		_ = db.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	// Part of the schema 3 upgrade: the index of the open merge requests.
	if err := s.BuildOpenIndex(ctx); err != nil {
		_ = db.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := s.buildReleaseLatestIndex(ctx); err != nil {
		_ = db.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// Close stops the database after the writes in progress.
func (s *Store) Close(ctx context.Context) error {
	if err := s.db.Close(ctx); err != nil {
		return fmt.Errorf("close ermite-kv: %w", err)
	}
	return nil
}

// Check reports whether the database accepts writes. It returns the failure
// once ermite-kv is closed or failed, and writes nothing.
//
// ermite-kv has no state accessor, and an empty batch sent through Apply would
// queue behind the writes in progress. Apply looks at the state of the database
// before it looks at its context, though, and an ended context stops it before
// the writer channel: Apply on a cancelled context therefore answers ErrClosed
// or ErrFailed when the database is stopped, and context.Canceled when it is
// healthy, without waiting for the writer.
func (s *Store) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()
	var b kv.Batch
	if _, err := s.db.Apply(probe, &b); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("ermite-kv: %w", err)
	}
	return nil
}

func (s *Store) checkSchema(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	key := []byte(schemaKey)
	for range 5 {
		item, ok := s.db.Get(ctx, key)
		if !ok {
			var b kv.Batch
			b.RequireAbsent(key)
			b.Put(key, mustMarshal(&storagepb.SchemaVersion{Version: SchemaVersion}))
			_, err := s.db.Apply(ctx, &b)
			if errors.Is(err, kv.ErrConflict) {
				continue // written meanwhile: read it
			}
			if err != nil {
				return fmt.Errorf("write storage schema: %w", err)
			}
			log.Info().Uint32("schema", SchemaVersion).Msg("storage schema initialized")
			return nil
		}
		var sv storagepb.SchemaVersion
		if err := proto.Unmarshal(item.Value, &sv); err != nil {
			return fmt.Errorf("decode storage schema: %w", err)
		}
		switch {
		case sv.GetVersion() > SchemaVersion:
			return fmt.Errorf("%w: found %d, supported %d", ErrSchemaTooNew, sv.GetVersion(), SchemaVersion)
		case sv.GetVersion() == 0:
			return fmt.Errorf("decode storage schema: invalid version 0")
		}
		if sv.GetVersion() < SchemaVersion {
			// Older data stays readable as it is (the changes are additive);
			// the new version is recorded so that older builds refuse it.
			// The tokens are upgraded in the same batch, so that a crash
			// leaves the old version and the upgrade runs again.
			target := uint32(SchemaVersion)
			switch {
			case sv.GetVersion() < 4:
				target = 4
			case sv.GetVersion() == 4:
				target = 5
			}
			var b kv.Batch
			b.RequireVersion(key, item.Version)
			if sv.GetVersion() < 4 {
				if err := s.upgradeTokens(ctx, &b); err != nil {
					return err
				}
			}
			if sv.GetVersion() == 4 {
				if err := s.upgradeForceMergeTokens(ctx, &b); err != nil {
					return err
				}
			}
			if sv.GetVersion() == 5 {
				if err := s.upgradeReleaseTokens(ctx, &b); err != nil {
					return err
				}
			}
			if target == SchemaVersion && sv.GetVersion() < 8 {
				if err := s.upgradeResolvedIssues(ctx, &b); err != nil {
					return err
				}
			}
			b.Put(key, mustMarshal(&storagepb.SchemaVersion{Version: target}))
			_, err := s.db.Apply(ctx, &b)
			if errors.Is(err, kv.ErrConflict) {
				continue // changed meanwhile: read it again
			}
			if err != nil {
				return fmt.Errorf("upgrade storage schema: %w", err)
			}
			log.Info().Uint32("from", sv.GetVersion()).Uint32("schema", target).Msg("storage schema upgraded")
			if target < SchemaVersion {
				continue
			}
			return nil
		}
		log.Info().Uint32("schema", sv.GetVersion()).Msg("storage schema checked")
		return nil
	}
	return errors.New("write storage schema: concurrent initialization did not settle")
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("kvstore: marshal %T: %v", m, err)) // only for invalid UTF-8 strings, checked by callers
	}
	return b
}

// preIssueScopes are the nine original scopes, before mr:review and issue:write.
var preIssueScopes = []domain.Scope{
	domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeRepoAdmin,
	domain.ScopeMRWrite, domain.ScopeMRMerge,
	domain.ScopeCIWrite, domain.ScopeSecretsWrite, domain.ScopeRunnerRegister,
	domain.ScopeTokenAdmin,
}

// upgradeTokens adds to b the schema 4 upgrade of the tokens: a token that
// holds all of preIssueScopes, limited to no repository and no branch prefix,
// had the owner rights, which schema 4 needed mr:review and issue:write
// for, so it gets each of them if missing (this is no escalation). Every other
// token is left as it is. The batch requires each token to be at the version it
// was read at.
func (s *Store) upgradeTokens(ctx context.Context, b *kv.Batch) error {
	return s.upgradeTokenScopes(ctx, b, preIssueScopes, []domain.Scope{domain.ScopeMRReview, domain.ScopeIssueWrite})
}

// preForceMergeScopes are the eleven scopes of schema 4, before mr:force-merge.
var preForceMergeScopes = append(slices.Clone(preIssueScopes), domain.ScopeMRReview, domain.ScopeIssueWrite)

// upgradeForceMergeTokens adds to b the schema 5 upgrade of the tokens: a
// token that holds all of preForceMergeScopes, limited to no repository and no
// branch prefix, had the owner rights, which AllScopes also needs
// mr:force-merge for, so it gets it. Every other token is left as it is.
func (s *Store) upgradeForceMergeTokens(ctx context.Context, b *kv.Batch) error {
	return s.upgradeTokenScopes(ctx, b, preForceMergeScopes, []domain.Scope{domain.ScopeMRForceMerge})
}

// preReleaseScopes are the twelve scopes of schema 5, before release:write.
var preReleaseScopes = append(slices.Clone(preForceMergeScopes), domain.ScopeMRForceMerge)

// upgradeReleaseTokens adds to b the schema 6 upgrade of the tokens: a token
// that holds all of preReleaseScopes, limited to no repository and no branch
// prefix, had the owner rights, which AllScopes also needs release:write for,
// so it gets it. Restricted or partial tokens are left as they are.
func (s *Store) upgradeReleaseTokens(ctx context.Context, b *kv.Batch) error {
	return s.upgradeTokenScopes(ctx, b, preReleaseScopes, []domain.Scope{domain.ScopeReleaseWrite})
}

func (s *Store) upgradeTokenScopes(ctx context.Context, b *kv.Batch, required, addedScopes []domain.Scope) error {
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(tokenPrefix)) {
		pb := &storagepb.Token{}
		if err := proto.Unmarshal(item.Value, pb); err != nil {
			return fmt.Errorf("upgrade token %s: %w", key, err)
		}
		if len(pb.GetRepos()) > 0 || len(pb.GetBranchPrefixes()) > 0 {
			continue
		}
		full := true
		for _, sc := range required {
			if !slices.Contains(pb.GetScopes(), sc.String()) {
				full = false
				break
			}
		}
		if !full {
			continue
		}
		added := false
		for _, sc := range addedScopes {
			if !slices.Contains(pb.GetScopes(), sc.String()) {
				pb.Scopes = append(pb.Scopes, sc.String())
				added = true
			}
		}
		if !added {
			continue
		}
		b.RequireVersion(slices.Clone(key), item.Version)
		b.Put(slices.Clone(key), mustMarshal(pb))
	}
	return nil
}

// upgradeResolvedIssues runs in the schema batch, without retroactive events or
// timestamp/index changes. Typical records are <1 KiB; the 16 MiB batch limit
// accommodates over 10k issues, consistent with the unchunked token upgrades.
func (s *Store) upgradeResolvedIssues(ctx context.Context, b *kv.Batch) error {
	count := 0
	for key, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte(issuePrefix)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pb storagepb.Issue
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return fmt.Errorf("upgrade issue %s: %w", key, err)
		}
		if pb.GetState() != storagepb.IssueState_ISSUE_STATE_CLOSED || pb.GetClosedByMergeRequest() == 0 {
			continue
		}
		pb.State = storagepb.IssueState_ISSUE_STATE_RESOLVED
		b.RequireVersion(key, item.Version)
		b.Put(key, mustMarshal(&pb))
		count++
	}
	log := zerowrap.FromCtx(ctx)
	log.Info().Int("count", count).Msg("merge-closed issues upgraded to resolved")
	return ctx.Err()
}
