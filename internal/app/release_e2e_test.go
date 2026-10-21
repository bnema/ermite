// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"bnema.dev/ermite/internal/adapters/blobdisk"
	"bnema.dev/ermite/internal/adapters/ciartifacts"
	"bnema.dev/ermite/internal/config"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReleaseBlobForge(t *testing.T) {
	for _, backend := range []string{"disk", "s3"} {
		t.Run(backend, func(t *testing.T) {
			env := map[string]string{}
			var admin atomic.Int64
			if backend == "s3" {
				mem := s3mem.New()
				require.NoError(t, mem.CreateBucket("assets"))
				fake := gofakes3.New(mem).Server()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Allow only the startup probe/list and object/multipart subset. No bucket administration.
					bucket := r.URL.Path == "/assets" || r.URL.Path == "/assets/"
					allowed := true
					if bucket {
						allowed = r.Method == "HEAD" || (r.Method == "GET" && (r.URL.Query().Get("list-type") == "2" || r.URL.Query().Has("uploads")))
					}
					for _, key := range []string{"acl", "policy", "tagging", "lifecycle", "versioning", "cors", "notification", "encryption", "website", "logging"} {
						if r.URL.Query().Has(key) {
							allowed = false
						}
					}
					if !allowed {
						admin.Add(1)
						http.Error(w, "unexpected bucket operation", 500)
						return
					}
					fake.ServeHTTP(w, r)
				}))
				t.Cleanup(srv.Close)
				env = map[string]string{config.EnvS3Bucket: "assets", config.EnvS3Endpoint: srv.URL, config.EnvS3Region: "us-east-1", config.EnvS3AccessKeyID: "test-private-id", config.EnvS3SecretAccessKey: "test-private-secret", config.EnvS3ForcePathStyle: "1", config.EnvS3Prefix: "forge/"}
			}
			h := newHarness(t, harnessOpts{env: env})
			h.publicRepo("demo", "README.md")
			rel, err := h.forge.Releases.CreateRelease(h.ctx, domain.Root(), h.name("demo"), ports.CreateReleaseParams{Tag: "v1", TargetCommitish: "main", Title: "Version one"})
			require.NoError(t, err)
			asset, err := h.forge.Releases.UploadAsset(h.ctx, domain.Root(), h.name("demo"), ports.UploadAssetParams{ReleaseID: rel.ID, Name: "demo.bin", Size: 7}, strings.NewReader("payload"))
			require.NoError(t, err)
			if backend == "disk" {
				data, err := os.ReadFile(filepath.Join(blobdisk.Dir(h.data), "objects", filepath.FromSlash(asset.BlobKey())))
				require.NoError(t, err)
				assert.Equal(t, "payload", string(data))
			}
			req, err := http.NewRequestWithContext(h.ctx, "GET", h.url(fmt.Sprintf("/-/github/api/repos/owner/demo/releases/assets/%d", asset.ID)), nil)
			require.NoError(t, err)
			req.Header.Set("Accept", "application/octet-stream")
			req.Header.Set("Range", "bytes=1-3")
			res, err := h.srv.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			res.Body.Close()
			assert.Equal(t, 206, res.StatusCode)
			assert.Equal(t, "ayl", string(body))
			assert.Empty(t, res.Header.Get("Location"))
			res, err = h.srv.Client().Get(h.url(fmt.Sprintf("/-/github/api/repos/owner/demo/releases/%d", rel.ID)))
			require.NoError(t, err)
			var obj map[string]any
			require.NoError(t, json.NewDecoder(res.Body).Decode(&obj))
			res.Body.Close()
			assert.Equal(t, 200, res.StatusCode)
			assert.Contains(t, obj["upload_url"], "http://example.test/-/github/api/uploads/")
			// New artifacts use the same production backend, not a test-local blob store.
			wt := h.workTree("main")
			h.mustGit(wt, "fetch", "-q", h.urlAuth(adminToken, "/demo.git"), "main")
			h.mustGit(wt, "reset", "--hard", "FETCH_HEAD")
			c := &ciHarness{harness: h, root: h.api(adminToken), repo: "demo", wt: wt}
			runner := c.registerRunner("alpha", "ubuntu-latest")
			_, job, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
			up := c.jobArtifacts(job)
			a := up.upload("dist", []byte("artifact bytes"))
			_, data, err := up.download(a.GetId(), 0)
			require.NoError(t, err)
			assert.Equal(t, "artifact bytes", string(data))
			record, err := h.forge.store.Artifacts().GetArtifact(h.ctx, a.GetId())
			require.NoError(t, err)
			assert.Equal(t, domain.CIArtifactStoreBlob, record.Store)
			_, err = h.forge.Blobs.Stat(h.ctx, record.BlobKey())
			require.NoError(t, err)
			assert.Contains(t, h.logs.String(), "blob store ready")
			assert.NotContains(t, h.logs.String(), "test-private-")
			assert.Zero(t, admin.Load(), "no bucket-admin operations")
		})
	}
}

func TestBlobStartupProbe(t *testing.T) {
	_, err := blobStore(context.Background(), config.Config{S3Bucket: "missing", S3Region: "us-east-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.EnvS3Bucket)
	mem := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(mem).Server())
	defer srv.Close()
	_, err = blobStore(context.Background(), config.Config{S3Bucket: "missing", S3Endpoint: srv.URL, S3Region: "us-east-1", S3AccessKeyID: "private-id", S3SecretAccessKey: "private-secret", S3ForcePathStyle: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ERMITE_S3_BUCKET")
	assert.Contains(t, err.Error(), "not reachable")
	assert.NotContains(t, err.Error(), "private-")
}

func TestArtifactLegacyProductionWiring(t *testing.T) {
	// Seed the old tree before forge startup, exactly as existing installations do.
	data, err := os.MkdirTemp("", "ermite-legacy")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(data) })
	require.NoError(t, prepareDataDir(data))
	require.NoError(t, os.MkdirAll(ciartifacts.Dir(data), 0700))
	h := newHarness(t, harnessOpts{env: map[string]string{config.EnvDataDir: data}})
	c := &ciHarness{harness: h, root: h.api(adminToken), repo: "proj", wt: h.workTree("main")}
	_, err = h.forge.Repos.Create(h.ctx, domain.Root(), ports.CreateRepoParams{Name: h.name("proj"), Visibility: domain.VisibilityPrivate})
	require.NoError(t, err)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, job, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(job)
	created := up.upload("old", []byte("legacy bytes"))
	store := h.forge.store.Artifacts()
	a, err := store.GetArtifact(h.ctx, created.GetId())
	require.NoError(t, err)
	path := filepath.Join(ciartifacts.Dir(data), fmt.Sprintf("%020d", a.RunID), fmt.Sprintf("%020d", a.ID))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("legacy bytes"), 0600))
	require.NoError(t, h.forge.Blobs.Delete(h.ctx, a.BlobKey()))
	a.Store = domain.CIArtifactStoreLegacy
	used, version, err := store.ArtifactUsage(h.ctx, a.Repo)
	require.NoError(t, err)
	require.NoError(t, store.ApplyArtifacts(h.ctx, ports.CIArtifactWrite{Repo: a.Repo, Used: used, UsageVersion: version, Put: []domain.CIArtifact{a}}))
	_, body, err := up.download(a.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "legacy bytes", string(body))
	// A fresh install does not need or create the legacy tree.
	fresh := newHarnessWith(t, harnessOpts{}, func(o *options) { o.ci = func(cfg *usecase.CIConfig) { assert.Nil(t, cfg.ArtifactFiles) } })
	assert.NoDirExists(t, ciartifacts.Dir(fresh.data))
}
