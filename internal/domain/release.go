// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Release binds release notes and assets to a repository tag.
type Release struct {
	ID                            int64
	Repo                          RepoName
	Tag, Title, Notes             string
	Draft, Prerelease, MakeLatest bool
	// TargetCommitish is the commit at publication, empty for an unresolved draft.
	TargetCommitish                   string
	Author                            string
	CreatedAt, PublishedAt, UpdatedAt time.Time
	Version                           uint64
}

// ReleaseAssetState distinguishes reserved uploads from visible assets.
type ReleaseAssetState string

const (
	ReleaseAssetUploading ReleaseAssetState = "uploading"
	ReleaseAssetUploaded  ReleaseAssetState = "uploaded"
)

// ReleaseAsset is metadata only; its contents live in the blob store.
type ReleaseAsset struct {
	ID, ReleaseID            int64
	Repo                     RepoName
	Name, Label, ContentType string
	Size                     int64
	Digest                   string
	State                    ReleaseAssetState
	Uploader                 string
	// Charged counts bytes reserved against repository quota.
	Charged int64
	// MultipartID identifies an unfinished upload for cleanup.
	MultipartID          string
	CreatedAt, UpdatedAt time.Time
	Version              uint64
}

// BlobKey contains only a globally allocated identifier.
func (a ReleaseAsset) BlobKey() string { return fmt.Sprintf("release/%020d", a.ID) }

const (
	ReleaseMaxTitleLen      = 256
	ReleaseMaxNotesLen      = 125_000
	ReleaseAssetMaxNameLen  = 255
	ReleaseAssetMaxLabelLen = 255
	ReleaseAssetPendingMax  = 24 * time.Hour
	DefaultReleaseAssetMax  = 2 << 30
	DefaultReleaseAssetsMax = 1000
	DefaultReleaseQuota     = 20 << 30
)

// ParseReleaseTag validates a short tag using Git reference-name rules.
func ParseReleaseTag(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty tag name", ErrInvalid)
	}
	if strings.HasPrefix(s, "refs/") {
		return "", fmt.Errorf("%w: %q: give the short tag name", ErrInvalid, s)
	}
	if reason := refNameProblem(s); reason != "" {
		return "", fmt.Errorf("%w: %q: %s", ErrInvalid, s, reason)
	}
	return s, nil
}

func cleanReleaseText(s string, limit int, multiline bool) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalid)
	}
	s = norm.NFC.String(s)
	s = strings.Map(func(r rune) rune {
		if multiline && (r == '\n' || r == '\t' || r == '\r') {
			return r
		}
		if unicode.IsControl(r) || (!multiline && isBidiControl(r)) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > limit {
		return "", fmt.Errorf("%w: text exceeds %d characters", ErrInvalid, limit)
	}
	return s, nil
}

// ParseReleaseTitle normalizes and cleans an optional title.
func ParseReleaseTitle(s string) (string, error) {
	return cleanReleaseText(s, ReleaseMaxTitleLen, false)
}

// ParseReleaseNotes validates and cleans multiline notes.
func ParseReleaseNotes(s string) (string, error) {
	return cleanReleaseText(s, ReleaseMaxNotesLen, true)
}

// ParseReleaseAssetLabel normalizes and cleans an optional label.
func ParseReleaseAssetLabel(s string) (string, error) {
	return cleanReleaseText(s, ReleaseAssetMaxLabelLen, false)
}

// ParseReleaseAssetName validates a single filename without rewriting it.
func ParseReleaseAssetName(s string) (string, error) {
	if !utf8.ValidString(s) || s == "" || s == "." || s == ".." || utf8.RuneCountInString(s) > ReleaseAssetMaxNameLen || strings.ContainsAny(s, "/\\") {
		return "", fmt.Errorf("%w: invalid asset name", ErrInvalid)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: control character in asset name", ErrInvalid)
		}
	}
	return s, nil
}

// ParseReleaseContentType validates a printable ASCII media type.
func ParseReleaseContentType(s string) (string, error) {
	if s == "" {
		return "application/octet-stream", nil
	}
	if len(s) > 255 {
		return "", fmt.Errorf("%w: media type too long", ErrInvalid)
	}
	for _, r := range s {
		if r < 32 || r > 126 {
			return "", fmt.Errorf("%w: invalid media type", ErrInvalid)
		}
	}
	if _, _, err := mime.ParseMediaType(s); err != nil {
		return "", fmt.Errorf("%w: media type: %v", ErrInvalid, err)
	}
	return s, nil
}

// Published reports whether the release is public to repository readers.
func (r Release) Published() bool { return !r.Draft }

// LatestRelease selects a pin first, then the newest stable published release.
func LatestRelease(rs []Release) (Release, bool) {
	var best Release
	ok := false
	for _, r := range rs {
		if r.Draft || r.Prerelease {
			continue
		}
		if !ok || (r.MakeLatest && !best.MakeLatest) || (r.MakeLatest == best.MakeLatest && (r.CreatedAt.After(best.CreatedAt) || (r.CreatedAt.Equal(best.CreatedAt) && r.ID > best.ID))) {
			best, ok = r, true
		}
	}
	return best, ok
}
