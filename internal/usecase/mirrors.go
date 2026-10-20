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

const mirrorQueueSize = 64

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
	QueueSize    int // jobs waiting for a worker; default 64
	// ValidateURL checks a resolved mirror URL; default domain.ValidateMirrorURL. Tests of
	// local transports inject a permissive check.
	ValidateURL func(string) error
}

// mirrorGen counts the changes of a repository (dirty) and the count the last successful
// sync saw (clean, meaningful once cleanKnown).
type mirrorGen struct {
	dirty, clean uint64
	cleanKnown   bool
}

// Mirrors manages outbound repository mirrors.
type Mirrors struct {
	cfg      MirrorsConfig
	configMu sync.RWMutex // configuration changes; held for reads only around store reads and writes, never during a push
	mu       sync.Mutex
	gens     map[domain.RepoName]mirrorGen
	flights  map[domain.RepoName]*mirrorFlight
	flightWG sync.WaitGroup // one per flight, for tests that wait for the queue to drain
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	start    sync.Once
	closed   bool
	jobs     chan domain.RepoName

	scheduleMu sync.Mutex
	lastMinute time.Time
	deferred   map[domain.RepoName]struct{} // due, but the queue was full

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
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = mirrorQueueSize
	}
	if cfg.ValidateURL == nil {
		cfg.ValidateURL = domain.ValidateMirrorURL
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Mirrors{
		cfg: cfg, ctx: ctx, cancel: cancel,
		gens:       make(map[domain.RepoName]mirrorGen),
		flights:    make(map[domain.RepoName]*mirrorFlight),
		deferred:   make(map[domain.RepoName]struct{}),
		jobs:       make(chan domain.RepoName, cfg.QueueSize),
		lastMinute: cfg.Clock.Now().UTC().Truncate(time.Minute),
	}
}
