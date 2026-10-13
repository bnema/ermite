// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ResolveActions turns the "uses" of a job into where to download the actions
// (ADR 0025, CI-2), as GitHub's action download info does: ermite resolves, the
// runner downloads from the sandbox.
//
//   - owner = ERMITE_OWNER and the repository exists on ermite: ermite wins (as
//     the actions of a GitHub Enterprise Server do). The ref is a tag, else a
//     branch, else a full commit id that exists; the archive is served by the
//     REST subset and downloaded with a read token for that repository only.
//   - anything else: github.com. The ref is resolved with `git ls-remote` (tag,
//     then branch, the peeled commit of an annotated tag); a full commit id is
//     returned as it is, without checking. The archive is codeload's, with no
//     token.
//
// The commit a ref resolves to is kept for the run attempt: every job and every
// call of the attempt gets the same one, the first that was stored. A
// reference that cannot be resolved is not an error of the call: its answer
// carries GitHub's message.
func (c *CI) ResolveActions(ctx context.Context, secret, jobID string, uses []string) ([]ports.ResolvedAction, error) {
	out, err := c.resolveActions(ctx, secret, jobID, uses)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		// Our own deadline, not the caller's: github.com (or the disk) was too
		// slow. The runner may retry, or fail the job.
		return nil, fmt.Errorf("%w: resolving the actions took more than %s", domain.ErrUnavailable, c.cfg.ResolveTimeout)
	}
	return out, err
}

func (c *CI) resolveActions(ctx context.Context, secret, jobID string, uses []string) ([]ports.ResolvedAction, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ResolveTimeout)
	defer cancel()
	j, err := c.jobFor(ctx, secret, jobID)
	if err != nil {
		return nil, err
	}
	if j.Ended() {
		return nil, endedError(j)
	}
	if len(uses) > maxActionRefs {
		return nil, fmt.Errorf("%w: at most %d actions per call", domain.ErrInvalid, maxActionRefs)
	}
	parsed := make([]actionRef, len(uses))
	for i, u := range uses {
		if strings.HasPrefix(u, "./") || strings.HasPrefix(u, ".\\") {
			return nil, fmt.Errorf("%w: %q is a local action: the runner reads it from the workspace", domain.ErrInvalid, u)
		}
		if strings.HasPrefix(u, "docker://") {
			return nil, fmt.Errorf("%w: %q is a Docker image: the runner pulls it", domain.ErrInvalid, u)
		}
		parsed[i] = parseActionRef(u)
	}

	src, err := c.cfg.Store.GetRunSource(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	by := c.onBehalfOf(src)

	// Resolve what the attempt has not resolved yet, outside the lock of the
	// run: a ls-remote takes seconds and must not hold up the job's calls.
	found := map[string]domain.CIActionResolution{}
	fails := map[string]string{}
	remote := newRemoteRefs(c.cfg.Remote, c.remoteSlots)
	for _, p := range parsed {
		if p.err != "" {
			continue
		}
		if _, done := found[p.key()]; done {
			continue
		}
		if _, bad := fails[p.key()]; bad {
			continue
		}
		if res, err := c.cfg.Store.GetActionResolution(ctx, j.RunID, j.RunAttempt, p.key()); err == nil {
			found[p.key()] = res
			continue
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		res, msg, err := c.resolveAction(ctx, j, p, remote, by)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			fails[p.key()] = msg
			continue
		}
		found[p.key()] = res
	}

	out := make([]ports.ResolvedAction, len(parsed))
	err = c.lockedJob(ctx, j, func(cur *domain.CIJob) error {
		if cur.Ended() {
			return endedError(*cur)
		}
		rs, err := c.loadRun(ctx, cur.RunID, cur.RunAttempt)
		if err != nil {
			return err
		}
		own := rs.find(cur.ID)
		if own == nil {
			return domain.ErrNotFound
		}
		for i, p := range parsed {
			out[i] = ports.ResolvedAction{Ref: p.uses}
			if p.err != "" {
				out[i].Error = p.err
				continue
			}
			if msg, bad := fails[p.key()]; bad {
				out[i].Error = msg
				continue
			}
			// The first resolution stored wins, ours or a concurrent job's.
			res, ok := found[p.key()]
			if cached, err := c.cfg.Store.GetActionResolution(ctx, cur.RunID, cur.RunAttempt, p.key()); err == nil {
				res, ok = cached, true
			} else if !errors.Is(err, domain.ErrNotFound) {
				return err
			} else if ok {
				rs.resolutions = append(rs.resolutions, res)
				found[p.key()] = res // two refs of the call with the same key
			}
			if !ok {
				continue
			}
			ra, err := c.answer(ctx, rs, own, p, res, by)
			if err != nil {
				return err
			}
			out[i] = ra
		}
		if err := rs.flush(ctx); err != nil {
			// The tokens were not stored: forget their secrets.
			c.mu.Lock()
			delete(c.archiveTokens, jobRef(*cur))
			c.mu.Unlock()
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// maxActionRefs is the most references of one ResolveActions call.
const maxActionRefs = 100

// answer builds the answer for a resolved reference. The archive token of an
// ermite action is bound to the commit of the answer: the one the job already
// holds for it, else a new one, written with the next flush. Whether the pusher
// may read the repository is asked again at each answer, not only when the
// commit was resolved: a token revoked since, or a repository made private,
// stops the answers.
func (c *CI) answer(ctx context.Context, rs *runState, j *domain.CIJob, p actionRef, res domain.CIActionResolution, by pusherAccess) (ports.ResolvedAction, error) {
	ra := ports.ResolvedAction{
		Ref: p.uses, NameWithOwner: res.NameWithOwner, Path: p.path, ResolvedSHA: res.ResolvedSHA, Source: res.Source,
	}
	switch res.Source {
	case domain.CIActionGitHub:
		// The case the workflow wrote: the stored resolution is shared by
		// spellings that differ only by case, and github.com ignores it.
		ra.NameWithOwner = p.owner + "/" + p.repo
		ra.TarballURL = "https://codeload.github.com/" + ra.NameWithOwner + "/tar.gz/" + res.ResolvedSHA
	case domain.CIActionErmite:
		name, err := domain.ParseRepoName(strings.ToLower(p.repo))
		if err != nil {
			return ports.ResolvedAction{Ref: p.uses, Error: errUnresolvedRepo(p)}, nil
		}
		repo, err := c.cfg.Meta.Get(ctx, name)
		if errors.Is(err, domain.ErrNotFound) {
			return ports.ResolvedAction{Ref: p.uses, Error: errUnresolvedRepo(p)}, nil
		} else if err != nil {
			return ports.ResolvedAction{}, err
		}
		if ok, err := c.pusherCanRead(ctx, by, rs.run.Repo, repo); err != nil {
			return ports.ResolvedAction{}, err
		} else if !ok {
			return ports.ResolvedAction{Ref: p.uses, Error: errUnresolvedRepo(p)}, nil
		}
		ra.TarballURL = c.cfg.PublicURL + apiPath + "/repos/" + res.NameWithOwner + "/tarball/" + res.ResolvedSHA
		ra.Token = c.archiveToken(rs, j, name, res.ResolvedSHA)
	}
	return ra, nil
}

// archiveToken returns the secret of the token of the job for the archive of
// the commit sha of repo, issuing it when the job has none. The token downloads
// that archive and nothing else (domain.RepoTokenPrincipal).
func (c *CI) archiveToken(rs *runState, j *domain.CIJob, repo domain.RepoName, sha string) string {
	ref, key := jobRef(*j), repo.String()+"@"+sha
	c.mu.Lock()
	tok, ok := c.archiveTokens[ref][key]
	c.mu.Unlock()
	if ok {
		return tok
	}
	tok = rs.issueRepoTokenFor(j, repo, sha)
	c.mu.Lock()
	if c.archiveTokens[ref] == nil {
		c.archiveTokens[ref] = map[string]string{}
	}
	c.archiveTokens[ref][key] = tok
	c.mu.Unlock()
	return tok
}

// pusherAccess is who the actions of a run are resolved on behalf of: the
// pusher recorded with the run.
type pusherAccess struct{ pusher domain.CIPusher }

func (c *CI) onBehalfOf(src domain.CIRunSource) pusherAccess { return pusherAccess{src.Pusher} }

// pusherCanRead tells whether the pusher of a run may read repo, with the rule
// of the forge (canRead) applied to the principal the pusher is now: the root
// token and the owner's session read everything; a stored token is looked up
// again, so a token revoked or expired since reads nothing, and its scopes and
// repository list apply; a run that records no pusher reads what an anonymous
// caller reads. The repository of the run itself needs no check: the job reads
// it with its own token.
func (c *CI) pusherCanRead(ctx context.Context, by pusherAccess, runRepo domain.RepoName, repo domain.Repo) (bool, error) {
	if repo.Name == runRepo {
		return true, nil
	}
	var p domain.Principal
	switch by.pusher.Kind {
	case domain.CIPusherRoot:
		p = domain.Root()
	case domain.CIPusherOwner:
		p = domain.Owner()
	case domain.CIPusherToken:
		if c.cfg.Tokens == nil {
			return false, nil
		}
		tok, err := c.cfg.Tokens.Get(ctx, by.pusher.TokenID)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if tok.Expired(c.now()) {
			return false, nil
		}
		p = domain.TokenPrincipal(tok)
	default:
		p = domain.Anonymous()
	}
	return canRead(p, repo), nil
}

// actionRef is a parsed "uses": owner/repo[/path]@ref.
type actionRef struct {
	uses, owner, repo, path, ref string
	// err is GitHub's message for a reference of the wrong shape.
	err string
}

// key identifies the resolution: GitHub names are case-insensitive, refs are
// not.
func (a actionRef) key() string { return strings.ToLower(a.owner+"/"+a.repo) + "@" + a.ref }

// parseActionRef splits a "uses" of a repository action. Only the shape
// owner/repo[/path]@ref is valid; GitHub splits the path on '/' and '\' and
// ignores empty segments (WorkflowTemplateConverter.cs), so do we.
func parseActionRef(uses string) actionRef {
	a := actionRef{uses: uses}
	left, ref, ok := strings.Cut(uses, "@")
	segs := strings.FieldsFunc(left, func(r rune) bool { return r == '/' || r == '\\' })
	if !ok || strings.Contains(ref, "@") || len(segs) < 2 || ref == "" {
		a.err = errActionFormat(uses)
		return a
	}
	for _, s := range segs[2:] {
		if s == "." || s == ".." {
			a.err = errActionFormat(uses)
			return a
		}
	}
	if !validActionName(segs[0]) || !validActionName(segs[1]) {
		a.err = errActionFormat(uses)
		return a
	}
	a.owner, a.repo, a.ref = segs[0], segs[1], ref
	a.path = strings.Join(segs[2:], "/")
	return a
}

// validActionName reports whether s can be an owner or a repository name on
// GitHub: letters, digits, '.', '_' and '-', 1 to 100 characters, not made of
// dots only ("." and ".." are path elements). Every source takes the same names, so that nothing but a name reaches a
// URL, a path or a command.
func validActionName(s string) bool {
	if s == "" || len(s) > 100 || strings.Trim(s, ".") == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// --- GitHub's texts ---
//
// All the messages a reference can fail with are here. The first is the
// runner's own parser text; the two others are the ones GitHub gives for an
// unknown version and an unknown repository, to be confirmed by recording
// against github.com (ci2-execution plan, phase A).

func errActionFormat(uses string) string {
	return "Expected format {org}/{repo}[/path]@ref. Actual '" + uses + "'"
}

func errUnresolvedRef(a actionRef) string {
	return "Unable to resolve action `" + a.owner + "/" + a.repo + "@" + a.ref + "`, unable to find version `" + a.ref + "`"
}

func errUnresolvedRepo(a actionRef) string {
	return "Unable to resolve action `" + a.owner + "/" + a.repo + "@" + a.ref + "`, repository not found"
}

// resolveAction resolves one reference. msg is the message of a reference that
// does not resolve; err is a failure of the call.
func (c *CI) resolveAction(ctx context.Context, j domain.CIJob, a actionRef, remote *remoteRefs, by pusherAccess) (res domain.CIActionResolution, msg string, err error) {
	res = domain.CIActionResolution{RunID: j.RunID, RunAttempt: j.RunAttempt, Key: a.key()}
	if domain.OwnerMatches(c.cfg.Owner, a.owner) {
		if name, perr := domain.ParseRepoName(strings.ToLower(a.repo)); perr == nil {
			if repo, gerr := c.cfg.Meta.Get(ctx, name); gerr == nil {
				// A repository the pusher may not read answers as one that does
				// not exist, before any ref is looked at, so that the texts leak
				// nothing about it.
				if ok, err := c.pusherCanRead(ctx, by, j.Repo, repo); err != nil {
					return res, "", err
				} else if !ok {
					return res, errUnresolvedRepo(a), nil
				}
				sha, found, err := c.resolveOnErmite(ctx, name, a.ref)
				if err != nil {
					return res, "", err
				}
				if !found {
					return res, errUnresolvedRef(a), nil
				}
				res.NameWithOwner, res.ResolvedSHA, res.Source = c.cfg.Owner+"/"+name.String(), sha, domain.CIActionErmite
				return res, "", nil
			} else if !errors.Is(gerr, domain.ErrNotFound) {
				return res, "", gerr
			}
		}
	}
	res.NameWithOwner, res.Source = a.owner+"/"+a.repo, domain.CIActionGitHub
	if low := strings.ToLower(a.ref); isHex40(low) {
		res.ResolvedSHA = low // a commit id is not checked: a missing one fails the download
		return res, "", nil
	}
	refs, err := remote.list(ctx, a.owner, a.repo)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return res, errUnresolvedRepo(a), nil
	case err != nil:
		return res, "", err
	}
	sha := pickRemoteRef(refs, a.ref)
	if sha == "" {
		return res, errUnresolvedRef(a), nil
	}
	res.ResolvedSHA = sha
	return res, "", nil
}

// pickRemoteRef finds the commit of a tag, else of a branch, in the refs of a
// remote. An annotated tag gives its peeled commit.
func pickRemoteRef(refs []ports.RemoteRef, name string) string {
	for _, prefix := range []string{"refs/tags/", "refs/heads/"} {
		for _, r := range refs {
			if r.Name != prefix+name {
				continue
			}
			if r.Peeled != "" {
				return r.Peeled
			}
			return r.ID
		}
	}
	return ""
}

// resolveOnErmite resolves a ref on a repository of this server: a tag, else
// a branch, else a full commit id that exists.
func (c *CI) resolveOnErmite(ctx context.Context, repo domain.RepoName, ref string) (sha string, found bool, err error) {
	refs, err := c.cfg.Reader.ListRefs(ctx, repo)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return "", false, err
	}
	for _, kind := range []domain.RefKind{domain.RefKindTag, domain.RefKindBranch} {
		i := slices.IndexFunc(refs, func(r domain.Ref) bool {
			return r.Kind == kind && r.ShortName == ref && !r.CommitID.IsZero()
		})
		if i >= 0 {
			return refs[i].CommitID.String(), true, nil
		}
	}
	if id, perr := domain.ParseObjectID(ref); perr == nil {
		ok, err := c.cfg.Git.HasCommit(ctx, repo, id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return "", false, err
		}
		if ok {
			return id.String(), true, nil
		}
	}
	return "", false, nil
}

// remoteRefs lists the refs of a remote once per repository within a call.
type remoteRefs struct {
	remote ports.GitRemote
	slots  chan struct{}
	seen   map[string]remoteList
}

type remoteList struct {
	refs []ports.RemoteRef
	err  error
}

func newRemoteRefs(r ports.GitRemote, slots chan struct{}) *remoteRefs {
	return &remoteRefs{remote: r, slots: slots, seen: map[string]remoteList{}}
}

func (r *remoteRefs) list(ctx context.Context, owner, repo string) ([]ports.RemoteRef, error) {
	if r.remote == nil {
		return nil, fmt.Errorf("%w: this server cannot reach github.com", domain.ErrUnavailable)
	}
	key := strings.ToLower(owner + "/" + repo)
	if l, ok := r.seen[key]; ok {
		return l.refs, l.err
	}
	// At most a few ls-remote at once on the whole server, whatever the number
	// of jobs resolving: a process each, and github.com is slow to answer.
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	refs, err := r.remote.ListRefs(ctx, owner, repo)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	r.seen[key] = remoteList{refs, err}
	return refs, err
}
