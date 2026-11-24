// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
)

func TestIsolationRoundTrip(t *testing.T) {
	for _, i := range []domain.CIIsolation{
		domain.CIIsolationVM,
		domain.CIIsolationContainerSysbox,
		domain.CIIsolationContainerPrivileged,
		domain.CIIsolationContainerRootless,
	} {
		p := encodeIsolation(i)
		assert.NotEqual(t, storagepb.CIIsolation_CI_ISOLATION_UNSPECIFIED, p, i)
		assert.Equal(t, i, decodeIsolation(p), i)
	}
	assert.Equal(t, storagepb.CIIsolation_CI_ISOLATION_CONTAINER_ROOTLESS, encodeIsolation(domain.CIIsolationContainerRootless))
	assert.Equal(t, storagepb.CIIsolation_CI_ISOLATION_UNSPECIFIED, encodeIsolation("unknown"))
}
