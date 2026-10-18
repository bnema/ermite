// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MaxSignatureCacheEntries bounds the verification results kept in memory.
const MaxSignatureCacheEntries = 10000

// Signatures manages the trusted keys and tells whether commits are signed by
// them (ADR 0030). The keys are loaded once and kept in memory: this process
// is their only writer.
type Signatures struct {
	store     ports.TrustedKeyStore
	inspector ports.KeyInspector
	verifiers map[domain.SignatureFormat]ports.SignatureVerifier
	objects   ports.CommitObjectReader
	builtin   domain.TrustedKey
	clock     ports.Clock

	// mu guards stored and generation. Mutations hold it for writing during
	// the store call, which serializes them and makes the limit check exact.
	mu     sync.RWMutex
	stored []domain.TrustedKey // oldest first
	// generation changes whenever the set of trusted keys does, so cached
	// results of an older set are never returned.
	generation uint64

	cacheMu sync.Mutex
	cache   map[sigKey]domain.CommitVerification
}

var (
	_ ports.TrustedKeyService = (*Signatures)(nil)
	_ ports.CommitVerifier    = (*Signatures)(nil)
)

// sigKey identifies a cached verification.
type sigKey struct {
	repo       domain.RepoName
	id         domain.ObjectID
	generation uint64
}

// NewSignatures loads the stored trusted keys. builtin is the instance
// signing key (Builtin, SSH, ID = its fingerprint), built by the caller; a
// zero ID means there is none.
func NewSignatures(store ports.TrustedKeyStore, inspector ports.KeyInspector, verifiers map[domain.SignatureFormat]ports.SignatureVerifier,
	objects ports.CommitObjectReader, builtin domain.TrustedKey, clock ports.Clock) (*Signatures, error) {
	keys, err := store.ListTrustedKeys(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load trusted keys: %w", err)
	}
	return &Signatures{
		store: store, inspector: inspector, verifiers: verifiers, objects: objects, builtin: builtin, clock: clock,
		stored: keys, cache: map[sigKey]domain.CommitVerification{},
	}, nil
}

func cloneKey(k domain.TrustedKey) domain.TrustedKey {
	k.Emails = slices.Clone(k.Emails)
	return k
}

// requireKeyOwner is the access rule of the key changes: the owner.
func requireKeyOwner(p domain.Principal) error { return requireOwnerFor(p, "managing trusted keys") }

// ListTrustedKeys lists the instance key first, then the stored keys oldest
// first. Any authenticated caller may.
func (s *Signatures) ListTrustedKeys(_ context.Context, p domain.Principal) ([]domain.TrustedKey, error) {
	if p.IsAnonymous() {
		return nil, domain.ErrUnauthenticated
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.TrustedKey, 0, len(s.stored)+1)
	if s.builtin.ID != "" {
		out = append(out, cloneKey(s.builtin))
	}
	for _, k := range s.stored {
		out = append(out, cloneKey(k))
	}
	return out, nil
}

// AddTrustedKey stores a key. Only the owner may. The instance key cannot be
// added again (domain.ErrAlreadyExists), and at most domain.MaxTrustedKeys
// keys are stored (domain.ErrResourceExhausted).
func (s *Signatures) AddTrustedKey(ctx context.Context, p domain.Principal, format domain.SignatureFormat, title, publicKey string, emails []string) (domain.TrustedKey, error) {
	if err := requireKeyOwner(p); err != nil {
		return domain.TrustedKey{}, err
	}
	title, err := domain.ParseTrustedKeyTitle(title)
	if err != nil {
		return domain.TrustedKey{}, err
	}
	emails, err = domain.NormalizeKeyEmails(emails)
	if err != nil {
		return domain.TrustedKey{}, err
	}
	if format, err = domain.ParseSignatureFormat(string(format)); err != nil {
		return domain.TrustedKey{}, err
	}
	if len(publicKey) > domain.MaxTrustedKeyBytes {
		return domain.TrustedKey{}, fmt.Errorf("%w: public key over %d bytes", domain.ErrInvalid, domain.MaxTrustedKeyBytes)
	}
	id, canonical, err := s.inspector.Inspect(format, publicKey)
	if err != nil {
		return domain.TrustedKey{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if (s.builtin.ID != "" && id == s.builtin.ID) || slices.ContainsFunc(s.stored, func(k domain.TrustedKey) bool { return k.ID == id }) {
		return domain.TrustedKey{}, fmt.Errorf("%w: trusted key %s", domain.ErrAlreadyExists, id)
	}
	if len(s.stored) >= domain.MaxTrustedKeys {
		return domain.TrustedKey{}, fmt.Errorf("%w: at most %d trusted keys", domain.ErrResourceExhausted, domain.MaxTrustedKeys)
	}
	key := domain.TrustedKey{
		ID: id, Format: format, Title: title, PublicKey: canonical, Emails: emails, CreatedAt: s.clock.Now().UTC(),
	}
	if err := s.store.CreateTrustedKey(ctx, key); err != nil {
		return domain.TrustedKey{}, err
	}
	s.stored = append(s.stored, key)
	s.generation++
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("key_id", id).Msg("trusted key added")
	return cloneKey(key), nil
}

// RemoveTrustedKey deletes a key. Only the owner may. The instance key is
// refused with domain.ErrPreconditionFailed.
func (s *Signatures) RemoveTrustedKey(ctx context.Context, p domain.Principal, id string) error {
	if err := requireKeyOwner(p); err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("%w: key id", domain.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.builtin.ID != "" && id == s.builtin.ID {
		return fmt.Errorf("%w: the instance signing key cannot be removed", domain.ErrPreconditionFailed)
	}
	if err := s.store.DeleteTrustedKey(ctx, id); err != nil {
		return err
	}
	s.stored = slices.DeleteFunc(s.stored, func(k domain.TrustedKey) bool { return k.ID == id })
	s.generation++
	log := zerowrap.FromCtx(ctx)
	log.Info().Str("key_id", id).Msg("trusted key removed")
	return nil
}

// snapshot returns the current trust generation and the trusted keys of each
// format, the instance key included.
func (s *Signatures) snapshot() (uint64, map[domain.SignatureFormat][]domain.TrustedKey) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byFormat := map[domain.SignatureFormat][]domain.TrustedKey{}
	if s.builtin.ID != "" {
		byFormat[s.builtin.Format] = append(byFormat[s.builtin.Format], s.builtin)
	}
	for _, k := range s.stored {
		byFormat[k.Format] = append(byFormat[k.Format], k)
	}
	return s.generation, byFormat
}

// VerifyCommits checks the signature of the commits against the trusted keys.
// Results are cached by repository, commit and trust generation. When the raw
// commits cannot be read, only the cached results are returned.
func (s *Signatures) VerifyCommits(ctx context.Context, repo domain.RepoName, commits []domain.Commit) map[domain.ObjectID]domain.CommitVerification {
	out := make(map[domain.ObjectID]domain.CommitVerification, len(commits))
	if len(commits) == 0 {
		return out
	}
	gen, keys := s.snapshot()

	var missing []domain.ObjectID
	byID := make(map[domain.ObjectID]domain.Commit, len(commits))
	s.cacheMu.Lock()
	for _, c := range commits {
		if _, dup := byID[c.ID]; dup {
			continue
		}
		byID[c.ID] = c
		if v, ok := s.cache[sigKey{repo, c.ID, gen}]; ok {
			out[c.ID] = v
		} else {
			missing = append(missing, c.ID)
		}
	}
	s.cacheMu.Unlock()
	if len(missing) == 0 {
		return out
	}

	raws, err := s.objects.ReadCommitObjects(ctx, repo, missing)
	if err != nil {
		log := zerowrap.FromCtx(ctx)
		log.Warn().Err(err).Msg("read commit objects for signature check")
		return out
	}
	fresh := make(map[sigKey]domain.CommitVerification, len(raws))
	for _, id := range missing {
		raw, ok := raws[id]
		if !ok {
			continue
		}
		v := s.verifyOne(byID[id], raw, keys)
		out[id] = v
		fresh[sigKey{repo, id, gen}] = v
	}
	s.remember(fresh)
	return out
}

// verifyOne checks one raw commit.
func (s *Signatures) verifyOne(c domain.Commit, raw []byte, keys map[domain.SignatureFormat][]domain.TrustedKey) domain.CommitVerification {
	payload, sig, signed := domain.SplitCommitSignature(raw)
	if !signed {
		return domain.CommitVerification{Status: domain.StatusUnsigned}
	}
	format := domain.SignatureFormatOf(sig)
	switch format {
	case domain.FormatNone:
		return domain.CommitVerification{Format: domain.FormatNone, Status: domain.StatusBadSignature}
	case domain.FormatX509:
		return domain.CommitVerification{Format: domain.FormatX509, Status: domain.StatusUnsupported}
	}
	v, ok := s.verifiers[format]
	if !ok {
		return domain.CommitVerification{Format: format, Status: domain.StatusUnsupported}
	}
	return v.Verify(payload, sig, c.Committer.Email, c.Committer.Time.Unix(), keys[format])
}

// remember adds results to the cache. The cache is a plain map: when adding
// would exceed MaxSignatureCacheEntries, an arbitrary half of the entries
// (the map iteration order) is dropped first. A result is cheap to compute
// again, so no recency bookkeeping is kept.
func (s *Signatures) remember(fresh map[sigKey]domain.CommitVerification) {
	if len(fresh) == 0 {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(fresh) >= MaxSignatureCacheEntries {
		clear(s.cache)
	}
	for len(s.cache)+len(fresh) > MaxSignatureCacheEntries && len(s.cache) > 0 {
		drop := len(s.cache) / 2
		if drop == 0 {
			drop = 1
		}
		for k := range s.cache {
			if drop == 0 {
				break
			}
			delete(s.cache, k)
			drop--
		}
	}
	// A page larger than the cache keeps only what fits.
	n := 0
	for k, v := range fresh {
		if n >= MaxSignatureCacheEntries {
			break
		}
		s.cache[k] = v
		n++
	}
}
