// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

type mrOut struct {
	Repo           string  `json:"repo"`
	URL            string  `json:"url"`
	Number         int64   `json:"number"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	SourceBranch   string  `json:"source_branch"`
	TargetBranch   string  `json:"target_branch"`
	State          string  `json:"state"`
	Author         string  `json:"author"`
	CreatedAt      *string `json:"created_at"`
	UpdatedAt      *string `json:"updated_at"`
	HeadID         string  `json:"head_id"`
	MergedCommitID string  `json:"merged_commit_id"`
	MergeMode      string  `json:"merge_mode"`
}

var mrKeys = []string{"author", "auto_merge", "auto_merged", "created_at", "description", "head_id", "merge_mode", "merged_commit_id", "number", "repo", "source_branch", "state", "target_branch", "title", "updated_at", "url"}

type commentOut struct {
	ID       string `json:"id"`
	Author   string `json:"author"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Side     string `json:"side"`
	CommitID string `json:"commit_id"`
	Text     string `json:"text"`
	Resolved bool   `json:"resolved"`
}

type showOut struct {
	mrOut
	Commits          []struct{ ID, Author, Summary string }
	CommitsTruncated bool   `json:"commits_truncated"`
	DiffBaseID       string `json:"diff_base_id"`
	Stats            struct {
		FilesChanged int `json:"files_changed"`
		Additions    int
		Deletions    int
	}
	Files []struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
		Status  string
		Hunks   []struct {
			Lines []struct {
				Kind      string
				OldNumber int `json:"old_number"`
				NewNumber int `json:"new_number"`
				Text      string
			}
		}
	}
	Comments     []commentOut
	LastReadID   string `json:"last_read_id"`
	LastReadLost bool   `json:"last_read_lost"`
}

// AC3 to AC6 through the CLI against a real forge, with branches pushed by git.
func TestMergeRequestsCLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	stdout, stderr, code := c.bin(t, adminToken, "token", "create", "daily", "--preset", "cli")
	require.Equal(t, 0, code, stderr)
	secret := strings.TrimSpace(stdout)
	agent := strings.TrimSpace(func() string {
		out, errOut, code := c.bin(t, adminToken, "token", "create", "agent-7", "--preset", "agent")
		require.Equal(t, 0, code, errOut)
		return out
	}())

	c.cli(t, secret, "repo", "create", "proj", "--visibility", "public").ok(t)
	wt := c.workTree("main", "README.md")
	url := c.urlAuth(secret, "/proj.git")
	c.mustGit(wt, "push", "-q", url, "main")
	main0 := c.bareGit("proj", "rev-parse", "main")

	c.mustGit(wt, "checkout", "-q", "-b", "feat/search")
	c.writeFile(wt, "search.go", []byte("package search\n\nfunc Find() {}\n"))
	c.writeFile(wt, "README.md", []byte("# proj\nsearch\n"))
	c.mustGit(wt, "push", "-q", url, "feat/search")
	head1 := c.bareGit("proj", "rev-parse", "feat/search")

	w := c.startWatch(t, secret)

	// create: needs mr:write; the agent token has it.
	res := c.cli(t, agent, "mr", "create", "proj", "--source", "feat/search", "--title", "Add search", "--description", "Finds things.\nSecond line.", "--json").ok(t)
	assert.Equal(t, mrKeys, keys(t, res.stdout))
	mr := decode[mrOut](t, res.stdout)
	assert.EqualValues(t, 1, mr.Number)
	assert.Equal(t, "open", mr.State)
	assert.Equal(t, "agent-7", mr.Author)
	assert.Equal(t, "main", mr.TargetBranch, "defaults to the default branch")
	assert.Equal(t, head1, mr.HeadID)
	assert.Empty(t, mr.MergedCommitID)
	assert.Empty(t, mr.MergeMode)
	ev := decode[eventOut](t, w.next(t, 10*time.Second))
	assert.Equal(t, "merge_request_opened", ev.Type)
	assert.Equal(t, "proj", ev.Repo)

	// The human output is one line.
	res = c.cli(t, agent, "mr", "create", "proj", "--source", "feat/search", "--target", "main", "--title", "dup")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "already exists")

	// Rules: every offending field in one message.
	_, stderr, code = c.bin(t, agent, "mr", "create", "proj", "--source", "nope", "--title", "bad\u202etitle")
	requireOneLineError(t, stderr, code, "invalid request")
	_, stderr, code = c.bin(t, agent, "mr", "create", "proj", "--source", "feat/search", "--target", "feat/search", "--title", "t")
	requireOneLineError(t, stderr, code, "invalid request")
	// Missing flags are usage errors that do not reach the server.
	res = c.cli(t, agent, "mr", "create", "proj", "--source", "feat/search")
	require.Error(t, res.err)

	// list.
	res = c.cli(t, "", "mr", "list", "proj", "--json").ok(t)
	list := decode[[]mrOut](t, res.stdout)
	require.Len(t, list, 1, "public: anonymous lists")
	assert.Equal(t, "Add search", list[0].Title)
	res = c.cli(t, "", "mr", "list", "proj").ok(t)
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 2)
	assert.Regexp(t, `^NUMBER\s+STATE\s+BRANCHES\s+AUTHOR\s+UPDATED\s+TITLE$`, lines[0])
	assert.Regexp(t, `^!1\s+open\s+feat/search -> main\s+agent-7\s+\d{4}-\d{2}-\d{2} \d{2}:\d{2}\s+Add search$`, lines[1])
	res = c.cli(t, "", "mr", "list", "proj", "--state", "merged", "--json").ok(t)
	assert.Equal(t, "[]\n", res.stdout)
	res = c.cli(t, "", "mr", "list", "proj", "--state", "merged").ok(t)
	assert.Equal(t, "ermite: no merge requests\n", res.stderr)
	res = c.cli(t, "", "mr", "list", "proj", "--state", "nonsense")
	require.Error(t, res.err)

	// show: the line numbers of the diff are what comment takes.
	res = c.cli(t, "", "mr", "show", "proj", "1", "--diff", "--json").ok(t)
	show := decode[showOut](t, res.stdout)
	assert.Equal(t, head1, show.HeadID)
	assert.Equal(t, main0, show.DiffBaseID)
	require.Len(t, show.Commits, 2)
	assert.Equal(t, 2, show.Stats.FilesChanged)
	paths := map[string]bool{}
	for _, f := range show.Files {
		paths[f.NewPath] = true
	}
	assert.Equal(t, map[string]bool{"README.md": true, "search.go": true}, paths)
	res = c.cli(t, "", "mr", "show", "proj", "!1", "--diff").ok(t)
	assert.Contains(t, res.stdout, "Add search")
	assert.Contains(t, res.stdout, "feat/search -> main")
	assert.Contains(t, res.stdout, "+package search")
	assert.Contains(t, res.stdout, "commits (2)")
	_, stderr, code = c.bin(t, "", "mr", "show", "proj", "7")
	requireOneLineError(t, stderr, code, "not found")

	// comment: general, on a line, and on a line that is not in the diff.
	res = c.cli(t, agent, "mr", "comment", "proj", "1", "-m", "Looks fine overall.", "--json").ok(t)
	general := decode[commentOut](t, res.stdout)
	assert.Equal(t, "1", general.ID)
	assert.Empty(t, general.Path)
	assert.Equal(t, "agent-7", general.Author)
	assert.Equal(t, "comment_added", decode[eventOut](t, w.next(t, 10*time.Second)).Type)
	res = c.cli(t, agent, "mr", "comment", "proj", "1", "--path", "search.go", "--line", "3", "-m", "Why empty?", "--json").ok(t)
	online := decode[commentOut](t, res.stdout)
	assert.Equal(t, "search.go", online.Path)
	assert.Equal(t, 3, online.Line)
	assert.Equal(t, "new", online.Side)
	assert.Equal(t, head1, online.CommitID)
	w.next(t, 10*time.Second)
	_, stderr, code = c.bin(t, agent, "mr", "comment", "proj", "1", "--path", "search.go", "--line", "99", "-m", "nowhere")
	requireOneLineError(t, stderr, code, "invalid request", "search.go:99")
	for _, args := range [][]string{
		{"--path", "search.go", "-m", "x"},                                // no line
		{"--line", "3", "-m", "x"},                                        // no path
		{"--path", "search.go", "--line", "3", "--side", "up", "-m", "x"}, // bad side
	} {
		res = c.cli(t, agent, append([]string{"mr", "comment", "proj", "1"}, args...)...)
		require.Error(t, res.err, "%v", args)
	}
	// stdin.
	res = runCLI(t, c.vars(agent), "from stdin\nwith two lines\n", "mr", "comment", "proj", "1", "-m", "-", "--json").ok(t)
	assert.Equal(t, "from stdin\nwith two lines", decode[commentOut](t, res.stdout).Text)
	w.next(t, 10*time.Second)
	// An agent cannot merge.
	_, stderr, code = c.bin(t, agent, "mr", "merge", "proj", "1", "--head", head1)
	requireOneLineError(t, stderr, code, "permission denied")

	// resolve, twice, and undone.
	res = c.cli(t, agent, "mr", "resolve", "proj", "1", "2", "--json").ok(t)
	assert.True(t, decode[commentOut](t, res.stdout).Resolved)
	res = c.cli(t, agent, "mr", "resolve", "proj", "1", "2", "--json").ok(t)
	assert.True(t, decode[commentOut](t, res.stdout).Resolved)
	res = c.cli(t, agent, "mr", "resolve", "proj", "1", "2", "--undo", "--json").ok(t)
	assert.False(t, decode[commentOut](t, res.stdout).Resolved)
	_, stderr, code = c.bin(t, agent, "mr", "resolve", "proj", "1", "42")
	requireOneLineError(t, stderr, code, "not found")

	// since-last-read: mark, push more, then only the new change.
	c.cli(t, agent, "mr", "show", "proj", "1", "--mark-read", "--json").ok(t)
	c.writeFile(wt, "extra.txt", []byte("extra\n"))
	c.mustGit(wt, "push", "-q", url, "feat/search")
	head2 := c.bareGit("proj", "rev-parse", "feat/search")
	res = c.cli(t, "", "mr", "show", "proj", "1", "--since-last-read", "--json").ok(t)
	show = decode[showOut](t, res.stdout)
	assert.Equal(t, head1, show.LastReadID)
	assert.Equal(t, head1, show.DiffBaseID)
	require.Len(t, show.Files, 1)
	assert.Equal(t, "extra.txt", show.Files[0].NewPath)
	assert.False(t, show.LastReadLost)
	// A force-push that drops the recorded commit: pruned, the whole change is shown, and said.
	c.mustGit(wt, "reset", "-q", "--hard", main0)
	c.writeFile(wt, "other.txt", []byte("o\n"))
	c.mustGit(wt, "push", "-q", "--force", url, "feat/search")
	// Push refreshes the pull refs asynchronously. Reconcile them synchronously
	// before GC, or the old pull head/merge can still retain the read commit.
	require.NoError(t, c.forge.CI.RescanPulls(c.ctx, c.name("proj")))
	require.NotContains(t, strings.Fields(c.bareGit("proj", "rev-list", "--all")), head1)
	c.bareGit("proj", "reflog", "expire", "--expire=now", "--all")
	c.bareGit("proj", "gc", "-q", "--prune=now")
	res = c.cli(t, "", "mr", "show", "proj", "1", "--since-last-read", "--json").ok(t)
	show = decode[showOut](t, res.stdout)
	assert.True(t, show.LastReadLost)
	assert.Equal(t, main0, show.DiffBaseID, "the whole change")
	assert.Contains(t, res.stderr, "head you last read is gone", "the reader is told")
	head3 := c.bareGit("proj", "rev-parse", "feat/search")
	_ = head2

	// Merging a stale head fails and says to read again.
	_, stderr, code = c.bin(t, secret, "mr", "merge", "proj", "1", "--head", head1)
	requireOneLineError(t, stderr, code, "precondition failed", "branch moved")
	assert.Equal(t, main0, c.bareGit("proj", "rev-parse", "main"))

	// Fast-forward with the reviewed head.
	res = c.cli(t, secret, "mr", "merge", "proj", "1", "--head", head3, "--json").ok(t)
	merged := decode[mrOut](t, res.stdout)
	assert.Equal(t, "merged", merged.State)
	assert.Equal(t, "fast-forward", merged.MergeMode)
	assert.Equal(t, head3, merged.MergedCommitID)
	assert.Equal(t, head3, c.bareGit("proj", "rev-parse", "main"))
	mev := w.nextOfType(t, "merge_request_merged")
	require.NotNil(t, mev.MergeRequest)
	assert.Equal(t, head3, mev.MergeRequest.MergedCommitID)
	pev := decode[eventOut](t, w.next(t, 10*time.Second))
	assert.Equal(t, "pushed", pev.Type, "the push of the target follows the merge")
	require.Len(t, pev.RefUpdates, 1)
	assert.Equal(t, "refs/heads/main", pev.RefUpdates[0].Ref)
	assert.Equal(t, head3, pev.RefUpdates[0].NewID)
	res = c.cli(t, "", "mr", "list", "proj", "--state", "all").ok(t)
	assert.Regexp(t, `!1\s+merged`, res.stdout)
	res = c.cli(t, secret, "mr", "close", "proj", "1")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "precondition failed")

	// Squash, with --mode squash. Without --head the merge is refused before it reaches the server.
	c.mustGit(wt, "checkout", "-q", "-b", "feat/two", head3)
	c.mustGit(wt, "-c", "user.name=Cy", "-c", "user.email=cy@example.org", "commit", "-q", "--allow-empty", "-m", "empty by Cy")
	c.writeFile(wt, "two.txt", []byte("two\n"))
	c.mustGit(wt, "push", "-q", url, "feat/two")
	c.cli(t, secret, "mr", "create", "proj", "--source", "feat/two", "--title", "Second").ok(t)
	w.nextOfType(t, "merge_request_opened")
	_, stderr, code = c.bin(t, secret, "mr", "merge", "proj", "2", "--mode", "squash")
	requireOneLineError(t, stderr, code, "--head is required", "ermite mr show proj 2")
	head4 := c.bareGit("proj", "rev-parse", "feat/two")
	res = c.cli(t, secret, "mr", "merge", "proj", "2", "--mode", "squash", "--head", head4)
	require.NoError(t, res.err, res.stderr)
	assert.Regexp(t, `^merged proj!2 into main \(squash\) at [0-9a-f]{10}\nthe commit is signed by the server's key SHA256:\S+; .ermite signing-key show. prints the key to verify it\n$`, res.stdout)
	sq := c.bareGit("proj", "rev-parse", "main")
	assert.Equal(t, head3, c.bareGit("proj", "rev-parse", sq+"^"))
	assert.Equal(t, "Second (!2)", c.bareGit("proj", "log", "-1", "--format=%s", sq))

	// Closing a third request.
	c.mustGit(wt, "checkout", "-q", "-b", "feat/three", head3)
	c.writeFile(wt, "three.txt", []byte("3\n"))
	c.mustGit(wt, "push", "-q", url, "feat/three")
	c.cli(t, secret, "mr", "create", "proj", "--source", "feat/three", "--title", "Third").ok(t)
	res = c.cli(t, secret, "mr", "close", "proj", "3", "--json").ok(t)
	assert.Equal(t, "closed", decode[mrOut](t, res.stdout).State)
	res = c.cli(t, secret, "mr", "close", "proj", "3")
	require.Error(t, res.err)
	_, stderr, code = c.bin(t, secret, "mr", "comment", "proj", "3", "-m", "late")
	requireOneLineError(t, stderr, code, "precondition failed")
}

// A private repository: merge requests do not exist for those who cannot read it.
func TestMergeRequestsOfAPrivateRepositoryAreNotFound(t *testing.T) {
	c := newCLIHarness(t)
	c.cli(t, adminToken, "repo", "create", "secret").ok(t)
	wt := c.workTree("main", "README.md")
	c.mustGit(wt, "checkout", "-q", "-b", "feat")
	c.writeFile(wt, "x.txt", []byte("x\n"))
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/secret.git"), "main", "feat")
	c.cli(t, adminToken, "mr", "create", "secret", "--source", "feat", "--title", "t").ok(t)

	unscoped := c.token([]domain.Scope{domain.ScopeRepoWrite})
	otherRepo := c.token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeMRWrite}, "elsewhere")
	for who, secret := range map[string]string{"anonymous": "", "no repo:read": unscoped, "token of another repository": otherRepo} {
		for _, args := range [][]string{
			{"mr", "list", "secret"},
			{"mr", "show", "secret", "1"},
			{"mr", "create", "secret", "--source", "feat", "--title", "t"},
			{"mr", "comment", "secret", "1", "-m", "x"},
			{"mr", "close", "secret", "1"},
			{"mr", "merge", "secret", "1", "--head", strings.Repeat("a", 40)},
		} {
			_, stderr, code := c.bin(t, secret, args...)
			requireOneLineError(t, stderr, code, "not found")
			assert.NotContains(t, stderr, "permission denied", "%s: %v", who, args)
		}
	}
	// And it is still there for the owner.
	res := c.cli(t, adminToken, "mr", "list", "secret", "--json").ok(t)
	assert.Len(t, decode[[]mrOut](t, res.stdout), 1)
}

// AC5 with the CLI: `ermite signing-key show` produces the allowed_signers line
// that git verify-commit uses for a squash commit.
func TestSigningKeyShowVerifiesASquashCommit(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("ssh-keygen not found: CI must have it (package openssh)")
		}
		t.Skip("ssh-keygen not found: install openssh to verify the signed commits")
	}
	c := newCLIHarness(t)
	c.cli(t, adminToken, "repo", "create", "proj", "--visibility", "public").ok(t)
	wt := c.workTree("main", "README.md")
	url := c.urlAuth(adminToken, "/proj.git")
	c.mustGit(wt, "checkout", "-q", "-b", "feat")
	c.writeFile(wt, "x.txt", []byte("x\n"))
	c.mustGit(wt, "push", "-q", url, "main", "feat")
	c.cli(t, adminToken, "mr", "create", "proj", "--source", "feat", "--title", "Add x").ok(t)
	res := c.cli(t, adminToken, "mr", "merge", "proj", "1", "--mode", "squash", "--head", c.bareGit("proj", "rev-parse", "feat"))
	require.NoError(t, res.err, res.stderr)
	sq := c.bareGit("proj", "rev-parse", "main")

	// No token is needed to read the key.
	show := c.cli(t, "", "signing-key", "show").ok(t)
	assert.Regexp(t, `(?m)^fingerprint\s+SHA256:`, show.stdout)
	assert.Regexp(t, `(?m)^committer\s+ermite <ermite@example.test>$`, show.stdout)
	allowed := strings.TrimSpace(c.cli(t, "", "signing-key", "show", "--allowed-signers").ok(t).stdout)
	pub := strings.TrimSpace(c.cli(t, "", "signing-key", "show", "--public-key").ok(t).stdout)
	assert.Equal(t, "ermite@example.test "+pub, allowed)
	assert.True(t, strings.HasPrefix(pub, "ssh-ed25519 "))
	js := c.cli(t, "", "signing-key", "show", "--json").ok(t)
	assert.Equal(t, []string{"allowed_signers", "committer_email", "committer_name", "fingerprint", "public_key"}, keys(t, js.stdout))
	assert.Equal(t, allowed, decode[struct {
		AllowedSigners string `json:"allowed_signers"`
	}](t, js.stdout).AllowedSigners)
	require.Error(t, c.cli(t, "", "signing-key", "show", "--json", "--public-key").err)
	require.Error(t, c.cli(t, "", "signing-key", "show", "--public-key", "--allowed-signers").err)

	// The file built from the CLI output verifies the commit with git.
	signers := filepath.Join(t.TempDir(), "allowed_signers")
	require.NoError(t, os.WriteFile(signers, []byte(allowed+"\n"), 0o600))
	out, err := c.gitEnv(nil, c.repoDir("proj"), "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", sq)
	require.NoError(t, err, out)
	assert.Contains(t, out, `Good "git" signature for ermite@example.test`)
	// A commit pushed by a person is not signed by the instance key.
	_, err = c.gitEnv(nil, c.repoDir("proj"), "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", c.bareGit("proj", "rev-parse", "feat"))
	assert.Error(t, err)
	// The key is stable across calls.
	again := strings.TrimSpace(c.cli(t, "", "signing-key", "show", "--public-key").ok(t).stdout)
	assert.Equal(t, pub, again)
}

// repo edit sets the merge settings; repo show reports them.
func TestRepoEditMergeSettingsCLI(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("proj", domain.VisibilityPublic)
	res := c.cli(t, adminToken, "repo", "show", "proj", "--json").ok(t)
	repo := decode[struct {
		MergeModes []string `json:"merge_modes"`
	}](t, res.stdout)
	assert.Equal(t, []string{"fast-forward", "squash", "merge"}, repo.MergeModes)

	res = c.cli(t, adminToken, "repo", "edit", "proj", "--merge-modes", "squash", "--json").ok(t)
	repo = decode[struct {
		MergeModes []string `json:"merge_modes"`
	}](t, res.stdout)
	assert.Equal(t, []string{"squash"}, repo.MergeModes)
	res = c.cli(t, adminToken, "repo", "show", "proj").ok(t)
	assert.Regexp(t, `(?m)^merge modes\s+squash$`, res.stdout)

	require.Error(t, c.cli(t, adminToken, "repo", "edit", "proj", "--merge-modes", "rebase").err)
	require.Error(t, c.cli(t, adminToken, "repo", "edit", "proj", "--merge-modes", "").err)
	_, stderr, code := c.bin(t, c.token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeMRMerge}), "repo", "edit", "proj", "--merge-modes", "squash")
	requireOneLineError(t, stderr, code, "permission denied")
}

// nextOfType returns the next event of the type, skipping the others (the pushes
// of the test itself).
func (w *watchProc) nextOfType(t *testing.T, typ string) eventOut {
	t.Helper()
	for range 20 {
		ev := decode[eventOut](t, w.next(t, 10*time.Second))
		if ev.Type == typ {
			return ev
		}
	}
	t.Fatalf("no %s event", typ)
	return eventOut{}
}

func TestMergeRequestNotesCLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	c.cli(t, adminToken, "repo", "create", "notes", "--visibility", "public").ok(t)
	wt := c.workTree("main", "README.md")
	c.mustGit(wt, "checkout", "-q", "-b", "feat/notes")
	c.writeFile(wt, "notes.txt", []byte("one\ntwo\nthree\nfour\n"))
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/notes.git"), "main", "feat/notes")
	c.cli(t, adminToken, "mr", "create", "notes", "--source", "feat/notes", "--title", "Notes").ok(t)
	w := c.startWatch(t, adminToken)
	res := c.cli(t, adminToken, "mr", "comment", "notes", "1", "--path", "notes.txt", "--line", "1-3", "-m", "**range**", "--json").ok(t)
	root := decode[jsonComment](t, res.stdout)
	assert.EqualValues(t, 1, root.StartLine)
	assert.EqualValues(t, 3, root.Line)
	w.nextOfType(t, "comment_added")
	res = c.cli(t, adminToken, "mr", "comment", "notes", "1", "--reply", "^"+root.ID, "-m", "reply to ^"+root.ID, "--json").ok(t)
	reply := decode[jsonComment](t, res.stdout)
	assert.Equal(t, root.ID, reply.ReplyTo)
	assert.Equal(t, root.Path, reply.Path)
	assert.Equal(t, root.StartLine, reply.StartLine)
	assert.Equal(t, root.Line, reply.Line)
	w.nextOfType(t, "comment_added")
	require.Error(t, c.cli(t, adminToken, "mr", "comment", "notes", "1", "--reply", reply.ID, "-m", "nested").err)
	require.Error(t, c.cli(t, adminToken, "mr", "resolve", "notes", "1", reply.ID).err)
	writer := c.token([]domain.Scope{domain.ScopeRepoRead, domain.ScopeMRWrite})
	denied := c.cli(t, writer, "mr", "comment", "notes", "1", "--approve")
	require.Error(t, denied.err)
	assert.Contains(t, denied.err.Error(), "mr:review")
	res = c.cli(t, adminToken, "mr", "comment", "notes", "1", "--approve", "--json").ok(t)
	verdict := decode[jsonComment](t, res.stdout)
	assert.Equal(t, "approve", verdict.Verdict)
	assert.True(t, verdict.Resolved)
	assert.Empty(t, verdict.Text)
	w.nextOfType(t, "comment_added")
	require.Error(t, c.cli(t, writer, "mr", "edit-comment", "notes", "1", verdict.ID, "-m", "not mine").err)
	res = runCLI(t, c.vars(adminToken), "edited from stdin\n", "mr", "edit-comment", "notes", "1", "#"+reply.ID, "-m", "-", "--json").ok(t)
	edited := decode[jsonComment](t, res.stdout)
	assert.Equal(t, "edited from stdin", edited.Text)
	require.NotNil(t, edited.EditedAt)
	w.nextOfType(t, "comment_edited")
	res = c.cli(t, adminToken, "mr", "delete-comment", "notes", "1", verdict.ID, "--json").ok(t)
	deleted := decode[jsonComment](t, res.stdout)
	assert.True(t, deleted.Deleted)
	assert.Equal(t, "approve", deleted.Verdict)
	assert.Empty(t, deleted.Text)
	require.NotNil(t, deleted.EditedAt)
	w.nextOfType(t, "comment_deleted")
	c.cli(t, adminToken, "mr", "delete-comment", "notes", "1", verdict.ID).ok(t)
	c.cli(t, adminToken, "mr", "resolve", "notes", "1", root.ID).ok(t)
	w.nextOfType(t, "comment_resolved")
	res = c.cli(t, "", "mr", "show", "notes", "1", "--json").ok(t)
	detail := decode[jsonMRDetail](t, res.stdout)
	require.Len(t, detail.Comments, 3)
	require.Len(t, detail.Verdicts, 1)
	assert.Equal(t, verdict.ID, detail.Verdicts[0].CommentID)
	assert.Equal(t, "approve", detail.Verdicts[0].Verdict)
	assert.False(t, detail.Verdicts[0].Stale)
	res = c.cli(t, "", "mr", "show", "notes", "1").ok(t)
	assert.Contains(t, res.stdout, "lines 1-3")
	assert.Contains(t, res.stdout, "(edited)")
	assert.Contains(t, res.stdout, "[deleted]")
	assert.Contains(t, res.stdout, "approved")
	assert.Contains(t, res.stdout, "      #"+reply.ID)
	c.cli(t, adminToken, "mr", "comment", "notes", "1", "--request-changes", "-m", "needs work").ok(t)
	res = c.cli(t, "", "mr", "show", "notes", "1", "--json").ok(t)
	detail = decode[jsonMRDetail](t, res.stdout)
	require.Len(t, detail.Verdicts, 1)
	assert.Equal(t, "changes_requested", detail.Verdicts[0].Verdict)
	c.cli(t, adminToken, "mr", "close", "notes", "1").ok(t)
	require.Error(t, c.cli(t, adminToken, "mr", "edit-comment", "notes", "1", root.ID, "-m", "late").err)
	require.Error(t, c.cli(t, adminToken, "mr", "delete-comment", "notes", "1", root.ID).err)
}

func TestMRReadyAndIgnoreChecksCLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	secret := strings.TrimSpace(c.cli(t, adminToken, "token", "create", "daily", "--preset", "cli").ok(t).stdout)
	agent := strings.TrimSpace(c.cli(t, adminToken, "token", "create", "agent", "--preset", "agent", "--scope", "mr:merge").ok(t).stdout)
	c.cli(t, secret, "repo", "create", "proj", "--visibility", "public").ok(t)
	wt := c.workTree("main", "README.md")
	url := c.urlAuth(secret, "/proj.git")
	c.mustGit(wt, "push", "-q", url, "main")
	c.mustGit(wt, "checkout", "-q", "-b", "feat/ready")
	c.writeFile(wt, "README.md", []byte("ready\n"))
	c.mustGit(wt, "push", "-q", url, "feat/ready")
	head := c.bareGit("proj", "rev-parse", "feat/ready")
	c.cli(t, agent, "mr", "create", "proj", "--source", "feat/ready", "--title", "WIP: Add feature").ok(t)
	ready := c.cli(t, agent, "mr", "ready", "proj", "1", "--json").ok(t)
	assert.Equal(t, mrKeys, keys(t, ready.stdout))
	assert.Equal(t, "Add feature", decode[mrOut](t, ready.stdout).Title)
	res := c.cli(t, agent, "mr", "ready", "proj", "1")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "not a draft")
	res = c.cli(t, secret, "mr", "merge", "proj", "1", "--head", head, "--ignore-checks", "--auto")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "ignore-checks")
	res = c.cli(t, agent, "mr", "merge", "proj", "1", "--head", head, "--ignore-checks")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "mr:force-merge")
	c.cli(t, secret, "mr", "merge", "proj", "1", "--head", head, "--ignore-checks").ok(t)
	c.mustGit(wt, "checkout", "-q", "-b", "feat/second")
	c.writeFile(wt, "README.md", []byte("second\n"))
	c.mustGit(wt, "push", "-q", url, "feat/second")
	head = c.bareGit("proj", "rev-parse", "feat/second")
	c.cli(t, agent, "mr", "create", "proj", "--source", "feat/second", "--title", "[WIP] Second").ok(t)
	res = c.cli(t, agent, "mr", "ready", "proj", "2").ok(t)
	assert.Contains(t, res.stdout, "marked ready proj!2 (feat/second into main)\n"+c.url+"/proj/merge-requests/2\n")
	c.cli(t, secret, "mr", "merge", "proj", "2", "--head", head).ok(t)
}
