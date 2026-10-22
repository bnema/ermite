// SPDX-License-Identifier: AGPL-3.0-or-later

package mediapolicy

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllows(t *testing.T) {
	for _, tc := range []struct {
		address string
		allowed bool
	}{
		{"https://github.com/a.mp4", true},
		{"http://github.com/a.mp4", true},
		{"https://GITHUB.COM:443/a.mp4", true},
		{"http://github.com:80/a.mp4", true},
		{"https://github.com:8080/a.mp4", false},
		{"https://github.com.evil.test/a.mp4", false},
		{"https://evil.test@github.com/a.mp4", false},
		{"//github.com/a.mp4", false},
		{"ftp://github.com/a.mp4", false},
		{"https://user-images.githubusercontent.com/a.mp4", true},
		{"https://private-user-images.githubusercontent.com/a.mp4", true},
		{"https://github-production-user-asset-6210df.s3.amazonaws.com/a.mp4", true},
	} {
		t.Run(tc.address, func(t *testing.T) {
			u, err := url.Parse(tc.address)
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, Allows(u, nil))
		})
	}
	assert.Equal(t, "https://clips.example http://clips.example", Sources([]string{"clips.example"}))
	assert.Empty(t, Sources([]string{}))
}
