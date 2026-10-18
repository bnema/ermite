// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli_test

import (
	"context"
	"testing"

	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/readertest"
)

func TestReadCommitObjects(t *testing.T) {
	readertest.CommitObjects(t, func(t *testing.T) (ports.CommitObjectReader, string) {
		g, data := newGit(t)
		t.Cleanup(func() { _ = g.Close(context.Background()) })
		return g, data
	})
}
