// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/domain"
)

func TestSettingsSecretsPerScopeAreSortedAndIsolated(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	other := name(t, "other")
	require.NoError(t, s.Create(ctx, domain.Repo{Name: other, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	_, err := st.CreateEnvironment(ctx, repo, "Prod", created)
	require.NoError(t, err)

	inst, repoScope, env := domain.InstanceScope(), domain.RepoScope(repo), domain.EnvironmentScope(repo, "prod")
	for _, c := range []struct {
		scope domain.SettingsScope
		name  string
	}{{inst, "B"}, {inst, "A"}, {repoScope, "R"}, {env, "E"}, {domain.RepoScope(other), "O"}} {
		_, err := st.PutSecret(ctx, c.scope, c.name, []byte("sealed-"+c.name), created)
		require.NoError(t, err)
	}
	list := func(sc domain.SettingsScope) []string {
		got, err := st.ListSealedSecrets(ctx, sc)
		require.NoError(t, err)
		var out []string
		for _, g := range got {
			out = append(out, g.Name+"="+string(g.Sealed))
		}
		return out
	}
	assert.Equal(t, []string{"A=sealed-A", "B=sealed-B"}, list(inst))
	assert.Equal(t, []string{"R=sealed-R"}, list(repoScope))
	assert.Equal(t, []string{"E=sealed-E"}, list(domain.EnvironmentScope(repo, "PROD")), "an environment is found without regard to case")
	assert.Equal(t, []string{"O=sealed-O"}, list(domain.RepoScope(other)))

	_, err = st.PutSecret(ctx, inst, "A", []byte("new"), created.Add(1))
	require.NoError(t, err)
	assert.Equal(t, []string{"A=new", "B=sealed-B"}, list(inst), "replaced")

	require.NoError(t, st.DeleteSecret(ctx, inst, "A"))
	assert.ErrorIs(t, st.DeleteSecret(ctx, inst, "A"), domain.ErrNotFound)
	assert.Equal(t, []string{"B=sealed-B"}, list(inst))
}

func TestSettingsMissingRepositoryOrEnvironmentIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	ghost := name(t, "ghost")
	for _, sc := range []domain.SettingsScope{domain.RepoScope(ghost), domain.EnvironmentScope(repo, "nope"), domain.EnvironmentScope(ghost, "x")} {
		_, err := st.PutSecret(ctx, sc, "A", []byte("x"), created)
		assert.ErrorIs(t, err, domain.ErrNotFound, sc.Key())
		_, err = st.PutVariable(ctx, sc, "A", "x", created)
		assert.ErrorIs(t, err, domain.ErrNotFound, sc.Key())
		_, err = st.ListSealedSecrets(ctx, sc)
		assert.ErrorIs(t, err, domain.ErrNotFound, sc.Key())
		_, err = st.ListVariables(ctx, sc)
		assert.ErrorIs(t, err, domain.ErrNotFound, sc.Key())
	}
	_, err := st.CreateEnvironment(ctx, ghost, "x", created)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.ListEnvironments(ctx, ghost)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, st.DeleteEnvironment(ctx, repo, "nope"), domain.ErrNotFound)
	assert.Error(t, func() error { _, err := st.PutSecret(ctx, domain.SettingsScope{}, "A", nil, created); return err }(), "an invalid scope")
}

func TestSettingsVariablesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	sc := domain.RepoScope(repo)
	_, err := st.PutVariable(ctx, sc, "Z", "", created)
	require.NoError(t, err)
	_, err = st.PutVariable(ctx, sc, "A", "multi\nline", created.Add(1))
	require.NoError(t, err)
	got, err := st.GetVariable(ctx, sc, "A")
	require.NoError(t, err)
	assert.Equal(t, domain.Variable{Name: "A", Value: "multi\nline", UpdatedAt: created.Add(1)}, got)
	_, err = st.GetVariable(ctx, sc, "NONE")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	all, err := st.ListVariables(ctx, sc)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "A", all[0].Name)
	assert.Equal(t, "", all[1].Value, "an empty value is kept")
	require.NoError(t, st.DeleteVariable(ctx, sc, "A"))
	assert.ErrorIs(t, st.DeleteVariable(ctx, sc, "A"), domain.ErrNotFound)
}

func TestSettingsEnvironmentsAreCaseInsensitiveAndDeleteTheirContent(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	e, err := st.CreateEnvironment(ctx, repo, "Prod", created)
	require.NoError(t, err)
	assert.Equal(t, "Prod", e.Name)
	again, err := st.CreateEnvironment(ctx, repo, "PROD", created.Add(1))
	require.NoError(t, err)
	assert.Equal(t, domain.Environment{Repo: repo, Name: "Prod", CreatedAt: created}, again, "the existing one, unchanged")
	_, err = st.CreateEnvironment(ctx, repo, "a/b", created)
	require.NoError(t, err)
	_, err = st.CreateEnvironment(ctx, repo, "a", created)
	require.NoError(t, err)

	sc := domain.EnvironmentScope(repo, "prod")
	_, err = st.PutSecret(ctx, sc, "S", []byte("x"), created)
	require.NoError(t, err)
	_, err = st.PutVariable(ctx, sc, "V", "v", created)
	require.NoError(t, err)
	_, err = st.PutSecret(ctx, domain.EnvironmentScope(repo, "a"), "KEEP", []byte("x"), created)
	require.NoError(t, err)
	_, err = st.PutSecret(ctx, domain.EnvironmentScope(repo, "a/b"), "KEEP", []byte("x"), created)
	require.NoError(t, err)

	all, err := st.ListEnvironments(ctx, repo)
	require.NoError(t, err)
	require.Len(t, all, 3)

	require.NoError(t, st.DeleteEnvironment(ctx, repo, "PROD"))
	_, err = st.ListSealedSecrets(ctx, sc)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	// A write to the deleted environment cannot leave a stray value.
	_, err = st.PutSecret(ctx, sc, "S", []byte("x"), created)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	// Its neighbours, whose names share a prefix with it, are untouched.
	for _, env := range []string{"a", "a/b"} {
		got, err := st.ListSealedSecrets(ctx, domain.EnvironmentScope(repo, env))
		require.NoError(t, err, env)
		assert.Len(t, got, 1, env)
	}
}

func TestSettingsDieWithTheRepository(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	keep := name(t, "proj2") // its name starts like the deleted one's
	require.NoError(t, s.Create(ctx, domain.Repo{Name: keep, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	for _, r := range []domain.RepoName{repo, keep} {
		_, err := st.CreateEnvironment(ctx, r, "prod", created)
		require.NoError(t, err)
		for _, sc := range []domain.SettingsScope{domain.RepoScope(r), domain.EnvironmentScope(r, "prod")} {
			_, err = st.PutSecret(ctx, sc, "S", []byte("x"), created)
			require.NoError(t, err)
			_, err = st.PutVariable(ctx, sc, "V", "v", created)
			require.NoError(t, err)
		}
	}
	inst := domain.InstanceScope()
	_, err := st.PutSecret(ctx, inst, "S", []byte("x"), created)
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, repo))

	// A repository created again under the name starts clean.
	require.NoError(t, s.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate, CreatedAt: created}))
	envs, err := st.ListEnvironments(ctx, repo)
	require.NoError(t, err)
	assert.Empty(t, envs)
	secrets, err := st.ListSealedSecrets(ctx, domain.RepoScope(repo))
	require.NoError(t, err)
	assert.Empty(t, secrets)
	vars, err := st.ListVariables(ctx, domain.RepoScope(repo))
	require.NoError(t, err)
	assert.Empty(t, vars)
	_, err = st.ListSealedSecrets(ctx, domain.EnvironmentScope(repo, "prod"))
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Other scopes stay.
	secrets, err = st.ListSealedSecrets(ctx, domain.EnvironmentScope(keep, "prod"))
	require.NoError(t, err)
	assert.Len(t, secrets, 1)
	secrets, err = st.ListSealedSecrets(ctx, inst)
	require.NoError(t, err)
	assert.Len(t, secrets, 1)
}

func TestBindSecretsKeyRefusesAnotherKeyAcrossARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := kvstore.Open(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, s.Settings().BindSecretsKey(ctx, []byte("check-one")))
	require.NoError(t, s.Settings().BindSecretsKey(ctx, []byte("check-one")), "the same key again")
	require.NoError(t, s.Close(ctx))

	s, err = kvstore.Open(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	assert.ErrorIs(t, s.Settings().BindSecretsKey(ctx, []byte("check-two")), domain.ErrSecretsKeyMismatch)
	assert.NoError(t, s.Settings().BindSecretsKey(ctx, []byte("check-one")))
}

// A value written into an environment after DeleteEnvironment took its snapshot
// is not missed: the delete conflicts and lists again, so nothing survives to be
// resurrected when the environment is created again.
func TestDeleteEnvironmentSeesAConcurrentInsertion(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	sc := domain.EnvironmentScope(repo, "prod")
	_, err := st.CreateEnvironment(ctx, repo, "prod", created)
	require.NoError(t, err)
	_, err = st.PutSecret(ctx, sc, "OLD", []byte("x"), created)
	require.NoError(t, err)

	calls := 0
	restore := kvstore.SetAfterSettingsSnapshot(func() {
		calls++
		if calls == 1 { // once, in the window after the first snapshot
			_, perr := st.PutSecret(ctx, sc, "LATE", []byte("x"), created)
			require.NoError(t, perr)
			_, perr = st.PutVariable(ctx, sc, "LATE_VAR", "v", created)
			require.NoError(t, perr)
		}
	})
	defer restore()
	require.NoError(t, st.DeleteEnvironment(ctx, repo, "prod"))
	assert.GreaterOrEqual(t, calls, 2, "the first attempt conflicted and listed again")
	restore()

	_, err = st.CreateEnvironment(ctx, repo, "prod", created)
	require.NoError(t, err)
	secrets, err := st.ListSealedSecrets(ctx, sc)
	require.NoError(t, err)
	assert.Empty(t, secrets, "nothing resurrected")
	vars, err := st.ListVariables(ctx, sc)
	require.NoError(t, err)
	assert.Empty(t, vars)
}

func TestGetSealedSecretIsAPointRead(t *testing.T) {
	ctx := context.Background()
	s, _, repo := ciRepo(t)
	st := s.Settings()
	sc := domain.RepoScope(repo)
	_, err := st.PutSecret(ctx, sc, "A", []byte("sealed-a"), created)
	require.NoError(t, err)
	got, err := st.GetSealedSecret(ctx, sc, "A")
	require.NoError(t, err)
	assert.Equal(t, "A", got.Name)
	assert.Equal(t, []byte("sealed-a"), got.Sealed)
	_, err = st.GetSealedSecret(ctx, sc, "B")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.GetSealedSecret(ctx, domain.RepoScope(name(t, "ghost")), "A")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = st.GetSealedSecret(ctx, domain.EnvironmentScope(repo, "nope"), "A")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
