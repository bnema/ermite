// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Who moves a branch or a tag to its object (CI-3, D1; domain.CIRefTip):
//
//	cireftip/<repo>/<full ref name>      CIRefTip
//
// Written under the guard of the repository key, deleted with the repository
// (ciKeys). Ref names hold '/', so the key is split at the first '/' after the
// repository, whose name holds none.
const refTipPrefix = "cireftip/"

var _ ports.CIRefTipStore = (*CI)(nil)

func refTipKey(repo domain.RepoName, ref string) []byte {
	return []byte(refTipPrefix + repo.String() + "/" + ref)
}

func refTipsPrefix(repo domain.RepoName) string { return refTipPrefix + repo.String() + "/" }

// updateAttemptsTip bounds the retries of a write that conflicts with another
// write of the repository.
const updateAttemptsTip = 8

// PutRefTip records who moves the ref to its object. A repository that does not
// exist is domain.ErrNotFound.
func (c *CI) PutRefTip(ctx context.Context, t domain.CIRefTip) error {
	val, err := proto.Marshal(&storagepb.CIRefTip{
		Sha: t.Sha, Actor: t.Actor, Trusted: t.Trusted,
		PusherKind: string(t.Pusher.Kind), PusherTokenId: t.Pusher.TokenID,
	})
	if err != nil {
		return fmt.Errorf("encode ref tip: %w", err)
	}
	for range updateAttemptsTip {
		var b kv.Batch
		if err := c.guardRepo(ctx, &b, t.Repo); err != nil {
			return err
		}
		b.Put(refTipKey(t.Repo, t.Ref), val)
		_, err := c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store ref tip: %w", err)
		}
		return nil
	}
	return fmt.Errorf("store ref tip: %w", domain.ErrConflict)
}

// GetRefTip reads the record of a ref, or domain.ErrNotFound.
func (c *CI) GetRefTip(ctx context.Context, repo domain.RepoName, ref string) (domain.CIRefTip, error) {
	it, ok := c.db.Get(ctx, refTipKey(repo, ref))
	if !ok {
		return domain.CIRefTip{}, domain.ErrNotFound
	}
	var pb storagepb.CIRefTip
	if err := proto.Unmarshal(it.Value, &pb); err != nil {
		return domain.CIRefTip{}, fmt.Errorf("decode ref tip: %w", err)
	}
	return domain.CIRefTip{
		Repo: repo, Ref: ref, Sha: pb.GetSha(), Actor: pb.GetActor(), Trusted: pb.GetTrusted(),
		Pusher: domain.CIPusher{Kind: domain.CIPusherKind(pb.GetPusherKind()), TokenID: pb.GetPusherTokenId()},
	}, nil
}

// DeleteRefTip removes the record of a ref (the ref is deleted).
func (c *CI) DeleteRefTip(ctx context.Context, repo domain.RepoName, ref string) error {
	var b kv.Batch
	b.Delete(refTipKey(repo, ref))
	if _, err := c.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete ref tip: %w", err)
	}
	return nil
}
