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

	// Resolve what the attempt has not resolved yet, outside the lock of the
	// run: a ls-remote takes seconds and must not hold up the job's calls.
	found := map[string]domain.CIActionResolution{}
	fails := map[string]string{}
	remote := newRemoteRefs(c.cfg.Remote)
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
		res, msg, err := c.resolveAction(ctx, j, p, remote)
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
			ra, err := c.answer(ctx, rs, own, p, res)
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
// ermite action is the one the job already holds for that repository, else a
// new one, written with the next flush.
func (c *CI) answer(ctx context.Context, rs *runState, j *domain.CIJob, p actionRef, res domain.CIActionResolution) (ports.ResolvedAction, error) {
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
		if _, err := c.cfg.Meta.Get(ctx, name); errors.Is(err, domain.ErrNotFound) {
			return ports.ResolvedAction{Ref: p.uses, Error: errUnresolvedRepo(p)}, nil
		} else if err != nil {
			return ports.ResolvedAction{}, err
		}
		ra.TarballURL = c.cfg.PublicURL + apiPath + "/repos/" + res.NameWithOwner + "/tarball/" + res.ResolvedSHA
		ra.Token = c.archiveToken(rs, j, name)
	}
	return ra, nil
}

// archiveToken returns the secret of the read token of the job for the
// archives of repo, issuing it when the job has none.
func (c *CI) archiveToken(rs *runState, j *domain.CIJob, repo domain.RepoName) string {
	ref := jobRef(*j)
	c.mu.Lock()
	tok, ok := c.archiveTokens[ref][repo]
	c.mu.Unlock()
	if ok {
		return tok
	}
	tok = rs.issueRepoTokenFor(j, repo, true)
	c.mu.Lock()
	if c.archiveTokens[ref] == nil {
		c.archiveTokens[ref] = map[domain.RepoName]string{}
	}
	c.archiveTokens[ref][repo] = tok
	c.mu.Unlock()
	return tok
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
	a.owner, a.repo, a.ref = segs[0], segs[1], ref
	a.path = strings.Join(segs[2:], "/")
	return a
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
func (c *CI) resolveAction(ctx context.Context, j domain.CIJob, a actionRef, remote *remoteRefs) (res domain.CIActionResolution, msg string, err error) {
	res = domain.CIActionResolution{RunID: j.RunID, RunAttempt: j.RunAttempt, Key: a.key()}
	if strings.EqualFold(a.owner, c.cfg.Owner) {
		if name, perr := domain.ParseRepoName(strings.ToLower(a.repo)); perr == nil {
			if _, gerr := c.cfg.Meta.Get(ctx, name); gerr == nil {
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
	seen   map[string]remoteList
}

type remoteList struct {
	refs []ports.RemoteRef
	err  error
}

func newRemoteRefs(r ports.GitRemote) *remoteRefs {
	return &remoteRefs{remote: r, seen: map[string]remoteList{}}
}

func (r *remoteRefs) list(ctx context.Context, owner, repo string) ([]ports.RemoteRef, error) {
	if r.remote == nil {
		return nil, fmt.Errorf("%w: this server cannot reach github.com", domain.ErrUnavailable)
	}
	key := strings.ToLower(owner + "/" + repo)
	if l, ok := r.seen[key]; ok {
		return l.refs, l.err
	}
	refs, err := r.remote.ListRefs(ctx, owner, repo)
	r.seen[key] = remoteList{refs, err}
	return refs, err
}
