// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/domain"
)

func needTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is needed", name)
		}
		t.Skipf("%s not installed", name)
	}
}

// ADR 0030 end to end: commits pushed signed with SSH and OpenPGP keys show
// their protocol and state over the API and on the web, and trusting a key
// turns them verified.
func TestCommitSignaturesEndToEnd(t *testing.T) {
	needTool(t, "ssh-keygen")
	needTool(t, "gpg")
	h := newHarness(t, harnessOpts{})
	ctx := h.ctx
	h.seed("sig", domain.VisibilityPublic)
	wt := h.workTree("main")
	keys := t.TempDir()

	// SSH key and commit.
	sshKey := filepath.Join(keys, "id")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "c", "-f", sshKey).CombinedOutput()
	require.NoError(t, err, string(out))
	h.writeFile(wt, "a.txt", []byte("a\n"))
	h.mustGit(wt, "-c", "gpg.format=ssh", "-c", "user.signingkey="+sshKey+".pub", "commit", "-q", "--amend", "-S", "--no-edit")
	sshCommit := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))

	// OpenPGP key and commit.
	// A short home: the agent socket path must fit sun_path.
	gnupg, err := os.MkdirTemp("", "gpg")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", gnupg, "--kill", "all").Run()
		_ = os.RemoveAll(gnupg)
	})
	gpg := func(args ...string) string {
		cmd := exec.Command("gpg", append([]string{"--batch", "--homedir", gnupg, "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
		b, err := cmd.CombinedOutput()
		require.NoError(t, err, string(b))
		return string(b)
	}
	gpg("--quick-gen-key", "T <t@example.test>", "ed25519", "sign", "never")
	wrapper := filepath.Join(keys, "gpg.sh")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nexec gpg --homedir '"+gnupg+"' --batch --pinentry-mode loopback --passphrase '' \"$@\"\n"), 0o700))
	h.writeFile(wt, "b.txt", []byte("b\n"))
	h.mustGit(wt, "-c", "gpg.program="+wrapper, "-c", "user.signingkey=t@example.test", "commit", "-q", "--amend", "-S", "--no-edit")
	gpgCommit := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))
	h.writeFile(wt, "c.txt", []byte("c\n"))
	unsigned := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/sig.git"), "main")

	root := h.api(adminToken)
	trust := ermiteconnect.NewTrustedKeyServiceClient(&http.Client{Transport: bearerTransport{secret: adminToken, next: http.DefaultTransport}}, h.url("/-/api"))
	states := func() map[string]*pb.CommitVerification {
		res, err := root.git.ListCommits(ctx, connect.NewRequest(&pb.ListCommitsRequest{Repo: "sig"}))
		require.NoError(t, err)
		m := map[string]*pb.CommitVerification{}
		for _, c := range res.Msg.GetCommits() {
			m[c.GetId()] = c.GetVerification()
		}
		return m
	}

	v, err := root.server.GetVersion(ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, v.Msg.GetFeatures(), "commit_signatures")

	s := states()
	assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_SSH, s[sshCommit].GetFormat())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNKNOWN_KEY, s[sshCommit].GetStatus())
	assert.Equal(t, pb.SignatureFormat_SIGNATURE_FORMAT_GPG, s[gpgCommit].GetFormat())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNKNOWN_KEY, s[gpgCommit].GetStatus())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNSIGNED, s[unsigned].GetStatus())

	// Trust both keys.
	pub, err := os.ReadFile(sshKey + ".pub")
	require.NoError(t, err)
	_, err = trust.AddTrustedKey(ctx, connect.NewRequest(&pb.AddTrustedKeyRequest{
		Format: pb.SignatureFormat_SIGNATURE_FORMAT_SSH, Title: "laptop", PublicKey: string(pub), Emails: []string{"T@example.test"},
	}))
	require.NoError(t, err)
	_, err = trust.AddTrustedKey(ctx, connect.NewRequest(&pb.AddTrustedKeyRequest{
		Format: pb.SignatureFormat_SIGNATURE_FORMAT_GPG, Title: "gpg", PublicKey: gpg("--armor", "--export", "t@example.test"), Emails: []string{"t@example.test"},
	}))
	require.NoError(t, err)

	s = states()
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, s[sshCommit].GetStatus())
	assert.Equal(t, "laptop", s[sshCommit].GetKeyTitle())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, s[gpgCommit].GetStatus())

	list, err := trust.ListTrustedKeys(ctx, connect.NewRequest(&pb.ListTrustedKeysRequest{}))
	require.NoError(t, err)
	require.Len(t, list.Msg.GetKeys(), 3)
	assert.True(t, list.Msg.GetKeys()[0].GetBuiltin(), "the instance key comes first")

	// A token without owner rights cannot change the list.
	weak := ermiteconnect.NewTrustedKeyServiceClient(&http.Client{Transport: bearerTransport{
		secret: h.token(scopes(domain.ScopeRepoRead)), next: http.DefaultTransport,
	}}, h.url("/-/api"))
	_, err = weak.RemoveTrustedKey(ctx, connect.NewRequest(&pb.RemoveTrustedKeyRequest{Id: list.Msg.GetKeys()[1].GetId()}))
	assert.Equal(t, "permission_denied", codeName(err))

	// The web shows the protocol and the state.
	code, _, body := h.get("/sig/commit/" + sshCommit)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "sig-ok")
	assert.Contains(t, body, "SSH")
	code, _, body = h.get("/sig/commit/" + gpgCommit)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "GPG")
	assert.Contains(t, body, "sig-ok")

	// A merge request lists its commits with their signature state, over the
	// API and on its page.
	h.mustGit(wt, "checkout", "-q", "-b", "feat")
	h.writeFile(wt, "d.txt", []byte("d\n"))
	h.mustGit(wt, "-c", "gpg.format=ssh", "-c", "user.signingkey="+sshKey+".pub", "commit", "-q", "--amend", "-S", "--no-edit")
	feat := strings.TrimSpace(h.mustGit(wt, "rev-parse", "HEAD"))
	h.mustGit(wt, "push", "-q", h.urlAuth(adminToken, "/sig.git"), "feat")
	_, err = root.mr.CreateMergeRequest(ctx, connect.NewRequest(&pb.CreateMergeRequestRequest{Repo: "sig", Title: "d", SourceBranch: "feat"}))
	require.NoError(t, err)
	mr, err := root.mr.GetMergeRequest(ctx, connect.NewRequest(&pb.GetMergeRequestRequest{Repo: "sig", Number: 1}))
	require.NoError(t, err)
	require.Len(t, mr.Msg.GetCommits(), 1)
	assert.Equal(t, feat, mr.Msg.GetCommits()[0].GetId())
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED, mr.Msg.GetCommits()[0].GetVerification().GetStatus())
	code, _, body = h.get("/sig/merge-requests/1")
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `<span class="sig-ico sig-ok"`)

	// Removing the key turns the commit unknown again.
	_, err = trust.RemoveTrustedKey(ctx, connect.NewRequest(&pb.RemoveTrustedKeyRequest{Id: list.Msg.GetKeys()[1].GetId()}))
	require.NoError(t, err)
	assert.Equal(t, pb.SignatureStatus_SIGNATURE_STATUS_UNKNOWN_KEY, states()[sshCommit].GetStatus())
}
