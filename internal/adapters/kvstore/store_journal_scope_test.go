// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	kv "bnema.dev/ermite-kv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func readTokenScopes(t *testing.T, dir string, ids ...string) map[string][]string {
	t.Helper()
	ctx := context.Background()
	db, err := kv.Open(ctx, dir, kv.Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close(ctx) }()
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		item, ok := db.Get(ctx, []byte("token/"+id))
		require.True(t, ok, id)
		var pb storagepb.Token
		require.NoError(t, proto.Unmarshal(item.Value, &pb))
		out[id] = pb.GetScopes()
	}
	return out
}

// Schema 10 grants journal:read to the tokens that had the owner rights: all
// the scopes before it, no repository list, no branch prefix. From schema 5,
// the release:write upgrade runs first, in its own batch.
func TestSchema10JournalTokenUpgrade(t *testing.T) {
	ctx := context.Background()
	var previous []string
	for _, sc := range domain.AllScopes() {
		if sc != domain.ScopeJournalRead {
			previous = append(previous, sc.String())
		}
	}
	withoutRelease := slices.DeleteFunc(slices.Clone(previous), func(s string) bool { return s == "release:write" })
	ids := []string{"full", "superset", "pre-release", "partial", "repo", "branch", "already"}
	for _, from := range []uint32{5, 6, 9} {
		t.Run(fmt.Sprint(from), func(t *testing.T) {
			dir := t.TempDir()
			putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: from}))
			for _, tc := range []struct {
				id              string
				scopes          []string
				repos, prefixes []string
			}{
				{id: "full", scopes: previous},
				{id: "superset", scopes: append(slices.Clone(previous), "future:scope")},
				{id: "pre-release", scopes: withoutRelease},
				{id: "partial", scopes: []string{"repo:read", "release:write"}},
				{id: "repo", scopes: previous, repos: []string{"a"}},
				{id: "branch", scopes: previous, prefixes: []string{"bot/"}},
				{id: "already", scopes: append(slices.Clone(previous), "journal:read")},
			} {
				putRaw(t, dir, "token/"+tc.id, mustProto(t, &storagepb.Token{Id: tc.id, Scopes: tc.scopes, Repos: tc.repos, BranchPrefixes: tc.prefixes}))
			}

			s, err := kvstore.Open(ctx, dir)
			require.NoError(t, err)
			require.NoError(t, s.Close(ctx))
			assert.Equal(t, uint32(kvstore.SchemaVersion), readSchema(t, dir))
			got := readTokenScopes(t, dir, ids...)
			assert.Contains(t, got["full"], "journal:read")
			assert.Len(t, got["full"], len(domain.AllScopes()))
			assert.Contains(t, got["superset"], "journal:read", "an extra scope still had the owner rights")
			assert.Contains(t, got["superset"], "future:scope", "the extra scope is kept")
			if from == 5 {
				assert.Contains(t, got["pre-release"], "journal:read", "release:write, then journal:read")
			} else {
				assert.NotContains(t, got["pre-release"], "journal:read", "had no owner rights at this schema")
			}
			for _, id := range []string{"partial", "repo", "branch"} {
				assert.NotContains(t, got[id], "journal:read", id)
			}
			assert.Len(t, got["already"], len(domain.AllScopes()), "not added twice")

			// Opening again changes nothing.
			s, err = kvstore.Open(ctx, dir)
			require.NoError(t, err)
			require.NoError(t, s.Close(ctx))
			assert.Equal(t, got, readTokenScopes(t, dir, ids...))
		})
	}
}

// A token stored with every scope by this build keeps the owner rights once
// read back.
func TestFullTokenKeepsOwnerRights(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	putRaw(t, dir, "meta/schema", mustProto(t, &storagepb.SchemaVersion{Version: 9}))
	var previous []string
	for _, sc := range domain.AllScopes() {
		if sc != domain.ScopeJournalRead {
			previous = append(previous, sc.String())
		}
	}
	putRaw(t, dir, "token/t1", mustProto(t, &storagepb.Token{Id: "t1", Label: "daily", Hash: make([]byte, 32), Scopes: previous}))
	s := openStore(t, dir)
	tok, err := s.Tokens().Get(ctx, "t1")
	require.NoError(t, err)
	assert.True(t, domain.TokenPrincipal(tok).HasOwnerRights())
}
