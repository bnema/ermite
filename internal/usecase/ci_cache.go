// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"encoding/json"
	"slices"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The scope of a job in the cache the runner serves (CI-3 D8, CI-4 D4), sent as
// Job.cache. Entries are keyed by repository and ref, as on GitHub: a job
// restores from the ref of its run, then the base branch of a merge request,
// then the default branch, and saves under the ref of its run.
//
// An entry carries the trust of the run that saved it, because restoring it
// runs what it holds (node_modules, a build cache): a trusted run (secrets,
// write token) must never restore what an untrusted one saved. The runner
// treats the refs of Job.cache as names (it checks "refs/..." only), so the
// server keeps two namespaces:
//
//   - a trusted run saves under its ref, refs/heads/x, and restores trusted
//     entries only;
//   - an untrusted run saves under refs/untrusted/heads/x, never under the
//     default branch (not even in its own namespace: nothing saved there would
//     ever be restored by a run that should), and restores both kinds, its own
//     namespace first at each level.
//
// A pull_request_target run is on the default branch but runs for a merge
// request: it saves nothing (the runner then sets ACTIONS_CACHE_MODE=read) and
// restores the default branch alone, never the merge ref, whose entries the
// pull_request runs of the request save from the head's code. GitHub makes
// these runs read-only on the default branch too.
//
// Every runner that serves a cache (ermite-runner v0.3.0 and later) honours
// Job.cache, which the server always sends; entries saved before it did were
// saved under the plain refs whatever the trust of the run.

// untrustedPrefix starts the refs the entries of untrusted runs are saved
// under. No run is ever on such a ref: runs are on refs/heads, refs/tags and
// refs/pull.
const untrustedPrefix = "refs/untrusted/"

// untrustedRef is the ref an untrusted run saves ref's entries under.
func untrustedRef(ref string) string { return untrustedPrefix + strings.TrimPrefix(ref, "refs/") }

// cacheLinks are the facts of the github context of a run that its cache scope
// needs.
type cacheLinks struct {
	BaseRef string `json:"base_ref"`
	Event   struct {
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	} `json:"event"`
}

// cacheScope is the scope of the jobs of a run (an attempt: a re-run that
// lowers the trust lowers the scope); githubJSON is the github context the run
// was created with (domain.CIRunSource), so the default branch is the one of
// that moment.
func (c *CI) cacheScope(run domain.CIRun, githubJSON string) ports.JobCache {
	var l cacheLinks
	_ = json.Unmarshal([]byte(githubJSON), &l) // a context that does not read: no default branch known
	sc := ports.JobCache{Repository: c.cfg.Owner + "/" + run.Repo.String()}
	def := ""
	if l.Event.Repository.DefaultBranch != "" {
		def = "refs/heads/" + l.Event.Repository.DefaultBranch
	}
	read := func(ref string) {
		if ref == "" {
			return
		}
		if !run.Trusted && ref != def { // never saved under the default branch
			sc.ReadRefs = appendNew(sc.ReadRefs, untrustedRef(ref))
		}
		sc.ReadRefs = appendNew(sc.ReadRefs, ref)
	}
	switch {
	case run.Event == eventPullTarget:
		// Read-only, the default branch only.
		if def != "" {
			sc.ReadRefs = []string{def}
		}
		return sc
	case run.Trusted:
		sc.WriteRef = run.Ref
	case def != "" && run.Ref != def:
		sc.WriteRef = untrustedRef(run.Ref)
	}
	read(run.Ref)
	if l.BaseRef != "" {
		read("refs/heads/" + l.BaseRef)
	}
	read(def)
	return sc
}

// appendNew appends ref unless refs has it.
func appendNew(refs []string, ref string) []string {
	if slices.Contains(refs, ref) {
		return refs
	}
	return append(refs, ref)
}
