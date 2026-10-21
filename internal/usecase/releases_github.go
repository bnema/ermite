// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// ReleasesGitHub builds the GitHub REST representations of releases. Authorization
// and all mutations remain in Releases; it owns no metadata or contents.
type ReleasesGitHub struct {
	rel              *Releases
	owner, publicURL string
}

var _ ports.GitHubReleaseAPI = (*ReleasesGitHub)(nil)

// NewReleasesGitHub wraps the shared release service. publicURL is the external
// server URL (without the API prefix), used in upload and browser download URLs.
func NewReleasesGitHub(rel *Releases, owner, publicURL string) *ReleasesGitHub {
	return &ReleasesGitHub{rel: rel, owner: owner, publicURL: strings.TrimRight(publicURL, "/")}
}

func (g *ReleasesGitHub) repo(owner, name string) (domain.RepoName, error) {
	if !domain.OwnerMatches(g.owner, owner) {
		return domain.RepoName{}, domain.ErrNotFound
	}
	rn, err := domain.ParseRepoName(strings.ToLower(name))
	if err != nil {
		return domain.RepoName{}, domain.ErrNotFound
	}
	return rn, nil
}
func (g *ReleasesGitHub) base(repo domain.RepoName) string {
	return g.publicURL + apiPath + "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(repo.String())
}
func (g *ReleasesGitHub) user(label string) map[string]any {
	if label == "" || label == "root" || label == "owner" {
		label = g.owner
	}
	u := ghURLs{server: g.publicURL, api: g.publicURL + apiPath}
	return u.userObj(ghIdentity{login: label, id: stableID("user", label)}, false)
}

// escapeReleaseTag preserves tag separators: the web routes join the tag path.
func escapeReleaseTag(tag string) string {
	parts := strings.Split(tag, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func (g *ReleasesGitHub) assetObj(a domain.ReleaseAsset, tag string) ordered {
	return ordered{
		{"url", g.base(a.Repo) + "/releases/assets/" + strconv.FormatInt(a.ID, 10)},
		{"id", a.ID}, {"node_id", ""}, {"name", a.Name}, {"label", a.Label},
		{"uploader", g.user(a.Uploader)}, {"content_type", a.ContentType}, {"state", "uploaded"},
		{"size", a.Size}, {"digest", a.Digest}, {"download_count", 0},
		{"created_at", a.CreatedAt.UTC().Format(timeLayout)}, {"updated_at", a.UpdatedAt.UTC().Format(timeLayout)},
		{"browser_download_url", g.publicURL + "/" + a.Repo.String() + "/releases/download/" + escapeReleaseTag(tag) + "/" + url.PathEscape(a.Name)},
	}
}
func (g *ReleasesGitHub) releaseObj(ctx context.Context, p domain.Principal, r domain.Release) (ordered, error) {
	assets, err := g.rel.ListAssets(ctx, p, r.Repo, r.ID)
	if err != nil {
		return nil, err
	}
	objects := make([]ordered, 0, len(assets))
	for _, a := range assets {
		objects = append(objects, g.assetObj(a, r.Tag))
	}
	base := g.base(r.Repo) + "/releases/" + strconv.FormatInt(r.ID, 10)
	var published any
	if !r.Draft && !r.PublishedAt.IsZero() {
		published = r.PublishedAt.UTC().Format(timeLayout)
	}
	return ordered{
		{"url", base}, {"assets_url", base + "/assets"},
		{"upload_url", g.publicURL + apiPath + "/uploads/repos/" + url.PathEscape(g.owner) + "/" + r.Repo.String() + "/releases/" + strconv.FormatInt(r.ID, 10) + "/assets{?name,label}"},
		{"html_url", g.publicURL + "/" + r.Repo.String() + "/releases/tag/" + escapeReleaseTag(r.Tag)},
		{"id", r.ID}, {"author", g.user(r.Author)}, {"node_id", ""}, {"tag_name", r.Tag}, {"target_commitish", r.TargetCommitish},
		{"name", r.Title}, {"draft", r.Draft}, {"prerelease", r.Prerelease}, {"created_at", r.CreatedAt.UTC().Format(timeLayout)},
		{"published_at", published}, {"assets", objects}, {"tarball_url", g.base(r.Repo) + "/tarball/" + escapeReleaseTag(r.Tag)},
		{"zipball_url", nil}, {"body", r.Notes},
	}, nil
}
func (g *ReleasesGitHub) marshalRelease(ctx context.Context, p domain.Principal, r domain.Release) ([]byte, error) {
	o, err := g.releaseObj(ctx, p, r)
	if err != nil {
		return nil, err
	}
	return json.Marshal(o)
}
func releaseFields(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("%w: JSON object required", domain.ErrInvalid)
	}
	return fields, nil
}
func releaseField[T any](fields map[string]json.RawMessage, key string) (*T, error) {
	b, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var v T
	if string(b) == "null" || json.Unmarshal(b, &v) != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalid, key)
	}
	return &v, nil
}
func releaseParams(body []byte) (ports.UpdateReleaseParams, error) {
	var p ports.UpdateReleaseParams
	f, err := releaseFields(body)
	if err != nil {
		return p, err
	}
	for key, dst := range map[string]**string{"tag_name": &p.Tag, "target_commitish": &p.TargetCommitish, "name": &p.Title, "body": &p.Notes} {
		*dst, err = releaseField[string](f, key)
		if err != nil {
			return p, err
		}
	}
	for key, dst := range map[string]**bool{"draft": &p.Draft, "prerelease": &p.Prerelease} {
		*dst, err = releaseField[bool](f, key)
		if err != nil {
			return p, err
		}
	}
	latest, err := releaseField[string](f, "make_latest")
	if err != nil {
		return p, err
	}
	if latest != nil {
		switch *latest {
		case "true":
			v := true
			p.MakeLatest = &v
		case "false":
			v := false
			p.MakeLatest = &v
		case "legacy":
		default:
			return p, fmt.Errorf("%w: make_latest", domain.ErrInvalid)
		}
	}
	return p, nil
}
func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}
	return v
}

// CreateRelease accepts GitHub's create body and returns its representation.
func (g *ReleasesGitHub) CreateRelease(ctx context.Context, p domain.Principal, owner, repo string, body []byte) ([]byte, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, err
	}
	params, err := releaseParams(body)
	if err != nil {
		return nil, err
	}
	if params.Tag == nil || *params.Tag == "" {
		return nil, fmt.Errorf("%w: tag_name", domain.ErrInvalid)
	}
	r, err := g.rel.CreateRelease(ctx, p, rn, ports.CreateReleaseParams{Tag: *params.Tag, TargetCommitish: value(params.TargetCommitish), Title: value(params.Title), Notes: value(params.Notes), Draft: value(params.Draft), Prerelease: value(params.Prerelease), MakeLatest: value(params.MakeLatest)})
	if err != nil {
		return nil, err
	}
	ctxLog(ctx).Info().Str("repo", rn.String()).Int64("release", r.ID).Msg("github release created")
	return g.marshalRelease(ctx, p, r)
}

// GetRelease resolves an id, tag or latest selector.
func (g *ReleasesGitHub) GetRelease(ctx context.Context, p domain.Principal, owner, repo string, ref ports.ReleaseRef) ([]byte, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, err
	}
	r, err := g.rel.GetRelease(ctx, p, rn, ref)
	if err != nil {
		return nil, err
	}
	return g.marshalRelease(ctx, p, r)
}

// UpdateRelease changes only present fields, including explicit false and empty strings.
func (g *ReleasesGitHub) UpdateRelease(ctx context.Context, p domain.Principal, owner, repo string, id int64, body []byte) ([]byte, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, err
	}
	params, err := releaseParams(body)
	if err != nil {
		return nil, err
	}
	r, err := g.rel.UpdateRelease(ctx, p, rn, id, params)
	if err != nil {
		return nil, err
	}
	ctxLog(ctx).Info().Str("repo", rn.String()).Int64("release", id).Msg("github release updated")
	return g.marshalRelease(ctx, p, r)
}

// DeleteRelease removes a release and all its assets.
func (g *ReleasesGitHub) DeleteRelease(ctx context.Context, p domain.Principal, owner, repo string, id int64) error {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return err
	}
	if err := g.rel.DeleteRelease(ctx, p, rn, id); err != nil {
		return err
	}
	ctxLog(ctx).Info().Str("repo", rn.String()).Int64("release", id).Msg("github release deleted")
	return nil
}
func releasePagination(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 30
	}
	return page, min(perPage, 100)
}
func releasePageBounds(page, perPage, total int) (int, int) {
	// Avoid overflow on arbitrary query parameters.
	if page-1 > total/perPage {
		return total, total
	}
	start := min((page-1)*perPage, total)
	return start, min(start+perPage, total)
}

// ListReleases returns visible releases newest first and GitHub Link pagination.
func (g *ReleasesGitHub) ListReleases(ctx context.Context, p domain.Principal, owner, repo string, page, perPage int) ([]byte, string, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, "", err
	}
	if _, err = g.rel.readable(ctx, p, rn); err != nil {
		return nil, "", err
	}
	all, err := g.rel.store.ListReleases(ctx, rn)
	if err != nil {
		return nil, "", err
	}
	all = slices.DeleteFunc(all, func(r domain.Release) bool { return r.Draft && !releaseWriter(p, rn) })
	slices.SortFunc(all, func(a, b domain.Release) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	page, perPage = releasePagination(page, perPage)
	start, end := releasePageBounds(page, perPage, len(all))
	objects := make([]ordered, 0, end-start)
	for _, r := range all[start:end] {
		o, e := g.releaseObj(ctx, p, r)
		if e != nil {
			return nil, "", e
		}
		objects = append(objects, o)
	}
	b, err := json.Marshal(objects)
	return b, pullFilesLink(g.base(rn)+"/releases", page, perPage, len(all)), err
}

// ListAssets lists uploaded assets with the same pagination conventions.
func (g *ReleasesGitHub) ListAssets(ctx context.Context, p domain.Principal, owner, repo string, id int64, page, perPage int) ([]byte, string, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, "", err
	}
	r, err := g.rel.GetRelease(ctx, p, rn, ports.ReleaseRef{ID: id})
	if err != nil {
		return nil, "", err
	}
	all, err := g.rel.ListAssets(ctx, p, rn, id)
	if err != nil {
		return nil, "", err
	}
	page, perPage = releasePagination(page, perPage)
	start, end := releasePageBounds(page, perPage, len(all))
	objects := make([]ordered, 0, end-start)
	for _, a := range all[start:end] {
		objects = append(objects, g.assetObj(a, r.Tag))
	}
	b, err := json.Marshal(objects)
	return b, pullFilesLink(g.base(rn)+"/releases/"+strconv.FormatInt(id, 10)+"/assets", page, perPage, len(all)), err
}
func (g *ReleasesGitHub) marshalAsset(ctx context.Context, p domain.Principal, a domain.ReleaseAsset) ([]byte, error) {
	r, err := g.rel.GetRelease(ctx, p, a.Repo, ports.ReleaseRef{ID: a.ReleaseID})
	if err != nil {
		return nil, err
	}
	return json.Marshal(g.assetObj(a, r.Tag))
}

// UploadAsset streams an exact-sized raw body; duplicate names are not replaced.
func (g *ReleasesGitHub) UploadAsset(ctx context.Context, p domain.Principal, owner, repo string, id int64, name, label, contentType string, size int64, body io.Reader) ([]byte, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, err
	}
	a, err := g.rel.UploadAsset(ctx, p, rn, ports.UploadAssetParams{ReleaseID: id, Name: name, Label: label, ContentType: contentType, Size: size}, body)
	if err != nil {
		return nil, err
	}
	return g.marshalAsset(ctx, p, a)
}

// AssetByID resolves a visible asset, including its repository ownership.
func (g *ReleasesGitHub) AssetByID(ctx context.Context, p domain.Principal, owner, repo string, id int64) (domain.ReleaseAsset, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return domain.ReleaseAsset{}, err
	}
	return g.rel.GetAsset(ctx, p, rn, ports.AssetRef{ID: id})
}

// GetAsset returns the JSON representation, not the contents.
func (g *ReleasesGitHub) GetAsset(ctx context.Context, p domain.Principal, owner, repo string, id int64) ([]byte, error) {
	a, err := g.AssetByID(ctx, p, owner, repo, id)
	if err != nil {
		return nil, err
	}
	return g.marshalAsset(ctx, p, a)
}

// UpdateAsset changes the name and label fields that are present.
func (g *ReleasesGitHub) UpdateAsset(ctx context.Context, p domain.Principal, owner, repo string, id int64, body []byte) ([]byte, error) {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return nil, err
	}
	f, err := releaseFields(body)
	if err != nil {
		return nil, err
	}
	name, err := releaseField[string](f, "name")
	if err != nil {
		return nil, err
	}
	label, err := releaseField[string](f, "label")
	if err != nil {
		return nil, err
	}
	a, err := g.rel.UpdateAsset(ctx, p, rn, id, ports.UpdateAssetParams{Name: name, Label: label})
	if err != nil {
		return nil, err
	}
	ctxLog(ctx).Info().Str("repo", rn.String()).Int64("asset", id).Msg("github release asset updated")
	return g.marshalAsset(ctx, p, a)
}

// DeleteAsset removes the asset record and its blob.
func (g *ReleasesGitHub) DeleteAsset(ctx context.Context, p domain.Principal, owner, repo string, id int64) error {
	rn, err := g.repo(owner, repo)
	if err != nil {
		return err
	}
	if err := g.rel.DeleteAsset(ctx, p, rn, id); err != nil {
		return err
	}
	ctxLog(ctx).Info().Str("repo", rn.String()).Int64("asset", id).Msg("github release asset deleted")
	return nil
}
