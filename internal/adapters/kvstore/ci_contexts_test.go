// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"strings"
	"testing"
	"time"

	kv "bnema.dev/ermite-kv"
	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestCIJobContextsStorage(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	repo, err := domain.ParseRepoName("proj")
	require.NoError(t, err)
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate}))
	ci := s.CI()
	payload := `{"event":"` + strings.Repeat("payload", 3500) + `"}`
	j := domain.CIJob{ID: "build", Repo: repo, RunID: 1, RunAttempt: 1, Attempt: 1, Phase: domain.CIPhaseReady, ContextsJSON: payload}
	save := func() {
		v, err := ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j}})
		require.NoError(t, err)
		j.Version = v
	}
	save()
	contextsKey := jobContextsKey(1, 1, j.ID)
	initial, ok := ci.db.Get(ctx, contextsKey)
	require.True(t, ok)
	for i := range 5 {
		j.Steps = []domain.CIStep{{Number: int32(i + 1), Name: "step", State: domain.CIStepInProgress}}
		save()
		stored, ok := ci.db.Get(ctx, jobKey(1, 1, j.ID))
		require.True(t, ok)
		require.NotContains(t, string(stored.Value), payload)
		var pb storagepb.CIJob
		require.NoError(t, proto.Unmarshal(stored.Value, &pb))
		require.Empty(t, pb.GetContextsJson())
		contexts, ok := ci.db.Get(ctx, contextsKey)
		require.True(t, ok)
		require.Equal(t, initial.Version, contexts.Version, "unchanged contexts must not be rewritten")
		got, err := ci.GetJob(ctx, 1, 1, j.ID)
		require.NoError(t, err)
		require.Equal(t, payload, got.ContextsJSON)
		jobs, err := ci.ListJobs(ctx, 1, 1)
		require.NoError(t, err)
		require.Equal(t, payload, jobs[0].ContextsJSON)
		ready, err := ci.ListReadyJobs(ctx, 0)
		require.NoError(t, err)
		require.Equal(t, payload, ready[0].ContextsJSON)
	}
	// A kept job in a re-run gets an independent contexts key.
	j.RunAttempt = 2
	j.Version = 0
	save()
	copied, ok := ci.db.Get(ctx, jobContextsKey(1, 2, j.ID))
	require.True(t, ok)
	require.Equal(t, payload, string(copied.Value))
	j.ContextsJSON = ""
	save()
	got, err := ci.GetJob(ctx, 1, 2, j.ID)
	require.NoError(t, err)
	require.Empty(t, got.ContextsJSON)
	// Sweep must remove contexts from every attempt as well as job records.
	_, err = ci.Apply(ctx, ports.CIWrite{Runs: []domain.CIRun{{Repo: repo, ID: 1, Status: domain.CICompleted, CompletedAt: time.Now().Add(-time.Hour)}}})
	require.NoError(t, err)
	ids, err := ci.Sweep(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, []int64{1}, ids)
	for range ci.db.Snapshot(ctx).Prefix(ctx, []byte(jobCtxPrefix)) {
		t.Fatal("orphan contexts key")
	}
}

func TestCIJobLegacyContexts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(ctx)) })
	ci := s.CI()
	var b kv.Batch
	b.Put(jobKey(1, 1, "build"), mustMarshal(&storagepb.CIJob{Id: "build", Repo: "proj", RunId: 1, RunAttempt: 1, ContextsJson: `{"legacy":true}`}))
	_, err = ci.db.Apply(ctx, &b)
	require.NoError(t, err)
	j, err := ci.GetJob(ctx, 1, 1, "build")
	require.NoError(t, err)
	require.Equal(t, `{"legacy":true}`, j.ContextsJSON)
	jobs, err := ci.ListJobs(ctx, 1, 1)
	require.NoError(t, err)
	require.Equal(t, j.ContextsJSON, jobs[0].ContextsJSON)
	// Updating the legacy job moves its contexts out of the protobuf.
	_, err = ci.Apply(ctx, ports.CIWrite{Jobs: []domain.CIJob{j}})
	require.NoError(t, err)
	got, err := ci.GetJob(ctx, 1, 1, "build")
	require.NoError(t, err)
	require.Equal(t, j.ContextsJSON, got.ContextsJSON)
	item, ok := ci.db.Get(ctx, jobKey(1, 1, "build"))
	require.True(t, ok)
	pbJob, err := decodeJob(item)
	require.NoError(t, err)
	require.Empty(t, pbJob.ContextsJSON)
}
