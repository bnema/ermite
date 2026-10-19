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
	// FeatureIssues is IssueService, RepoSettings.issues and the issue:write
	// scope (`ermite issue`, `ermite repo edit --issues`).
	FeatureIssues = "issues"
	// FeatureSigningKey is ServerService.GetSigningKey (`ermite signing-key`).
	FeatureSigningKey = "signing_key"
	// FeatureSession is SessionService (`ermite web login`, `web logout`).
	FeatureSession = "session"
	// FeaturePasskeys is SessionService.ListPasskeys and RemovePasskey
	// (`ermite web passkey`). A server lists it only when passkeys are
	// available (https public URL with a DNS host name).
	FeaturePasskeys = "passkeys"
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
	// FeatureCISecrets is SecretService, VariableService and EnvironmentService
	// (`ermite secret`, `variable`, `environment`).
	FeatureCISecrets = "ci.secrets"
	// FeatureCIDispatch is RunService.DispatchWorkflow (`ermite workflow run`).
	FeatureCIDispatch = "ci.dispatch"
	// FeatureCIWorkflows is RunService.ListWorkflows, DisableWorkflow and
	// EnableWorkflow (`ermite workflow list|disable|enable`).
	FeatureCIWorkflows = "ci.workflows"
	// FeatureCIArtifacts is ArtifactService (`ermite run artifacts`,
	// `run download`).
	FeatureCIArtifacts = "ci.artifacts"
	// FeatureCommitSignatures is TrustedKeyService and Commit.verification
	// (`ermite signing-key trust`).
	FeatureCommitSignatures = "commit_signatures"
	// FeatureMergeability is MergeRequest.mergeability and the
	// merge_request_mergeability event.
	FeatureMergeability = "merge_requests.mergeability"
)

// UsedFeatures lists the features the CLI uses, in the order they are reported.
func UsedFeatures() []string {
	return []string{FeatureRepoUpdate, FeatureEventsResume, FeatureMergeRequests, FeatureMergeDeleteBranch, FeatureIssues, FeatureMergeability, FeatureSigningKey, FeatureSession, FeatureTokenBranchPrefixes, FeatureCIRuns, FeatureCIRunners, FeatureCISecrets, FeatureCIDispatch, FeatureCIWorkflows, FeatureCIArtifacts, FeaturePasskeys, FeatureCommitSignatures}
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
