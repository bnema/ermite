// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/mock"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

// keyFixture is a Mirrors with only the key store, a sealing fake and a clock.
type keyFixture struct {
	m               *usecase.Mirrors
	keys            *mocks.MirrorKeyStore
	sealerOpenFails bool
	mu              sync.Mutex
	sealed          [][]byte
}

func newKeyFixture(t *testing.T) *keyFixture {
	t.Helper()
	f := &keyFixture{keys: mocks.NewMirrorKeyStore(t)}
	sealer, clock := mocks.NewSecretSealer(t), mocks.NewClock(t)
	clock.EXPECT().Now().Return(now).Maybe()
	sealer.EXPECT().Seal(mock.Anything, []byte(domain.MirrorKeyPath)).RunAndReturn(func(seed, _ []byte) []byte {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sealed = append(f.sealed, append([]byte(nil), seed...))
		return append([]byte("sealed:"), seed...)
	}).Maybe()
	sealer.EXPECT().Open(mock.Anything, []byte(domain.MirrorKeyPath)).RunAndReturn(func(b, _ []byte) ([]byte, error) {
		if f.sealerOpenFails {
			return nil, errors.New("cannot open")
		}
		return b[len("sealed:"):], nil
	}).Maybe()
	f.m = usecase.NewMirrors(usecase.MirrorsConfig{Keys: f.keys, Sealer: sealer, Clock: clock, PublicURL: "https://forge.example.com"})
	return f
}

// seeds are the seeds generated so far.
func (f *keyFixture) seeds() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sealed...)
}
