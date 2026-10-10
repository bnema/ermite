// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"testing"

	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

// TestConformance runs the shared RepoReader table against the CLI adapter,
// the oracle of the others.
func TestConformance(t *testing.T) {
	readertest.Conformance(t, func(t *testing.T) (ports.RepoReader, string) {
		g, data := newReader(t, nil)
		return g, data
	})
}
