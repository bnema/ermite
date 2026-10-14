// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"bnema.dev/ermite/internal/usecase"
)

// Which secrets a job refers to: a name by dot or bracket, anything else is all.
func TestSecretsUsedBy(t *testing.T) {
	t.Parallel()
	steps := func(run string) string {
		return "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: " + run + "\n"
	}
	cases := []struct {
		name string
		yaml string
		want usecase.SecretUse
	}{
		{"none", steps("echo hi"), usecase.SecretUse{}},
		{"dot", steps("echo ${{ secrets.Token }}"), usecase.SecretUse{Names: []string{"TOKEN"}}},
		{"bracket", steps(`echo "${{ secrets['my_Token'] }}"`), usecase.SecretUse{Names: []string{"MY_TOKEN"}}},
		{"bracket with double quotes is not an expression string", steps(`echo '${{ secrets["x"] }}'`), usecase.SecretUse{All: true}},
		{"several, sorted, deduplicated", steps("echo ${{ secrets.B }} ${{ secrets.a }} ${{ secrets.B }}"), usecase.SecretUse{Names: []string{"A", "B"}}},
		{"in a condition", steps("echo\n        if: ${{ secrets.FLAG != '' }}"), usecase.SecretUse{Names: []string{"FLAG"}}},
		{"in a function", steps("echo ${{ format('{0}', secrets.X) }}"), usecase.SecretUse{Names: []string{"X"}}},
		{"the context name is case-insensitive", steps("echo ${{ SECRETS.x }}"), usecase.SecretUse{Names: []string{"X"}}},
		{"bare context", steps("echo ${{ secrets }}"), usecase.SecretUse{All: true}},
		{"toJSON(secrets)", steps("echo ${{ toJSON(secrets) }}"), usecase.SecretUse{All: true}},
		{"computed index", steps("echo ${{ secrets[matrix.name] }}"), usecase.SecretUse{All: true}},
		{"star", steps("echo ${{ secrets.* }}"), usecase.SecretUse{All: true}},
		{"a named secret next to a bare use", steps("echo ${{ secrets.A }} ${{ join(secrets) }}"), usecase.SecretUse{All: true, Names: []string{"A"}}},
		{"a string literal that says secrets is not a use", steps("echo ${{ 'secrets.NOT' }}"), usecase.SecretUse{}},
		{"text outside the braces is not a use", steps("echo secrets.NOT"), usecase.SecretUse{}},
		{"an expression with a quoted brace", steps("echo ${{ format('}}', secrets.Q) }}"), usecase.SecretUse{Names: []string{"Q"}}},
		{"does not parse but mentions secrets", steps("echo ${{ secrets.A == }}"), usecase.SecretUse{All: true}},
		{"does not parse and does not mention them", steps("echo ${{ == }}"), usecase.SecretUse{}},
		{"not YAML", "on: [", usecase.SecretUse{All: true}},
		{"not a mapping", "- a\n- b\n", usecase.SecretUse{All: true}},
		{"empty", "", usecase.SecretUse{All: true}},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, usecase.SecretsUsedBy(tc.yaml, "a"), tc.name)
	}
}

func TestSecretsUsedByCoversTheWholeSubtreeAndWorkflowEnvOnly(t *testing.T) {
	t.Parallel()
	src := `on: push
env:
  WF: ${{ secrets.WORKFLOW_LEVEL }}
defaults:
  run:
    shell: bash
jobs:
  a:
    runs-on: ${{ secrets.IN_RUNS_ON }}
    name: ${{ secrets.IN_NAME }}
    env:
      E: ${{ secrets.JOB_ENV }}
    container:
      image: x
      credentials:
        password: ${{ secrets.REGISTRY }}
    services:
      db:
        image: y
        env:
          P: ${{ secrets.SERVICE }}
    outputs:
      o: ${{ secrets.OUTPUT }}
    strategy:
      matrix:
        k: [ "${{ secrets.MATRIX }}" ]
    steps:
      - uses: some/action@v1
        with:
          token: ${{ secrets.WITH_INPUT }}
      - run: echo
        env: { K: "${{ secrets.FLOW_STYLE }}" }
  b:
    runs-on: x
    steps:
      - run: echo ${{ secrets.OTHER_JOB }}
`
	got := usecase.SecretsUsedBy(src, "a")
	assert.False(t, got.All)
	assert.Equal(t, []string{
		"FLOW_STYLE", "IN_NAME", "IN_RUNS_ON", "JOB_ENV", "MATRIX", "OUTPUT", "REGISTRY", "SERVICE", "WITH_INPUT", "WORKFLOW_LEVEL",
	}, got.Names, "every expression of the job and of the workflow env; not another job's")
	assert.Equal(t, []string{"OTHER_JOB", "WORKFLOW_LEVEL"}, usecase.SecretsUsedBy(src, "b").Names)
	assert.Equal(t, []string{"WORKFLOW_LEVEL"}, usecase.SecretsUsedBy(src, "missing").Names)
}

// An anchor defined in another job (or reached through a chain of aliases) is
// followed: the job that aliases it refers to its secrets.
func TestSecretsUsedByFollowsYAMLAliases(t *testing.T) {
	t.Parallel()
	src := `on: push
jobs:
  b:
    runs-on: x
    env: &shared
      K: ${{ secrets.FROM_OTHER_JOB }}
    steps:
      - run: echo
  a:
    runs-on: x
    env: *shared
    steps:
      - run: echo
        env: &chain
          J: ${{ secrets.CHAIN_ROOT }}
      - run: echo
        env: *chain
  c:
    runs-on: x
    steps:
      - run: echo
        with: { list: [ *shared, *chain ] }
`
	assert.Equal(t, []string{"FROM_OTHER_JOB"}, usecase.SecretsUsedBy(src, "b").Names)
	assert.Equal(t, []string{"CHAIN_ROOT", "FROM_OTHER_JOB"}, usecase.SecretsUsedBy(src, "a").Names, "anchor from another job, and an anchor used twice")
	assert.Equal(t, []string{"CHAIN_ROOT", "FROM_OTHER_JOB"}, usecase.SecretsUsedBy(src, "c").Names, "aliases inside a flow sequence")
}

// Deeply nested YAML is read as "all secrets" rather than followed forever.
func TestSecretsUsedByDeepNestingIsAll(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: echo\n        env: ")
	b.WriteString(strings.Repeat("[", 400) + strings.Repeat("]", 400) + "\n")
	assert.True(t, usecase.SecretsUsedBy(b.String(), "a").All)
}

// The analysis is made once per run and job, kept apart from the run it
// belongs to, and dropped with it. It holds names only.
func TestSecretUseIsCachedPerRunAndJob(t *testing.T) {
	src := "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - run: echo ${{ secrets.A }}\n  b:\n    runs-on: x\n    steps:\n      - run: echo ${{ secrets.B }}\n"
	c := usecase.NewSecretUseCache(4)
	before := usecase.SecretWalks()
	for range 5 {
		assert.Equal(t, []string{"A"}, c.Use(1, src, "a").Names)
	}
	assert.EqualValues(t, 1, usecase.SecretWalks()-before, "five legs, one walk")
	assert.Equal(t, []string{"B"}, c.Use(1, src, "b").Names, "another job of the run has its own")
	assert.Equal(t, []string{"A"}, c.Use(2, src, "a").Names, "another run too")
	assert.EqualValues(t, 3, usecase.SecretWalks()-before)
	c.Drop(1)
	c.Use(1, src, "a")
	assert.EqualValues(t, 4, usecase.SecretWalks()-before, "dropped with the run")
}
