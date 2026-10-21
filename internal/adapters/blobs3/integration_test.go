// SPDX-License-Identifier: AGPL-3.0-or-later
package blobs3_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"bnema.dev/ermite/internal/adapters/blobs3"
	"bnema.dev/ermite/internal/adapters/blobtest"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

func TestIntegration_Contract(t *testing.T) {
	get := func(k string) string { return os.Getenv("ERMITE_TEST_S3_" + k) }
	if get("BUCKET") == "" || get("ACCESS_KEY_ID") == "" || get("SECRET_ACCESS_KEY") == "" {
		t.Skip("set ERMITE_TEST_S3_* to run")
	}
	region := get("REGION")
	if region == "" {
		region = "us-east-1"
	}
	blobtest.Run(t, func(t *testing.T) ports.BlobStore {
		b := make([]byte, 16)
		_, e := rand.Read(b)
		require.NoError(t, e)
		s, e := blobs3.New(context.Background(), blobs3.Config{Bucket: get("BUCKET"), Endpoint: get("ENDPOINT"), Region: region, AccessKeyID: get("ACCESS_KEY_ID"), SecretAccessKey: get("SECRET_ACCESS_KEY"), ForcePathStyle: get("FORCE_PATH_STYLE") == "1", Prefix: "ermite-test/" + hex.EncodeToString(b) + "/"})
		require.NoError(t, e)
		t.Cleanup(func() {
			ctx := context.Background()
			require.NoError(t, s.List(ctx, "", func(i ports.BlobInfo) error { return s.Delete(ctx, i.Key) }))
			require.NoError(t, s.ListMultipart(ctx, "", func(i ports.MultipartInfo) error { return s.AbortMultipart(ctx, i.Key, i.ID) }))
		})
		return s
	})
}
