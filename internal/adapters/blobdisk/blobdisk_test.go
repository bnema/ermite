// SPDX-License-Identifier: AGPL-3.0-or-later
package blobdisk_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"bnema.dev/ermite/internal/adapters/blobdisk"
	"bnema.dev/ermite/internal/adapters/blobtest"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

func TestContract(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) ports.BlobStore { return blobdisk.New(t.TempDir()) })
}
func TestCrashTempInvisible(t *testing.T) {
	root := t.TempDir()
	s := blobdisk.New(root)
	ctx := context.Background()
	require.NoError(t, s.Check(ctx))
	require.NoError(t, os.WriteFile(filepath.Join(root, "tmp", ".tmp-crashed"), []byte("partial"), 0600))
	require.NoError(t, s.List(ctx, "", func(ports.BlobInfo) error { t.Fatal("temporary object listed"); return nil }))
	_, e := s.Stat(ctx, ".tmp-crashed")
	require.ErrorIs(t, e, domain.ErrNotFound)
}
