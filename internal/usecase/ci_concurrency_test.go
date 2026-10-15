// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

const concurrencyYAML = `on: push
concurrency: %s
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo
`

func evalGroup(t *testing.T, value string) (domain.CIConcurrency, error) {
	t.Helper()
	wf, errs := workflow.Parse("w.yml", []byte(fmt.Sprintf(concurrencyYAML, value)))
	require.Empty(t, errs)
	gh, err := expr.FromJSON([]byte(`{"ref":"refs/heads/main","workflow":"ci"}`))
	require.NoError(t, err)
	env := template.Env{Contexts: map[string]expr.Value{"github": gh, "inputs": expr.NewDictionary(), "vars": expr.NewDictionary()}}
	return usecase.EvalConcurrency(context.Background(), wf.Concurrency, env)
}

func TestEvalConcurrency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, yaml string
		want       domain.CIConcurrency
		err        string
	}{
		{"a string is the group", `Deploy`, domain.CIConcurrency{Group: "Deploy"}, ""},
		{"an expression is the group", `${{ github.workflow }}-${{ github.ref }}`, domain.CIConcurrency{Group: "ci-refs/heads/main"}, ""},
		{"mapping", "{group: g}", domain.CIConcurrency{Group: "g"}, ""},
		{"cancel true", "{group: g, cancel-in-progress: true}", domain.CIConcurrency{Group: "g", Cancel: true}, ""},
		{"cancel expression", "{group: g, cancel-in-progress: \"${{ github.ref == 'refs/heads/main' }}\"}", domain.CIConcurrency{Group: "g", Cancel: true}, ""},
		{"cancel expression false", "{group: g, cancel-in-progress: \"${{ github.ref != 'refs/heads/main' }}\"}", domain.CIConcurrency{Group: "g"}, ""},
		{"queue single", "{group: g, queue: single}", domain.CIConcurrency{Group: "g"}, ""},
		{"queue max", "{group: g, queue: max}", domain.CIConcurrency{}, "not supported yet: queue: max"},
		{"queue max with cancel true", "{group: g, queue: max, cancel-in-progress: true}", domain.CIConcurrency{}, "'queue: max' cannot be combined with 'cancel-in-progress: true'."},
		{"empty group", `${{ vars.NOPE }}`, domain.CIConcurrency{}, "concurrency group name cannot be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := evalGroup(t, tt.yaml)
			if tt.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	// GitHub's limit counts UTF-16 code units, like C#'s String.Length.
	for _, tc := range []struct {
		name, group string
		ok          bool
	}{
		{"400 BMP characters", strings.Repeat("é", 400), true},
		{"401 BMP characters", strings.Repeat("é", 401), false},
		{"200 emoji are 400 units", strings.Repeat("😀", 200), true},
		{"201 emoji are 402 units", strings.Repeat("😀", 201), false},
		{"199 emoji and 2 letters", strings.Repeat("😀", 199) + "ab", true},
		{"199 emoji and 3 letters", strings.Repeat("😀", 199) + "abc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := evalGroup(t, `"`+tc.group+`"`)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "less than 400 characters")
		})
	}
	t.Run("too long", func(t *testing.T) {
		t.Parallel()
		_, err := evalGroup(t, strings.Repeat("a", 401))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "less than 400 characters")
	})
}

func TestConcurrencyIdentity(t *testing.T) {
	t.Parallel()
	assert.Equal(t, domain.ConcurrencyKey("Deploy"), domain.ConcurrencyKey("DEPLOY"))
	assert.Equal(t, "Canceling since a higher priority waiting request for 'g' exists", domain.ConcurrencyCancelMessage("g"))
	pending := domain.CIJob{Phase: domain.CIPhaseWaiting, Concurrency: domain.CIConcurrency{State: domain.CIConcurrencyPending}}
	assert.Equal(t, domain.CIPending, pending.Status())
	pending.Phase = domain.CIPhaseEnded
	assert.Equal(t, domain.CICompleted, pending.Status(), "an ended job is not pending any more")
	assert.Equal(t, domain.CIQueued, domain.CIJob{Phase: domain.CIPhaseWaiting}.Status())
}
