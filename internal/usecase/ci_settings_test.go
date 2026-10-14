// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/adapters/kvstore"
	"bnema.dev/ermite/internal/adapters/secretbox"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

type settingsEnv struct {
	store *kvstore.Store
	s     *usecase.CISettings
	repo  domain.RepoName
}

func newSettings(t *testing.T, key byte) *settingsEnv {
	t.Helper()
	ctx := context.Background()
	store, err := kvstore.Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })
	repo := name(t, "proj")
	require.NoError(t, store.Create(ctx, domain.Repo{Name: repo, Visibility: domain.VisibilityPrivate, CreatedAt: now}))
	var sealer ports.SecretSealer
	if key != 0 {
		box, err := secretbox.New(bytes.Repeat([]byte{key}, secretbox.KeyLen))
		require.NoError(t, err)
		sealer = box
	}
	s, err := usecase.NewCISettings(ctx, store, store.Settings(), sealer, &fakeClock{t: now})
	require.NoError(t, err)
	return &settingsEnv{store: store, s: s, repo: repo}
}

// Precedence: environment over repository over instance, for variables and
// secrets, and only the secrets that were asked for are opened.
func TestResolvePrecedenceAndSelection(t *testing.T) {
	ctx, root := context.Background(), domain.Root()
	e := newSettings(t, 1)
	inst, repo, env := domain.InstanceScope(), domain.RepoScope(e.repo), domain.EnvironmentScope(e.repo, "prod")
	_, err := e.s.CreateEnvironment(ctx, root, e.repo, "Prod")
	require.NoError(t, err)

	for _, c := range []struct {
		sc          domain.SettingsScope
		name, value string
	}{
		{inst, "A", "inst"}, {inst, "B", "inst"}, {inst, "C", "inst"},
		{repo, "B", "repo"}, {repo, "C", "repo"},
		{env, "C", "env"}, {env, "D", "env"},
	} {
		_, err := e.s.SetSecret(ctx, root, c.sc, c.name, c.value)
		require.NoError(t, err)
		_, err = e.s.SetVariable(ctx, root, c.sc, c.name, "v-"+c.value)
		require.NoError(t, err)
	}

	got, err := e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, Environment: "PROD", WantSecrets: true, AllSecrets: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "v-inst", "B": "v-repo", "C": "v-env", "D": "v-env"}, got.Vars)
	assert.Equal(t, map[string]string{"A": "inst", "B": "repo", "C": "env", "D": "env"}, got.Secrets)

	// Without an environment: instance and repository only.
	got, err = e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, WantSecrets: true, AllSecrets: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "v-inst", "B": "v-repo", "C": "v-repo"}, got.Vars)
	assert.Equal(t, map[string]string{"A": "inst", "B": "repo", "C": "repo"}, got.Secrets)

	// Only the names asked for (any case) come out.
	got, err = e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, Environment: "prod", WantSecrets: true, SecretNames: []string{"d", "A"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "inst", "D": "env"}, got.Secrets)

	// Not wanted (an untrusted run), or nothing named: no secret at all.
	for _, req := range []ports.JobSettingsRequest{
		{Repo: e.repo, AllSecrets: true, SecretNames: []string{"A"}},
		{Repo: e.repo, WantSecrets: true},
	} {
		got, err = e.s.Resolve(ctx, req)
		require.NoError(t, err)
		assert.Empty(t, got.Secrets)
		assert.NotEmpty(t, got.Vars, "variables are not secret")
	}

	// A missing environment adds nothing and is not created unless asked.
	got, err = e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, Environment: "new"})
	require.NoError(t, err)
	assert.Equal(t, "v-repo", got.Vars["C"])
	page, err := e.s.ListEnvironments(ctx, root, e.repo, 0, "")
	require.NoError(t, err)
	assert.Len(t, page.Environments, 1)
	_, err = e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, Environment: "New", CreateEnvironment: true})
	require.NoError(t, err)
	page, err = e.s.ListEnvironments(ctx, root, e.repo, 0, "")
	require.NoError(t, err)
	assert.Len(t, page.Environments, 2)
}

// A sealed value moved to another secret is not given to a job: the ciphertext
// is bound to its scope and name.
func TestResolveLeavesOutASwappedCiphertext(t *testing.T) {
	ctx, root := context.Background(), domain.Root()
	e := newSettings(t, 1)
	sc := domain.RepoScope(e.repo)
	_, err := e.s.SetSecret(ctx, root, sc, "A", "value-of-a")
	require.NoError(t, err)
	_, err = e.s.SetSecret(ctx, root, sc, "B", "value-of-b")
	require.NoError(t, err)

	st := e.store.Settings()
	sealed, err := st.ListSealedSecrets(ctx, sc)
	require.NoError(t, err)
	require.Len(t, sealed, 2)
	// Swap the two stored values behind the use case's back.
	_, err = st.PutSecret(ctx, sc, "A", sealed[1].Sealed, now)
	require.NoError(t, err)
	_, err = st.PutSecret(ctx, sc, "B", sealed[0].Sealed, now)
	require.NoError(t, err)

	got, err := e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, WantSecrets: true, AllSecrets: true})
	require.NoError(t, err)
	assert.Empty(t, got.Secrets, "neither opens")
}

func TestSetSecretWithoutAKeyIsAFailedPrecondition(t *testing.T) {
	ctx, root := context.Background(), domain.Root()
	e := newSettings(t, 0)
	_, err := e.s.SetSecret(ctx, root, domain.RepoScope(e.repo), "A", "v")
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	// Checked after the arguments: a bad name is still invalid.
	_, err = e.s.SetSecret(ctx, root, domain.RepoScope(e.repo), "1", "v")
	assert.True(t, domain.IsInvalid(err))
	// Variables and environments need no key, and no secret is sent.
	_, err = e.s.SetVariable(ctx, root, domain.RepoScope(e.repo), "V", "v")
	require.NoError(t, err)
	got, err := e.s.Resolve(ctx, ports.JobSettingsRequest{Repo: e.repo, WantSecrets: true, AllSecrets: true})
	require.NoError(t, err)
	assert.True(t, got.SecretsOff)
	assert.Empty(t, got.Secrets)
	assert.Equal(t, "v", got.Vars["V"])
}

func TestNewCISettingsRefusesAnotherKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	open := func(key byte) error {
		store, err := kvstore.Open(ctx, dir)
		require.NoError(t, err)
		defer func() { _ = store.Close(ctx) }()
		box, err := secretbox.New(bytes.Repeat([]byte{key}, secretbox.KeyLen))
		require.NoError(t, err)
		_, err = usecase.NewCISettings(ctx, store, store.Settings(), box, &fakeClock{t: time.Now()})
		return err
	}
	require.NoError(t, open(1))
	require.NoError(t, open(1))
	assert.ErrorIs(t, open(2), domain.ErrSecretsKeyMismatch)
	assert.NoError(t, open(1), "the first key still starts")
}

func TestSettingsAccess(t *testing.T) {
	ctx := context.Background()
	e := newSettings(t, 1)
	sc := domain.RepoScope(e.repo)
	tok := func(scopes []domain.Scope, repos []domain.RepoName, prefixes []string) domain.Principal {
		return domain.TokenPrincipal(domain.Token{ID: "t", Scopes: scopes, Repos: repos, BranchPrefixes: prefixes})
	}
	all := domain.AllScopes()
	cases := []struct {
		name string
		p    domain.Principal
		sc   domain.SettingsScope
		want error
	}{
		{"root", domain.Root(), sc, nil},
		{"owner session", domain.Owner(), sc, nil},
		{"token with the scope", tok([]domain.Scope{domain.ScopeSecretsWrite}, nil, nil), sc, nil},
		{"token on its repository", tok([]domain.Scope{domain.ScopeSecretsWrite}, []domain.RepoName{e.repo}, nil), sc, nil},
		{"every scope but branch-limited", tok(all, nil, []string{"agent/"}), sc, domain.ErrForbidden},
		{"branch-limited, instance scope", tok(all, nil, []string{"agent/"}), domain.InstanceScope(), domain.ErrForbidden},
		{"reads the repository but lacks the scope", tok([]domain.Scope{domain.ScopeRepoRead}, nil, nil), sc, domain.ErrForbidden},
		{"cannot read the private repository", tok([]domain.Scope{domain.ScopeMRWrite}, nil, nil), sc, domain.ErrNotFound},
		{"anonymous", domain.Anonymous(), sc, domain.ErrUnauthenticated},
		{"job token", domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j", Repo: e.repo}), sc, domain.ErrForbidden},
		{"instance, repository-restricted token", tok([]domain.Scope{domain.ScopeSecretsWrite}, []domain.RepoName{e.repo}, nil), domain.InstanceScope(), domain.ErrForbidden},
		{"instance, unrestricted", tok([]domain.Scope{domain.ScopeSecretsWrite}, nil, nil), domain.InstanceScope(), nil},
		{"invalid scope", domain.Root(), domain.SettingsScope{}, domain.ErrInvalid},
	}
	for _, tc := range cases {
		_, err := e.s.ListSecrets(ctx, tc.p, tc.sc, 0, "")
		if tc.want == nil {
			assert.NoError(t, err, tc.name)
			continue
		}
		assert.ErrorIs(t, err, tc.want, tc.name)
	}
}

// A higher scope whose ciphertext does not open withholds the secret: the job
// never gets the value of a lower scope that the override was meant to replace.
// Named and "all" resolution agree, and an unrelated secret is not affected.
func TestResolveWithholdsASecretWhoseOverrideDoesNotOpen(t *testing.T) {
	ctx, root := context.Background(), domain.Root()
	e := newSettings(t, 1)
	inst, repo := domain.InstanceScope(), domain.RepoScope(e.repo)
	for _, c := range []struct {
		sc          domain.SettingsScope
		name, value string
	}{{inst, "A", "lower-a"}, {inst, "B", "lower-b"}, {repo, "A", "upper-a"}} {
		_, err := e.s.SetSecret(ctx, root, c.sc, c.name, c.value)
		require.NoError(t, err)
	}
	_, err := e.store.Settings().PutSecret(ctx, repo, "A", []byte("garbage that is not a sealed value at all"), now)
	require.NoError(t, err)

	for name, req := range map[string]ports.JobSettingsRequest{
		"all":   {Repo: e.repo, WantSecrets: true, AllSecrets: true},
		"named": {Repo: e.repo, WantSecrets: true, SecretNames: []string{"a", "B"}},
	} {
		got, err := e.s.Resolve(ctx, req)
		require.NoError(t, err, name)
		assert.Equal(t, map[string]string{"B": "lower-b"}, got.Secrets, name)
	}
}

// Named resolution reads the names it needs, not whole scopes.
func TestResolveNamedSecretsUsePointReads(t *testing.T) {
	ctx := context.Background()
	store := mocks.NewCISettingsStore(t)
	sealer := mocks.NewSecretSealer(t)
	sealer.EXPECT().KeyCheck().Return([]byte("k"))
	store.EXPECT().BindSecretsKey(mock.Anything, mock.Anything).Return(nil)
	s, err := usecase.NewCISettings(ctx, mocks.NewRepoMetaStore(t), store, sealer, &fakeClock{t: now})
	require.NoError(t, err)
	repo := name(t, "proj")
	store.EXPECT().ListVariables(mock.Anything, mock.Anything).Return(nil, nil)
	// Two scopes (instance, repository), one name each: no ListSealedSecrets call.
	store.EXPECT().GetSealedSecret(mock.Anything, domain.InstanceScope(), "A").Return(ports.SealedSecret{}, domain.ErrNotFound)
	store.EXPECT().GetSealedSecret(mock.Anything, domain.RepoScope(repo), "A").Return(ports.SealedSecret{Name: "A", Sealed: []byte("s")}, nil)
	sealer.EXPECT().Open([]byte("s"), domain.SecretAAD(domain.RepoScope(repo), "A")).Return([]byte("v"), nil)
	got, err := s.Resolve(ctx, ports.JobSettingsRequest{Repo: repo, WantSecrets: true, SecretNames: []string{"a", "A"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "v"}, got.Secrets)
}
