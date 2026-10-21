// SPDX-License-Identifier: AGPL-3.0-or-later
package config_test

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func s3Env() map[string]string {
	return map[string]string{
		config.EnvS3Bucket: "bucket", config.EnvS3Region: "us-east-1",
		config.EnvS3AccessKeyID: "private-id", config.EnvS3SecretAccessKey: "private-secret",
	}
}
func TestStorageConfig(t *testing.T) {
	c, err := config.Load(lookup(valid(nil)))
	require.NoError(t, err)
	assert.Empty(t, c.S3Bucket)
	env := s3Env()
	env[config.EnvS3Endpoint] = "http://localhost:9000/"
	env[config.EnvS3Prefix] = "/forge/blobs/"
	env[config.EnvS3ForcePathStyle] = "1"
	env[config.EnvReleaseAssetMax] = "512MiB"
	env[config.EnvReleaseQuota] = "10GiB"
	env[config.EnvReleaseAssetsMax] = "7"
	c, err = config.Load(lookup(valid(env)))
	require.NoError(t, err)
	assert.Equal(t, "forge/blobs/", c.S3Prefix)
	assert.Equal(t, "http://localhost:9000", c.S3Endpoint)
	assert.True(t, c.S3ForcePathStyle)
	assert.EqualValues(t, 512<<20, c.ReleaseAssetMax)
	assert.EqualValues(t, 10<<30, c.ReleaseQuota)
	assert.Equal(t, 7, c.ReleaseAssetsMax)
	for _, key := range []string{config.EnvS3Region, config.EnvS3AccessKeyID, config.EnvS3SecretAccessKey} {
		t.Run(key, func(t *testing.T) {
			env := s3Env()
			env[key] = ""
			_, err := config.Load(lookup(valid(env)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.NotContains(t, err.Error(), "private-")
		})
	}
	for key, value := range map[string]string{config.EnvS3Endpoint: "https://private-id:private-secret@example.org", config.EnvS3ForcePathStyle: "yes", config.EnvS3Prefix: "x/../y", config.EnvReleaseAssetMax: "0", config.EnvReleaseQuota: "-1", config.EnvReleaseAssetsMax: "0"} {
		t.Run(key+" invalid", func(t *testing.T) {
			env := s3Env()
			env[key] = value
			_, err := config.Load(lookup(valid(env)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			assert.NotContains(t, err.Error(), "private-")
		})
	}
	for _, value := range []string{"0", "1", "false", "true"} {
		env := s3Env()
		env[config.EnvS3ForcePathStyle] = value
		_, err := config.Load(lookup(valid(env)))
		require.NoError(t, err)
	}
}
func TestSecretFiles(t *testing.T) {
	for _, key := range []string{config.EnvAdminToken, config.EnvSecretsKey, config.EnvS3AccessKeyID, config.EnvS3SecretAccessKey} {
		t.Run(key, func(t *testing.T) {
			secret := strings.Repeat("s", 32)
			if key == config.EnvSecretsKey {
				secret = base64.StdEncoding.EncodeToString([]byte(secret))
			}
			path := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(path, []byte(secret+"\r\n"), 0600))
			env := valid(s3Env())
			env[key] = ""
			env[key+"_FILE"] = path
			c, err := config.Load(lookup(env))
			require.NoError(t, err)
			switch key {
			case config.EnvAdminToken:
				assert.Equal(t, sha256.Sum256([]byte(secret)), c.AdminTokenHash)
			case config.EnvSecretsKey:
				assert.Len(t, c.SecretsKey, 32)
			case config.EnvS3AccessKeyID:
				assert.Equal(t, secret, c.S3AccessKeyID)
			case config.EnvS3SecretAccessKey:
				assert.Equal(t, secret, c.S3SecretAccessKey)
			}
			env[key] = secret
			_, err = config.Load(lookup(env))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "both")
			assert.NotContains(t, err.Error(), secret)
			env[key] = ""
			env[key+"_FILE"] = path + "missing"
			_, err = config.Load(lookup(env))
			require.Error(t, err)
			assert.Contains(t, err.Error(), key+"_FILE")
			assert.NotContains(t, err.Error(), path)
			require.NoError(t, os.WriteFile(path, []byte("\n"), 0600))
			env[key+"_FILE"] = path
			_, err = config.Load(lookup(env))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "empty")
		})
	}
}
