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
//hexcheck:ignore suspicious-business-logic-in-adapter business rule for commit eligibility and byte budgets; pending move to domain/usecase
func (a *Adapter) ReadCommitObjects(ctx context.Context, repo domain.RepoName, ids []domain.ObjectID) (map[domain.ObjectID][]byte, error) {
	if len(ids) > ports.MaxCommitLimit {
		return nil, fmt.Errorf("%w: %d commit objects asked, at most %d", domain.ErrInvalid, len(ids), ports.MaxCommitLimit)
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
		var total int64
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, done := out[id]; done {
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
			if typ != git.TypeCommit || size > domain.MaxCommitObjectBytes {
				continue
			}
			if total+size > domain.MaxCommitObjectsBytes {
				break
			}
			typ, data, err := r.ReadObject(gid, nil)
			if errors.Is(err, git.ErrNotFound) {
				continue // removed meanwhile
			}
			if err != nil {
				return nil, err
			}
			if typ == git.TypeCommit && len(data) <= domain.MaxCommitObjectBytes && total+int64(len(data)) <= domain.MaxCommitObjectsBytes {
				out[id] = data
				total += int64(len(data))
			}
		}
		return out, nil
	}, cli)
}
