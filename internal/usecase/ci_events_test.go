// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/usecase"
)

func dispatchInputs(t *testing.T, inputs string) []workflow.Input {
	t.Helper()
	wf, errs := workflow.Parse("d.yml", []byte("on:\n  workflow_dispatch:\n    inputs:\n"+inputs+"\njobs:\n  j:\n    runs-on: x\n    steps:\n      - run: 'true'\n"))
	require.Empty(t, errs)
	return wf.On.Event("workflow_dispatch").Inputs
}

func TestResolveDispatchInputs(t *testing.T) {
	defs := dispatchInputs(t, `      s: {type: string, required: true}
      b: {type: boolean, default: true}
      n: {type: number, default: 5}
      c: {type: choice, options: [x, y]}
      e: {type: environment}
      o: {description: optional}
`)
	got, err := usecase.ResolveDispatchInputs(defs, map[string]string{"s": "v"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"s": "v", "b": "true", "n": "5", "c": "x"}, got, "defaults, first option of a choice, optional inputs absent")

	got, err = usecase.ResolveDispatchInputs(defs, map[string]string{"s": "v", "b": "false", "n": "-1.5", "c": "y", "e": "prod", "o": ""})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"s": "v", "b": "false", "n": "-1.5", "c": "y", "e": "prod", "o": ""}, got)

	for name, in := range map[string]map[string]string{
		"required missing": {},
		"required empty":   {"s": ""},
		"unknown":          {"s": "v", "zz": "1"},
		"boolean":          {"s": "v", "b": "yes"},
		"number":           {"s": "v", "n": "five"},
		"number hex":       {"s": "v", "n": "0x1"},
		"number nan":       {"s": "v", "n": "NaN"},
		"choice":           {"s": "v", "c": "z"},
		"environment":      {"s": "v", "e": " padded"},
	} {
		_, err := usecase.ResolveDispatchInputs(defs, in)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
}

func TestResolveDispatchInputsLimits(t *testing.T) {
	var spec string
	for i := range 26 {
		spec += fmt.Sprintf("      i%d: {}\n", i)
	}
	_, err := usecase.ResolveDispatchInputs(dispatchInputs(t, spec), nil)
	require.Error(t, err, "more than 25 inputs")
	assert.Contains(t, err.Error(), "at most 25")

	defs := dispatchInputs(t, "      a: {}\n")
	big := make([]byte, 65535)
	for i := range big {
		big[i] = 'x'
	}
	_, err = usecase.ResolveDispatchInputs(defs, map[string]string{"a": string(big)})
	require.Error(t, err, "payload over 65,535 characters")
}
