// SPDX-License-Identifier: AGPL-3.0-or-later

package ports

import (
	"bnema.dev/ermite/internal/domain"
	"context"
	"io"
)

// ReleaseWrite atomically changes one repository's metadata. Every write
// requires UsageVersion (zero requires absence), and each record's Version.
// Used includes both uploading and uploaded assets.
type ReleaseWrite struct {
	Repo                        domain.RepoName
	Used                        int64
	UsageVersion                uint64
	PutReleases, DeleteReleases []domain.Release
	PutAssets, DeleteAssets     []domain.ReleaseAsset
}

// ReleaseStore keeps metadata, never contents. *kvstore.Releases implements it.
type ReleaseStore interface {
	NextReleaseID(ctx context.Context) (int64, error)
	ReleaseUsage(ctx context.Context, repo domain.RepoName) (used int64, version uint64, err error)
	GetRelease(ctx context.Context, repo domain.RepoName, id int64) (domain.Release, error)
	GetReleaseByTag(ctx context.Context, repo domain.RepoName, tag string) (domain.Release, error)
	ReleaseRepo(ctx context.Context, id int64) (domain.RepoName, error)
	ListReleases(ctx context.Context, repo domain.RepoName) ([]domain.Release, error)
	GetAsset(ctx context.Context, id int64) (domain.ReleaseAsset, error)
	GetAssetByName(ctx context.Context, repo domain.RepoName, releaseID int64, name string) (domain.ReleaseAsset, error)
	ListAssets(ctx context.Context, repo domain.RepoName, releaseID int64) ([]domain.ReleaseAsset, error)
	AllAssets(ctx context.Context) ([]domain.ReleaseAsset, error)
	ApplyReleases(ctx context.Context, w ReleaseWrite) error
}

// ReleaseLimits defaults to domain.DefaultReleaseAssetMax (ERMITE_RELEASE_ASSET_MAX),
// domain.DefaultReleaseAssetsMax (ERMITE_RELEASE_ASSETS_MAX), and
// domain.DefaultReleaseQuota (ERMITE_RELEASE_QUOTA).
type ReleaseLimits struct{ AssetMax, AssetsMax, Quota int64 }
type CreateReleaseParams struct {
	Tag, TargetCommitish, Title, Notes string
	Draft, Prerelease, MakeLatest      bool
}
type UpdateReleaseParams struct {
	Tag, TargetCommitish, Title, Notes *string
	Draft, Prerelease, MakeLatest      *bool
}

// ReleaseRef selects exactly one of ID, Tag, Latest.
type ReleaseRef struct {
	ID     int64
	Tag    string
	Latest bool
}

// AssetRef selects ID, (Tag, Name), or (Latest, Name).
type AssetRef struct {
	ID        int64
	Tag, Name string
	Latest    bool
}
type UploadAssetParams struct {
	ReleaseID                int64
	Name, Label, ContentType string
	Size                     int64
	Replace                  bool
}
type UpdateAssetParams struct{ Name, Label *string }
type ReleaseUsage struct {
	Used   int64
	Limits ReleaseLimits
}
type AssetChunk struct {
	Asset *domain.ReleaseAsset
	Data  []byte
}
type ReleasePage struct {
	Releases      []domain.Release
	NextPageToken string
}

// ReleaseService manages releases and streams assets; *usecase.Releases implements it.
type ReleaseService interface {
	CreateRelease(ctx context.Context, p domain.Principal, repo domain.RepoName, params CreateReleaseParams) (domain.Release, error)
	GetRelease(ctx context.Context, p domain.Principal, repo domain.RepoName, ref ReleaseRef) (domain.Release, error)
	ListReleases(ctx context.Context, p domain.Principal, repo domain.RepoName, pageSize int, pageToken string) (ReleasePage, error)
	UpdateRelease(ctx context.Context, p domain.Principal, repo domain.RepoName, id int64, params UpdateReleaseParams) (domain.Release, error)
	DeleteRelease(ctx context.Context, p domain.Principal, repo domain.RepoName, id int64) error
	ListAssets(ctx context.Context, p domain.Principal, repo domain.RepoName, releaseID int64) ([]domain.ReleaseAsset, error)
	UploadAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, params UploadAssetParams, body io.Reader) (domain.ReleaseAsset, error)
	UpdateAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, assetID int64, params UpdateAssetParams) (domain.ReleaseAsset, error)
	DeleteAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, assetID int64) error
	GetAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, ref AssetRef) (domain.ReleaseAsset, error)
	// DownloadAsset sends metadata first, then chunks at most domain.CIArtifactChunk;
	// length less than zero reads to the end.
	DownloadAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, ref AssetRef, offset, length int64, send func(AssetChunk) error) error
	Usage(ctx context.Context, p domain.Principal, repo domain.RepoName) (ReleaseUsage, error)
}

// ReleaseTagTrigger handles tag deletion; *usecase.Releases implements it.
type ReleaseTagTrigger interface {
	OnRefsMoved(ctx context.Context, repo domain.RepoName, updates []domain.RefUpdate)
}

// ReleaseSweeper removes stale assets and blobs; *usecase.Releases implements it.
type ReleaseSweeper interface{ Sweep(ctx context.Context) }

// ReleaseRepoCleaner removes contents after metadata deletion; *usecase.Releases implements it.
type ReleaseRepoCleaner interface {
	AssetsOf(ctx context.Context, repo domain.RepoName) ([]domain.ReleaseAsset, error)
	OnRepoDeleted(ctx context.Context, repo domain.RepoName, assets []domain.ReleaseAsset)
}
