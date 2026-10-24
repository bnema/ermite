// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestArchivedReleaseWrites(t *testing.T) {
	n := name(t, "proj")
	job := domain.RepoTokenPrincipal(domain.CIRepoToken{Repo: n, Permissions: map[string]string{"contents": "write"}})
	for _, p := range []domain.Principal{root, job} {
		t.Run(p.Label(), func(t *testing.T) {
			meta := mocks.NewRepoMetaStore(t)
			meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, Visibility: domain.VisibilityPublic, Archived: true}, nil)
			svc := usecase.NewReleases(mocks.NewReleaseStore(t), mocks.NewBlobStore(t), meta, mocks.NewRepoReader(t), mocks.NewReleaseTagWriter(t), mocks.NewPushHookService(t), mocks.NewEventBus(t), fixedClock{}, ports.ReleaseLimits{})
			_, err := svc.CreateRelease(t.Context(), p, n, ports.CreateReleaseParams{})
			require.ErrorIs(t, err, domain.ErrArchived)
			_, err = svc.UpdateRelease(t.Context(), p, n, 1, ports.UpdateReleaseParams{})
			require.ErrorIs(t, err, domain.ErrArchived)
			require.ErrorIs(t, svc.DeleteRelease(t.Context(), p, n, 1), domain.ErrArchived)
			_, err = svc.UploadAsset(t.Context(), p, n, ports.UploadAssetParams{}, strings.NewReader("data"))
			require.ErrorIs(t, err, domain.ErrArchived)
			_, err = svc.UpdateAsset(t.Context(), p, n, 1, ports.UpdateAssetParams{})
			require.ErrorIs(t, err, domain.ErrArchived)
			require.ErrorIs(t, svc.DeleteAsset(t.Context(), p, n, 1), domain.ErrArchived)
		})
	}
}

func TestArchivedDispatchAndRerun(t *testing.T) {
	n := name(t, "proj")
	job := domain.RepoTokenPrincipal(domain.CIRepoToken{Repo: n, Permissions: map[string]string{"actions": "write"}})
	for _, p := range []domain.Principal{root, job} {
		t.Run(p.Label(), func(t *testing.T) {
			u := newCIUnit(t, nil)
			u.meta.EXPECT().Get(mock.Anything, n).Return(domain.Repo{Name: n, Visibility: domain.VisibilityPublic, Archived: true}, nil)
			_, err := u.ci.DispatchWorkflow(t.Context(), p, n, ports.DispatchParams{})
			require.ErrorIs(t, err, domain.ErrArchived)
			if !p.ViaJobToken() {
				for _, failedOnly := range []bool{false, true} {
					_, err = u.ci.RerunRun(t.Context(), p, n, 1, failedOnly)
					require.ErrorIs(t, err, domain.ErrArchived)
				}
			}
		})
	}
}
