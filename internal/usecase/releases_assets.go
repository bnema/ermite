// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// UploadAsset reserves quota before reading, stores bytes, then publishes metadata.
func (r *Releases) UploadAsset(ctx context.Context, p domain.Principal, name domain.RepoName, params ports.UploadAssetParams, body io.Reader) (result domain.ReleaseAsset, err error) {
	repo, err := r.writable(ctx, p, name)
	if err != nil {
		return result, err
	}
	if params.Size < 0 || params.Size > r.limits.AssetMax {
		return result, fmt.Errorf("%w: asset size must be between zero and %s", domain.ErrResourceExhausted, sizeText(r.limits.AssetMax))
	}
	a := domain.ReleaseAsset{Repo: name, ReleaseID: params.ReleaseID, Size: params.Size, Charged: params.Size, State: domain.ReleaseAssetUploading, Uploader: domain.AuthorName(p), CreatedAt: r.clock.Now(), UpdatedAt: r.clock.Now()}
	if a.Name, err = domain.ParseReleaseAssetName(params.Name); err != nil {
		return result, err
	}
	if a.Label, err = domain.ParseReleaseAssetLabel(params.Label); err != nil {
		return result, err
	}
	if a.ContentType, err = domain.ParseReleaseContentType(params.ContentType); err != nil {
		return result, err
	}
	select {
	case r.uploads <- struct{}{}:
		defer func() { <-r.uploads }()
	default:
		return result, fmt.Errorf("%w: too many uploads in progress, retry", domain.ErrResourceExhausted)
	}
	a.ID, err = r.store.NextReleaseID(ctx)
	if err != nil {
		return result, err
	}
	var replaced []domain.ReleaseAsset
	err = r.updateReleases(ctx, name, func(w *ports.ReleaseWrite) error {
		replaced = nil
		if _, err := r.store.GetRelease(ctx, name, a.ReleaseID); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				owner, lookupErr := r.store.ReleaseRepo(ctx, a.ReleaseID)
				if lookupErr == nil && owner != name {
					return fmt.Errorf("%w: release belongs to another repository", domain.ErrPreconditionFailed)
				}
			}
			return err
		}
		all, err := r.store.ListAssets(ctx, name, a.ReleaseID)
		if err != nil {
			return err
		}
		for _, old := range all {
			if old.Name != a.Name {
				continue
			}
			if (old.State != domain.ReleaseAssetUploading || r.clock.Now().Sub(old.UpdatedAt) <= 60*time.Second) && (old.State != domain.ReleaseAssetUploaded || !params.Replace) {
				return fmt.Errorf("%w: an asset named %q already exists", domain.ErrAlreadyExists, a.Name)
			}
			replaced = append(replaced, old)
			w.DeleteAssets = append(w.DeleteAssets, old)
			w.Used -= old.Charged
		}
		if int64(len(all)-len(replaced)) >= r.limits.AssetsMax {
			return fmt.Errorf("%w: release asset count limit is %d", domain.ErrResourceExhausted, r.limits.AssetsMax)
		}
		if a.Size > r.limits.Quota-w.Used {
			return fmt.Errorf("%w: release quota is %s", domain.ErrResourceExhausted, sizeText(r.limits.Quota))
		}
		w.Used += a.Charged
		w.PutAssets = []domain.ReleaseAsset{a}
		return nil
	})
	if err != nil {
		return result, err
	}
	r.removeAssetBlobs(context.WithoutCancel(ctx), replaced)
	body = releaseContextReader{ctx: ctx, reader: body}
	// From here every failure removes the reservation and any completed blob.
	defer func() {
		if err != nil {
			clean := context.WithoutCancel(ctx)
			if e := r.removeAssetRecord(clean, a, nil); e != nil && !errors.Is(e, domain.ErrNotFound) {
				ctxLog(ctx).Error().Err(e).Int64("asset", a.ID).Msg("refund failed release upload")
			}
			r.removeAssetBlobs(clean, []domain.ReleaseAsset{a})
		}
	}()
	if a.Size <= ports.BlobPartSize {
		var info ports.BlobInfo
		limited := &io.LimitedReader{R: body, N: a.Size}
		info, err = r.blobs.Put(ctx, a.BlobKey(), limited, a.Size)
		if err != nil {
			if limited.N > 0 && errors.Is(err, domain.ErrInvalid) {
				err = fmt.Errorf("%w: body shorter than size", domain.ErrInvalid)
			}
			return result, err
		}
		a.Digest = info.Digest
		if !validReleaseDigest(a.Digest) {
			return result, fmt.Errorf("%w: blob store returned no SHA-256 digest", domain.ErrInvalid)
		}
	} else {
		var upload ports.MultipartUpload
		upload, err = r.blobs.NewMultipart(ctx, a.BlobKey())
		if err != nil {
			return result, err
		}
		completed := false
		defer func() {
			if !completed {
				if e := upload.Abort(context.WithoutCancel(ctx)); e != nil {
					ctxLog(ctx).Error().Err(e).Msg("abort failed release upload")
				}
			}
		}()
		a.MultipartID = upload.ID()
		err = r.changeAsset(ctx, a.ID, name, func(current *domain.ReleaseAsset) error { current.MultipartID = a.MultipartID; return nil })
		if err != nil {
			return result, err
		}
		hash := sha256.New()
		buf := make([]byte, ports.BlobPartSize)
		remaining := a.Size
		for part := 1; remaining > 0; part++ {
			if part > ports.BlobMaxParts {
				return result, domain.ErrResourceExhausted
			}
			want := min(remaining, int64(len(buf)))
			var n int
			n, err = io.ReadFull(body, buf[:want])
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					err = fmt.Errorf("%w: body shorter than size", domain.ErrInvalid)
				}
				return result, err
			}
			hash.Write(buf[:n])
			err = upload.UploadPart(ctx, part, bytes.NewReader(buf[:n]), int64(n))
			if err != nil {
				return result, err
			}
			remaining -= int64(n)
		}
		if err = releaseBodyEnd(body); err != nil {
			return result, err
		}
		_, err = upload.Complete(ctx)
		if err != nil {
			return result, err
		}
		completed = true
		a.Digest = fmt.Sprintf("sha256:%x", hash.Sum(nil))
		a.MultipartID = ""
	}
	if a.Size <= ports.BlobPartSize {
		if err = releaseBodyEnd(body); err != nil {
			return result, err
		}
	}
	err = r.changeAsset(ctx, a.ID, name, func(current *domain.ReleaseAsset) error {
		if current.State != domain.ReleaseAssetUploading {
			return domain.ErrConflict
		}
		current.State = domain.ReleaseAssetUploaded
		current.Digest = a.Digest
		current.MultipartID = ""
		current.UpdatedAt = r.clock.Now()
		return nil
	})
	if err != nil {
		return result, err
	}
	result, err = r.store.GetAsset(ctx, a.ID)
	if err != nil {
		return result, err
	}
	rel, e := r.store.GetRelease(ctx, name, a.ReleaseID)
	if e == nil {
		r.emit(ctx, repo, rel, "updated")
	}
	ctxLog(ctx).Info().Int64("release", a.ReleaseID).Int64("asset", a.ID).Int64("size", a.Size).Msg("release asset uploaded")
	return result, nil
}
func validReleaseDigest(s string) bool {
	if len(s) != 71 || s[:7] != "sha256:" {
		return false
	}
	for _, c := range s[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// releaseContextReader checks cancellation between reads, including multipart
// buffering and the final extra-byte check. The transport owns interrupting a
// body.Read already in progress.
type releaseContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r releaseContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func releaseBodyEnd(body io.Reader) error {
	var extra [1]byte
	n, err := io.ReadFull(body, extra[:])
	if n > 0 {
		return fmt.Errorf("%w: body longer than size", domain.ErrInvalid)
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
func (r *Releases) changeAsset(ctx context.Context, id int64, repo domain.RepoName, fn func(*domain.ReleaseAsset) error) error {
	return r.updateReleases(ctx, repo, func(w *ports.ReleaseWrite) error {
		a, err := r.store.GetAsset(ctx, id)
		if err != nil {
			return err
		}
		if a.Repo != repo {
			return domain.ErrNotFound
		}
		if err = fn(&a); err != nil {
			return err
		}
		w.PutAssets = []domain.ReleaseAsset{a}
		return nil
	})
}

// removeAssetRecord conditionally removes the current record; sweep predicates
// are evaluated again after a conflict so a just-completed upload is protected.
func (r *Releases) removeAssetRecord(ctx context.Context, a domain.ReleaseAsset, predicate func(domain.ReleaseAsset) bool) error {
	return r.updateReleases(ctx, a.Repo, func(w *ports.ReleaseWrite) error {
		current, err := r.store.GetAsset(ctx, a.ID)
		if err != nil {
			return err
		}
		if current.Repo != a.Repo {
			return domain.ErrNotFound
		}
		if predicate != nil && !predicate(current) {
			return domain.ErrConflict
		}
		w.DeleteAssets = []domain.ReleaseAsset{current}
		w.Used -= current.Charged
		return nil
	})
}

// ListAssets returns completed assets only, ordered by name.
func (r *Releases) ListAssets(ctx context.Context, p domain.Principal, repo domain.RepoName, id int64) ([]domain.ReleaseAsset, error) {
	if _, err := r.GetRelease(ctx, p, repo, ports.ReleaseRef{ID: id}); err != nil {
		return nil, err
	}
	all, err := r.store.ListAssets(ctx, repo, id)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(a domain.ReleaseAsset) bool { return a.State != domain.ReleaseAssetUploaded }), nil
}

// GetAsset resolves an uploaded asset, enforcing its release's draft rule.
func (r *Releases) GetAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, ref ports.AssetRef) (domain.ReleaseAsset, error) {
	var a domain.ReleaseAsset
	var err error
	if ref.ID != 0 {
		if ref.Tag != "" || ref.Latest || ref.Name != "" {
			return a, domain.ErrInvalid
		}
		if _, err = r.readable(ctx, p, repo); err != nil {
			return a, err
		}
		a, err = r.store.GetAsset(ctx, ref.ID)
		if err != nil {
			return a, err
		}
		if a.Repo != repo {
			return domain.ReleaseAsset{}, domain.ErrNotFound
		}
		_, err = r.GetRelease(ctx, p, repo, ports.ReleaseRef{ID: a.ReleaseID})
	} else {
		if ref.Name == "" {
			return a, domain.ErrInvalid
		}
		var rel domain.Release
		rel, err = r.GetRelease(ctx, p, repo, ports.ReleaseRef{Tag: ref.Tag, Latest: ref.Latest})
		if err != nil {
			return a, err
		}
		a, err = r.store.GetAssetByName(ctx, repo, rel.ID, ref.Name)
	}
	if err == nil && a.State != domain.ReleaseAssetUploaded {
		err = domain.ErrNotFound
	}
	return a, err
}

// UpdateAsset changes an uploaded asset's name and label without moving bytes.
func (r *Releases) UpdateAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, id int64, params ports.UpdateAssetParams) (domain.ReleaseAsset, error) {
	meta, err := r.writable(ctx, p, repo)
	if err != nil {
		return domain.ReleaseAsset{}, err
	}
	a, err := r.GetAsset(ctx, p, repo, ports.AssetRef{ID: id})
	if err != nil {
		return a, err
	}
	var newName, newLabel string
	if params.Name != nil {
		newName, err = domain.ParseReleaseAssetName(*params.Name)
		if err != nil {
			return a, err
		}
	}
	if params.Label != nil {
		newLabel, err = domain.ParseReleaseAssetLabel(*params.Label)
		if err != nil {
			return a, err
		}
	}
	err = r.changeAsset(ctx, id, repo, func(current *domain.ReleaseAsset) error {
		if current.State != domain.ReleaseAssetUploaded {
			return domain.ErrNotFound
		}
		if params.Name != nil {
			current.Name = newName
		}
		if params.Label != nil {
			current.Label = newLabel
		}
		current.UpdatedAt = r.clock.Now()
		return nil
	})
	if err != nil {
		return domain.ReleaseAsset{}, err
	}
	a, err = r.store.GetAsset(ctx, id)
	if err != nil {
		return a, err
	}
	if rel, e := r.store.GetRelease(ctx, repo, a.ReleaseID); e == nil {
		r.emit(ctx, meta, rel, "updated")
	}
	return a, nil
}

// DeleteAsset refunds quota before deleting the blob.
func (r *Releases) DeleteAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, id int64) error {
	meta, err := r.writable(ctx, p, repo)
	if err != nil {
		return err
	}
	a, err := r.GetAsset(ctx, p, repo, ports.AssetRef{ID: id})
	if err != nil {
		return err
	}
	if err = r.removeAssetRecord(ctx, a, nil); err != nil {
		return err
	}
	r.removeAssetBlobs(context.WithoutCancel(ctx), []domain.ReleaseAsset{a})
	if rel, e := r.store.GetRelease(ctx, repo, a.ReleaseID); e == nil {
		r.emit(ctx, meta, rel, "updated")
	}
	return nil
}

// DownloadAsset sends metadata followed by bounded chunks of the requested range.
func (r *Releases) DownloadAsset(ctx context.Context, p domain.Principal, repo domain.RepoName, ref ports.AssetRef, offset, length int64, send func(ports.AssetChunk) error) error {
	a, err := r.GetAsset(ctx, p, repo, ref)
	if err != nil {
		return err
	}
	var body io.ReadCloser
	if a.Size == 0 && offset == 0 {
		// Blob stores reject Open on an empty object: stat it instead.
		if _, err = r.blobs.Stat(ctx, a.BlobKey()); err == nil {
			body = io.NopCloser(strings.NewReader(""))
		}
	} else {
		body, _, err = r.blobs.Open(ctx, a.BlobKey(), offset, length)
	}
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			ctxLog(ctx).Error().Int64("asset", a.ID).Msg("asset blob missing")
		}
		return err
	}
	defer func() { _ = body.Close() }()
	if err = send(ports.AssetChunk{Asset: &a}); err != nil {
		return err
	}
	buf := make([]byte, domain.CIArtifactChunk)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := body.Read(buf)
		if n > 0 {
			if err = send(ports.AssetChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
		if e != nil {
			return e
		}
	}
}

// Usage reports quota and configured limits to a repository reader.
func (r *Releases) Usage(ctx context.Context, p domain.Principal, repo domain.RepoName) (ports.ReleaseUsage, error) {
	if _, err := r.readable(ctx, p, repo); err != nil {
		return ports.ReleaseUsage{}, err
	}
	used, _, err := r.store.ReleaseUsage(ctx, repo)
	return ports.ReleaseUsage{Used: used, Limits: r.limits}, err
}
