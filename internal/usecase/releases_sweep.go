// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Sweep collects abandoned uploads, missing contents, and old orphan blobs.
// A failed metadata scan never authorizes blob deletion. Sweeps are serialized
// so the two observations of a missing blob are always distinct sweeps.
func (r *Releases) Sweep(ctx context.Context) {
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	log := ctxLog(ctx)
	now := r.clock.Now()
	all, err := r.store.AllAssets(ctx)
	if err != nil {
		log.Error().Err(err).Msg("scan release assets")
		return
	}
	seen := make(map[int64]bool, len(all))
	for _, a := range all {
		seen[a.ID] = true
		remove := false
		if a.State == domain.ReleaseAssetUploading {
			remove = now.Sub(a.UpdatedAt) > domain.ReleaseAssetPendingMax
		} else if a.State == domain.ReleaseAssetUploaded {
			_, err := r.blobs.Stat(ctx, a.BlobKey())
			switch {
			case errors.Is(err, domain.ErrNotFound):
				r.misses[a.ID]++
				remove = r.misses[a.ID] >= 2
				if !remove {
					log.Error().Int64("asset", a.ID).Msg("asset blob missing")
				}
			case err != nil:
				log.Error().Err(err).Int64("asset", a.ID).Msg("stat release asset")
			default:
				delete(r.misses, a.ID)
			}
		}
		if !remove {
			continue
		}
		err := r.removeAssetRecord(ctx, a, func(current domain.ReleaseAsset) bool { return current.Version == a.Version })
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrConflict) {
				log.Error().Err(err).Int64("asset", a.ID).Msg("remove stale release asset")
			}
			continue
		}
		delete(r.misses, a.ID)
		r.removeAssetBlobs(ctx, []domain.ReleaseAsset{a})
	}
	for id := range r.misses {
		if !seen[id] {
			delete(r.misses, id)
		}
	}
	// Re-check the store for each old key: concurrent reservations must not be
	// classified as orphans from a stale snapshot.
	err = r.blobs.List(ctx, "release/", func(info ports.BlobInfo) error {
		if now.Sub(info.ModTime) <= domain.ReleaseAssetPendingMax {
			return nil
		}
		// Only a canonical key can name an asset. Check its record directly
		// rather than repeating the full metadata scan for every object.
		id, parseErr := strconv.ParseInt(strings.TrimPrefix(info.Key, "release/"), 10, 64)
		if parseErr == nil && (domain.ReleaseAsset{ID: id}).BlobKey() == info.Key {
			_, e := r.store.GetAsset(ctx, id)
			if e == nil {
				return nil
			}
			if !errors.Is(e, domain.ErrNotFound) {
				log.Error().Err(e).Msg("check orphan release blob")
				return nil
			}
		}
		if e := r.blobs.Delete(ctx, info.Key); e != nil {
			log.Error().Err(e).Str("key", info.Key).Msg("remove orphan release blob")
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Msg("list release blobs")
	}
	err = r.blobs.ListMultipart(ctx, "release/", func(info ports.MultipartInfo) error {
		if now.Sub(info.Started) > domain.ReleaseAssetPendingMax {
			if e := r.blobs.AbortMultipart(ctx, info.Key, info.ID); e != nil {
				log.Error().Err(e).Msg("abort abandoned release multipart")
			}
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Msg("list release multipart uploads")
	}
}
