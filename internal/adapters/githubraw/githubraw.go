// SPDX-License-Identifier: AGPL-3.0-or-later

// Package githubraw downloads a reusable workflow file from
// raw.githubusercontent.com at a commit (ports.WorkflowFetcher). It sends no
// credentials, bounds the time and the size of the download, and follows no
// redirect to another host.
package githubraw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// DefaultBaseURL is where the files are.
const DefaultBaseURL = "https://raw.githubusercontent.com"

// DefaultTimeout bounds one download.
const DefaultTimeout = 20 * time.Second

// Config configures a Fetcher; tests replace BaseURL and Client.
type Config struct {
	BaseURL string
	Timeout time.Duration
	Client  *http.Client
}

// Fetcher implements ports.WorkflowFetcher over HTTPS.
type Fetcher struct {
	base   string
	client *http.Client
}

var _ ports.WorkflowFetcher = (*Fetcher)(nil)

// New builds a Fetcher.
func New(cfg Config) *Fetcher {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	c := cfg.Client
	if c == nil {
		c = &http.Client{Transport: http.DefaultTransport}
	}
	cc := *c
	cc.Timeout = cfg.Timeout
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Fetcher{base: strings.TrimRight(cfg.BaseURL, "/"), client: &cc}
}

// FetchWorkflow downloads path of owner/repo at sha.
func (f *Fetcher) FetchWorkflow(ctx context.Context, owner, repo, sha, path string, maxBytes int64) ([]byte, error) {
	segs := []string{owner, repo, sha}
	segs = append(segs, strings.Split(path, "/")...)
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("%w: bad workflow location", domain.ErrInvalid)
		}
	}
	esc := make([]string, len(segs))
	for i, s := range segs {
		esc[i] = url.PathEscape(s)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/"+strings.Join(esc, "/"), nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: download the reusable workflow: %w", domain.ErrUnavailable, err)
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return nil, domain.ErrNotFound
	default:
		return nil, fmt.Errorf("%w: the download answered %d", domain.ErrUnavailable, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: download the reusable workflow: %w", domain.ErrUnavailable, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: the reusable workflow is larger than %d bytes", domain.ErrResourceExhausted, maxBytes)
	}
	return data, nil
}
