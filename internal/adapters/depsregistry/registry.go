// SPDX-License-Identifier: AGPL-3.0-or-later

// Package depsregistry queries dependency registries without credentials.
package depsregistry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Config supplies the GitHub remote and optional HTTP bases for tests.
// Empty bases select the public Go proxy and OSV service.
type Config struct {
	GitRemote  ports.GitRemote
	GoProxyURL string
	OSVURL     string
}

// Registry implements the dependency registry outbound port.
type Registry struct {
	remote     ports.GitRemote
	proxy, osv string
	client     *http.Client
}

var _ ports.DepsRegistry = (*Registry)(nil)

// New constructs a bounded registry client. Base URLs cannot change afterward.
func New(cfg Config) *Registry {
	if cfg.GoProxyURL == "" {
		cfg.GoProxyURL = "https://proxy.golang.org"
	}
	if cfg.OSVURL == "" {
		cfg.OSVURL = "https://api.osv.dev"
	}
	return &Registry{remote: cfg.GitRemote, proxy: strings.TrimRight(cfg.GoProxyURL, "/"), osv: strings.TrimRight(cfg.OSVURL, "/"), client: &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("registry redirect refused")
			}
			return nil
		},
	}}
}

func (r *Registry) request(req *http.Request, notFound bool) ([]byte, error) {
	req.Header.Set("User-Agent", "ermite-bernard")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if notFound && (res.StatusCode == 404 || res.StatusCode == 410) {
		return nil, domain.ErrNotFound
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: registry HTTP %d", domain.ErrUnavailable, res.StatusCode)
	}
	const cap = 1 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, cap+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cap {
		return nil, fmt.Errorf("%w: registry body exceeds 1 MiB", domain.ErrResourceExhausted)
	}
	return data, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 100 || strings.Trim(s, ".") == "" {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// ActionTags lists GitHub v* tags, preferring the peeled commit of annotated tags.
func (r *Registry) ActionTags(ctx context.Context, owner, repo string) ([]domain.DepTag, error) {
	if !validName(owner) || !validName(repo) {
		return nil, domain.ErrInvalid
	}
	if r.remote == nil {
		return nil, fmt.Errorf("%w: GitRemote not configured", domain.ErrUnavailable)
	}
	// GitRemote already maps GitHub's 404/authentication exit messages to
	// ErrNotFound; network and timeout failures remain errors.
	refs, err := r.remote.ListRefs(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var tags []domain.DepTag
	for _, ref := range refs {
		if !strings.HasPrefix(ref.Name, "refs/tags/v") {
			continue
		}
		sha := ref.Peeled
		if sha == "" {
			sha = ref.ID
		}
		tags = append(tags, domain.DepTag{Name: strings.TrimPrefix(ref.Name, "refs/tags/"), SHA: sha})
	}
	return tags, nil
}
