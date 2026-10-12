// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// branchToken creates a stored token limited to branch prefixes, as root.
func (h *harness) branchToken(scopes []domain.Scope, prefixes []string, repos ...string) (secret, id string) {
	h.t.Helper()
	p := ports.CreateTokenParams{Label: "claude", Scopes: scopes, BranchPrefixes: prefixes}
	for _, r := range repos {
		p.Repos = append(p.Repos, h.name(r))
	}
	got, err := h.forge.Tokens.Create(h.ctx, domain.Root(), p)
	require.NoError(h.t, err)
	return got.Secret, got.Token.ID
}

// AC5: a token limited to branch prefixes, through real git pushes and the API.
func TestAC5BranchLimitedToken(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "a.txt")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, out)
	mainID := h.bareGit("proj", "rev-parse", "refs/heads/main")

	secret, _ := h.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoWrite, domain.ScopeMRWrite, domain.ScopeMRMerge),
		[]string{"agent/claude/"}, "proj")
	remote := h.urlAuth(secret, "/proj.git")
	commit := func(file string) {
		require.NoError(t, os.WriteFile(filepath.Join(wt, file), []byte(file), 0o644))
		h.mustGit(wt, "add", file)
		h.mustGit(wt, "commit", "-q", "-m", file)
	}
	refs := func() string { return h.bareGit("proj", "for-each-ref", "--format=%(refname) %(objectname)") }

	// It pushes, force-pushes and deletes under its prefix.
	h.mustGit(wt, "checkout", "-q", "-b", "agent/claude/x")
	commit("x1")
	out, err = h.git(wt, "push", "-q", remote, "agent/claude/x")
	require.NoError(t, err, out)
	assert.Equal(t, h.mustGit(wt, "rev-parse", "HEAD")[:40], h.bareGit("proj", "rev-parse", "refs/heads/agent/claude/x"))

	h.mustGit(wt, "reset", "-q", "--hard", "main")
	commit("x2")
	out, err = h.git(wt, "push", "-q", "--force", remote, "agent/claude/x")
	require.NoError(t, err, out)
	assert.Equal(t, h.mustGit(wt, "rev-parse", "HEAD")[:40], h.bareGit("proj", "rev-parse", "refs/heads/agent/claude/x"))

	h.mustGit(wt, "branch", "agent/claude/deep/y")
	out, err = h.git(wt, "push", "-q", remote, "agent/claude/deep/y")
	require.NoError(t, err, out)
	out, err = h.git(wt, "push", "-q", remote, ":refs/heads/agent/claude/deep/y")
	require.NoError(t, err, out)
	_, err = h.git(h.repoDir("proj"), "rev-parse", "--verify", "-q", "refs/heads/agent/claude/deep/y")
	require.Error(t, err, "deleted")

	// Every other ref refuses the push, with a message naming it.
	before := refs()
	for _, tt := range []struct{ name, refspec, ref string }{
		{"main", "HEAD:refs/heads/main", "refs/heads/main"},
		{"force-push to main", "+HEAD:refs/heads/main", "refs/heads/main"},
		{"delete main", ":refs/heads/main", "refs/heads/main"},
		{"a new branch outside", "HEAD:refs/heads/other", "refs/heads/other"},
		{"the prefix without a name", "HEAD:refs/heads/agent/claude", "refs/heads/agent/claude"},
		{"a sibling sharing the text", "HEAD:refs/heads/agent/claudex/y", "refs/heads/agent/claudex/y"},
		{"a tag", "HEAD:refs/tags/v1", "refs/tags/v1"},
		{"a tag named like the prefix", "HEAD:refs/tags/agent/claude/v1", "refs/tags/agent/claude/v1"},
		{"a notes ref", "HEAD:refs/notes/commits", "refs/notes/commits"},
		{"a custom ref", "HEAD:refs/agent/claude/z", "refs/agent/claude/z"},
	} {
		out, err = h.git(wt, "push", remote, tt.refspec)
		require.Error(t, err, tt.name)
		assert.Contains(t, out, tt.ref+" is outside the branches this token may push (agent/claude/)", tt.name)
		assert.Equal(t, before, refs(), "%s: nothing moved", tt.name)
	}

	// A mixed push is refused as a whole: the allowed ref does not move either.
	commit("x3")
	out, err = h.git(wt, "push", "--atomic", remote, "HEAD:refs/heads/agent/claude/ok", "HEAD:refs/heads/main")
	require.Error(t, err)
	assert.Contains(t, out, "refs/heads/main is outside")
	assert.Equal(t, before, refs())
	out, err = h.git(wt, "push", remote, "HEAD:refs/heads/agent/claude/ok", "HEAD:refs/tags/v2")
	require.Error(t, err)
	assert.Contains(t, out, "refs/tags/v2 is outside")
	assert.Equal(t, before, refs(), "git receives both updates in one pack: pre-receive refuses both")
	assert.Equal(t, mainID, h.bareGit("proj", "rev-parse", "refs/heads/main"))

	// It cannot create a repository: not by push-to-create, not by the API.
	h.mustGit(wt, "checkout", "-q", "agent/claude/x")
	out, err = h.git(wt, "push", "-q", h.urlAuth(secret, "/brand-new.git"), "agent/claude/x")
	require.Error(t, err, out)
	assert.NoDirExists(t, h.repoDir("brand-new"))
	unlimited, _ := h.branchToken(scopes(domain.ScopeRepoWrite), []string{"agent/claude/"}) // not limited to a repository
	out, err = h.git(wt, "push", "-q", h.urlAuth(unlimited, "/brand-new.git"), "agent/claude/x")
	require.Error(t, err, out)
	assert.NoDirExists(t, h.repoDir("brand-new"))
	assert.Equal(t, 404, h.statusAuth("GET", "/brand-new.git/info/refs?service=git-receive-pack", unlimited))
	_, err = h.api(unlimited).repo.CreateRepo(h.ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "via-api"}))
	assert.Equal(t, "permission_denied", codeName(err))
	assert.NoDirExists(t, h.repoDir("via-api"))
	// The same token without prefixes does both.
	free := h.token(scopes(domain.ScopeRepoWrite))
	_, err = h.api(free).repo.CreateRepo(h.ctx, connect.NewRequest(&pb.CreateRepoRequest{Name: "via-api"}))
	require.NoError(t, err)

	// It cannot delete a repository, even with repo:admin.
	admin, _ := h.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeRepoAdmin), []string{"agent/claude/"}, "via-api")
	_, err = h.api(admin).repo.DeleteRepo(h.ctx, connect.NewRequest(&pb.DeleteRepoRequest{Repo: "via-api"}))
	assert.Equal(t, "permission_denied", codeName(err))
	assert.DirExists(t, h.repoDir("via-api"))
	// ... nor change its settings (default branch, visibility, ...).
	_, err = h.api(admin).repo.UpdateRepo(h.ctx, connect.NewRequest(&pb.UpdateRepoRequest{
		Repo: "via-api", Visibility: pb.Visibility_VISIBILITY_PUBLIC, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"visibility"}},
	}))
	assert.Equal(t, "permission_denied", codeName(err))
	got, err := h.api(adminToken).repo.GetRepo(h.ctx, connect.NewRequest(&pb.GetRepoRequest{Repo: "via-api"}))
	require.NoError(t, err)
	assert.Equal(t, pb.Visibility_VISIBILITY_PRIVATE, got.Msg.GetRepo().GetVisibility())

	// It never has owner rights, whatever its scopes.
	all, _ := h.branchToken(domain.AllScopes(), []string{"agent/claude/"})
	_, err = h.api(all).session.SignOutEverywhere(h.ctx, connect.NewRequest(&pb.SignOutEverywhereRequest{}))
	assert.Equal(t, "permission_denied", codeName(err))
	_, err = h.api(all).session.CreateLoginLink(h.ctx, connect.NewRequest(&pb.CreateLoginLinkRequest{}))
	assert.Equal(t, "permission_denied", codeName(err))
}

// AC5: merge requests write branches: a limited token merges only into its prefixes.
func TestAC5BranchLimitedMerge(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wt := h.workTree("main", "a.txt")
	out, err := h.git(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "main")
	require.NoError(t, err, out)
	h.mustGit(wt, "checkout", "-q", "-b", "agent/claude/feat")
	h.writeFile(wt, "f.txt", []byte("f\n"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/proj.git"), "agent/claude/feat")
	feat := h.bareGit("proj", "rev-parse", "refs/heads/agent/claude/feat")
	mainID := h.bareGit("proj", "rev-parse", "refs/heads/main")

	secret, _ := h.branchToken(scopes(domain.ScopeRepoRead, domain.ScopeMRWrite, domain.ScopeMRMerge), []string{"agent/claude/"}, "proj")
	c := h.api(secret)
	created, err := c.mr.CreateMergeRequest(h.ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{
		Repo: "proj", Title: "feat", SourceBranch: "agent/claude/feat",
	}))
	require.NoError(t, err, "opening a request writes no ref")
	n := created.Msg.GetMergeRequest().GetNumber()

	for _, del := range []bool{false, true} {
		_, err = c.mr.MergeMergeRequest(h.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{
			Repo: "proj", Number: n, HeadId: feat, DeleteSourceBranch: del,
		}))
		assert.Equal(t, "permission_denied", codeName(err), "into main, delete=%v", del)
	}
	assert.Equal(t, mainID, h.bareGit("proj", "rev-parse", "refs/heads/main"))
	assert.Equal(t, feat, h.bareGit("proj", "rev-parse", "refs/heads/agent/claude/feat"))

	// The merge goes through for a token without prefixes.
	m := h.api(h.token(scopes(domain.ScopeRepoRead, domain.ScopeMRMerge)))
	_, err = m.mr.MergeMergeRequest(h.ctx, connect.NewRequest(&pb.MergeMergeRequestRequest{Repo: "proj", Number: n, HeadId: feat}))
	require.NoError(t, err)
	assert.Equal(t, feat, h.bareGit("proj", "rev-parse", "refs/heads/main"))
}

// AC5: a limited token only creates tokens within its prefixes, over the API.
func TestAC5BranchLimitedTokenCreation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	secret, _ := h.branchToken(scopes(domain.ScopeTokenAdmin, domain.ScopeRepoWrite), []string{"agent/claude/"})
	c := h.api(secret)
	create := func(prefixes ...string) (*connect.Response[pb.CreateTokenResponse], error) {
		return c.token.CreateToken(h.ctx, connect.NewRequest(&pb.CreateTokenRequest{
			Label: "child", Scopes: []pb.Scope{pb.Scope_SCOPE_REPO_WRITE}, BranchPrefixes: prefixes,
		}))
	}
	_, err := create()
	assert.Equal(t, "permission_denied", codeName(err), "no prefixes")
	_, err = create("agent/")
	assert.Equal(t, "permission_denied", codeName(err), "wider")
	_, err = create("agent/claudex/")
	assert.Equal(t, "permission_denied", codeName(err), "sibling")
	_, err = create("agent")
	assert.Equal(t, "invalid_argument", codeName(err), "malformed")
	res, err := create("agent/claude/sub/")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent/claude/sub/"}, res.Msg.GetToken().GetBranchPrefixes())

	// The listing shows the prefixes.
	list, err := h.api(adminToken).token.ListTokens(h.ctx, connect.NewRequest(&pb.ListTokensRequest{}))
	require.NoError(t, err)
	var seen int
	for _, tk := range list.Msg.GetTokens() {
		if tk.GetLabel() == "child" {
			assert.Equal(t, []string{"agent/claude/sub/"}, tk.GetBranchPrefixes())
			seen++
		}
	}
	assert.Equal(t, 1, seen)
}
