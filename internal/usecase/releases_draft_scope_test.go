// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"bytes"
	"io"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"github.com/stretchr/testify/require"
)

// TestReleaseDraftScopeMatrix checks every release write for release:draft,
// release:write and a job token with contents: write.
func TestReleaseDraftScopeMatrix(t *testing.T) {
	limits := ports.ReleaseLimits{AssetMax: 100, AssetsMax: 10, Quota: 1000}
	principals := map[string]func(domain.RepoName) domain.Principal{
		"release:draft": func(domain.RepoName) domain.Principal {
			return domain.TokenPrincipal(domain.Token{ID: "d", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseDraft}})
		},
		"release:write": func(domain.RepoName) domain.Principal {
			return domain.TokenPrincipal(domain.Token{ID: "w", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseWrite}})
		},
		"job": func(n domain.RepoName) domain.Principal {
			return domain.RepoTokenPrincipal(domain.CIRepoToken{ID: "j", Repo: n, Permissions: map[string]string{"contents": "write"}})
		},
	}
	no, yes, title := false, true, "changed"
	for name, mk := range principals {
		t.Run(name, func(t *testing.T) {
			r := newReleaseRig(t, limits)
			p := mk(r.repo)
			full := name != "release:draft"
			check := func(t *testing.T, allowed bool, err error) {
				t.Helper()
				if allowed {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, domain.ErrForbidden)
				}
			}
			published := r.create(t, "v1", false)
			pubAsset := r.upload(t, published, "pub.bin", []byte("p"))
			r.resolve(t, "d1", nil)
			r.resolve(t, "d2", nil)
			r.resolve(t, "renamed", nil)

			// Drafts: create, view, list, edit, assets.
			draft, err := r.svc.CreateRelease(r.ctx, p, r.repo, ports.CreateReleaseParams{Tag: "d1", Draft: true})
			require.NoError(t, err)
			_, err = r.svc.GetRelease(r.ctx, p, r.repo, ports.ReleaseRef{ID: draft.ID})
			require.NoError(t, err)
			page, err := r.svc.ListReleases(r.ctx, p, r.repo, 10, "")
			require.NoError(t, err)
			require.Len(t, page.Releases, 2, "drafts are listed")
			tag := "renamed"
			_, err = r.svc.UpdateRelease(r.ctx, p, r.repo, draft.ID, ports.UpdateReleaseParams{Title: &title, Tag: &tag})
			require.NoError(t, err)
			_, err = r.svc.UploadAsset(r.ctx, p, r.repo, ports.UploadAssetParams{ReleaseID: draft.ID, Name: "a.bin", Size: 1}, bytes.NewReader([]byte("a")))
			require.NoError(t, err)
			_, err = r.svc.UploadAsset(r.ctx, p, r.repo, ports.UploadAssetParams{ReleaseID: draft.ID, Name: "a.bin", Size: 1, Replace: true}, bytes.NewReader([]byte("b")))
			require.NoError(t, err)
			assets, err := r.svc.ListAssets(r.ctx, p, r.repo, draft.ID)
			require.NoError(t, err)
			require.Len(t, assets, 1)
			a := assets[0]
			label := "l"
			_, err = r.svc.UpdateAsset(r.ctx, p, r.repo, a.ID, ports.UpdateAssetParams{Label: &label})
			require.NoError(t, err)
			require.NoError(t, r.svc.DeleteAsset(r.ctx, p, r.repo, a.ID))

			// Publishing, published releases, deletion.
			_, err = r.svc.CreateRelease(r.ctx, p, r.repo, ports.CreateReleaseParams{Tag: "d2"})
			check(t, full, err)
			_, err = r.svc.UpdateRelease(r.ctx, p, r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no})
			check(t, full, err)
			if !full {
				_, err = r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no})
				require.NoError(t, err)
			}
			_, err = r.svc.UpdateRelease(r.ctx, p, r.repo, published.ID, ports.UpdateReleaseParams{Title: &title})
			check(t, full, err)
			_, err = r.svc.UploadAsset(r.ctx, p, r.repo, ports.UploadAssetParams{ReleaseID: published.ID, Name: "new.bin", Size: 1}, bytes.NewReader([]byte("n")))
			check(t, full, err)
			_, err = r.svc.UploadAsset(r.ctx, p, r.repo, ports.UploadAssetParams{ReleaseID: published.ID, Name: "pub.bin", Size: 1, Replace: true}, bytes.NewReader([]byte("x")))
			check(t, full, err)
			pubAssets, err := r.svc.ListAssets(r.ctx, domain.Root(), r.repo, published.ID)
			require.NoError(t, err)
			for _, pa := range pubAssets {
				if pa.Name == "pub.bin" {
					pubAsset = pa
				}
			}
			_, err = r.svc.UpdateAsset(r.ctx, p, r.repo, pubAsset.ID, ports.UpdateAssetParams{Label: &label})
			check(t, full, err)
			check(t, full, r.svc.DeleteAsset(r.ctx, p, r.repo, pubAsset.ID))
			_, err = r.svc.UpdateRelease(r.ctx, p, r.repo, published.ID, ports.UpdateReleaseParams{Draft: &yes})
			check(t, full, err)
			check(t, full, r.svc.DeleteRelease(r.ctx, p, r.repo, published.ID))
			if !full {
				// Deleting is refused even for a draft.
				d, err := r.svc.CreateRelease(r.ctx, p, r.repo, ports.CreateReleaseParams{Tag: "d2", Draft: true})
				require.NoError(t, err)
				check(t, false, r.svc.DeleteRelease(r.ctx, p, r.repo, d.ID))
			}
		})
	}
}

// publishingReader publishes the release on its first read, as a concurrent
// trusted job would while a draft asset is streamed.
type publishingReader struct {
	io.Reader
	publish func()
}

func (p *publishingReader) Read(b []byte) (int, error) {
	if p.publish != nil {
		p.publish()
		p.publish = nil
	}
	return p.Reader.Read(b)
}

func TestReleaseDraftScopeUploadRacesPublish(t *testing.T) {
	r := newReleaseRig(t, ports.ReleaseLimits{AssetMax: 100, AssetsMax: 10, Quota: 1000})
	p := domain.TokenPrincipal(domain.Token{ID: "d", Scopes: []domain.Scope{domain.ScopeRepoRead, domain.ScopeReleaseDraft}})
	r.resolve(t, "v1", nil)
	draft, err := r.svc.CreateRelease(r.ctx, p, r.repo, ports.CreateReleaseParams{Tag: "v1", Draft: true})
	require.NoError(t, err)
	no := false
	body := &publishingReader{Reader: bytes.NewReader([]byte("abc")), publish: func() {
		_, err := r.svc.UpdateRelease(r.ctx, domain.Root(), r.repo, draft.ID, ports.UpdateReleaseParams{Draft: &no})
		require.NoError(t, err)
	}}
	_, err = r.svc.UploadAsset(r.ctx, p, r.repo, ports.UploadAssetParams{ReleaseID: draft.ID, Name: "late.bin", Size: 3}, body)
	require.ErrorIs(t, err, domain.ErrForbidden)
	assets, err := r.svc.ListAssets(r.ctx, domain.Root(), r.repo, draft.ID)
	require.NoError(t, err)
	require.Empty(t, assets)
	usage, err := r.svc.Usage(r.ctx, domain.Root(), r.repo)
	require.NoError(t, err)
	require.Zero(t, usage.Used)
}
