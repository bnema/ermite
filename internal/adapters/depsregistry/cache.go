// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"context"
	"errors"
	"slices"
	"sync"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

type result[T any] struct {
	value T
	err   error
}
type cached struct {
	registry   ports.DepsRegistry
	mu         sync.Mutex
	goVersions map[[2]string]result[[]domain.DepVersion]
	directives map[[2]string]result[string]
	tags       map[[2]string]result[[]domain.DepTag]
	malicious  map[[3]string]result[bool]
}

// NewCached creates a memo cache owned by one check, not shared across checks.
// Calls are serialized so concurrent requests for the same key query only once.
func NewCached(registry ports.DepsRegistry) ports.DepsRegistry {
	return &cached{registry: registry, goVersions: make(map[[2]string]result[[]domain.DepVersion]), directives: make(map[[2]string]result[string]), tags: make(map[[2]string]result[[]domain.DepTag]), malicious: make(map[[3]string]result[bool])}
}
func cacheable(err error) bool { return err == nil || errors.Is(err, domain.ErrNotFound) }

func (c *cached) GoVersions(ctx context.Context, name, current string) ([]domain.DepVersion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]string{name, current}
	if v, ok := c.goVersions[key]; ok {
		return slices.Clone(v.value), v.err
	}
	value, err := c.registry.GoVersions(ctx, name, current)
	if cacheable(err) {
		c.goVersions[key] = result[[]domain.DepVersion]{slices.Clone(value), err}
	}
	return value, err
}
func (c *cached) GoDirective(ctx context.Context, name, version string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]string{name, version}
	if v, ok := c.directives[key]; ok {
		return v.value, v.err
	}
	value, err := c.registry.GoDirective(ctx, name, version)
	if cacheable(err) {
		c.directives[key] = result[string]{value, err}
	}
	return value, err
}
func (c *cached) ActionTags(ctx context.Context, owner, repo string) ([]domain.DepTag, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [2]string{owner, repo}
	if v, ok := c.tags[key]; ok {
		return slices.Clone(v.value), v.err
	}
	value, err := c.registry.ActionTags(ctx, owner, repo)
	if cacheable(err) {
		c.tags[key] = result[[]domain.DepTag]{slices.Clone(value), err}
	}
	return value, err
}
func (c *cached) Malicious(ctx context.Context, eco domain.DepsEcosystem, name, version string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := [3]string{string(eco), name, version}
	if v, ok := c.malicious[key]; ok {
		return v.value, v.err
	}
	value, err := c.registry.Malicious(ctx, eco, name, version)
	if cacheable(err) {
		c.malicious[key] = result[bool]{value, err}
	}
	return value, err
}
