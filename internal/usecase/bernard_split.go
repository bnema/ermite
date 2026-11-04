// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"slices"

	"bnema.dev/ermite/internal/domain"
)

// Split pulls the majors of name out of their family batch into their own
// batch (split true), or puts them back (split false). The pending updates are
// re-batched without a registry lookup. It refuses while a batch the change
// touches is applying or queued.
func (b *Bernard) Split(ctx context.Context, p domain.Principal, repo domain.RepoName, name domain.DepName, split bool) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	if _, err := lookupRepo(ctx, b.cfg.Meta, repo); err != nil {
		return domain.RepoDeps{}, err
	}
	defer b.repoLock(repo)()
	changed := false
	rec, err := b.cfg.Store.Mutate(ctx, repo, func(d *domain.RepoDeps) (bool, error) {
		changed = false
		if slices.Contains(d.SplitOut, name) == split {
			return false, nil
		}
		var ups []domain.DepUpdate
		isNew := map[domain.DepRef]bool{}
		for _, batch := range d.Batches {
			for _, u := range batch.Updates {
				ups = append(ups, u)
				isNew[u.DepRef] = u.New
			}
		}
		// The batches the dependency leaves and joins, under both settings.
		affected := map[domain.DepBatchKey]bool{}
		for _, u := range ups {
			if u.Ecosystem == name.Ecosystem && u.Name == name.Name {
				affected[domain.FamilyBatchKey(u, nil)] = true
				affected[domain.FamilyBatchKey(u, []domain.DepName{name})] = true
			}
		}
		for _, batch := range d.Batches {
			if affected[batch.Key] && (batch.State == domain.DepsApplying || batch.State == domain.DepsQueued) {
				return false, fmt.Errorf("%w: dependency updates are being applied", domain.ErrPreconditionFailed)
			}
		}
		if split {
			d.SplitOut = append(slices.Clone(d.SplitOut), name)
		} else {
			d.SplitOut = slices.DeleteFunc(slices.Clone(d.SplitOut), func(n domain.DepName) bool { return n == name })
		}
		_, _ = rebatch(d, ups)
		// Moving between batches does not make a version new.
		for i := range d.Batches {
			for j := range d.Batches[i].Updates {
				u := &d.Batches[i].Updates[j]
				u.New = isNew[u.DepRef]
			}
		}
		d.Repo = repo
		changed = true
		return true, nil
	})
	if err != nil {
		return domain.RepoDeps{}, fmt.Errorf("split the dependency: %w", err)
	}
	if changed {
		b.changed(ctx, repo)
	}
	return rec, nil
}
