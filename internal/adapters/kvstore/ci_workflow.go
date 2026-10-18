// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The workflows a repository disabled (`ermite workflow disable`):
//
//	ciwfoff/<repo>/<workflow path>      CIWorkflowDisabled
//
// <workflow path> is ".github/workflows/<file>", whether or not the file exists.
// Written under the guard of the repository key, deleted with the repository
// (ciKeys).
const workflowOffPrefix = "ciwfoff/"

var _ ports.CIWorkflowStore = (*CI)(nil)

func workflowOffKey(repo domain.RepoName, path string) []byte {
	return []byte(workflowOffPrefix + repo.String() + "/" + path)
}

func workflowOffPrefixOf(repo domain.RepoName) string { return workflowOffPrefix + repo.String() + "/" }

// DisableWorkflow records the path as disabled. One already disabled keeps its
// date. A repository that does not exist is domain.ErrNotFound.
func (c *CI) DisableWorkflow(ctx context.Context, repo domain.RepoName, path string, at time.Time) error {
	val, err := proto.Marshal(&storagepb.CIWorkflowDisabled{DisabledAt: timestamppb.New(at)})
	if err != nil {
		return fmt.Errorf("encode disabled workflow: %w", err)
	}
	key := workflowOffKey(repo, path)
	for range updateAttemptsTip {
		if _, ok := c.db.Get(ctx, key); ok {
			if _, ok := c.db.Get(ctx, repoKey(repo)); !ok {
				return domain.ErrNotFound
			}
			return nil
		}
		var b kv.Batch
		if err := c.guardRepo(ctx, &b, repo); err != nil {
			return err
		}
		b.RequireAbsent(key)
		b.Put(key, val)
		_, err := c.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store disabled workflow: %w", err)
		}
		return nil
	}
	return fmt.Errorf("store disabled workflow: %w", domain.ErrConflict)
}

// EnableWorkflow removes the record of the path.
func (c *CI) EnableWorkflow(ctx context.Context, repo domain.RepoName, path string) error {
	var b kv.Batch
	b.Delete(workflowOffKey(repo, path))
	if _, err := c.db.Apply(ctx, &b); err != nil {
		return fmt.Errorf("delete disabled workflow: %w", err)
	}
	return nil
}

// DisabledWorkflows returns the disabled paths of the repository, sorted.
func (c *CI) DisabledWorkflows(ctx context.Context, repo domain.RepoName) ([]string, error) {
	prefix := workflowOffPrefixOf(repo)
	var out []string
	for k := range c.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		out = append(out, strings.TrimPrefix(string(k), prefix))
	}
	sort.Strings(out)
	return out, nil
}
