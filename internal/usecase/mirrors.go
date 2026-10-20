// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// MirrorsConfig supplies the mirror use case's dependencies.
type MirrorsConfig struct {
	Store        ports.MirrorStore
	Keys         ports.MirrorKeyStore
	Sealer       ports.SecretSealer
	Pusher       ports.MirrorPusher
	Meta         ports.RepoMetaStore
	Git          ports.GitRepos
	Reader       ports.RepoReader
	Bus          ports.EventBus
	Clock        ports.Clock
	PublicURL    string
	ScheduleTick time.Duration
	Workers      int
	AllowAnyURL  bool
}

// Mirrors manages outbound repository mirrors.
type Mirrors struct {
	cfg        MirrorsConfig
	configMu   sync.RWMutex
	mu         sync.Mutex
	dirty      map[domain.RepoName]uint64
	clean      map[domain.RepoName]uint64
	flights    map[domain.RepoName]*mirrorFlight
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	start      sync.Once
	closed     bool
	jobs       chan domain.RepoName
	scheduleMu sync.Mutex
	lastMinute time.Time
	keyMu      sync.Mutex
	key        ed25519.PrivateKey
	keyCreated time.Time
}

// NewMirrors builds the mirror use case.
func NewMirrors(cfg MirrorsConfig) *Mirrors {
	if cfg.ScheduleTick <= 0 {
		cfg.ScheduleTick = time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Mirrors{cfg: cfg, ctx: ctx, cancel: cancel, dirty: make(map[domain.RepoName]uint64), clean: make(map[domain.RepoName]uint64), flights: make(map[domain.RepoName]*mirrorFlight), jobs: make(chan domain.RepoName, 64), lastMinute: cfg.Clock.Now().UTC().Truncate(time.Minute)}
}

// privateKey returns a copy so a transport cannot mutate the cached key.
func (m *Mirrors) privateKey(ctx context.Context) (ed25519.PrivateKey, error) {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if err := m.loadKey(ctx, false); err != nil {
		return nil, err
	}
	return append(ed25519.PrivateKey(nil), m.key...), nil
}
