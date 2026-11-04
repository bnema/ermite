// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// DisabledBernard is the DepsService of a server that runs without Bernard
// (ERMITE_DEPS_ENABLED=0): it lists nothing and refuses every action.
type DisabledBernard struct{}

var _ ports.DepsService = DisabledBernard{}

func disabledBernard(p domain.Principal) (domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return domain.RepoDeps{}, domain.ErrNotFound
	}
	return domain.RepoDeps{}, fmt.Errorf("%w: Bernard is disabled", domain.ErrPreconditionFailed)
}

// List returns no record to the owner.
func (DisabledBernard) List(_ context.Context, p domain.Principal, _ domain.RepoName) ([]domain.RepoDeps, error) {
	if !p.HasOwnerRights() {
		return nil, domain.ErrNotFound
	}
	return nil, nil
}

// Check refuses: Bernard is disabled.
func (DisabledBernard) Check(_ context.Context, p domain.Principal, _ domain.RepoName) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// Open refuses: Bernard is disabled.
func (DisabledBernard) Open(_ context.Context, p domain.Principal, _ domain.RepoName, _ domain.DepBatchKey, _ []domain.DepRef) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// Ignore refuses: Bernard is disabled.
func (DisabledBernard) Ignore(_ context.Context, p domain.Principal, _ domain.RepoName, _ domain.DepBatchKey, _ []domain.DepRef) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// SetAutoOpen refuses: Bernard is disabled.
func (DisabledBernard) SetAutoOpen(_ context.Context, p domain.Principal, _ domain.RepoName, _ bool) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// Split refuses: Bernard is disabled.
func (DisabledBernard) Split(_ context.Context, p domain.Principal, _ domain.RepoName, _ domain.DepName, _ bool) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// Snooze refuses: Bernard is disabled.
func (DisabledBernard) Snooze(_ context.Context, p domain.Principal, _ domain.RepoName) (domain.RepoDeps, error) {
	return disabledBernard(p)
}

// Queue returns an empty queue to the owner.
func (DisabledBernard) Queue(_ context.Context, p domain.Principal) (ports.DepsQueue, []domain.DepsQueueEntry, error) {
	if !p.HasOwnerRights() {
		return ports.DepsQueue{}, nil, domain.ErrNotFound
	}
	return ports.DepsQueue{}, nil, nil
}
