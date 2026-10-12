// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
)

// testPublicURL and testSourceURL are the instance of the servers the tests
// build.
const (
	testPublicURL = "https://forge.example"
	testSourceURL = "https://forge.example/ermite"
)

// repoName parses a repository name the test knows is valid.
func repoName(t testing.TB, s string) domain.RepoName {
	t.Helper()
	n, err := domain.ParseRepoName(s)
	require.NoError(t, err)
	return n
}

// objectID parses a commit id the test knows is valid.
func objectID(t testing.TB, s string) domain.ObjectID {
	t.Helper()
	id, err := domain.ParseObjectID(s)
	require.NoError(t, err)
	return id
}

// publicRepo is the repository the use cases of the tests answer with:
// public, with main as its default branch.
func publicRepo(t testing.TB, name string) ports.RepoView {
	t.Helper()
	return ports.RepoView{
		Repo:          domain.Repo{Name: repoName(t, name), Visibility: domain.VisibilityPublic, Settings: domain.DefaultRepoSettings()},
		DefaultBranch: "main",
	}
}

// reposFailing is a RepoService whose reads fail with err, whatever they ask.
func reposFailing(t testing.TB, err error) *mocks.RepoService {
	t.Helper()
	m := mocks.NewRepoService(t)
	m.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(domain.Repo{}, err).Maybe()
	m.EXPECT().ListPage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(ports.RepoPage{}, err).Maybe()
	return m
}

// reposServing is a RepoService that knows the repositories, by name, as seen
// by every caller.
func reposServing(t testing.TB, views ...ports.RepoView) *mocks.RepoService {
	t.Helper()
	m := mocks.NewRepoService(t)
	byName := map[string]ports.RepoView{}
	for _, v := range views {
		byName[v.Name.String()] = v
	}
	m.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Principal, name domain.RepoName) (domain.Repo, error) {
			v, ok := byName[name.String()]
			if !ok {
				return domain.Repo{}, domain.ErrNotFound
			}
			return v.Repo, nil
		}).Maybe()
	m.EXPECT().View(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, r domain.Repo) (ports.RepoView, error) {
			v, ok := byName[r.Name.String()]
			if !ok {
				return ports.RepoView{}, domain.ErrNotFound
			}
			return v, nil
		}).Maybe()
	m.EXPECT().ListPage(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, domain.Principal, int, string) (ports.RepoPage, error) {
			var page ports.RepoPage
			for _, v := range views {
				page.Repos = append(page.Repos, v.Repo)
			}
			return page, nil
		}).Maybe()
	return m
}

// newTestServer builds a Server from cfg, with the instance filled in.
func newTestServer(t testing.TB, cfg Config) *Server {
	t.Helper()
	if cfg.PublicURL == "" {
		cfg.PublicURL = testPublicURL
	}
	if cfg.SourceURL == "" {
		cfg.SourceURL = testSourceURL
	}
	if cfg.Version == "" {
		cfg.Version = "v1"
	}
	srv, err := New(cfg)
	require.NoError(t, err)
	return srv
}
