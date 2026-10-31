// SPDX-License-Identifier: AGPL-3.0-or-later

// Package depsregistry queries dependency registries without credentials.
package depsregistry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Config supplies the GitHub remote and optional HTTP bases for tests.
// Empty bases select the public Go proxy, npm registry and OSV service.
type Config struct {
	GitRemote      ports.GitRemote
	GoProxyURL     string
	OSVURL         string
	NPMRegistryURL string
}

// Registry implements the dependency registry outbound port.
type Registry struct {
	remote          ports.GitRemote
	proxy, osv, npm string
	client          *http.Client
	npmBytes        int64 // the npm document cap; tests lower it
	sleep           func(context.Context, time.Duration) error
	jitter          func(time.Duration) time.Duration
}

var _ ports.DepsRegistry = (*Registry)(nil)

// New constructs a bounded registry client. Base URLs cannot change afterward.
func New(cfg Config) *Registry {
	if cfg.GoProxyURL == "" {
		cfg.GoProxyURL = "https://proxy.golang.org"
	}
	if cfg.NPMRegistryURL == "" {
		cfg.NPMRegistryURL = "https://registry.npmjs.org"
	}
	if cfg.OSVURL == "" {
		cfg.OSVURL = "https://api.osv.dev"
	}
	return &Registry{remote: cfg.GitRemote, npm: strings.TrimRight(cfg.NPMRegistryURL, "/"), proxy: strings.TrimRight(cfg.GoProxyURL, "/"), osv: strings.TrimRight(cfg.OSVURL, "/"), npmBytes: npmDocBytes, client: &http.Client{
		// Each request supplies its own timeout.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("registry redirect refused")
			}
			return nil
		},
	}}
}

// Error bodies are drained at most this far to reuse the connection.
const maxDrainBytes = 64 << 10

// largeBody marks requests whose attempt timeout is spent reading a large
// document: a second attempt would likely time out again.
const largeBody = 1 << 20

func (r *Registry) request(req *http.Request, notFound bool, cap int64, timeout time.Duration) ([]byte, error) {
	var data []byte
	err := r.stream(req, notFound, cap, timeout, func(body io.Reader) error {
		var err error
		data, err = io.ReadAll(body)
		return err
	})
	return data, err
}

// cappedReader fails with domain.ErrResourceExhausted past n bytes.
type cappedReader struct {
	r   io.Reader
	n   int64
	cap int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n > c.cap {
		return 0, fmt.Errorf("%w: registry body exceeds %d bytes", domain.ErrResourceExhausted, c.cap)
	}
	if int64(len(p)) > c.cap+1-c.n {
		p = p[:c.cap+1-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.cap {
		return n, fmt.Errorf("%w: registry body exceeds %d bytes", domain.ErrResourceExhausted, c.cap)
	}
	return n, err
}

// stream sends req with retries and hands a successful body, bounded by cap,
// to consume within the attempt deadline.
func (r *Registry) stream(req *http.Request, notFound bool, cap int64, timeout time.Duration, consume func(io.Reader) error) error {
	parent := req.Context()
	if req.Body != nil && req.GetBody != nil {
		defer func() { _ = req.Body.Close() }()
	}
	// Constructors usually supply GetBody; buffer other bodies once before sending.
	if req.Body != nil && req.GetBody == nil {
		data, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return err
		}
		req = req.Clone(parent)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	jitter := r.jitter
	if jitter == nil {
		jitter = func(max time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(max) + 1)) }
	}
	for attempt := 0; ; attempt++ {
		if err := parent.Err(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		current := req.Clone(ctx)
		current.Header.Set("User-Agent", "ermite-bernard")
		if req.GetBody != nil {
			var err error
			current.Body, err = req.GetBody()
			if err != nil {
				cancel()
				return err
			}
		}
		res, err := r.client.Do(current)
		retry := false
		wait := time.Duration(0)
		if err != nil {
			err = fmt.Errorf("registry request: %w", err)
			// A response alongside an error indicates a refused redirect,
			// not a transient transport failure (Do already closed its body).
			retry = res == nil && (cap <= largeBody || !errors.Is(err, context.DeadlineExceeded))
		} else {
			status := res.StatusCode
			switch {
			case (status == 403 || status == 429) && res.Header.Get("X-RateLimit-Remaining") == "0":
				reset := res.Header.Get("X-RateLimit-Reset")
				if seconds, parseErr := strconv.ParseInt(reset, 10, 64); parseErr == nil {
					reset = time.Unix(seconds, 0).UTC().Format(time.RFC3339)
				}
				err = fmt.Errorf("%w: registry HTTP %d: %s rate limit resets at %s", domain.ErrUnavailable, status, req.URL.Host, reset)
			case notFound && (status == 404 || status == 410):
				err = domain.ErrNotFound
			case status != http.StatusOK:
				err = fmt.Errorf("%w: registry HTTP %d", domain.ErrUnavailable, status)
				retry = status == 429 || status == 500 || status == 502 || status == 503 || status == 504
				if status == 429 || status == 503 {
					value := res.Header.Get("Retry-After")
					if seconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && seconds >= 0 {
						if seconds > 30 {
							retry = false
						} else {
							wait = time.Duration(seconds) * time.Second
						}
					} else if date, parseErr := http.ParseTime(value); parseErr == nil {
						wait = time.Until(date)
						if wait > 30*time.Second {
							retry = false
						}
					}
				}
			default:
				err = consume(&cappedReader{r: res.Body, cap: cap})
				_ = res.Body.Close()
				cancel()
				if err != nil && parent.Err() != nil {
					return parent.Err()
				}
				return err
			}
			// Bound draining both by bytes and the attempt deadline.
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxDrainBytes))
			_ = res.Body.Close()
		}
		cancel()
		if parent.Err() != nil {
			return parent.Err()
		}
		if !retry || attempt == 2 {
			return err
		}
		backoff := min(500*time.Millisecond*time.Duration(1<<attempt), 5*time.Second)
		wait = max(wait, jitter(backoff))
		if err := sleep(parent, wait); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
