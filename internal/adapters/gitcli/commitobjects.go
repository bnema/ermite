// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.CommitObjectReader = (*Git)(nil)

// ReadCommitObjects reads the raw content of commits with the cat-file pool:
// one `info` per id, then `contents` for the commits of acceptable size. Ids
// that are missing, are not commits, or are larger than
// domain.MaxCommitObjectBytes are left out. The read ends at the first commit
// that would take the total over domain.MaxCommitObjectsBytes (known from
// `info`, before any content is read, so the stream stays consistent).
//
//hexcheck:ignore suspicious-business-logic-in-adapter streams object headers and bytes; eligibility, deduplication and byte limits are decided by domain.CommitObjectBudget, request validation by domain.ValidateCommitObjects
func (g *Git) ReadCommitObjects(ctx context.Context, repo domain.RepoName, ids []domain.ObjectID) (map[domain.ObjectID][]byte, error) {
	if err := domain.ValidateCommitObjects(ids, ports.MaxCommitLimit); err != nil {
		return nil, err
	}
	dir, err := g.repoDir(repo)
	if err != nil {
		return nil, err
	}
	out := make(map[domain.ObjectID][]byte, len(ids))
	err = g.cat.do(ctx, dir, func(c *catConn) error {
		clear(out) // a retry on a fresh process starts over
		var budget domain.CommitObjectBudget
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if budget.Seen(id) {
				continue
			}
			h, err := c.info(id.String())
			if notFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			take, stop := budget.Admit(id, h.Type == "commit", h.Size)
			if stop {
				break
			}
			if !take {
				continue
			}
			_, data, err := c.contents(id.String(), domain.MaxCommitObjectBytes)
			if err != nil {
				return err
			}
			out[id] = data
			budget.Charge(id, int64(len(data)))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read commit objects of %q: %w", repo, err)
	}
	return out, nil
}
