// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/rpc"
	"bnema.dev/ermite/internal/clientlimit"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// principals of the access tables.
const (
	pAnon    = "anonymous"
	pRoot    = "root"
	pReader  = "reader"  // repo:read on every repository
	pLimited = "limited" // read, write, admin and token:admin on a list of repositories
	pNoScope = "noscope" // a token with no repository scope
	pBad     = "badtoken"
)

// accessRig is a harness with one caller per principal.
type accessRig struct {
	*harness
	who map[string]*clients
}

func newAccessRig(t *testing.T) *accessRig {
	t.Helper()
	h := newHarness(t, harnessOpts{trustedProxies: "127.0.0.0/8"})
	// The limited token may touch "mine" and the fresh repositories m1…m99.
	listed := []string{"mine"}
	for i := 1; i < 100; i++ {
		listed = append(listed, fmt.Sprintf("m%d", i))
	}
	r := &accessRig{harness: h, who: map[string]*clients{
		pAnon:   h.api(""),
		pRoot:   h.api(adminToken),
		pReader: h.api(h.token(scopes(domain.ScopeRepoRead))),
		pLimited: h.api(h.token(scopes(
			domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeRepoAdmin, domain.ScopeTokenAdmin), listed...)),
		pNoScope: h.api(h.token(scopes(domain.ScopeMRWrite))),
		pBad:     newDisposableClients(h.url("/-/api"), "ermite_"+strings.Repeat("x", 43)),
	}}
	return r
}

// every principal in a stable order.
var allPrincipals = []string{pAnon, pRoot, pReader, pLimited, pNoScope, pBad}

// TestListPathsRPC: the owner lists public and private repositories, an
// anonymous caller sees the private one as missing, and so is a bad revision.
func TestListPathsRPC(t *testing.T) {
	r := newAccessRig(t)
	r.seed("pub", domain.VisibilityPublic, "README.md", "docs/a.txt")
	r.seed("priv", domain.VisibilityPrivate, "README.md")
	ctx := r.ctx
	root, anon := r.who[pRoot], r.who[pAnon]

	list := func(c *clients, repo, rev string) (*pb.ListPathsResponse, error) {
		resp, err := c.git.ListPaths(ctx, connect.NewRequest(&pb.ListPathsRequest{Repo: repo, Revision: rev}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	pub, err := list(root, "pub", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md", "docs/a.txt"}, pub.GetPaths())
	assert.NotEmpty(t, pub.GetCommitId())
	assert.NotEmpty(t, pub.GetTreeId())
	assert.False(t, pub.GetTruncated())

	priv, err := list(root, "priv", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md"}, priv.GetPaths())

	_, err = list(anon, "priv", "")
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	_, err = list(root, "pub", "nope")
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

// TestAC2ReadAccess: every read RPC on a public repository, a private one and
// one inside the limited token's list.
func TestAC2ReadAccess(t *testing.T) {
	r := newAccessRig(t)
	r.seed("pub", domain.VisibilityPublic, "README.md", "docs/a.txt")
	r.seed("priv", domain.VisibilityPrivate, "README.md")
	r.seed("mine", domain.VisibilityPrivate, "README.md")
	ctx := r.ctx

	rpcs := map[string]func(c *clients, repo string) error{
		"GetRepo": func(c *clients, repo string) error {
			_, err := c.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: repo}))
			return err
		},
		"ListRefs": func(c *clients, repo string) error {
			_, err := c.git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: repo}))
			return err
		},
		"GetTree": func(c *clients, repo string) error {
			_, err := c.git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: repo}))
			return err
		},
		"ListPaths": func(c *clients, repo string) error {
			_, err := c.git.ListPaths(ctx, connect.NewRequest(&pb.ListPathsRequest{Repo: repo}))
			return err
		},
		"GetBlob": func(c *clients, repo string) error {
			_, err := readBlob(ctx, c, &pb.GetBlobRequest{Repo: repo, Path: "README.md"})
			return err
		},
		"ListCommits": func(c *clients, repo string) error {
			_, err := c.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: repo}))
			return err
		},
		"GetCommit": func(c *clients, repo string) error {
			_, err := c.git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: repo}))
			return err
		},
		"ResolveRevision": func(c *clients, repo string) error {
			_, err := c.git.ResolveRevision(ctx, connect.NewRequest(&pb.ResolveRevisionRequest{Repo: repo, Revision: "main"}))
			return err
		},
	}
	// want[principal] is the outcome for pub, priv, mine.
	want := map[string][3]string{
		pAnon:    {"ok", "not_found", "not_found"},
		pRoot:    {"ok", "ok", "ok"},
		pReader:  {"ok", "ok", "ok"},
		pLimited: {"ok", "not_found", "ok"},
		pNoScope: {"ok", "not_found", "not_found"},
		pBad:     {"unauthenticated", "unauthenticated", "unauthenticated"},
	}
	for rpcName, call := range rpcs {
		for _, who := range allPrincipals {
			for i, repo := range []string{"pub", "priv", "mine"} {
				err := call(r.who[who], repo)
				assert.Equal(t, want[who][i], codeName(err), "%s as %s on %s", rpcName, who, repo)
			}
		}
	}

	t.Run("a hidden repository looks like a missing one", func(t *testing.T) {
		for rpcName, call := range rpcs {
			hidden := call(r.who[pAnon], "priv")
			missing := call(r.who[pAnon], "nothing-here")
			require.Error(t, hidden, rpcName)
			assert.Equal(t, missing.Error(), hidden.Error(), rpcName)
		}
	})

	t.Run("ListRepos shows what the caller may read", func(t *testing.T) {
		names := func(who string) []string {
			resp, err := r.who[who].repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
			if err != nil {
				return []string{codeName(err)}
			}
			var out []string
			for _, repo := range resp.Msg.GetRepos() {
				out = append(out, repo.GetName())
			}
			return out
		}
		assert.Equal(t, []string{"pub"}, names(pAnon))
		assert.Equal(t, []string{"mine", "priv", "pub"}, names(pRoot))
		assert.Equal(t, []string{"mine", "priv", "pub"}, names(pReader))
		assert.Equal(t, []string{"mine", "pub"}, names(pLimited))
		assert.Equal(t, []string{"pub"}, names(pNoScope))
		assert.Equal(t, []string{"unauthenticated"}, names(pBad))
	})

	t.Run("GetVersion needs no authentication", func(t *testing.T) {
		for _, who := range allPrincipals {
			resp, err := r.who[who].server.GetVersion(ctx, connect.NewRequest(&pb.GetVersionRequest{}))
			require.NoError(t, err, who)
			assert.ElementsMatch(t, []string{"repo.update", "events.resume", "merge_requests", "merge_requests.delete_branch", "signing_key", "session", "git.list_paths", "token.branch_prefixes", "ci.runs", "ci.runs.filters", "ci.runners", "ci.actions", "ci.annotations", "ci.summaries", "ci.concurrency", "ci.dispatch", "ci.workflows", "ci.cache", "ci.secrets", "ci.artifacts", "commit_signatures"}, resp.Msg.GetFeatures(), who)
			assert.NotEmpty(t, resp.Msg.GetVersion())
		}
	})
}

// writeCell is one cell of the write table.
type writeCell struct {
	name string
	// vis is the visibility of the repository made for the cell; "" makes none.
	vis domain.Visibility
	// prefix is "m" for a repository inside the limited token's list, "x" outside.
	prefix string
	call   func(c *clients, repo string) error
	want   map[string]string
}

// TestAC2WriteAccess: the mutating RPCs for every principal.
func TestAC2WriteAccess(t *testing.T) {
	r := newAccessRig(t)
	ctx := r.ctx

	desc := func(c *clients, repo string) error {
		_, err := c.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
			Repo: repo, Description: "hello", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		}))
		return err
	}
	del := func(c *clients, repo string) error {
		_, err := c.repo.DeleteRepo(ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: repo}))
		return err
	}
	create := func(c *clients, repo string) error {
		_, err := c.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: repo}))
		return err
	}
	createToken := func(c *clients, _ string) error {
		_, err := c.token.CreateToken(ctx, connect.NewRequest(&pb.CreateTokenRequest{
			Label: "t", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"m1"},
		}))
		return err
	}
	listTokens := func(c *clients, _ string) error {
		_, err := c.token.ListTokens(ctx, connect.NewRequest(&pb.ListTokensRequest{}))
		return err
	}
	revoke := func(c *clients, _ string) error {
		_, err := c.token.RevokeToken(ctx, connect.NewRequest(&pb.RevokeTokenRequest{Id: "0000000000000000"}))
		return err
	}

	ok, nf, denied, unauth := "ok", "not_found", "permission_denied", "unauthenticated"
	cells := []writeCell{
		{"CreateRepo", "", "m", create, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: ok, pNoScope: denied, pBad: unauth}},
		{"CreateRepo outside the list", "", "x", create, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: denied, pNoScope: denied, pBad: unauth}},
		{"UpdateRepo private in list", domain.VisibilityPrivate, "m", desc, map[string]string{pAnon: nf, pRoot: ok, pReader: denied, pLimited: ok, pNoScope: nf, pBad: unauth}},
		{"UpdateRepo private outside the list", domain.VisibilityPrivate, "x", desc, map[string]string{pAnon: nf, pRoot: ok, pReader: denied, pLimited: nf, pNoScope: nf, pBad: unauth}},
		{"UpdateRepo public outside the list", domain.VisibilityPublic, "x", desc, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: denied, pNoScope: denied, pBad: unauth}},
		{"DeleteRepo private in list", domain.VisibilityPrivate, "m", del, map[string]string{pAnon: nf, pRoot: ok, pReader: denied, pLimited: ok, pNoScope: nf, pBad: unauth}},
		{"DeleteRepo private outside the list", domain.VisibilityPrivate, "x", del, map[string]string{pAnon: nf, pRoot: ok, pReader: denied, pLimited: nf, pNoScope: nf, pBad: unauth}},
		{"DeleteRepo public outside the list", domain.VisibilityPublic, "x", del, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: denied, pNoScope: denied, pBad: unauth}},
		{"CreateToken", "", "", createToken, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: ok, pNoScope: denied, pBad: unauth}},
		{"ListTokens", "", "", listTokens, map[string]string{pAnon: unauth, pRoot: ok, pReader: denied, pLimited: ok, pNoScope: denied, pBad: unauth}},
		{"RevokeToken", "", "", revoke, map[string]string{pAnon: unauth, pRoot: nf, pReader: denied, pLimited: nf, pNoScope: denied, pBad: unauth}},
	}
	counter := 0
	for _, cell := range cells {
		for _, who := range allPrincipals {
			counter++
			repo := fmt.Sprintf("%s%d", cell.prefix, counter%99+1)
			if cell.prefix == "x" {
				repo = fmt.Sprintf("x%d", counter)
			}
			if cell.vis != "" {
				_, err := r.forge.Repos.Create(ctx, domain.Root(), createParams(r.harness, repo, cell.vis))
				require.NoError(t, err)
			}
			err := cell.call(r.who[who], repo)
			assert.Equal(t, cell.want[who], codeName(err), "%s as %s on %s: %v", cell.name, who, repo, err)
		}
	}
}

// TestTokenRPCs: the secret is returned once, listed tokens never carry it,
// and a limited token cannot create a stronger one.
func TestTokenRPCs(t *testing.T) {
	r := newAccessRig(t)
	ctx := r.ctx
	root := r.who[pRoot]

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	created, err := root.token.CreateToken(ctx, connect.NewRequest(&pb.CreateTokenRequest{
		Label: "laptop", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ, pb.Scope_SCOPE_REPO_WRITE},
		Repos: []string{"a", "b"}, ExpiresAt: timestamppb.New(exp),
	}))
	require.NoError(t, err)
	tok := created.Msg.GetToken()
	assert.True(t, strings.HasPrefix(created.Msg.GetSecret(), "ermite_"))
	assert.Equal(t, "laptop", tok.GetLabel())
	assert.Equal(t, []pb.Scope{pb.Scope_SCOPE_REPO_READ, pb.Scope_SCOPE_REPO_WRITE}, tok.GetScopes())
	assert.Equal(t, []string{"a", "b"}, tok.GetRepos())
	assert.Equal(t, exp, tok.GetExpiresAt().AsTime())
	assert.NotNil(t, tok.GetCreatedAt())
	assert.Nil(t, tok.GetLastUsedAt())

	// The secret works.
	_, err = r.api(created.Msg.GetSecret()).repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	require.NoError(t, err)

	list, err := root.token.ListTokens(ctx, connect.NewRequest(&pb.ListTokensRequest{}))
	require.NoError(t, err)
	var found bool
	for _, tk := range list.Msg.GetTokens() {
		found = found || tk.GetId() == tok.GetId()
	}
	assert.True(t, found)
	assert.NotContains(t, list.Msg.String(), created.Msg.GetSecret())

	// Paging: one at a time, newest first, no repeats.
	seen := map[string]bool{}
	var pageToken string
	for range 20 {
		page, err := root.token.ListTokens(ctx, connect.NewRequest(&pb.ListTokensRequest{PageSize: 1, PageToken: pageToken}))
		require.NoError(t, err)
		for _, tk := range page.Msg.GetTokens() {
			assert.False(t, seen[tk.GetId()], "token listed twice")
			seen[tk.GetId()] = true
		}
		if pageToken = page.Msg.GetNextPageToken(); pageToken == "" {
			break
		}
	}
	assert.Len(t, seen, len(list.Msg.GetTokens()))
	_, err = root.token.ListTokens(ctx, connect.NewRequest(&pb.ListTokensRequest{PageToken: "garbage"}))
	assert.Equal(t, "invalid_argument", codeName(err))

	// No escalation: the limited token cannot grant more than it has.
	limited := r.who[pLimited]
	for name, req := range map[string]*pb.CreateTokenRequest{
		"a scope it lacks":     {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_CI_WRITE}, Repos: []string{"m1"}},
		"no repository list":   {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}},
		"a repository outside": {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"other"}},
	} {
		_, err := limited.token.CreateToken(ctx, connect.NewRequest(req))
		assert.Equal(t, "permission_denied", codeName(err), name)
	}
	// Invalid requests.
	for name, req := range map[string]*pb.CreateTokenRequest{
		"no scope":          {Label: "x"},
		"unspecified scope": {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_UNSPECIFIED}},
		"empty label":       {Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}},
		"reserved owner":    {Label: "owner", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}},
		"reserved root":     {Label: " Root ", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}},
		"expired":           {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, ExpiresAt: timestamppb.New(time.Now().Add(-time.Hour))},
		"bad repository":    {Label: "x", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_READ}, Repos: []string{"Bad Name"}},
	} {
		_, err := root.token.CreateToken(ctx, connect.NewRequest(req))
		assert.Equal(t, "invalid_argument", codeName(err), name)
	}

	// Revoke: the token stops working at once.
	_, err = root.token.RevokeToken(ctx, connect.NewRequest(&pb.RevokeTokenRequest{Id: tok.GetId()}))
	require.NoError(t, err)
	_, err = r.api(created.Msg.GetSecret()).repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	assert.Equal(t, "unauthenticated", codeName(err))
	_, err = root.token.RevokeToken(ctx, connect.NewRequest(&pb.RevokeTokenRequest{Id: tok.GetId()}))
	assert.Equal(t, "not_found", codeName(err))
}

// TestAuthFailuresAreCountedAndBlock: bad tokens on the API count toward the
// same budget as on the Git routes, and a blocked client gets
// resource_exhausted (HTTP 429) whatever it presents.
func TestAuthFailuresAreCountedAndBlock(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic, "README.md")
	bad := h.api("ermite_" + strings.Repeat("y", 43))
	req := func() *connect.Request[pb.GetRepoRequest] { return connect.NewRequest(&pb.GetRepoRequest{Repo: "pub"}) }

	for range clientlimit.MaxFailures {
		_, err := bad.repo.GetRepo(h.ctx, req())
		require.Equal(t, "unauthenticated", codeName(err))
		var ce *connect.Error
		require.ErrorAs(t, err, &ce)
	}
	_, err := bad.repo.GetRepo(h.ctx, req())
	assert.Equal(t, "resource_exhausted", codeName(err))
	var ce *connect.Error
	require.ErrorAs(t, err, &ce)
	assert.NotEmpty(t, ce.Meta().Get("Retry-After"))

	// Even a valid token and anonymous calls are refused while blocked ...
	_, err = h.api(adminToken).repo.GetRepo(h.ctx, req())
	assert.Equal(t, "resource_exhausted", codeName(err))
	_, err = h.api("").repo.GetRepo(h.ctx, req())
	assert.Equal(t, "resource_exhausted", codeName(err))
	// ... and so is the Git route: one budget per client.
	assert.Equal(t, http.StatusTooManyRequests, h.status("GET", "/pub.git/info/refs?service=git-upload-pack", nil))

	// The rejection is on the Connect protocol, with HTTP 429.
	httpReq, _ := http.NewRequest(http.MethodPost, h.url("/-/api/ermite.RepoService/GetRepo"), strings.NewReader(`{"repo":"pub"}`))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Contains(t, string(body), `"resource_exhausted"`)
}

// TestUnauthenticatedIsAConnectError: a bad token is a Connect error with a
// JSON body, not a bare HTTP 401 page; the secret never appears in the logs.
func TestUnauthenticatedIsAConnectError(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	secret := "ermite_" + strings.Repeat("z", 43)
	for _, hdr := range []string{"Bearer " + secret, "Basic dXNlcjpwYXNz", "Bearer", "token " + secret} {
		req, _ := http.NewRequest(http.MethodPost, h.url("/-/api/ermite.RepoService/ListRepos"), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", hdr)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, hdr)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"), hdr)
		assert.Contains(t, string(body), `"code":"unauthenticated"`, hdr)
	}
	assert.NotContains(t, h.logs.String(), secret)
}

// TestCodecsAndPaths: binary and JSON codecs, the mount point, and the
// request log with procedure and code.
func TestCodecsAndPaths(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic, "README.md")

	for name, opts := range map[string][]connect.ClientOption{
		"binary":      nil,
		"json":        {connect.WithProtoJSON()},
		"grpc-binary": {connect.WithGRPC()},
	} {
		c := h.api("", opts...)
		resp, err := c.repo.GetRepo(h.ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "pub"}))
		require.NoError(t, err, name)
		assert.Equal(t, "pub", resp.Msg.GetRepo().GetName(), name)
		assert.Equal(t, "main", resp.Msg.GetRepo().GetDefaultBranch(), name)
		assert.Equal(t, pb.Visibility_VISIBILITY_PUBLIC, resp.Msg.GetRepo().GetVisibility(), name)
		_, err = c.repo.GetRepo(h.ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "nothing"}))
		assert.Equal(t, "not_found", codeName(err), name)
	}

	// curl-style JSON, at the documented path.
	resp, err := http.Post(h.url("/-/api/ermite.RepoService/GetRepo"), "application/json", strings.NewReader(`{"repo":"pub"}`))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), `"name":"pub"`)
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"), "security headers apply to the API too")

	// Nothing outside the services.
	assert.Equal(t, 404, h.status("POST", "/-/api/ermite.RepoService/Nope", nil))
	assert.Equal(t, 404, h.status("POST", "/-/api/other.Service/Method", nil))

	logs := h.logs.String()
	assert.Contains(t, logs, `"procedure":"/ermite.RepoService/GetRepo"`)
	assert.Contains(t, logs, `"code":"not_found"`)
	assert.Contains(t, logs, `"code":"ok"`)
}

// TestRPCErrorCodes: validation errors are invalid_argument, not internal.
func TestRPCErrorCodes(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic, "README.md", "docs/a.txt")
	root := h.api(adminToken)
	ctx := h.ctx

	repoCases := map[string]func() error{
		"get uppercase name": func() error {
			_, err := root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "Pub"}))
			return err
		},
		"get empty name": func() error {
			_, err := root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{}))
			return err
		},
		"create invalid name": func() error {
			_, err := root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "No Good"}))
			return err
		},
		"create reserved name": func() error {
			_, err := root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "-"}))
			return err
		},
		"create with a .git suffix": func() error {
			_, err := root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "x.git"}))
			return err
		},
		"create multi-line description": func() error {
			_, err := root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "ok1", Description: "a\nb"}))
			return err
		},
		"list negative page size": func() error {
			_, err := root.repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{PageSize: -1}))
			return err
		},
		"list bad page token": func() error {
			_, err := root.repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{PageToken: "!!"}))
			return err
		},
		"refs unspecified kind": func() error {
			_, err := root.git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: "pub", Kinds: []pb.RefKind{pb.RefKind_REF_KIND_UNSPECIFIED}}))
			return err
		},
		"tree bad path": func() error {
			_, err := root.git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: "pub", Path: "a/../b"}))
			return err
		},
		"tree on a file": func() error {
			_, err := root.git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: "pub", Path: "README.md"}))
			return err
		},
		"blob on a directory": func() error {
			_, err := readBlob(ctx, root, &pb.GetBlobRequest{Repo: "pub", Path: "docs"})
			return err
		},
		"blob without a path": func() error {
			_, err := readBlob(ctx, root, &pb.GetBlobRequest{Repo: "pub"})
			return err
		},
		"commits bad revision": func() error {
			_, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "pub", Revision: "a..b"}))
			return err
		},
		"commits bad page token": func() error {
			_, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "pub", PageToken: "zz"}))
			return err
		},
		"update visibility unspecified": func() error {
			_, err := root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: "pub", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}}}))
			return err
		},
		"update without mask": func() error {
			_, err := root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: "pub", Description: "x"}))
			return err
		},
		"update unknown mask path": func() error {
			_, err := root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: "pub", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}}))
			return err
		},
		"watch unknown topic": func() error { return watchErr(ctx, root, "everything") },
		"watch bad repo name": func() error { return watchErr(ctx, root, "repo:Bad Name") },
	}
	for name, call := range repoCases {
		assert.Equal(t, "invalid_argument", codeName(call()), name)
	}

	notFound := map[string]func() error{
		"commits unknown revision": func() error {
			_, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "pub", Revision: "nope"}))
			return err
		},
		"resolve unknown revision": func() error {
			_, err := root.git.ResolveRevision(ctx, connect.NewRequest(&pb.ResolveRevisionRequest{Repo: "pub", Revision: "nope"}))
			return err
		},
		"tree unknown path": func() error {
			_, err := root.git.GetTree(ctx, connect.NewRequest(&pb.GetTreeRequest{Repo: "pub", Path: "nowhere"}))
			return err
		},
		"blob unknown path": func() error {
			_, err := readBlob(ctx, root, &pb.GetBlobRequest{Repo: "pub", Path: "nowhere.txt"})
			return err
		},
		"update unknown repository": func() error {
			_, err := root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: "ghost", Description: "x", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}))
			return err
		},
		"delete unknown repository": func() error {
			_, err := root.repo.DeleteRepo(ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "ghost"}))
			return err
		},
		"watch unknown repository": func() error { return watchErr(ctx, root, "repo:ghost") },
	}
	for name, call := range notFound {
		assert.Equal(t, "not_found", codeName(call()), name)
	}

	// An empty repository has no commit to resolve.
	h.seed("empty", domain.VisibilityPublic)
	_, err := root.git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: "empty"}))
	assert.Equal(t, "not_found", codeName(err))
	refs, err := root.git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: "empty"}))
	require.NoError(t, err)
	require.Len(t, refs.Msg.GetRefs(), 1, "HEAD of an unborn branch is listed")

	_, err = root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "pub"}))
	assert.Equal(t, "already_exists", codeName(err))

	_, err = root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "big", Description: strings.Repeat("x", 2<<20)}))
	assert.Equal(t, "resource_exhausted", codeName(err), "a request over the size limit")

	// A page past the end is empty, not an error.
	page, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "pub", PageSize: 1}))
	require.NoError(t, err)
	require.Len(t, page.Msg.GetCommits(), 1)
	require.NotEmpty(t, page.Msg.GetNextPageToken())
	page2, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "pub", PageSize: 1, PageToken: page.Msg.GetNextPageToken()}))
	require.NoError(t, err)
	require.Len(t, page2.Msg.GetCommits(), 1)
	assert.NotEqual(t, page.Msg.GetCommits()[0].GetId(), page2.Msg.GetCommits()[0].GetId())
	assert.Empty(t, page2.Msg.GetNextPageToken())
}

// watchErr opens a Watch and returns the error it ends with.
func watchErr(ctx context.Context, c *clients, topics ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.event.Watch(ctx, connect.NewRequest(&pb.WatchRequest{Topics: topics}))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	for st.Receive() {
	}
	return st.Err()
}

// TestGetBlobStreamsInChunks: chunks of at most 64 KiB, metadata in the first
// message only, content intact, binary detected.
func TestGetBlobStreamsInChunks(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.forge.Repos.Create(h.ctx, domain.Root(), createParams(h, "blobs", domain.VisibilityPublic))
	require.NoError(t, err)

	big := make([]byte, 3*rpc.BlobChunkSize+1234)
	_, err = rand.Read(big)
	require.NoError(t, err)
	big[10] = 0                                                  // a NUL in the first bytes: binary
	huge := bytes.Repeat([]byte("line of text\n"), (1<<20)/13+1) // 1 MiB+: past the in-memory limit of gitcli
	exact := bytes.Repeat([]byte("e"), rpc.BlobChunkSize)

	wt := t.TempDir()
	h.mustGit(wt, "init", "-q", "--initial-branch=main")
	h.writeFile(wt, "big.bin", big)
	h.writeFile(wt, "huge.txt", huge)
	h.writeFile(wt, "small.txt", []byte("hello\n"))
	h.writeFile(wt, "empty.txt", nil)
	h.writeFile(wt, "exact.txt", exact)
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/blobs.git"), "main")

	c := h.api("")
	for _, tc := range []struct {
		path   string
		want   []byte
		binary bool
		media  string
	}{
		{"big.bin", big, true, "application/octet-stream"},
		{"huge.txt", huge, false, "text/plain"},
		{"small.txt", []byte("hello\n"), false, "text/plain"},
		{"empty.txt", nil, false, ""},
		{"exact.txt", exact, false, "text/plain"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			msgs, err := readBlob(h.ctx, c, &pb.GetBlobRequest{Repo: "blobs", Path: tc.path})
			require.NoError(t, err)
			require.NotEmpty(t, msgs)
			assert.NotNil(t, msgs[0].GetInfo(), "metadata in the first message")
			var got []byte
			for i, m := range msgs {
				if i > 0 {
					assert.Nil(t, m.GetInfo(), "metadata only in the first message")
					assert.NotEmpty(t, m.GetChunk(), "no empty chunk after the first message")
				}
				assert.LessOrEqual(t, len(m.GetChunk()), rpc.BlobChunkSize)
				got = append(got, m.GetChunk()...)
			}
			assert.True(t, bytes.Equal(tc.want, got), "content differs (%d bytes, want %d)", len(got), len(tc.want))
			info := msgs[0].GetInfo()
			assert.Equal(t, int64(len(tc.want)), info.GetSize())
			assert.Equal(t, tc.binary, info.GetBinary())
			assert.Equal(t, tc.path, info.GetPath())
			assert.EqualValues(t, 0o100644, info.GetMode())
			assert.Len(t, info.GetId(), 40)
			if tc.media != "" {
				assert.Contains(t, info.GetMediaType(), tc.media)
			}
			if len(tc.want) > rpc.BlobChunkSize {
				assert.GreaterOrEqual(t, len(msgs), len(tc.want)/rpc.BlobChunkSize)
			}
		})
	}
}

func createParams(h *harness, name string, vis domain.Visibility) ports.CreateRepoParams {
	return ports.CreateRepoParams{Name: h.name(name), Visibility: vis}
}

// TestUpdateRepoDefaultBranch: HEAD moves with git symbolic-ref once the
// branch is known to exist.
func TestUpdateRepoDefaultBranch(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("proj", domain.VisibilityPublic, "README.md")
	wt := h.workTree("dev", "dev.txt")
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "dev")
	root := h.api(adminToken)
	ctx := h.ctx
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

	got, err := root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Equal(t, "main", got.Msg.GetRepo().GetDefaultBranch())

	// A missing branch changes nothing, even next to a valid field.
	_, err = root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", DefaultBranch: "nope", Description: "never stored", UpdateMask: mask("default_branch", "description"),
	}))
	assert.Equal(t, "failed_precondition", codeName(err))
	assert.Equal(t, "refs/heads/main", h.bareGit("proj", "symbolic-ref", "HEAD"))
	got, err = root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "proj"}))
	require.NoError(t, err)
	assert.Empty(t, got.Msg.GetRepo().GetDescription())

	// A tag is not a branch.
	h.bareGit("proj", "tag", "v1", "main")
	_, err = root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{Repo: "proj", DefaultBranch: "v1", UpdateMask: mask("default_branch")}))
	assert.Equal(t, "failed_precondition", codeName(err))

	resp, err := root.repo.UpdateRepo(ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "proj", DefaultBranch: "dev", Description: "the project", Visibility: pb.Visibility_VISIBILITY_PRIVATE,
		UpdateMask: mask("default_branch", "description", "visibility"),
	}))
	require.NoError(t, err)
	assert.Equal(t, "dev", resp.Msg.GetRepo().GetDefaultBranch())
	assert.Equal(t, "the project", resp.Msg.GetRepo().GetDescription())
	assert.Equal(t, pb.Visibility_VISIBILITY_PRIVATE, resp.Msg.GetRepo().GetVisibility())
	assert.Equal(t, "refs/heads/dev", h.bareGit("proj", "symbolic-ref", "HEAD"))

	// Reads follow HEAD, and the repository is now hidden from anonymous.
	refs, err := root.git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: "proj", Kinds: []pb.RefKind{pb.RefKind_REF_KIND_HEAD}}))
	require.NoError(t, err)
	require.Len(t, refs.Msg.GetRefs(), 1)
	assert.Equal(t, "dev", refs.Msg.GetRefs()[0].GetShortName())
	_, err = h.api("").repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "proj"}))
	assert.Equal(t, "not_found", codeName(err))

	// A clone gets the new default branch.
	dir := t.TempDir()
	h.mustGit(dir, "clone", "-q", h.urlAuth(adminToken, "/proj.git"), "c")
	assert.Equal(t, "dev", strings.TrimSpace(h.mustGit(filepath.Join(dir, "c"), "rev-parse", "--abbrev-ref", "HEAD")))
}

// TestDeleteRepoMovesToTrash: the directory goes to trash/<name>-<unix>.git,
// the metadata goes, the name can be reused, and nothing of the old
// repository leaks into the new one.
func TestDeleteRepoMovesToTrash(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("doomed", domain.VisibilityPrivate, "README.md")
	root := h.api(adminToken)
	ctx := h.ctx
	oldHead := h.bareGit("doomed", "rev-parse", "HEAD")

	// Warm the cat-file process of the old directory.
	_, err := root.git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: "doomed"}))
	require.NoError(t, err)

	before := time.Now().Unix()
	_, err = root.repo.DeleteRepo(ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "doomed"}))
	require.NoError(t, err)

	assert.NoDirExists(t, h.repoDir("doomed"))
	matches, err := filepath.Glob(filepath.Join(h.data, "trash", "doomed-*.git"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	var unix int64
	_, err = fmt.Sscanf(filepath.Base(matches[0]), "doomed-%d.git", &unix)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, unix, before)
	assert.Equal(t, oldHead, strings.TrimSpace(h.mustGit(matches[0], "rev-parse", "HEAD")), "the data is recoverable by hand")

	for name, call := range map[string]func() error{
		"get": func() error {
			_, err := root.repo.GetRepo(ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "doomed"}))
			return err
		},
		"refs": func() error {
			_, err := root.git.ListRefs(ctx, connect.NewRequest(&pb.ListRefsRequest{Repo: "doomed"}))
			return err
		},
		"delete again": func() error {
			_, err := root.repo.DeleteRepo(ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "doomed"}))
			return err
		},
	} {
		assert.Equal(t, "not_found", codeName(call()), name)
	}
	list, err := root.repo.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	require.NoError(t, err)
	assert.Empty(t, list.Msg.GetRepos())

	// The name is free again, and the new repository is empty.
	_, err = root.repo.CreateRepo(ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "doomed"}))
	require.NoError(t, err)
	_, err = root.git.GetCommit(ctx, connect.NewRequest(&pb.GetCommitRequest{Repo: "doomed"}))
	assert.Equal(t, "not_found", codeName(err), "no commit of the deleted repository survives in a reader process")
}

// A principal in the base context of the HTTP server does not authenticate
// requests: requests from the network are judged by their headers.
func TestNetworkRequestsIgnoreAPrincipalInTheContext(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.seed("pub", domain.VisibilityPublic, "README.md")
	h.seed("priv", domain.VisibilityPrivate, "README.md")

	srv := httptest.NewUnstartedServer(h.forge.API.Handler())
	srv.Config.BaseContext = func(net.Listener) context.Context {
		return domain.ContextWithPrincipal(h.ctx, domain.Root())
	}
	srv.Start()
	t.Cleanup(srv.Close)

	list := func(secret string) []string {
		c := newClients(srv.URL, secret)
		resp, err := c.repo.ListRepos(h.ctx, connect.NewRequest(&pb.ListReposRequest{}))
		require.NoError(t, err)
		var out []string
		for _, r := range resp.Msg.GetRepos() {
			out = append(out, r.GetName())
		}
		return out
	}
	assert.Equal(t, []string{"pub"}, list(""), "anonymous by its headers, not root by its context")
	assert.Equal(t, []string{"priv", "pub"}, list(adminToken))
	_, err := newClients(srv.URL, "").repo.CreateRepo(h.ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "nope"}))
	assert.Equal(t, "unauthenticated", codeName(err))
}
