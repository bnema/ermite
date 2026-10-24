// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchivedRepositoryPushEndToEnd(t *testing.T) {
	c := newCLIHarness(t)
	c.cli(t, adminToken, "repo", "create", "archived").ok(t)
	wt := c.workTree("main", "README.md")
	remote := c.urlAuth(adminToken, "/archived.git")
	c.mustGit(wt, "push", "-q", remote, "main")
	c.cli(t, adminToken, "repo", "edit", "archived", "--archived").ok(t)
	c.mustGit(wt, "commit", "--allow-empty", "-qm", "After archiving")
	out, err := c.git(wt, "push", remote, "main")
	require.Error(t, err, out)
	require.Contains(t, out, "repository is archived")
	c.cli(t, adminToken, "repo", "edit", "archived", "--archived=false").ok(t)
	c.mustGit(wt, "push", "-q", remote, "main")
}
