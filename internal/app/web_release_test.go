// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Releases and web use the production forge wiring.
func TestWebRelease(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.webRepo("demo")
	blobs, releases := h.forge.Blobs, h.forge.Releases
	repo := h.name("demo")
	rel, err := releases.CreateRelease(h.ctx, domain.Root(), repo, ports.CreateReleaseParams{Tag: "v0.2.0", Title: "Demo release", Notes: "## Install\n\n**Download** the binary. <script>alert(1)</script>"})
	require.NoError(t, err)
	payload := "binary release data\x00\xff\n"
	asset, err := releases.UploadAsset(h.ctx, domain.Root(), repo, ports.UploadAssetParams{ReleaseID: rel.ID, Name: "demo.bin", ContentType: "application/octet-stream", Size: int64(len(payload))}, strings.NewReader(payload))
	require.NoError(t, err)
	draft, err := releases.CreateRelease(h.ctx, domain.Root(), repo, ports.CreateReleaseParams{Tag: "draft", Title: "Hidden draft", Draft: true})
	require.NoError(t, err)
	draftAsset, err := releases.UploadAsset(h.ctx, domain.Root(), repo, ports.UploadAssetParams{ReleaseID: draft.ID, Name: "hidden.bin", Size: 1}, strings.NewReader("x"))
	require.NoError(t, err)
	ts := h.srv
	get := func(method, path, byteRange string) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, nil)
		require.NoError(t, err)
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		res, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, res.Header, string(body)
	}
	status, _, body := get("GET", "/demo/releases", "")
	require.Equal(t, 200, status)
	assert.Contains(t, body, "Demo release")
	assert.NotContains(t, body, "Hidden draft")
	status, _, body = get("GET", "/demo/releases/tag/v0.2.0", "")
	require.Equal(t, 200, status)
	assert.Contains(t, body, "<strong>Download</strong>")
	assert.NotContains(t, body, "<script>alert")
	assert.Contains(t, body, "demo.bin")
	assert.Contains(t, body, "/demo/releases/assets/"+strconv.FormatInt(asset.ID, 10))
	for _, path := range []string{"/demo/releases/download/v0.2.0/demo.bin", "/demo/releases/latest/download/demo.bin", "/demo/releases/assets/" + strconv.FormatInt(asset.ID, 10)} {
		status, headers, body := get("GET", path, "")
		require.Equal(t, 200, status)
		assert.Equal(t, payload, body)
		assert.Equal(t, strconv.Itoa(len(payload)), headers.Get("Content-Length"))
		assert.Equal(t, "bytes", headers.Get("Accept-Ranges"))
		assert.Contains(t, headers.Get("Content-Disposition"), "attachment")
		status, headers, body = get("GET", path, "bytes=2-6")
		require.Equal(t, 206, status)
		assert.Equal(t, payload[2:7], body)
		assert.Equal(t, "bytes 2-6/"+strconv.Itoa(len(payload)), headers.Get("Content-Range"))
		assert.Equal(t, "5", headers.Get("Content-Length"))
		status, headers, body = get("HEAD", path, "")
		require.Equal(t, 200, status)
		assert.Empty(t, body)
		assert.Equal(t, strconv.Itoa(len(payload)), headers.Get("Content-Length"))
	}
	for _, path := range []string{"/demo/releases/tag/draft", "/demo/releases/download/draft/hidden.bin", "/demo/releases/assets/" + strconv.FormatInt(draftAsset.ID, 10)} {
		status, _, _ := get("GET", path, "")
		assert.Equal(t, 404, status)
	}
	// Slash-tag page and asset-id download remain unambiguous.
	slash, err := releases.CreateRelease(h.ctx, domain.Root(), repo, ports.CreateReleaseParams{Tag: "series/v1", TargetCommitish: "main", Title: "Slash tag"})
	require.NoError(t, err)
	status, _, body = get("GET", "/demo/releases/tag/series/v1", "")
	assert.Equal(t, 200, status)
	assert.Contains(t, body, slash.Title)
	// A missing blob is a 404 page before any binary headers are written.
	require.NoError(t, blobs.Delete(context.Background(), asset.BlobKey()))
	status, headers, body := get("GET", "/demo/releases/assets/"+strconv.FormatInt(asset.ID, 10), "")
	assert.Equal(t, 404, status)
	assert.Contains(t, headers.Get("Content-Type"), "text/html")
	assert.Contains(t, body, "no such page")
	assert.Empty(t, headers.Get("Content-Disposition"))
}
