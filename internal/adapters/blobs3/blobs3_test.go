// SPDX-License-Identifier: AGPL-3.0-or-later
package blobs3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/adapters/blobs3"
	"bnema.dev/ermite/internal/adapters/blobtest"
	"bnema.dev/ermite/internal/ports"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/require"
)

func fake(t *testing.T) (*blobs3.Store, *s3mem.Backend) {
	t.Helper()
	backend := s3mem.New()
	require.NoError(t, backend.CreateBucket("b"))
	handler := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("S3 operation: %s %s", r.Method, r.URL.RequestURI())
		for _, q := range []string{"policy", "acl", "tagging", "lifecycle", "versioning"} {
			if r.URL.Query().Has(q) {
				t.Errorf("unexpected admin operation %s", r.URL)
			}
		}
		if r.Method == http.MethodPut && strings.Trim(r.URL.Path, "/") == "b" {
			t.Error("unexpected bucket creation")
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/b/") {
			if r.Header.Get("X-Amz-Content-Sha256") == "UNSIGNED-PAYLOAD" || r.Header.Get("X-Amz-Content-Sha256") == "" {
				t.Error("payload must be signed")
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s, e := blobs3.New(context.Background(), blobs3.Config{Bucket: "b", Prefix: "/pfx/", Endpoint: srv.URL, Region: "us-east-1", AccessKeyID: "test", SecretAccessKey: "test-secret", ForcePathStyle: true})
	require.NoError(t, e)
	return s, backend
}
func TestContract(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) ports.BlobStore { s, _ := fake(t); return s })
}
func TestPrefix(t *testing.T) {
	s, b := fake(t)
	_, e := s.Put(context.Background(), "ci/one", strings.NewReader("x"), 1)
	require.NoError(t, e)
	objects, e := b.ListBucket("b", nil, gofakes3.ListBucketPage{})
	require.NoError(t, e)
	require.Len(t, objects.Contents, 1)
	require.Equal(t, "pfx/ci/one", objects.Contents[0].Key)
}
func TestMissingBucket(t *testing.T) {
	s, b := fake(t)
	require.NoError(t, b.DeleteBucket("b"))
	e := s.Check(context.Background())
	require.ErrorContains(t, e, `bucket "b" is not reachable at http://`)
	require.NotContains(t, e.Error(), "test-secret")
}

func TestCheckDeniedHeadFallsBack(t *testing.T) {
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("max-keys") != "1" || r.URL.Query().Get("prefix") != "pfx/" {
			t.Errorf("unexpected probe: %s %s", r.Method, r.URL)
		}
		listed = true
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>b</Name><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	defer srv.Close()
	s, e := blobs3.New(context.Background(), blobs3.Config{Bucket: "b", Prefix: "pfx", Endpoint: srv.URL, Region: "test-region", AccessKeyID: "test", SecretAccessKey: "test-secret", ForcePathStyle: true})
	require.NoError(t, e)
	require.NoError(t, s.Check(context.Background()))
	require.True(t, listed)
}
