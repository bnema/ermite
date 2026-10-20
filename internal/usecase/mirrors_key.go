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
	"bnema.dev/ermite/internal/ports"
)

const noSecretsKeyDetail = "the server has no ERMITE_SECRETS_KEY, so it cannot store the mirror key"

// ShowKey lazily generates the server's mirror key for authenticated callers.
func (m *Mirrors) ShowKey(ctx context.Context, p domain.Principal) (domain.MirrorPublicKey, error) {
	if p.IsAnonymous() {
		return domain.MirrorPublicKey{}, domain.ErrUnauthenticated
	}
	if m.cfg.Sealer == nil {
		return domain.MirrorPublicKey{}, fmt.Errorf("%w: %s", domain.ErrPreconditionFailed, noSecretsKeyDetail)
	}
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if err := m.loadKey(ctx, true); err != nil {
		return domain.MirrorPublicKey{}, err
	}
	return m.publicKeyLocked(), nil
}

// RotateKey immediately replaces the mirror key and returns the replaced one (zero when there
// was none, or when it could not be opened). Remote authorization must be updated separately.
func (m *Mirrors) RotateKey(ctx context.Context, p domain.Principal) (ports.MirrorKeyRotation, error) {
	if p.IsAnonymous() {
		return ports.MirrorKeyRotation{}, domain.ErrUnauthenticated
	}
	if !p.HasOwnerRights() {
		return ports.MirrorKeyRotation{}, domain.ErrForbidden
	}
	if m.cfg.Sealer == nil {
		return ports.MirrorKeyRotation{}, fmt.Errorf("%w: %s", domain.ErrPreconditionFailed, noSecretsKeyDetail)
	}
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	var previous domain.MirrorPublicKey
	// A missing key is not created just to be replaced, and a stored key that cannot be read
	// or opened must not prevent its replacement.
	if err := m.loadKey(ctx, false); err == nil {
		previous = m.publicKeyLocked()
	}
	if err := m.storeNewKey(ctx, false); err != nil {
		return ports.MirrorKeyRotation{}, err
	}
	return ports.MirrorKeyRotation{Key: m.publicKeyLocked(), Previous: previous}, nil
}

func (m *Mirrors) publicKeyLocked() domain.MirrorPublicKey {
	host := strings.TrimPrefix(domain.MirrorKeyComment(m.cfg.PublicURL), "ermite-mirror@")
	return domain.MirrorPublicKeyOf(m.key.Public().(ed25519.PublicKey), host, m.keyCreated)
}

// privateKey returns a copy so a transport cannot mutate the cached key. Failures are
// *domain.MirrorError of class internal.
func (m *Mirrors) privateKey(ctx context.Context) (ed25519.PrivateKey, error) {
	if m.cfg.Sealer == nil {
		return nil, &domain.MirrorError{Class: domain.MirrorClassInternal, Detail: noSecretsKeyDetail}
	}
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if err := m.loadKey(ctx, true); err != nil {
		detail := "cannot read the mirror key"
		if errors.Is(err, domain.ErrPreconditionFailed) {
			detail = err.Error()
		}
		return nil, &domain.MirrorError{Class: domain.MirrorClassInternal, Detail: detail}
	}
	return append(ed25519.PrivateKey(nil), m.key...), nil
}

// loadKey is called with keyMu held and a sealer present: it caches the stored key, creating
// one when none exists and create is set (ErrNotFound otherwise).
func (m *Mirrors) loadKey(ctx context.Context, create bool) error {
	if m.key != nil {
		return nil
	}
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
	if !create {
		return err
	}
	err = m.storeNewKey(ctx, true)
	if errors.Is(err, domain.ErrAlreadyExists) {
		return m.loadKey(ctx, true) // another instance created it first
	}
	return err
}

// storeNewKey generates, seals and persists a key, caching it only once persisted.
func (m *Mirrors) storeNewKey(ctx context.Context, ifAbsent bool) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate mirror key: %w", err)
	}
	created := m.cfg.Clock.Now().UTC()
	sealed := m.cfg.Sealer.Seal(key.Seed(), []byte(domain.MirrorKeyPath))
	if err := m.cfg.Keys.PutMirrorKey(ctx, sealed, created, ifAbsent); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return err
		}
		return fmt.Errorf("store mirror key: %w", err)
	}
	m.key, m.keyCreated = key, created
	return nil
}
