// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// ShowKey lazily generates the server's mirror key for authenticated callers.
func (m *Mirrors) ShowKey(ctx context.Context, p domain.Principal) (domain.MirrorPublicKey, error) {
	if p.IsAnonymous() {
		return domain.MirrorPublicKey{}, domain.ErrUnauthenticated
	}
	return m.publicKey(ctx, false)
}

// RotateKey immediately replaces the mirror key. Remote authorization must be updated separately.
func (m *Mirrors) RotateKey(ctx context.Context, p domain.Principal) (domain.MirrorPublicKey, error) {
	if p.IsAnonymous() {
		return domain.MirrorPublicKey{}, domain.ErrUnauthenticated
	}
	if !p.HasOwnerRights() {
		return domain.MirrorPublicKey{}, domain.ErrForbidden
	}
	return m.publicKey(ctx, true)
}

func (m *Mirrors) publicKey(ctx context.Context, rotate bool) (domain.MirrorPublicKey, error) {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if err := m.loadKey(ctx, rotate); err != nil {
		return domain.MirrorPublicKey{}, err
	}
	host := strings.TrimPrefix(domain.MirrorKeyComment(m.cfg.PublicURL), "ermite-mirror@")
	return domain.MirrorPublicKeyOf(m.key.Public().(ed25519.PublicKey), host, m.keyCreated), nil
}

// loadKey is called with keyMu held. Only successfully persisted material is cached.
func (m *Mirrors) loadKey(ctx context.Context, rotate bool) error {
	if m.cfg.Sealer == nil {
		return fmt.Errorf("%w: the server has no ERMITE_SECRETS_KEY, so it cannot store the mirror key", domain.ErrPreconditionFailed)
	}
	if !rotate && m.key != nil {
		return nil
	}
	if !rotate {
		sealed, created, err := m.cfg.Keys.GetMirrorKey(ctx)
		if err == nil {
			seed, err := m.cfg.Sealer.Open(sealed, []byte(domain.MirrorKeyPath))
			if err != nil {
				return fmt.Errorf("%w: cannot open the stored mirror key", domain.ErrPreconditionFailed)
			}
			if len(seed) != ed25519.SeedSize {
				return fmt.Errorf("%w: invalid mirror key seed", domain.ErrPreconditionFailed)
			}
			m.key, m.keyCreated = ed25519.NewKeyFromSeed(seed), created
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("read mirror key: %w", err)
		}
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate mirror key: %w", err)
	}
	created := m.cfg.Clock.Now().UTC()
	sealed := m.cfg.Sealer.Seal(key.Seed(), []byte(domain.MirrorKeyPath))
	err = m.cfg.Keys.PutMirrorKey(ctx, sealed, created, !rotate)
	if !rotate && errors.Is(err, domain.ErrAlreadyExists) {
		return m.loadKey(ctx, false)
	}
	if err != nil {
		return fmt.Errorf("store mirror key: %w", err)
	}
	m.key, m.keyCreated = key, created
	return nil
}
