// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit

import (
	"context"
	"errors"
	"fmt"

	git "bnema.dev/ermite-git"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.CommitObjectReader = (*Adapter)(nil)

// ReadCommitObjects reads the raw content of commits with ermite-git. Ids that
// are missing, are not commits, or are larger than
// domain.MaxCommitObjectBytes are left out. The read ends at the first commit
// that would take the total over domain.MaxCommitObjectsBytes. It is not routable: the library
// always serves it, and the wrapped reader only the repositories the library
// cannot read.
//
//hexcheck:ignore suspicious-business-logic-in-adapter streams object headers and bytes; eligibility, deduplication and byte limits are decided by domain.CommitObjectBudget, request validation by domain.ValidateCommitObjects
func (a *Adapter) ReadCommitObjects(ctx context.Context, repo domain.RepoName, ids []domain.ObjectID) (map[domain.ObjectID][]byte, error) {
	if err := domain.ValidateCommitObjects(ids, ports.MaxCommitLimit); err != nil {
		return nil, err
	}
	cli := func() (map[domain.ObjectID][]byte, error) {
		r, ok := a.cli.(ports.CommitObjectReader)
		if !ok {
			return nil, fmt.Errorf("read commit objects of %q: the wrapped reader cannot read commit objects", repo)
		}
		return r.ReadCommitObjects(ctx, repo, ids)
	}
	return serve(ctx, a, repo, func(r *git.Repo) (map[domain.ObjectID][]byte, error) {
		out := make(map[domain.ObjectID][]byte, len(ids))
		var budget domain.CommitObjectBudget
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if budget.Seen(id) {
				continue
			}
			gid, err := git.ParseID(id.String())
			if err != nil {
				return nil, err
			}
			typ, size, err := r.ObjectHeader(gid)
			if errors.Is(err, git.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			take, stop := budget.Admit(id, typ == git.TypeCommit, size)
			if stop {
				break
			}
			if !take {
				continue
			}
			typ, data, err := r.ReadObject(gid, nil)
			if errors.Is(err, git.ErrNotFound) {
				continue // removed meanwhile
			}
			if err != nil {
				return nil, err
			}
			if take, _ := budget.Admit(id, typ == git.TypeCommit, int64(len(data))); take {
				out[id] = data
				budget.Charge(id, int64(len(data)))
			}
		}
		return out, nil
	}, cli)
}
