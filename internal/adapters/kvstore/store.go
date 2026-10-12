// SPDX-License-Identifier: AGPL-3.0-or-later

// Package kvstore implements the metadata ports over ermite-kv. Values are
// the protobuf messages of proto/ermite/storage.
package kvstore

import (
	"context"
	"errors"
	"fmt"

	kv "bnema.dev/ermite-kv"
	"github.com/bnema/zerowrap"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/ports"
)

// SchemaVersion is the storage schema this build reads and writes. Version 2
// adds branch prefixes to tokens: a build that does not know them would treat
// a branch-limited token as unrestricted, so it must refuse the data
// (ErrSchemaTooNew) instead.
const SchemaVersion = 2

const schemaKey = "meta/schema"

// ErrSchemaTooNew means the data was written by a newer ermite.
var ErrSchemaTooNew = errors.New("storage schema is newer than this ermite supports")

// Store is the ermite-kv backed metadata store.
type Store struct {
	db *kv.DB
}

var _ ports.RepoMetaStore = (*Store)(nil)

// Open opens (or creates) the database in dir and checks the storage schema:
// a missing version is written as SchemaVersion, a lower one is upgraded to it,
// a higher one stops with ErrSchemaTooNew. dir must exist.
func Open(ctx context.Context, dir string) (*Store, error) {
	db, err := kv.Open(ctx, dir, kv.Options{})
	if err != nil {
		return nil, fmt.Errorf("open ermite-kv in %s: %w", dir, err)
	}
	s := &Store{db: db}
	if err := s.checkSchema(ctx); err != nil {
		_ = db.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// Close stops the database after the writes in progress.
func (s *Store) Close(ctx context.Context) error {
	if err := s.db.Close(ctx); err != nil {
		return fmt.Errorf("close ermite-kv: %w", err)
	}
	return nil
}

// Check reports whether the database accepts writes. It returns the failure
// once ermite-kv is closed or failed, and writes nothing.
//
// ermite-kv has no state accessor, and an empty batch sent through Apply would
// queue behind the writes in progress. Apply looks at the state of the database
// before it looks at its context, though, and an ended context stops it before
// the writer channel: Apply on a cancelled context therefore answers ErrClosed
// or ErrFailed when the database is stopped, and context.Canceled when it is
// healthy, without waiting for the writer.
func (s *Store) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()
	var b kv.Batch
	if _, err := s.db.Apply(probe, &b); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("ermite-kv: %w", err)
	}
	return nil
}

func (s *Store) checkSchema(ctx context.Context) error {
	log := zerowrap.FromCtx(ctx)
	key := []byte(schemaKey)
	for range 2 {
		item, ok := s.db.Get(ctx, key)
		if !ok {
			var b kv.Batch
			b.RequireAbsent(key)
			b.Put(key, mustMarshal(&storagepb.SchemaVersion{Version: SchemaVersion}))
			_, err := s.db.Apply(ctx, &b)
			if errors.Is(err, kv.ErrConflict) {
				continue // written meanwhile: read it
			}
			if err != nil {
				return fmt.Errorf("write storage schema: %w", err)
			}
			log.Info().Uint32("schema", SchemaVersion).Msg("storage schema initialized")
			return nil
		}
		var sv storagepb.SchemaVersion
		if err := proto.Unmarshal(item.Value, &sv); err != nil {
			return fmt.Errorf("decode storage schema: %w", err)
		}
		switch {
		case sv.GetVersion() > SchemaVersion:
			return fmt.Errorf("%w: found %d, supported %d", ErrSchemaTooNew, sv.GetVersion(), SchemaVersion)
		case sv.GetVersion() == 0:
			return fmt.Errorf("decode storage schema: invalid version 0")
		}
		if sv.GetVersion() < SchemaVersion {
			// Older data stays readable as it is (the changes are additive);
			// the new version is recorded so that older builds refuse it.
			var b kv.Batch
			b.RequireVersion(key, item.Version)
			b.Put(key, mustMarshal(&storagepb.SchemaVersion{Version: SchemaVersion}))
			_, err := s.db.Apply(ctx, &b)
			if errors.Is(err, kv.ErrConflict) {
				continue // changed meanwhile: read it again
			}
			if err != nil {
				return fmt.Errorf("upgrade storage schema: %w", err)
			}
			log.Info().Uint32("from", sv.GetVersion()).Uint32("schema", SchemaVersion).Msg("storage schema upgraded")
			return nil
		}
		log.Info().Uint32("schema", sv.GetVersion()).Msg("storage schema checked")
		return nil
	}
	return errors.New("write storage schema: concurrent initialization did not settle")
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("kvstore: marshal %T: %v", m, err)) // only for invalid UTF-8 strings, checked by callers
	}
	return b
}
