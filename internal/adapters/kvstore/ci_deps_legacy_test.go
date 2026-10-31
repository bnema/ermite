// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestDecodeLegacyDepsJob(t *testing.T) {
	old := &storagepb.CIDepsJob{GoVersion: "1.24", Updates: []*storagepb.DepRef{{Ecosystem: "gomod", Name: "example.com/lib", Version: "v1.2.0"}}}
	data, err := proto.Marshal(old)
	require.NoError(t, err)
	var stored storagepb.CIDepsJob
	require.NoError(t, proto.Unmarshal(data, &stored))
	got := decodeDepsJob(&stored)
	require.Equal(t, []domain.CIDepsUpdate{{Ref: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Name: "example.com/lib", Version: "v1.2.0"}}}, got.Details)
	require.Empty(t, encodeDepsJob(got).GetUpdates())
	require.Len(t, encodeDepsJob(got).GetDetails(), 1)
}
