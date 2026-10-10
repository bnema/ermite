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
)

const (
	repoTokenPrefix  = "rp1:"
	tokenTokenPrefix = "tp1:"
)

// RepoPage is a page of repositories ordered by name.
type RepoPage struct {
	Repos []domain.Repo
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// ListPage returns a page of the repositories the principal may read, ordered
// by name. size is 0 for DefaultPageSize and lowered to MaxPageSize; the page
// token names the last repository of the previous page, so repositories that
// appear or disappear between two pages never shift the others.
func (r *Repos) ListPage(ctx context.Context, p domain.Principal, size int, token string) (RepoPage, error) {
	n, err := pageSize(size)
	if err != nil {
		return RepoPage{}, err
	}
	after, err := decodeRepoToken(token)
	if err != nil {
		return RepoPage{}, err
	}
	all, err := r.List(ctx, p)
	if err != nil {
		return RepoPage{}, err
	}
	var page RepoPage
	for _, repo := range all {
		if after != "" && repo.Name.String() <= after {
			continue
		}
		if len(page.Repos) == n {
			page.NextPageToken = encodeRepoToken(page.Repos[n-1].Name)
			break
		}
		page.Repos = append(page.Repos, repo)
	}
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

// TokenPage is a page of stored tokens, newest first.
type TokenPage struct {
	Tokens []domain.Token
	// NextPageToken is empty on the last page.
	NextPageToken string
}

// ListPage returns a page of the tokens List returns, newest first (then by
// id). size and the page token work like Repos.ListPage; the token names the
// last token of the previous page.
func (t *Tokens) ListPage(ctx context.Context, p domain.Principal, size int, token string) (TokenPage, error) {
	n, err := pageSize(size)
	if err != nil {
		return TokenPage{}, err
	}
	after, err := decodeTokenCursor(token)
	if err != nil {
		return TokenPage{}, err
	}
	all, err := t.List(ctx, p)
	if err != nil {
		return TokenPage{}, err
	}
	slices.SortFunc(all, func(a, b domain.Token) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	var page TokenPage
	for _, tok := range all {
		if after != nil && !tokenAfter(tok, *after) {
			continue
		}
		if len(page.Tokens) == n {
			page.NextPageToken = encodeTokenCursor(page.Tokens[n-1])
			break
		}
		page.Tokens = append(page.Tokens, tok)
	}
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
