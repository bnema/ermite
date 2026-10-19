// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"fmt"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowChangeOf(t *testing.T) {
	id := func(n int) domain.ObjectID {
		v, err := domain.ParseObjectID(fmt.Sprintf("%040x", n))
		require.NoError(t, err)
		return v
	}
	a, b, c, d := id(1), id(2), id(3), id(4)
	x, y, z := id(11), id(12), id(13)
	dir := func(v domain.ObjectID) domain.Subtree { return domain.Subtree{Kind: domain.EntryDirectory, ID: v} }
	up := func(old, new domain.ObjectID) domain.RefUpdate {
		o, n := old.String(), new.String()
		if old.IsZero() {
			o = domain.ZeroOID
		}
		if new.IsZero() {
			n = domain.ZeroOID
		}
		return domain.RefUpdate{Old: o, New: n}
	}
	zero := domain.ObjectID{}
	commit := func(v domain.ObjectID, parents ...domain.ObjectID) domain.PushedCommit {
		return domain.PushedCommit{ID: v, Parents: parents}
	}
	type row struct {
		name    string
		ups     []domain.RefUpdate
		commits []domain.PushedCommit
		trees   map[string]domain.Subtree
		blobs   []domain.ObjectID
		want    domain.WorkflowTreeChange
		found   bool
		missing bool
	}
	change := func(old, new domain.ObjectID) domain.WorkflowTreeChange {
		return domain.WorkflowTreeChange{Old: old, New: new}
	}
	rows := []row{
		{name: "no updates"},
		{name: "delete", ups: []domain.RefUpdate{up(a, zero)}},
		{name: "new blob ignores even a misleading subtree", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y)}, blobs: []domain.ObjectID{b}},
		{name: "old blob is not a directory", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y)}, blobs: []domain.ObjectID{a}, want: change(zero, y), found: true},
		{name: "new ref at HEAD", ups: []domain.RefUpdate{up(zero, a)}, trees: map[string]domain.Subtree{domain.HeadKey: dir(x), a.String(): dir(x)}},
		{name: "new ref same directory as HEAD", ups: []domain.RefUpdate{up(zero, b)}, trees: map[string]domain.Subtree{domain.HeadKey: dir(x), b.String(): dir(x)}},
		{name: "new ref old workflows", ups: []domain.RefUpdate{up(zero, b)}, trees: map[string]domain.Subtree{domain.HeadKey: dir(x), b.String(): dir(y)}, want: change(x, y), found: true},
		{name: "unborn HEAD", ups: []domain.RefUpdate{up(zero, a)}, trees: map[string]domain.Subtree{a.String(): dir(x)}, want: change(zero, x), found: true},
		{name: "HEAD has no workflows", ups: []domain.RefUpdate{up(zero, a)}},
		{name: "same workflow directory", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(x)}},
		{name: "move back or remove one file from surviving directory", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y)}, want: change(x, y), found: true},
		{name: "directory disappears", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x)}},
		{name: "lookalike path absent", ups: []domain.RefUpdate{up(a, b)}},
		{name: "root with workflows", commits: []domain.PushedCommit{commit(a)}, trees: map[string]domain.Subtree{a.String(): dir(x)}, want: change(zero, x), found: true},
		{name: "root without workflows", commits: []domain.PushedCommit{commit(a)}},
		{name: "plain pushed commit", commits: []domain.PushedCommit{commit(b, a)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(x)}},
		{name: "pushed directory disappears", commits: []domain.PushedCommit{commit(b, a)}, trees: map[string]domain.Subtree{a.String(): dir(x)}},
		{name: "merge keeps second parent", commits: []domain.PushedCommit{commit(c, a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(y)}},
		{name: "merge differs from all parents", commits: []domain.PushedCommit{commit(c, a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(z)}, want: change(x, z), found: true},
		{name: "merge new side changes workflows", commits: []domain.PushedCommit{commit(d, a, c), commit(c, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(z), d.String(): dir(x)}, want: change(y, z), found: true},
		{name: "merge adopts other parent refused by ref rule", ups: []domain.RefUpdate{up(a, c)}, commits: []domain.PushedCommit{commit(c, a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(y)}, want: change(x, y), found: true},
		{name: "change then revert", ups: []domain.RefUpdate{up(a, c)}, commits: []domain.PushedCommit{commit(c, b), commit(b, a)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(x)}, want: change(y, x), found: true},
		{name: "change below plain tip", commits: []domain.PushedCommit{commit(d, c), commit(c, b)}, trees: map[string]domain.Subtree{b.String(): dir(x), c.String(): dir(y), d.String(): dir(y)}, want: change(x, y), found: true},
		{name: "all new commits before any ref", ups: []domain.RefUpdate{up(a, b)}, commits: []domain.PushedCommit{commit(d, c)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(y), d.String(): dir(z)}, want: change(y, z), found: true},
		{name: "first ref wins", ups: []domain.RefUpdate{up(a, b), up(c, d)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(y), c.String(): dir(y), d.String(): dir(z)}, want: change(x, y), found: true},
		{name: "plain ref then changed ref", ups: []domain.RefUpdate{up(a, b), up(c, d)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): dir(x), c.String(): dir(y), d.String(): dir(z)}, want: change(y, z), found: true},
		{name: "missing ends fail closed", ups: []domain.RefUpdate{up(a, b)}, missing: true},
	}
	rows = append(rows,
		row{name: "new tree end with workflows", ups: []domain.RefUpdate{up(zero, a)}, trees: map[string]domain.Subtree{a.String(): dir(x)}, want: change(zero, x), found: true},
		row{name: "new tree end without workflows", ups: []domain.RefUpdate{up(zero, a)}},
		row{name: "new blob ref", ups: []domain.RefUpdate{up(zero, a)}, blobs: []domain.ObjectID{a}},
	)

	for _, kind := range []domain.EntryKind{domain.EntrySubmodule, domain.EntrySymlink, domain.EntryFile, domain.EntryExecutable} {
		rows = append(rows,
			row{name: string(kind) + " to directory same id", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): {Kind: kind, ID: x}, b.String(): dir(x)}, want: change(zero, x), found: true},
			row{name: "directory to " + string(kind), ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): dir(x), b.String(): {Kind: kind, ID: x}}},
			row{name: "same " + string(kind), ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): {Kind: kind, ID: x}, b.String(): {Kind: kind, ID: x}}},
		)
	}
	rows = append(rows, row{name: "gitlink to symlink", ups: []domain.RefUpdate{up(a, b)}, trees: map[string]domain.Subtree{a.String(): {Kind: domain.EntrySubmodule, ID: x}, b.String(): {Kind: domain.EntrySymlink, ID: y}}})
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			data := domain.PushSubtrees{Ends: map[string]domain.PushedEnd{}, Commits: r.commits, Subtrees: r.trees}
			if !r.missing {
				for _, u := range r.ups {
					for _, oid := range []string{u.Old, u.New} {
						data.Ends[oid] = domain.PushedEnd{Type: domain.ObjectCommit}
					}
				}
			}
			if r.name == "new tree end with workflows" || r.name == "new tree end without workflows" {
				data.Ends[a.String()] = domain.PushedEnd{Type: domain.ObjectTree}
			}
			for _, b := range r.blobs {
				data.Ends[b.String()] = domain.PushedEnd{Type: domain.ObjectBlob}
			}
			got, found, err := domain.WorkflowChangeOf(r.ups, data)
			if r.missing {
				require.ErrorIs(t, err, domain.ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, r.found, found)
			assert.Equal(t, r.want, got)
		})
	}
}
