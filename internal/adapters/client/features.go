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
)

// UsedFeatures lists the features the CLI uses, in the order they are reported.
func UsedFeatures() []string {
	return []string{FeatureRepoUpdate, FeatureEventsResume}
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
