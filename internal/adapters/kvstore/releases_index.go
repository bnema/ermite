// SPDX-License-Identifier: AGPL-3.0-or-later
package kvstore

import (
	"context"
	"fmt"
	"strconv"

	kv "bnema.dev/ermite-kv"
	"bnema.dev/ermite/internal/domain"
)

const releaseLatestIndexMarker = "meta/release-latest-index"

// buildReleaseLatestIndex runs before the store is exposed to writers. A crash
// before the marker is written safely repeats the pass on the next open.
// Selection remains owned by domain.LatestRelease.
func (s *Store) buildReleaseLatestIndex(ctx context.Context) error {
	if _, ok := s.db.Get(ctx, []byte(releaseLatestIndexMarker)); ok {
		return nil
	}
	var repo domain.RepoName
	var best domain.Release
	var eligible bool
	flush := func() error {
		var b kv.Batch
		if eligible {
			b.Put(relLatestKey(repo), []byte(strconv.FormatInt(best.ID, 10)))
		} else {
			b.Delete(relLatestKey(repo))
		}
		if _, err := s.db.Apply(ctx, &b); err != nil {
			return fmt.Errorf("build latest release index: %w", err)
		}
		return nil
	}
	haveRepo := false
	for _, item := range s.db.Snapshot(ctx).Prefix(ctx, []byte("rel/")) {
		rel, err := decodeRelease(item)
		if err != nil {
			return fmt.Errorf("build latest release index: %w", err)
		}
		if haveRepo && rel.Repo != repo {
			if err := flush(); err != nil {
				return err
			}
			best, eligible = domain.Release{}, false
		}
		repo, haveRepo = rel.Repo, true
		if eligible {
			best, eligible = domain.LatestRelease([]domain.Release{best, rel})
		} else {
			best, eligible = domain.LatestRelease([]domain.Release{rel})
		}
	}
	if haveRepo {
		if err := flush(); err != nil {
			return err
		}
	}
	var b kv.Batch
	b.Put([]byte(releaseLatestIndexMarker), []byte{1})
	if _, err := s.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("mark latest release index: %w", err)
	}
	return nil
}
