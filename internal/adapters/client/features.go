// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import "slices"

// Contract features this client relies on, as listed by
// ServerService.GetVersion. A server without one still works for everything
// else: the CLI warns instead of failing.
const (
	// FeatureRepoUpdate is RepoService.UpdateRepo (`ermite repo edit`).
	FeatureRepoUpdate = "repo.update"
	// FeatureEventsResume is EventService.Watch with resume_after, which
	// `ermite watch` uses to continue after a reconnection.
	FeatureEventsResume = "events.resume"
	// FeatureMergeRequests is MergeRequestService (`ermite mr`).
	FeatureMergeRequests = "merge_requests"
	// FeatureMergeDeleteBranch is MergeMergeRequest with delete_source_branch
	// (`ermite mr merge --delete-branch`).
	FeatureMergeDeleteBranch = "merge_requests.delete_branch"
	// FeatureSigningKey is ServerService.GetSigningKey (`ermite signing-key`).
	FeatureSigningKey = "signing_key"
	// FeatureSession is SessionService (`ermite web login`, `web logout`).
	FeatureSession = "session"
	// FeatureTokenBranchPrefixes is Token.branch_prefixes and
	// CreateTokenRequest.branch_prefixes (`ermite token create --branch-prefix`).
	// A server without it ignores the field and would create an unlimited
	// token: `token create --branch-prefix` reads the features first and
	// refuses when it is missing (the main guard), and also checks the token it
	// gets back (a backstop).
	FeatureTokenBranchPrefixes = "token.branch_prefixes"
	// FeatureCIRuns is RunService (`ermite run`), FeatureCIRunners its runner
	// calls (`ermite runner`).
	FeatureCIRuns    = "ci.runs"
	FeatureCIRunners = "ci.runners"
)

// UsedFeatures lists the features the CLI uses, in the order they are reported.
func UsedFeatures() []string {
	return []string{FeatureRepoUpdate, FeatureEventsResume, FeatureMergeRequests, FeatureMergeDeleteBranch, FeatureSigningKey, FeatureSession, FeatureTokenBranchPrefixes, FeatureCIRuns, FeatureCIRunners}
}

// MissingFeatures returns the features the CLI uses that have is lacking.
func MissingFeatures(have []string) []string {
	var missing []string
	for _, f := range UsedFeatures() {
		if !slices.Contains(have, f) {
			missing = append(missing, f)
		}
	}
	return missing
}
