// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit_test

import (
	"testing"

	"bnema.dev/ermite/internal/adapters/ermitegit"
	"bnema.dev/ermite/internal/readertest"
)

// TestCompareWithCLI asks the CLI reader and the adapter, with every method on
// ermite-git, the same questions about each fixture and requires the same
// answers, errors included.
func TestCompareWithCLI(t *testing.T) {
	for _, c := range readertest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
			f := c.Build(t, data)
			readertest.Compare(t, g, a, f.Name, f.Bare, readertest.SmallBudget)
		})
	}
}

// TestCompareDefaultRoutes is the comparison through the routes of production.
func TestCompareDefaultRoutes(t *testing.T) {
	for _, c := range readertest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			a, g, data := newAdapter(t, ermitegit.DefaultRoutes(), nil)
			f := c.Build(t, data)
			readertest.Compare(t, g, a, f.Name, f.Bare, readertest.SmallBudget)
		})
	}
}

// TestCompareWithCLIOnBenchRepo is the same comparison on a real repository
// (ERMITE_GIT_BENCH_REPO, for instance lazygit): clone, maintenance as ermite
// runs it, then the questions on refs, trees with last commits, blobs and
// pages of history.
func TestCompareWithCLIOnBenchRepo(t *testing.T) {
	a, g, data := newAdapter(t, ermitegit.AllRoutes(ermitegit.ErmiteGit), nil)
	repo := cloneBench(t, g, data)
	readertest.Compare(t, g, a, repo.name, repo.dir, readertest.LargeBudget)
}
