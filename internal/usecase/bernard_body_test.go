// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bnema.dev/ermite/internal/domain"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDepsMRBodySideEffects(t *testing.T) {
	ups := []domain.DepUpdate{{DepRef: domain.DepRef{Name: "example.org/a", Version: "v1.1.0"}, Current: "v1.0.0"}}
	require.Equal(t, domain.BotCommitBody(ups), depsMRBody(ups, nil))
	require.Equal(t, "- example.org/a v1.0.0 → v1.1.0\n\n## Also changed by Go\n- `example.org/b` v1.0.0 → v1.2.0\n- `example.org/c` removed, no longer needed (sub)\n", depsMRBody(ups, []domain.DepSideEffect{{Name: "example.org/b", From: "v1.0.0", To: "v1.2.0"}, {Directory: "sub", Name: "example.org/c", From: "v1.0.0"}}))
}
