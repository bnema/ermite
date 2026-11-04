// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBlobLastChangeMetadata(t *testing.T) {
	body := renderToString(t, Blob(Layout{}, BlobPage{Last: &CommitRow{
		Short: "abc1234", CHref: "/r/commit/abc1234", Subject: "Fix a bug", Author: "owner",
		When: "2 hours ago", WhenAt: "2026-11-04T03:00:00Z",
	}}))
	assert.Contains(t, body, `<a class="mono" href="/r/commit/abc1234">abc1234</a>`)
	assert.Contains(t, body, `title="Fix a bug">Fix a bug</a>`)
	assert.Contains(t, body, `<time datetime="2026-11-04T03:00:00Z" title="2026-11-04 03:00 UTC">2 hours ago</time>`)
	assert.Equal(t, "", blobTimeTitle(""))
}
