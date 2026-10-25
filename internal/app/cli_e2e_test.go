// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/client"
	"bnema.dev/ermite/internal/domain"
)

// cliHarness drives the CLI against a real forge.
type cliHarness struct {
	*harness
	url string
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	h := newHarness(t, harnessOpts{})
	return &cliHarness{harness: h, url: h.srv.URL}
}

// vars is the CLI environment for a token ("" is anonymous).
func (c *cliHarness) vars(secret string) map[string]string {
	v := map[string]string{client.EnvURL: c.url}
	if secret != "" {
		v[client.EnvToken] = secret
	}
	return v
}

// cli runs the command tree in process.
func (c *cliHarness) cli(t *testing.T, secret string, args ...string) result {
	t.Helper()
	return runCLI(t, c.vars(secret), "", args...)
}

// bin runs the built binary; stdout and stderr are returned with the exit code.
func (c *cliHarness) bin(t *testing.T, secret string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var env []string
	for k, v := range c.vars(secret) {
		env = append(env, k+"="+v)
	}
	return runBinary(t, env, args...)
}

// keys returns the sorted field names of a JSON object.
func keys(t *testing.T, raw string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &m), raw)
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal([]byte(raw), &v), raw)
	return v
}

// requireOneLineError checks a failed run: exit 1, a single "ermite: …" line.
func requireOneLineError(t *testing.T, stderr string, code int, contains ...string) {
	t.Helper()
	assert.Equal(t, 1, code)
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "one line: %q", stderr)
	assert.True(t, strings.HasPrefix(stderr, "ermite: "), stderr)
	for _, s := range contains {
		assert.Contains(t, stderr, s)
	}
}

// watchProc is a running `ermite watch --json`.
type watchProc struct {
	cmd    *exec.Cmd
	lines  chan string
	stderr *syncBuffer
	done   chan error
}

// startWatch runs the binary and waits until the server has the stream.
func (c *cliHarness) startWatch(t *testing.T, secret string, args ...string) *watchProc {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"watch", "--json"}, args...)...)
	cmd.Env = baseEnv()
	for k, v := range c.vars(secret) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	w := &watchProc{cmd: cmd, lines: make(chan string, 64), stderr: &syncBuffer{}, done: make(chan error, 1)}
	cmd.Stderr = w.stderr
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			w.lines <- sc.Text()
		}
		close(w.lines)
		w.done <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	require.Eventually(t, func() bool { return c.forge.EventLog.Watchers() >= 1 }, 10*time.Second, 10*time.Millisecond,
		"the watch never reached the server; stderr: %s", w.stderr.String())
	return w
}

// next returns the next event line or fails after d. Mergeability events,
// which arrive asynchronously after a push or an opening, are skipped.
func (w *watchProc) next(t *testing.T, d time.Duration) (raw string) {
	t.Helper()
	for {
		l := w.nextAny(t, d)
		if !strings.Contains(l, `"type":"`+eventMRMergeability+`"`) {
			return l
		}
	}
}

// nextAny returns the next event line, whatever its kind, or fails after d.
func (w *watchProc) nextAny(t *testing.T, d time.Duration) (raw string) {
	t.Helper()
	select {
	case l, ok := <-w.lines:
		if !ok {
			t.Fatalf("watch ended; stderr: %s", w.stderr.String())
		}
		return l
	case <-time.After(d):
		t.Fatalf("no event within %v; stderr: %s", d, w.stderr.String())
	}
	return ""
}

// interrupt sends SIGINT and returns the exit code.
func (w *watchProc) interrupt(t *testing.T) int {
	t.Helper()
	require.NoError(t, w.cmd.Process.Signal(syscall.SIGINT))
	select {
	case err := <-w.done:
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		return exit.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not stop on SIGINT")
	}
	return -1
}

type tokenOut struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Scopes     []string `json:"scopes"`
	Repos      []string `json:"repos"`
	ExpiresAt  *string  `json:"expires_at"`
	CreatedAt  *string  `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	Secret     string   `json:"secret"`
}

type repoOut struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Topics        []string `json:"topics"`
	Visibility    string   `json:"visibility"`
	DefaultBranch string   `json:"default_branch"`
	CreatedAt     *string  `json:"created_at"`
	PushedAt      *string  `json:"pushed_at"`
}

type eventOut struct {
	ID         string  `json:"id"`
	Time       *string `json:"time"`
	Type       string  `json:"type"`
	Repo       string  `json:"repo"`
	Repository *repoOut
	RefUpdates []struct {
		Ref   string `json:"ref"`
		OldID string `json:"old_id"`
		NewID string `json:"new_id"`
	} `json:"ref_updates"`
	MergeRequest *mrOut `json:"merge_request"`
}

var repoKeys = []string{"allow_auto_merge", "archived", "created_at", "default_branch", "description", "issues", "merge_modes", "name", "pushed_at", "topics", "visibility"}

// AC6: the CLI end to end against a real forge.
func TestAC6CLIEndToEnd(t *testing.T) {
	c := newCLIHarness(t)

	// Root creates the daily token with the cli preset (binary: secret alone on stdout).
	stdout, stderr, code := c.bin(t, adminToken, "token", "create", "daily", "--preset", "cli")
	require.Equal(t, 0, code, stderr)
	secret := strings.TrimSpace(stdout)
	require.True(t, strings.HasPrefix(secret, "ermite_"), stdout)
	assert.Equal(t, 1, strings.Count(stdout, "\n"), "the secret is the only thing on stdout")
	assert.Contains(t, stderr, "created token")
	assert.NotContains(t, stderr, secret)

	// Follow events with that token, through the binary.
	w := c.startWatch(t, secret)

	// Create. Private by default; here public with a description.
	res := c.cli(t, secret, "repo", "create", "proj", "--description", "first", "--topics", "Go,cli,go", "--visibility", "public", "--json").ok(t)
	assert.Equal(t, repoKeys, keys(t, res.stdout))
	created := decode[repoOut](t, res.stdout)
	assert.Equal(t, "proj", created.Name)
	assert.Equal(t, "first", created.Description)
	assert.Equal(t, []string{"cli", "go"}, created.Topics, "normalized: lowercase, sorted, deduplicated")
	assert.Equal(t, "public", created.Visibility)
	assert.Equal(t, "main", created.DefaultBranch)
	require.NotNil(t, created.CreatedAt)
	_, err := time.Parse(time.RFC3339, *created.CreatedAt)
	require.NoError(t, err)
	assert.Nil(t, created.PushedAt, "no push yet")

	ev := decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "repo_created", ev.Type)
	assert.Equal(t, "proj", ev.Repo)
	require.NotNil(t, ev.Repository)
	assert.Equal(t, "public", ev.Repository.Visibility)
	assert.NotEmpty(t, ev.ID)
	assert.Contains(t, ev.ID, "-", "ids are <boot>-<seq>, opaque to the CLI")
	require.NotNil(t, ev.Time)

	// Creating it again, or with a bad name, is a one-line friendly error.
	stdout, stderr, code = c.bin(t, secret, "repo", "create", "proj")
	assert.Empty(t, stdout)
	requireOneLineError(t, stderr, code, "already exists")
	stdout, stderr, code = c.bin(t, secret, "repo", "create", "Bad_Name")
	assert.Empty(t, stdout)
	requireOneLineError(t, stderr, code, "invalid request")

	// Editing the default branch to one that does not exist fails and changes nothing.
	_, stderr, code = c.bin(t, secret, "repo", "edit", "proj", "--default-branch", "dev", "--description", "never")
	requireOneLineError(t, stderr, code, "precondition failed")
	res = c.cli(t, secret, "repo", "show", "proj", "--json").ok(t)
	assert.Equal(t, "first", decode[repoOut](t, res.stdout).Description)

	// Push two branches with git, using the same token.
	wt := c.workTree("main", "README.md")
	c.mustGit(wt, "checkout", "-q", "-b", "dev")
	c.writeFile(wt, "dev.txt", []byte("dev\n"))
	c.mustGit(wt, "push", "-q", c.urlAuth(secret, "/proj.git"), "main", "dev")

	ev = decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "pushed", ev.Type)
	assert.Equal(t, "proj", ev.Repo)
	var refs []string
	for _, u := range ev.RefUpdates {
		refs = append(refs, u.Ref)
		assert.Empty(t, u.OldID)
		assert.Len(t, u.NewID, 40)
	}
	assert.ElementsMatch(t, []string{"refs/heads/main", "refs/heads/dev"}, refs)

	// Edit description, visibility and default branch in one call.
	res = c.cli(t, secret, "repo", "edit", "proj", "--description", "the project", "--visibility", "private", "--default-branch", "dev", "--json").ok(t)
	assert.Equal(t, repoKeys, keys(t, res.stdout))
	edited := decode[repoOut](t, res.stdout)
	assert.Equal(t, "the project", edited.Description)
	assert.Equal(t, "private", edited.Visibility)
	assert.Equal(t, "dev", edited.DefaultBranch)
	require.NotNil(t, edited.PushedAt, "set by the push")
	assert.Equal(t, "refs/heads/dev", c.bareGit("proj", "symbolic-ref", "HEAD"))

	ev = decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "repo_updated", ev.Type)
	require.NotNil(t, ev.Repository)
	assert.Equal(t, "dev", ev.Repository.DefaultBranch)
	assert.Equal(t, "private", ev.Repository.Visibility)

	// Clearing the description takes an explicit empty value.
	res = c.cli(t, secret, "repo", "edit", "proj", "--description", "", "--json").ok(t)
	assert.Empty(t, decode[repoOut](t, res.stdout).Description)
	assert.Equal(t, "repo_updated", decode[eventOut](t, w.next(t, time.Second)).Type)

	// Topics are replaced as a whole and cleared with an empty value.
	res = c.cli(t, secret, "repo", "edit", "proj", "--topics", "forge,git", "--json").ok(t)
	assert.Equal(t, []string{"forge", "git"}, decode[repoOut](t, res.stdout).Topics)
	assert.Equal(t, "repo_updated", decode[eventOut](t, w.next(t, time.Second)).Type)
	_, stderr, code = c.bin(t, secret, "repo", "edit", "proj", "--topics", "Bad_Topic")
	requireOneLineError(t, stderr, code, "invalid request")
	res = c.cli(t, secret, "repo", "edit", "proj", "--topics", "", "--json").ok(t)
	assert.Empty(t, decode[repoOut](t, res.stdout).Topics)
	assert.Equal(t, "repo_updated", decode[eventOut](t, w.next(t, time.Second)).Type)

	// list and show: same shape, human tables too.
	res = c.cli(t, secret, "repo", "list", "--json").ok(t)
	list := decode[[]json.RawMessage](t, res.stdout)
	require.Len(t, list, 1)
	assert.Equal(t, repoKeys, keys(t, string(list[0])))
	assert.Equal(t, "proj", decode[repoOut](t, string(list[0])).Name)

	res = c.cli(t, secret, "repo", "show", "proj", "--json").ok(t)
	assert.Equal(t, repoKeys, keys(t, res.stdout))
	shown := decode[repoOut](t, res.stdout)
	assert.Equal(t, "dev", shown.DefaultBranch)
	assert.Equal(t, "private", shown.Visibility)

	res = c.cli(t, secret, "repo", "list").ok(t)
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	require.Len(t, lines, 2)
	assert.Regexp(t, `^NAME\s+VISIBILITY\s+BRANCH\s+LAST PUSH\s+DESCRIPTION$`, lines[0])
	assert.Regexp(t, `^proj\s+private\s+dev\s+\d{4}-\d{2}-\d{2} \d{2}:\d{2}`, lines[1])
	res = c.cli(t, secret, "repo", "show", "proj").ok(t)
	assert.Regexp(t, `(?m)^default branch\s+dev$`, res.stdout)

	// Private now: invisible to anonymous callers (not_found, never forbidden).
	_, stderr, code = c.bin(t, "", "repo", "show", "proj")
	requireOneLineError(t, stderr, code, "not found")
	res = c.cli(t, "", "repo", "list", "--json").ok(t)
	assert.JSONEq(t, `[]`, res.stdout)

	// version: no token needed. This server runs on http://127.0.0.1, where
	// passkeys are not available.
	res = c.cli(t, "", "version", "--json").ok(t)
	assert.Equal(t, 1, strings.Count(res.stderr, "warning:"), res.stderr)
	assert.Contains(t, res.stderr, "the server does not support passkeys")
	type versionOut struct {
		Client struct{ Version string }
		Server struct {
			URL             string
			Version         string
			Features        []string
			MissingFeatures []string `json:"missing_features"`
		}
	}
	ver := decode[versionOut](t, res.stdout)
	assert.Equal(t, c.url, ver.Server.URL)
	assert.NotEmpty(t, ver.Server.Version)
	assert.ElementsMatch(t, []string{"repos.archived", "repo.update", "events.resume", "merge_requests", "merge_requests.delete_branch", "issues", "issues.links", "issues.resolve", "issues.edit", "merge_requests.edit", "merge_requests.mergeability", "signing_key", "session", "git.list_paths", "token.branch_prefixes", "ci.runs", "ci.runs.filters", "ci.runners", "ci.actions", "ci.annotations", "ci.summaries", "ci.concurrency", "ci.dispatch", "ci.workflows", "ci.cache", "ci.secrets", "ci.artifacts", "commit_signatures", "mirror", "merge_requests.auto_merge", "merge_requests.ignore_checks", "merge_requests.ready", "deps.runs", "releases", "deps", "journal"}, ver.Server.Features)
	assert.Equal(t, []string{"passkeys"}, ver.Server.MissingFeatures)
	stdout, stderr, code = c.bin(t, secret, "version")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "server")
	assert.Equal(t, 1, strings.Count(stderr, "warning:"), stderr)
	assert.Contains(t, stderr, "the server does not support passkeys")

	// Tokens: create a second one, list, revoke it, see it stop working.
	// (through the binary: the in-process runs use a fixed clock)
	stdout, stderr, code = c.bin(t, secret, "token", "create", "tmp", "--preset", "ci", "--repo", "proj", "--expires", "7d", "--json")
	require.Equal(t, 0, code, stderr)
	res = result{stdout: stdout}
	assert.Equal(t, []string{"branch_prefixes", "created_at", "expires_at", "id", "label", "last_used_at", "repos", "scopes", "secret"}, keys(t, res.stdout))
	tmp := decode[tokenOut](t, res.stdout)
	assert.Equal(t, []string{"repo:read"}, tmp.Scopes)
	assert.Equal(t, []string{"proj"}, tmp.Repos)
	require.NotNil(t, tmp.ExpiresAt)
	exp, err := time.Parse(time.RFC3339, *tmp.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), exp, time.Minute)
	assert.True(t, strings.HasPrefix(tmp.Secret, "ermite_"))

	// The ci token reads the private repo it is limited to, and cannot write or manage tokens.
	res = c.cli(t, tmp.Secret, "repo", "show", "proj", "--json").ok(t)
	assert.Equal(t, "proj", decode[repoOut](t, res.stdout).Name)
	_, stderr, code = c.bin(t, tmp.Secret, "repo", "create", "nope")
	requireOneLineError(t, stderr, code, "permission denied")
	_, stderr, code = c.bin(t, tmp.Secret, "token", "list")
	requireOneLineError(t, stderr, code, "permission denied")

	res = c.cli(t, secret, "token", "list", "--json").ok(t)
	tokens := decode[[]tokenOut](t, res.stdout)
	require.Len(t, tokens, 2)
	byLabel := map[string]tokenOut{}
	for _, tk := range tokens {
		assert.Empty(t, tk.Secret, "list never shows secrets")
		byLabel[tk.Label] = tk
	}
	assert.Len(t, byLabel["daily"].Scopes, len(client.AllScopes()))
	assert.Contains(t, byLabel["daily"].Scopes, "release:write")
	assert.Equal(t, tmp.ID, byLabel["tmp"].ID)
	assert.NotContains(t, res.stdout, `"secret"`)
	res = c.cli(t, secret, "token", "list").ok(t)
	assert.Regexp(t, `(?m)^ID\s+LABEL\s+SCOPES\s+REPOS\s+BRANCHES\s+EXPIRES\s+LAST USED$`, res.stdout)
	assert.Contains(t, res.stdout, tmp.ID)

	stdout, stderr, code = c.bin(t, secret, "token", "revoke", tmp.ID)
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, "revoked "+tmp.ID+"\n", stdout)
	_, stderr, code = c.bin(t, secret, "token", "revoke", tmp.ID)
	requireOneLineError(t, stderr, code, "not found")
	_, stderr, code = c.bin(t, tmp.Secret, "repo", "list")
	requireOneLineError(t, stderr, code, "not authenticated", "ERMITE_TOKEN")
	res = c.cli(t, secret, "token", "list", "--json").ok(t)
	assert.Len(t, decode[[]tokenOut](t, res.stdout), 1)

	// Delete: refuses without confirmation, then --yes; the repo is gone and the name free.
	stdout, stderr, code = c.bin(t, secret, "repo", "delete", "proj")
	assert.Empty(t, stdout)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "ermite: aborted")
	c.cli(t, secret, "repo", "show", "proj").ok(t)

	stdout, stderr, code = c.bin(t, secret, "repo", "delete", "proj", "--yes")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, "deleted proj\n", stdout)
	ev = decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "repo_deleted", ev.Type)
	assert.Equal(t, "proj", ev.Repo)

	stdout, stderr, code = c.bin(t, secret, "repo", "show", "proj")
	assert.Empty(t, stdout)
	requireOneLineError(t, stderr, code, "not found")
	res = c.cli(t, secret, "repo", "list", "--json").ok(t)
	assert.JSONEq(t, `[]`, res.stdout)

	// Ctrl-C ends the watch cleanly.
	assert.Equal(t, 0, w.interrupt(t))
	assert.NotContains(t, w.stderr.String(), "connection lost")

	// No secret reached the server's logs.
	assert.NotContains(t, c.logs.String(), secret)
	assert.NotContains(t, c.logs.String(), tmp.Secret)
}

// A bad or missing token gets the hint about ERMITE_TOKEN; the real server
// answers on the Connect protocol, so the CLI sees `unauthenticated`.
func TestAC6CLIUnauthenticatedHint(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("pub", domain.VisibilityPublic)

	bad := "ermite_" + strings.Repeat("x", 43)
	stdout, stderr, code := c.bin(t, bad, "repo", "list")
	assert.Empty(t, stdout)
	requireOneLineError(t, stderr, code, "not authenticated", "ERMITE_TOKEN")
	assert.NotContains(t, stderr, bad)

	// Anonymous reads of public repositories work, and anonymous writes ask for a token.
	res := c.cli(t, "", "repo", "show", "pub", "--json").ok(t)
	assert.Equal(t, "pub", decode[repoOut](t, res.stdout).Name)
	_, stderr, code = c.bin(t, "", "repo", "create", "x")
	requireOneLineError(t, stderr, code, "ERMITE_TOKEN")
	_, stderr, code = c.bin(t, "", "token", "list")
	requireOneLineError(t, stderr, code, "ERMITE_TOKEN")
}

// Once the server blocks a client for failed authentications, the CLI says so
// in one line instead of showing a raw error.
func TestAC6CLIBlockedClient(t *testing.T) {
	c := newCLIHarness(t)
	bad := "ermite_" + strings.Repeat("y", 43)
	var last result
	for range 50 {
		last = c.cli(t, bad, "repo", "list")
		require.Error(t, last.err)
		if strings.Contains(last.err.Error(), "too many") {
			break
		}
		assert.Contains(t, last.err.Error(), "not authenticated")
	}
	require.Contains(t, last.err.Error(), "too many requests", "the limiter never blocked")
	assert.NotContains(t, last.err.Error(), "\n")

	_, stderr, code := c.bin(t, adminToken, "repo", "list")
	requireOneLineError(t, stderr, code, "too many requests")
}

// The client survives a dropped connection: it reconnects with backoff and
// resumes after the last event it printed, without losing or repeating events.
func TestAC6CLIWatchResumesAfterDrop(t *testing.T) {
	c := newCLIHarness(t)
	w := c.startWatch(t, adminToken)

	c.cli(t, adminToken, "repo", "create", "one").ok(t)
	first := decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "one", first.Repo)

	// Cut every connection, then make an event happen while the CLI is away.
	c.srv.CloseClientConnections()
	c.cli(t, adminToken, "repo", "create", "two").ok(t)
	c.cli(t, adminToken, "repo", "create", "three").ok(t)

	second := decode[eventOut](t, w.next(t, 10*time.Second))
	third := decode[eventOut](t, w.next(t, 2*time.Second))
	assert.Equal(t, []string{"two", "three"}, []string{second.Repo, third.Repo}, "replayed in order, none repeated")
	assert.Equal(t, "repo_created", second.Type)
	assert.NotEqual(t, first.ID, second.ID)

	// Live again after the replay.
	c.cli(t, adminToken, "repo", "create", "four").ok(t)
	assert.Equal(t, "four", decode[eventOut](t, w.next(t, time.Second)).Repo)

	assert.Equal(t, 0, w.interrupt(t))
	assert.Contains(t, w.stderr.String(), "ermite: connection lost")
	assert.Contains(t, w.stderr.String(), "ermite: reconnected")
	assert.NotContains(t, w.stderr.String(), "no resume point", "the first event gave a resume point")
}

// A watch on one repository only sees that repository, and an unknown one is
// a one-line not_found without any retry.
func TestAC6CLIWatchOneRepo(t *testing.T) {
	c := newCLIHarness(t)
	c.seed("a", domain.VisibilityPublic)
	c.seed("b", domain.VisibilityPublic)

	stdout, stderr, code := c.bin(t, adminToken, "watch", "missing")
	assert.Empty(t, stdout)
	requireOneLineError(t, stderr, code, "not found")

	w := c.startWatch(t, adminToken, "a")
	wt := c.workTree("main", "f")
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/b.git"), "main")
	c.mustGit(wt, "push", "-q", c.urlAuth(adminToken, "/a.git"), "main")
	ev := decode[eventOut](t, w.next(t, time.Second))
	assert.Equal(t, "pushed", ev.Type)
	assert.Equal(t, "a", ev.Repo)
	assert.Equal(t, 0, w.interrupt(t))
}

func TestIssueResolvedCLIEndToEnd(t *testing.T) {
	r := newMRRig(t, domain.VisibilityPublic)
	env := envFor(r.srv.URL)
	env[client.EnvToken] = adminToken
	cli := func(args ...string) result { return runCLI(t, env, "", args...).ok(t) }
	cli("repo", "edit", "proj", "--issues", "owner")
	cli("issue", "create", "proj", "--title", "Crash")
	runCLI(t, env, "Fixes #1", "mr", "create", "proj", "--source", "feat", "--title", "Fix crash", "--description-file", "-").ok(t)
	before := cli("mr", "show", "proj", "2")
	assert.Contains(t, before.stdout, "#1 open Crash · resolves")
	cli("mr", "merge", "proj", "2", "--head", r.head)
	r.forge.MergeRequests.Wait(r.ctx)
	shown := cli("issue", "show", "proj", "1")
	assert.Regexp(t, `state\s+resolved`, shown.stdout)
	assert.Regexp(t, `resolved by\s+!2`, shown.stdout)
	assert.Contains(t, shown.stdout, "root referenced this in !2 (will resolve)")
	assert.Contains(t, shown.stdout, "root resolved this by merging !2")
	detail := decode[jsonIssueDetail](t, cli("issue", "show", "proj", "1", "--json").stdout)
	assert.Equal(t, "resolved", detail.State)
	assert.EqualValues(t, 2, detail.ClosedByMergeRequest)
	require.Len(t, detail.Events, 2)
	assert.Equal(t, "resolved", detail.Events[1].Kind)
	mr := cli("mr", "show", "proj", "2")
	assert.Contains(t, mr.stdout, "#1 resolved Crash · resolves")
	assert.Contains(t, mr.stdout, "root resolved #1 by merging !2")
	md := decode[jsonMRDetail](t, cli("mr", "show", "proj", "2", "--json").stdout)
	require.Len(t, md.Issues, 1)
	require.Len(t, md.IssueEvents, 2)
	cli("issue", "reopen", "proj", "1")
	cli("issue", "resolve", "proj", "1")
	cli("issue", "list", "proj", "--state", "resolved")
	cli("issue", "reopen", "proj", "1")
	detail = decode[jsonIssueDetail](t, cli("issue", "show", "proj", "1", "--json").stdout)
	assert.Equal(t, "open", detail.State)
	assert.Zero(t, detail.ClosedByMergeRequest)
	require.Len(t, detail.Events, 5)
}
