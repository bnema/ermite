// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
)

// This file builds the "github" context of a push and the "job" keys the
// server sends, to match what github.com gives (recordings of
// ermite-conformance, normalised by its normalize.go).
//
// Value provenance, used in the comments below:
//   - real: a value ermite has (repository name, visibility, description,
//     dates, commits, default branch, configuration);
//   - derived: computed from real values by a stable rule (ids, node ids,
//     URLs);
//   - constant: GitHub's value for a repository or account with no such
//     feature, or a placeholder where ermite has no concept (no avatars,
//     stars, forks, issues...).

const (
	// ghSecretSource is github.secret_source: where the secrets of the job
	// come from. Constant: ermite has one source.
	ghSecretSource = "Actions"
	// ghCacheLimit is github.artifact_cache_size_limit, in GB, as recorded on
	// github.com. Constant.
	ghCacheLimit = "10"
	// apiPath is where the GitHub-shaped API base of the payload lives under
	// the public URL. github.api_url is that base, github.graphql_url is
	// <base>/graphql and the *_url keys of the repository and the users are
	// <base>/repos/... and <base>/users/..., as on github.com. ermite serves
	// none of them yet: they are identifiers in the shape of GitHub's.
	apiPath = "/-/github/api"
)

// pair is a key and a value of an ordered JSON object.
type pair struct {
	k string
	v any
}

// ordered is a JSON object that keeps the order of its keys, as the github
// context of GitHub does (toJSON(github) prints that order).
type ordered []pair

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.k)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(p.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// stableID is a positive number that depends only on the kind and the name:
// the id of a user or of a repository, which ermite does not number. It fits
// in a JavaScript number (GitHub's own ids are well below 2^53).
func stableID(kind, name string) int64 {
	sum := sha256.Sum256([]byte(kind + "\x00" + name))
	return int64(binary.BigEndian.Uint64(sum[:8])>>20) + 1<<20
}

// nodeID is the legacy form of a GraphQL node id: base64 of "<n>:<Type><id>"
// (GitHub: "04:User5212137" is "MDQ6VXNlcjUyMTIxMzc=").
func nodeID(prefix, typ string, id int64) string {
	return base64.StdEncoding.EncodeToString([]byte(prefix + ":" + typ + strconv.FormatInt(id, 10)))
}

// ghIdentity is the account a push is attributed to.
type ghIdentity struct {
	login string
	id    int64
}

// identity maps the actor of a run to a login: "owner" and "root" (the
// owner's session or the root token) are the owner of the forge, a token
// label is itself.
func (c *CI) identity(actor string) ghIdentity {
	login := actor
	if actor == "owner" || actor == "root" || actor == "" {
		login = c.cfg.Owner
	}
	return ghIdentity{login: login, id: stableID("user", login)}
}

// ghURLs holds the URL bases of the payload of one repository.
type ghURLs struct {
	server string // github.server_url
	api    string // github.api_url
	host   string // host (and port) of the server, for git:// and ssh URLs
	owner  string
	name   string
	full   string // <owner>/<repo>
	repo   string // <server>/<owner>/<repo>, the page GitHub's html_url points at
}

func (c *CI) urls(repo domain.Repo) ghURLs {
	u := ghURLs{server: c.cfg.PublicURL, owner: c.cfg.Owner, name: repo.Name.String()}
	if p, err := url.Parse(c.cfg.PublicURL); err == nil {
		u.host = p.Host
	}
	u.api = u.server + apiPath
	u.full = u.owner + "/" + u.name
	u.repo = u.server + "/" + u.full
	return u
}

// userObj is the account object of an event (sender, repository owner).
// Derived: ids from the login; URLs from the API base. Constant: type, no
// avatar (ermite has none: "" is what GitHub gives for gravatar_id too), not
// a site admin.
func (u ghURLs) userObj(id ghIdentity, owner bool) map[string]any {
	api := u.api + "/users/" + id.login
	o := map[string]any{
		"avatar_url": "", "events_url": api + "/events{/privacy}", "followers_url": api + "/followers",
		"following_url": api + "/following{/other_user}", "gists_url": api + "/gists{/gist_id}", "gravatar_id": "",
		"html_url": u.server + "/" + id.login, "id": id.id, "login": id.login,
		"node_id":           nodeID("04", "User", id.id),
		"organizations_url": api + "/orgs", "received_events_url": api + "/received_events", "repos_url": api + "/repos",
		"site_admin": false, "starred_url": api + "/starred{/owner}{/repo}", "subscriptions_url": api + "/subscriptions",
		"type": "User", "url": api, "user_view_type": "public",
	}
	if owner {
		o["email"], o["name"] = "", id.login
	}
	return o
}

// repoLinks are the URL templates of the repository object, relative to
// <api>/repos/<owner>/<repo>. Derived (constant templates, as GitHub's).
var repoLinks = map[string]string{
	"archive_url": "/{archive_format}{/ref}", "assignees_url": "/assignees{/user}", "blobs_url": "/git/blobs{/sha}",
	"branches_url": "/branches{/branch}", "collaborators_url": "/collaborators{/collaborator}",
	"comments_url": "/comments{/number}", "commits_url": "/commits{/sha}", "compare_url": "/compare/{base}...{head}",
	"contents_url": "/contents/{+path}", "contributors_url": "/contributors", "deployments_url": "/deployments",
	"downloads_url": "/downloads", "events_url": "/events", "forks_url": "/forks", "git_commits_url": "/git/commits{/sha}",
	"git_refs_url": "/git/refs{/sha}", "git_tags_url": "/git/tags{/sha}", "hooks_url": "/hooks",
	"issue_comment_url": "/issues/comments{/number}", "issue_events_url": "/issues/events{/number}",
	"issues_url": "/issues{/number}", "keys_url": "/keys{/key_id}", "labels_url": "/labels{/name}",
	"languages_url": "/languages", "merges_url": "/merges", "milestones_url": "/milestones{/number}",
	"notifications_url": "/notifications{?since,all,participating}", "pulls_url": "/pulls{/number}",
	"releases_url": "/releases{/id}", "stargazers_url": "/stargazers", "statuses_url": "/statuses/{sha}",
	"subscribers_url": "/subscribers", "subscription_url": "/subscription", "tags_url": "/tags", "teams_url": "/teams",
	"trees_url": "/git/trees{/sha}", "url": "",
}

// repoObj is event.repository: the 83 keys of GitHub's push payload.
//
//	real:     name, full_name, private, visibility, description, default_branch,
//	          master_branch, created_at, pushed_at, updated_at
//	derived:  id, node_id, owner, html_url, svn_url, clone_url, git_url,
//	          ssh_url, url and the other *_url keys
//	constant: the rest (no forks, stars, issues, pages..., the features of a
//	          new GitHub repository; license, language, homepage and
//	          mirror_url null; size 0)
func (c *CI) repoObj(repo domain.Repo, u ghURLs, defaultBranch string, at time.Time) map[string]any {
	owner := c.identity("owner")
	id := stableID("repo", u.full)
	pushed := repo.PushedAt
	if pushed.IsZero() {
		pushed = at
	}
	var desc any
	if repo.Description != "" {
		desc = repo.Description
	}
	r := map[string]any{
		"allow_forking": true, "archived": false, "clone_url": u.repo + ".git",
		"created_at": repo.CreatedAt.Unix(), "default_branch": defaultBranch, "description": desc,
		"disabled": false, "fork": false, "forks": 0, "forks_count": 0, "full_name": u.full,
		"git_url":         "git://" + u.host + "/" + u.full + ".git",
		"has_discussions": false, "has_downloads": true, "has_issues": true, "has_pages": false, "has_projects": true,
		"has_pull_requests": true, "has_wiki": true, "homepage": nil, "html_url": u.repo, "id": id,
		"is_template": false, "language": nil, "license": nil, "master_branch": defaultBranch, "mirror_url": nil,
		"name": u.name, "node_id": nodeID("010", "Repository", id), "open_issues": 0, "open_issues_count": 0,
		"owner":   u.userObj(owner, true),
		"private": repo.Visibility == domain.VisibilityPrivate, "pull_request_creation_policy": "all",
		"pushed_at": pushed.Unix(), "size": 0, "ssh_url": "git@" + u.host + ":" + u.full + ".git",
		"stargazers": 0, "stargazers_count": 0, "svn_url": u.repo, "topics": []string{},
		"updated_at": pushed.UTC().Format(time.RFC3339), "visibility": string(repo.Visibility),
		"watchers": 0, "watchers_count": 0, "web_commit_signoff_required": false,
	}
	for k, suffix := range repoLinks {
		r[k] = u.api + "/repos/" + u.full + suffix
	}
	return r
}

// commitJSON is one commit of the push event: GitHub's push payload has
// exactly these keys in the recordings (no added/removed/modified, no
// username).
func (c *CI) commitJSON(ctx context.Context, repo domain.Repo, u ghURLs, cm domain.Commit) map[string]any {
	tree := ""
	if id, err := c.cfg.Git.CommitTree(ctx, repo.Name, cm.ID); err == nil {
		tree = id.String()
	}
	return map[string]any{
		"id": cm.ID.String(), "tree_id": tree, "distinct": true, "message": cm.Message,
		"timestamp": cm.Committer.Time.UTC().Format(time.RFC3339),
		"url":       u.repo + "/commit/" + cm.ID.String(),
		"author":    person(cm.Author.Name, cm.Author.Email), "committer": person(cm.Committer.Name, cm.Committer.Email),
	}
}

// pushPayload is the part of the push event that is the same for every
// workflow of one ref: the commits, each with its tree id (one Git call per
// commit). It is built once per ref and shared by the probes and the runs.
type pushPayload struct {
	commits []any
	head    map[string]any
}

// pushPayload builds the commit part of the push event of f.
func (c *CI) pushPayload(ctx context.Context, repo domain.Repo, f pushFacts) *pushPayload {
	u := c.urls(repo)
	p := &pushPayload{commits: make([]any, 0, len(f.commits)), head: c.commitJSON(ctx, repo, u, f.head)}
	for _, cm := range f.commits {
		p.commits = append(p.commits, c.commitJSON(ctx, repo, u, cm))
	}
	return p
}

func person(name, email string) map[string]any { return map[string]any{"name": name, "email": email} }

// githubJSON builds the "github" context of the run as GitHub does for a push
// (the job-dependent keys, job and run_attempt, are filled in when a job is
// planned). Keys the runner owns (token, workspace, action, event_path, path,
// env, output, state, step_summary, artifacts...) are not here.
//
// The order of the keys is GitHub's, so that toJSON(github) prints the same.
// Ids and counters are strings, so is retention_days; "compare" has
// 12-character ids: the commit page for a created ref with one commit,
// "<first>^...<after>" for a created ref with several, "<before>...<after>" for
// an update.
//
// The commits of the event come from f.payload, built once per ref; without
// one they are read from Git here.
func (c *CI) githubJSON(ctx context.Context, repo domain.Repo, run domain.CIRun, f pushFacts) string {
	if f.pull != nil {
		return c.pullGithubJSON(repo, run, f)
	}
	u := c.urls(repo)
	id := c.identity(run.Actor)
	refName, refType := strings.TrimPrefix(run.Ref, "refs/heads/"), "branch"
	if f.isTag {
		refName, refType = strings.TrimPrefix(run.Ref, "refs/tags/"), "tag"
	}
	before := f.update.Old
	if f.created {
		before = domain.ZeroOID
	}
	compare := u.repo + "/compare/" + shortID(before) + "..." + shortID(f.update.New)
	switch {
	case f.created && len(f.commits) > 1:
		compare = u.repo + "/compare/" + shortID(f.commits[0].ID.String()) + "^..." + shortID(f.update.New)
	case f.created:
		compare = u.repo + "/commit/" + shortID(f.update.New)
	}
	payload := f.payload
	if payload == nil {
		payload = c.pushPayload(ctx, repo, f)
	}
	event := map[string]any{
		"ref": run.Ref, "before": before, "after": f.update.New, "created": f.created, "deleted": false, "forced": f.forced,
		"base_ref": nil, "compare": compare, "commits": payload.commits, "head_commit": payload.head,
		"pusher":     map[string]any{"name": id.login, "email": ""},
		"repository": c.repoObj(repo, u, f.defaultBranch, run.CreatedAt),
		"sender":     u.userObj(id, false),
	}
	return c.githubContext(repo, run, ghEvent{name: "push", refName: refName, refType: refType, payload: event})
}

// ghEvent is what differs between the github contexts of the events.
type ghEvent struct {
	name, refName, refType string
	// headRef and baseRef are github.head_ref and github.base_ref: set for a
	// pull_request only.
	headRef, baseRef string
	payload          map[string]any
}

// githubContext assembles the "github" context of a run in GitHub's key order
// around the payload of the event.
func (c *CI) githubContext(repo domain.Repo, run domain.CIRun, e ghEvent) string {
	u := c.urls(repo)
	id := c.identity(run.Actor)
	owner := c.identity("owner")
	repoID := strconv.FormatInt(stableID("repo", u.full), 10)
	gh := ordered{
		{"job", ""}, {"ref", run.Ref}, {"sha", run.Sha}, {"repository", u.full}, {"repository_owner", c.cfg.Owner},
		{"repository_owner_id", strconv.FormatInt(owner.id, 10)}, {"repositoryUrl", "git://" + u.host + "/" + u.full + ".git"},
		{"run_id", strconv.FormatInt(run.ID, 10)}, {"run_number", strconv.FormatInt(run.Number, 10)},
		{"retention_days", strconv.Itoa(c.retentionDays())}, {"run_attempt", "1"},
		{"artifact_cache_size_limit", ghCacheLimit}, {"repository_visibility", string(repo.Visibility)},
		{"actor_id", strconv.FormatInt(id.id, 10)}, {"actor", id.login}, {"workflow", run.WorkflowName},
		{"head_ref", e.headRef}, {"base_ref", e.baseRef}, {"event_name", e.name}, {"server_url", u.server},
		{"api_url", u.api}, {"graphql_url", u.api + "/graphql"}, {"ref_name", e.refName}, {"ref_protected", false},
		{"ref_type", e.refType}, {"secret_source", ghSecretSource}, {"event", e.payload},
		{"workflow_ref", u.full + "/" + run.WorkflowPath + "@" + run.Ref}, {"workflow_sha", run.Sha},
		{"repository_id", repoID}, {"triggering_actor", id.login},
	}
	b, err := json.Marshal(gh)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Layout of job.check_run_id, 53 bits so that it is exact as a JavaScript
// number (2^53 is the first integer that is not):
//
//	bits 0-8    combo:   matrix leg, 9 bits (0-511); a matrix has at most 256
//	bits 9-18   index:   position of the job in the workflow file, 10 bits
//	                     (0-1023); ermite has no limit of its own on the number
//	                     of jobs
//	bits 19-25  attempt: run attempt, 7 bits (0-127), so that every re-run
//	                     gets new ids, as on GitHub
//	bits 26-52  run:     run id, 27 bits (below 134 million)
//
// A field that overflows would wrap into another instance's id, so the whole
// id is 0 then (see checkRunID).
const (
	checkComboBits   = 9
	checkIndexBits   = 10
	checkAttemptBits = 7
	checkRunBits     = 53 - checkComboBits - checkIndexBits - checkAttemptBits
)

// checkRunID is the number github.com shows in job.check_run_id: derived from
// the run, its attempt and the position of the job instance, as ermite's
// checks are not numbered. It is unique and stable per attempt, job and matrix
// leg, and fits in a JavaScript number.
//
// The key is informational, so it never fails a job: a value outside its field
// (which would wrap and give two instances the same id) is logged at warn with
// the run, attempt and job, and the id falls back to 0.
func checkRunID(ctx context.Context, run domain.CIRun, index, combo int) int64 {
	attempt := int64(run.Attempt)
	switch {
	case run.ID < 0 || run.ID >= 1<<checkRunBits,
		attempt < 0 || attempt >= 1<<checkAttemptBits,
		index < 0 || index >= 1<<checkIndexBits,
		combo < 0 || combo >= 1<<checkComboBits:
		log := zerowrap.FromCtx(ctx)
		log.Warn().Int64("run_id", run.ID).Int32("attempt", run.Attempt).Int("job_index", index).Int("leg", combo).
			Msg("ci check_run_id is out of range: the job context shows 0")
		return 0
	}
	return run.ID<<(checkAttemptBits+checkIndexBits+checkComboBits) |
		attempt<<(checkIndexBits+checkComboBits) |
		int64(index)<<checkComboBits |
		int64(combo)
}

// jobKeys are the keys of the "job" context that the server knows, in
// GitHub's order (the runner adds "status"). The runner copies the "job" key
// of the contexts it is given into the job context.
func (c *CI) jobKeys(ctx context.Context, run domain.CIRun, index, combo int) ordered {
	id := checkRunID(ctx, run, index, combo)
	u := c.urls(domain.Repo{Name: run.Repo})
	return ordered{
		{"check_run_id", id},
		{"workflow_ref", u.full + "/" + run.WorkflowPath + "@" + run.Ref},
		{"workflow_sha", run.Sha}, {"workflow_repository", u.full}, {"workflow_file_path", run.WorkflowPath},
	}
}
