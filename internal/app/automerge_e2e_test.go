// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The RPC enable/cancel and Watch conversions belong to Batch D. This test
// exercises the composition root through use cases and domain events instead.
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
