// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// This test exercises the composition root through use cases and domain
// events; TestAutoMergeEndToEndAPI does the same through the RPC API.
func TestAutoMergeEndToEnd(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": `name: ci
on:
  push:
    branches: [feat]
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`}, "")
	head := c.push("feat", map[string]string{"change.txt": "feature\n"}, "")
	number := c.openMR(c.root, "feat")
	repo := c.name("proj")
	allow := true
	_, err := c.forge.Repos.Update(c.ctx, domain.Root(), repo, ports.UpdateRepoParams{AllowAutoMerge: &allow})
	require.NoError(t, err)
	events := c.forge.Bus.Subscribe(c.ctx)
	enabled, err := c.forge.MergeRequests.EnableAutoMerge(c.ctx, domain.Owner(), repo, number, ports.EnableAutoMergeParams{})
	require.NoError(t, err)
	require.NotNil(t, enabled.AutoMerge)
	assert.Equal(t, head, enabled.AutoMerge.HeadID.String())
	select {
	case ev := <-events:
		require.IsType(t, domain.MergeRequestAutoMergeEnabled{}, ev)
	case <-time.After(5 * time.Second):
		t.Fatal("no enabled event")
	}
	c.finish(runner)
	// A successful push check alone must not merge while the pull run is queued.
	got, err := c.forge.MergeRequests.Get(c.ctx, domain.Root(), repo, number, ports.GetMergeRequestOptions{})
	require.NoError(t, err)
	assert.True(t, got.MergeRequest.IsOpen())
	c.finish(runner)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if merged, ok := ev.(domain.MergeRequestMerged); ok && merged.MergeRequest.Number == number {
				assert.True(t, merged.Auto)
				assert.True(t, merged.MergeRequest.AutoMerged)
				assert.Nil(t, merged.MergeRequest.AutoMerge)
				return
			}
		case <-deadline:
			t.Fatal("no automatic merged event")
		}
	}
}

// TestAutoMergeEndToEndAPI enables auto-merge through the API client, lets CI
// succeed, and reads the Watch stream: enabled, then merged with auto=true.
func TestAutoMergeEndToEndAPI(t *testing.T) {
	c := newCIHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	c.push("main", map[string]string{".github/workflows/ci.yml": `name: ci
on:
  push:
    branches: [feat]
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`}, "")
	c.push("feat", map[string]string{"change.txt": "feature\n"}, "")
	number := c.openMR(c.root, "feat")

	// Off by default: the server refuses.
	_, err := c.root.mr.EnableAutoMerge(c.ctx, connect.NewRequest(&pb.EnableAutoMergeRequest{Repo: c.repo, Number: number}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	ver, err := c.root.server.GetVersion(c.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, ver.Msg.GetFeatures(), "merge_requests.auto_merge")

	up, err := c.root.repo.UpdateRepo(c.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo:       c.repo,
		Settings:   &pb.RepoSettings{AllowAutoMerge: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"settings.allow_auto_merge"}},
	}))
	require.NoError(t, err)
	assert.True(t, up.Msg.GetRepo().GetSettings().GetAllowAutoMerge())

	w := c.watch(c.root, "", "home")
	res, err := c.root.mr.EnableAutoMerge(c.ctx, connect.NewRequest(&pb.EnableAutoMergeRequest{
		Repo: c.repo, Number: number, Mode: pb.MergeMode_MERGE_MODE_SQUASH,
	}))
	require.NoError(t, err)
	am := res.Msg.GetMergeRequest().GetAutoMerge()
	require.NotNil(t, am)
	assert.Equal(t, pb.MergeMode_MERGE_MODE_SQUASH, am.GetMode())
	assert.Equal(t, res.Msg.GetMergeRequest().GetHeadId(), am.GetHeadId())
	assert.NotEmpty(t, am.GetEnabledBy())
	assert.NotNil(t, am.GetEnabledAt())

	got, err := c.root.mr.GetMergeRequest(c.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: c.repo, Number: number}))
	require.NoError(t, err)
	assert.NotNil(t, got.Msg.GetMergeRequest().GetAutoMerge(), "Get reports the pending auto-merge")

	c.finish(runner)
	c.finish(runner)

	var sawEnabled bool
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-w.events:
			require.True(t, ok, "the stream ended: %v", w.err)
			if e := ev.GetMergeRequestAutoMergeEnabled(); e != nil {
				assert.EqualValues(t, number, e.GetMergeRequest().GetNumber())
				assert.NotNil(t, e.GetMergeRequest().GetAutoMerge())
				sawEnabled = true
			}
			if m := ev.GetMergeRequestMerged(); m != nil && m.GetMergeRequest().GetNumber() == number {
				assert.True(t, sawEnabled, "enabled comes before merged")
				assert.True(t, m.GetAuto())
				assert.True(t, m.GetMergeRequest().GetAutoMerged())
				assert.Nil(t, m.GetMergeRequest().GetAutoMerge())
				final, err := c.root.mr.GetMergeRequest(c.ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: c.repo, Number: number}))
				require.NoError(t, err)
				assert.Equal(t, pb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, final.Msg.GetMergeRequest().GetState())
				assert.True(t, final.Msg.GetMergeRequest().GetAutoMerged())
				return
			}
		case <-deadline:
			t.Fatal("no automatic merged event")
		}
	}
}
