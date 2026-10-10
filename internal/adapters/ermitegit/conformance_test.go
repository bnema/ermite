// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"testing"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

// TestConformance runs the shared RepoReader table against the adapter with
// every method sent to ermite-git, whatever DefaultRoutes says: a method may
// only be routed to ermite-git if it passes here.
func TestConformance(t *testing.T) {
	readertest.Conformance(t, func(t *testing.T) (ports.RepoReader, string) {
		a, _, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
		return a, data
	})
}

// TestConformanceDefaultRoutes runs the same table through the routes of
// production, where some methods are served by the CLI reader.
func TestConformanceDefaultRoutes(t *testing.T) {
	readertest.Conformance(t, func(t *testing.T) (ports.RepoReader, string) {
		a, _, data := newAdapter(t, ermitegit.DefaultRoutes(), nil)
		return a, data
	})
}
