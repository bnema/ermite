// SPDX-License-Identifier: AGPL-3.0-or-later
package ports_test

import (
	"strings"
	"testing"

	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

func TestValidBlobKey(t *testing.T) {
	for _, v := range []struct {
		key string
		ok  bool
	}{{"ci/00000000000000000001/x", true}, {"a/.tmp-object", true}, {"a.parts/b", true}, {"", false}, {"/a", false}, {"a//b", false}, {"a/../b", false}, {"a/./b", false}, {"é", false}, {strings.Repeat("a", 513), false}} {
		t.Run(v.key, func(t *testing.T) { require.Equal(t, v.ok, ports.ValidBlobKey(v.key)) })
	}
}
