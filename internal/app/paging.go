// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

const (
	repoTokenPrefix  = "rp1:"
	tokenTokenPrefix = "tp1:"

	// maxTokenLen bounds a page token before it is decoded.
	maxTokenLen = 1024
)

// Page sizes of the listings.
const (
	// DefaultPageSize is the page size when the caller gives none.
	DefaultPageSize = 50
	// MaxPageSize is the largest page; a larger request is lowered to it.
	MaxPageSize = 100
	// MaxRepoPageSize is the largest page of ListRepos. A repository list is
	// one scan of the metadata whatever the page, so a caller that wants the
	// whole list (the web pages) asks for it in one page rather than in
	// several scans.
	MaxRepoPageSize = 1000
	// MaxRefPageSize is the largest page of ListRefs, for the same reason:
	// each page of refs runs the whole for-each-ref again.
	MaxRefPageSize = 1000
)

// MaxCommitSkip bounds how deep into a history a page token may point: each
// page walks the history from its start with `git log --skip`.
const MaxCommitSkip = 100_000

// pageSize applies the default and the maximum.
func pageSize(n int) (int, error) { return pageSizeUpTo(n, MaxPageSize) }

// pageSizeUpTo applies the default and the given maximum.
func pageSizeUpTo(n, most int) (int, error) {
	switch {
	case n < 0:
		return 0, fmt.Errorf("%w: %d", domain.ErrInvalidPageSize, n)
	case n == 0:
		return DefaultPageSize, nil
	}
	return min(n, most), nil
}

func decodeToken(tok, prefix string) (string, bool) {
	if len(tok) > maxTokenLen {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", false
	}
	return strings.CutPrefix(string(raw), prefix)
}

// pageAfter returns the first size items of the ordered items that satisfy
// after (nil keeps all of them) and, when more follow, the token of the last
// one returned. Tokens name the last item, so items that appear or disappear
// between two pages never shift the others.
func pageAfter[T any](items []T, size int, after func(T) bool, token func(T) string) (page []T, next string) {
	for _, it := range items {
		if after != nil && !after(it) {
			continue
		}
		if len(page) == size {
			return page, token(page[size-1])
		}
		page = append(page, it)
	}
	return page, ""
}

// ListPage returns a page of the repositories the principal may read, ordered
// by name. size is 0 for DefaultPageSize and lowered to MaxRepoPageSize; the page
// token names the last repository of the previous page, so repositories that
// appear or disappear between two pages never shift the others.
func (r *Repos) ListPage(ctx context.Context, p domain.Principal, size int, token string) (ports.RepoPage, error) {
	n, err := pageSizeUpTo(size, MaxRepoPageSize)
	if err != nil {
		return ports.RepoPage{}, err
	}
	after, err := decodeRepoToken(token)
	if err != nil {
		return ports.RepoPage{}, err
	}
	all, err := r.List(ctx, p)
	if err != nil {
		return ports.RepoPage{}, err
	}
	var page ports.RepoPage
	page.Repos, page.NextPageToken = pageAfter(all, n,
		func(repo domain.Repo) bool { return after == "" || repo.Name.String() > after },
		func(repo domain.Repo) string { return encodeRepoToken(repo.Name) })
	return page, nil
}

func encodeRepoToken(last domain.RepoName) string {
	return base64.RawURLEncoding.EncodeToString([]byte(repoTokenPrefix + last.String()))
}

// decodeRepoToken returns "" for the empty token.
func decodeRepoToken(tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	raw, ok := decodeToken(tok, repoTokenPrefix)
	if !ok || raw == "" {
		return "", fmt.Errorf("%w: repositories", domain.ErrInvalidPageToken)
	}
	return raw, nil
}

// ListPage returns a page of the tokens List returns, newest first (then by
// id). size and the page token work like Repos.ListPage; the token names the
// last token of the previous page.
func (t *Tokens) ListPage(ctx context.Context, p domain.Principal, size int, token string) (ports.TokenPage, error) {
	n, err := pageSize(size)
	if err != nil {
		return ports.TokenPage{}, err
	}
	after, err := decodeTokenCursor(token)
	if err != nil {
		return ports.TokenPage{}, err
	}
	all, err := t.List(ctx, p)
	if err != nil {
		return ports.TokenPage{}, err
	}
	slices.SortFunc(all, func(a, b domain.Token) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	var page ports.TokenPage
	page.Tokens, page.NextPageToken = pageAfter(all, n,
		func(tok domain.Token) bool { return after == nil || tokenAfter(tok, *after) },
		encodeTokenCursor)
	return page, nil
}

type tokenCursor struct {
	created int64
	id      string
}

// tokenAfter reports whether tok comes after the cursor in newest-first order.
func tokenAfter(tok domain.Token, c tokenCursor) bool {
	if created := tok.CreatedAt.UnixNano(); created != c.created {
		return created < c.created
	}
	return tok.ID < c.id
}

func encodeTokenCursor(last domain.Token) string {
	raw := tokenTokenPrefix + strconv.FormatInt(last.CreatedAt.UnixNano(), 10) + ":" + last.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeTokenCursor returns nil for the empty token.
func decodeTokenCursor(tok string) (*tokenCursor, error) {
	if tok == "" {
		return nil, nil
	}
	bad := fmt.Errorf("%w: tokens", domain.ErrInvalidPageToken)
	raw, ok := decodeToken(tok, tokenTokenPrefix)
	if !ok {
		return nil, bad
	}
	created, id, ok := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(created, 10, 64)
	if !ok || err != nil || id == "" {
		return nil, bad
	}
	return &tokenCursor{created: n, id: id}, nil
}
