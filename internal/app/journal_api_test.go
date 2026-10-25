// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
)

// The journal and the unreleased commits through the API, on a real forge:
// pushes and tags recorded by the hooks, read by the owner, a journal:read
// token, a token limited to one repository, and refused to the others.
func TestJournalAPIEndToEnd(t *testing.T) {
	h := newHarness(t, harnessOpts{trustedProxies: "127.0.0.0/8"})
	h.seed("pub", domain.VisibilityPublic, "README.md")
	h.seed("priv", domain.VisibilityPrivate, "README.md")
	for _, repo := range []string{"pub", "priv"} {
		wt := h.t.TempDir()
		url := h.urlAuth(adminToken, "/"+repo+".git")
		h.mustGit(wt, "clone", "-q", url, ".")
		h.mustGit(wt, "tag", "v0.1.0")
		h.mustGit(wt, "push", "-q", url, "v0.1.0")
		h.writeFile(wt, "a.txt", []byte("a\n"))
		h.writeFile(wt, "b.txt", []byte("b\n"))
		h.mustGit(wt, "push", "-q", url, "main")
	}

	type caller struct {
		journal ermiteconnect.JournalServiceClient
		release ermiteconnect.ReleaseServiceClient
	}
	as := func(secret string) caller {
		base := h.url("/-/api")
		hc := &http.Client{Transport: bearerTransport{secret: secret, next: http.DefaultTransport}}
		return caller{ermiteconnect.NewJournalServiceClient(hc, base), ermiteconnect.NewReleaseServiceClient(hc, base)}
	}
	owner := as(adminToken)
	reader := as(h.token([]domain.Scope{domain.ScopeJournalRead}))
	limited := as(h.token([]domain.Scope{domain.ScopeJournalRead}, "pub"))
	other := as(h.token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite}))
	anon := as("")
	// Fixed instants, far before and after the entries: no wall clock. A
	// since older than the retention returns what is kept.
	since := timestamppb.New(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	future := timestamppb.New(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC))

	list := func(c caller, req *pb.ListActivityRequest) ([]*pb.ActivityEntry, string, error) {
		res, err := c.journal.ListActivity(h.ctx, connect.NewRequest(req))
		if err != nil {
			return nil, "", err
		}
		return res.Msg.GetEntries(), res.Msg.GetNextPageToken(), nil
	}
	repos := func(es []*pb.ActivityEntry) map[string]int {
		out := map[string]int{}
		for _, e := range es {
			out[e.GetRepo()]++
		}
		return out
	}

	all, next, err := list(owner, &pb.ListActivityRequest{Since: since})
	require.NoError(t, err)
	assert.Empty(t, next)
	// Each repository: the first push of main, the tag, the push of a and b.
	assert.Equal(t, map[string]int{"pub": 3, "priv": 3}, repos(all))
	for i := 1; i < len(all); i++ {
		assert.False(t, all[i].GetAt().AsTime().After(all[i-1].GetAt().AsTime()), "newest first")
	}
	assert.Equal(t, pb.ActivityKind_ACTIVITY_KIND_PUSH, all[0].GetKind())
	assert.EqualValues(t, 2, all[0].GetCommitCount())

	got, _, err := list(reader, &pb.ListActivityRequest{Since: since})
	require.NoError(t, err)
	assert.Len(t, got, len(all), "journal:read sees every repository")
	got, _, err = list(limited, &pb.ListActivityRequest{Since: since})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"pub": 3}, repos(got), "a limited token sees its repositories only")
	priv := "priv"
	_, _, err = list(limited, &pb.ListActivityRequest{Since: since, Repo: &priv})
	assert.Equal(t, "not_found", codeName(err))
	_, _, err = list(other, &pb.ListActivityRequest{Since: since})
	assert.Equal(t, "permission_denied", codeName(err))
	_, _, err = list(anon, &pb.ListActivityRequest{Since: since})
	assert.Equal(t, "unauthenticated", codeName(err))

	// Kinds, paging and the arguments.
	got, _, err = list(owner, &pb.ListActivityRequest{Since: since, Kinds: []pb.ActivityKind{pb.ActivityKind_ACTIVITY_KIND_TAG_CREATED}})
	require.NoError(t, err)
	assert.Len(t, got, 2)
	var paged []*pb.ActivityEntry
	req := &pb.ListActivityRequest{Since: since, PageSize: 4}
	for range 5 {
		page, next, err := list(owner, req)
		require.NoError(t, err)
		paged = append(paged, page...)
		if next == "" {
			break
		}
		req.PageToken = next
	}
	require.Len(t, paged, len(all))
	for i := range all {
		assert.Equal(t, all[i].GetId(), paged[i].GetId())
	}
	for _, bad := range []*pb.ListActivityRequest{
		{},
		{Since: future},
		{Since: since, PageToken: "garbage"},
	} {
		_, _, err := list(owner, bad)
		assert.Equal(t, "invalid_argument", codeName(err))
	}
	// Unreleased: two commits after v0.1.0 in each repository.
	unreleased := func(c caller, repo *string) ([]*pb.UnreleasedRepo, error) {
		res, err := c.release.ListUnreleased(h.ctx, connect.NewRequest(&pb.ListUnreleasedRequest{Repo: repo}))
		if err != nil {
			return nil, err
		}
		return res.Msg.GetRepos(), nil
	}
	u, err := unreleased(owner, nil)
	require.NoError(t, err)
	require.Len(t, u, 2)
	assert.Equal(t, "priv", u[0].GetRepo())
	assert.Equal(t, "v0.1.0", u[1].GetLatestTag())
	assert.EqualValues(t, 2, u[1].GetCommitsSince())
	assert.Equal(t, "main", u[1].GetDefaultBranch())
	assert.NotNil(t, u[1].GetLatestTagAt())
	u, err = unreleased(limited, nil)
	require.NoError(t, err)
	require.Len(t, u, 1)
	assert.Equal(t, "pub", u[0].GetRepo())
	_, err = unreleased(limited, &priv)
	assert.Equal(t, "not_found", codeName(err))
	_, err = unreleased(other, nil)
	assert.Equal(t, "permission_denied", codeName(err))
	_, err = unreleased(anon, nil)
	assert.Equal(t, "unauthenticated", codeName(err))
}
