// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the secrets, variables and environments of CI. <scope> is
// domain.SettingsScope.Key: "i", "r/<repo>" or "e/<repo>/<environment>" with
// the environment lower-cased and path-escaped. Names are upper case and hold
// letters, digits and "_", so a key never runs into another scope's.
//
//	secret/<scope>/<NAME>         SecretRecord (the value sealed, see secretbox)
//	var/<scope>/<NAME>            VariableRecord
//	env/<repo>/<environment>      EnvironmentRecord
//	secretskey                    the key check of ERMITE_SECRETS_KEY
//
// Writes under a repository guard its key (guardRepoKey), like the merge
// requests and the CI runs, so a deleted repository leaves nothing behind.
// Writes in an environment scope also require the environment record at the
// version they read, so deleting the environment cannot leave a stray value.
const (
	secretPrefix    = "secret/"
	variablePrefix  = "var/"
	envPrefix       = "env/"
	secretsKeyCheck = "secretskey"
)

// afterSettingsSnapshot is a test hook: it runs between the snapshot of
// DeleteEnvironment and its batch, to write in that window.
var afterSettingsSnapshot func()

// Settings is the ermite-kv backed ports.CISettingsStore. It shares the
// database of the Store it comes from.
type Settings struct {
	db *kv.DB
}

var _ ports.CISettingsStore = (*Settings)(nil)

// Settings returns the CI settings store over the same database. It is valid
// until the Store is closed.
func (s *Store) Settings() *Settings { return &Settings{db: s.db} }

func settingKey(prefix string, scope domain.SettingsScope, name string) []byte {
	return []byte(prefix + scope.Key() + "/" + name)
}
func settingPrefix(prefix string, scope domain.SettingsScope) string {
	return prefix + scope.Key() + "/"
}
func envKey(repo domain.RepoName, name string) []byte {
	return []byte(envPrefix + repo.String() + "/" + domain.EnvironmentKey(name))
}
func envRepoPrefix(repo domain.RepoName) string { return envPrefix + repo.String() + "/" }

// settingsKeys lists every key of the snapshot that belongs to the secrets,
// variables and environments of the repository: its own and its environments'.
func settingsKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, p := range []string{
		secretPrefix + "r/" + repo.String() + "/", variablePrefix + "r/" + repo.String() + "/",
		secretPrefix + "e/" + repo.String() + "/", variablePrefix + "e/" + repo.String() + "/",
		envRepoPrefix(repo),
	} {
		for k := range snap.Prefix(ctx, []byte(p)) {
			keys = append(keys, slices.Clone(k))
		}
	}
	return keys
}

// BindSecretsKey stores the key check when there is none and compares it
// otherwise.
//
//hexcheck:ignore suspicious-business-logic-in-adapter initializes or compares the persisted encryption-key check with compare-and-swap retries
func (s *Settings) BindSecretsKey(ctx context.Context, check []byte) error {
	key := []byte(secretsKeyCheck)
	for range updateAttempts {
		item, ok := s.db.Get(ctx, key)
		if ok {
			if !bytes.Equal(item.Value, check) {
				return domain.ErrSecretsKeyMismatch
			}
			return nil
		}
		var b kv.Batch
		b.RequireAbsent(key)
		b.Put(key, check)
		if _, err := s.db.Apply(ctx, &b); err != nil {
			if errors.Is(err, kv.ErrConflict) {
				continue // written meanwhile: compare it
			}
			return fmt.Errorf("store the secrets key check: %w", err)
		}
		return nil
	}
	return errors.New("store the secrets key check: concurrent initialization did not settle")
}

// stage makes the batch depend on the scope: the repository key is guarded
// and, for an environment, its record is required at the version read.
//
//hexcheck:ignore suspicious-business-logic-in-adapter validates a settings scope and adds repository and environment existence/version preconditions to a storage batch
func (s *Settings) stage(ctx context.Context, b *kv.Batch, scope domain.SettingsScope) error {
	if !scope.Valid() {
		return fmt.Errorf("%w: scope", domain.ErrInvalid)
	}
	if scope.Kind == domain.ScopeKindInstance {
		return nil
	}
	if err := guardRepoKey(ctx, s.db, b, scope.Repo); err != nil {
		return err
	}
	if scope.Kind == domain.ScopeKindEnvironment {
		item, ok := s.db.Get(ctx, envKey(scope.Repo, scope.Env))
		if !ok {
			return domain.ErrNotFound
		}
		b.RequireVersion(envKey(scope.Repo, scope.Env), item.Version)
	}
	return nil
}

// write applies build's batch, staged for the scope, again when a concurrent
// write of the repository made it conflict.
func (s *Settings) write(ctx context.Context, scope domain.SettingsScope, what string, build func(b *kv.Batch) error) error {
	for range updateAttempts {
		var b kv.Batch
		if err := s.stage(ctx, &b, scope); err != nil {
			return err
		}
		if err := build(&b); err != nil {
			return err
		}
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	return fmt.Errorf("%s: %w", what, domain.ErrConflict)
}

// PutSecret implements ports.CISettingsStore.
func (s *Settings) PutSecret(ctx context.Context, scope domain.SettingsScope, name string, sealed []byte, at time.Time) (domain.Secret, error) {
	val := mustMarshal(&storagepb.SecretRecord{Sealed: sealed, UpdatedAt: encodeTime(at)})
	err := s.write(ctx, scope, "store secret", func(b *kv.Batch) error {
		b.Put(settingKey(secretPrefix, scope, name), val)
		return nil
	})
	if err != nil {
		return domain.Secret{}, err
	}
	return domain.Secret{Name: name, UpdatedAt: at}, nil
}

// ListSealedSecrets implements ports.CISettingsStore.
func (s *Settings) ListSealedSecrets(ctx context.Context, scope domain.SettingsScope) ([]ports.SealedSecret, error) {
	snap := s.db.Snapshot(ctx)
	if err := s.existsIn(ctx, snap, scope); err != nil {
		return nil, err
	}
	prefix := settingPrefix(secretPrefix, scope)
	var out []ports.SealedSecret
	for key, item := range snap.Prefix(ctx, []byte(prefix)) {
		var pb storagepb.SecretRecord
		if err := proto.Unmarshal(item.Value, &pb); err != nil {
			return nil, fmt.Errorf("decode secret %s: %w", key, err)
		}
		out = append(out, ports.SealedSecret{Name: strings.TrimPrefix(string(key), prefix), Sealed: pb.GetSealed(), UpdatedAt: decodeTime(pb.GetUpdatedAt())})
	}
	return out, nil
}

// GetSealedSecret implements ports.CISettingsStore.
func (s *Settings) GetSealedSecret(ctx context.Context, scope domain.SettingsScope, name string) (ports.SealedSecret, error) {
	snap := s.db.Snapshot(ctx)
	if err := s.existsIn(ctx, snap, scope); err != nil {
		return ports.SealedSecret{}, err
	}
	item, ok := snap.Get(ctx, settingKey(secretPrefix, scope, name))
	if !ok {
		return ports.SealedSecret{}, domain.ErrNotFound
	}
	var pb storagepb.SecretRecord
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return ports.SealedSecret{}, fmt.Errorf("decode secret %s: %w", name, err)
	}
	return ports.SealedSecret{Name: name, Sealed: pb.GetSealed(), UpdatedAt: decodeTime(pb.GetUpdatedAt())}, nil
}

// existsIn is exists on a snapshot, so a listing and its check agree.
//
//hexcheck:ignore suspicious-business-logic-in-adapter validates a settings scope and checks repository and environment keys in the same snapshot as a listing
func (s *Settings) existsIn(ctx context.Context, snap *kv.Snapshot, scope domain.SettingsScope) error {
	if !scope.Valid() {
		return fmt.Errorf("%w: scope", domain.ErrInvalid)
	}
	if scope.Kind == domain.ScopeKindInstance {
		return nil
	}
	if _, ok := snap.Get(ctx, repoKey(scope.Repo)); !ok {
		return domain.ErrNotFound
	}
	if scope.Kind == domain.ScopeKindEnvironment {
		if _, ok := snap.Get(ctx, envKey(scope.Repo, scope.Env)); !ok {
			return domain.ErrNotFound
		}
	}
	return nil
}

// DeleteSecret implements ports.CISettingsStore.
func (s *Settings) DeleteSecret(ctx context.Context, scope domain.SettingsScope, name string) error {
	return s.deleteOne(ctx, scope, secretPrefix, name, "delete secret")
}

func (s *Settings) deleteOne(ctx context.Context, scope domain.SettingsScope, prefix, name, what string) error {
	key := settingKey(prefix, scope, name)
	return s.write(ctx, scope, what, func(b *kv.Batch) error {
		item, ok := s.db.Get(ctx, key)
		if !ok {
			return domain.ErrNotFound
		}
		b.RequireVersion(key, item.Version)
		b.Delete(key)
		return nil
	})
}

// PutVariable implements ports.CISettingsStore.
func (s *Settings) PutVariable(ctx context.Context, scope domain.SettingsScope, name, value string, at time.Time) (domain.Variable, error) {
	val := mustMarshal(&storagepb.VariableRecord{Value: value, UpdatedAt: encodeTime(at)})
	err := s.write(ctx, scope, "store variable", func(b *kv.Batch) error {
		b.Put(settingKey(variablePrefix, scope, name), val)
		return nil
	})
	if err != nil {
		return domain.Variable{}, err
	}
	return domain.Variable{Name: name, Value: value, UpdatedAt: at}, nil
}

func decodeVariable(name string, val []byte) (domain.Variable, error) {
	var pb storagepb.VariableRecord
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.Variable{}, fmt.Errorf("decode variable %s: %w", name, err)
	}
	return domain.Variable{Name: name, Value: pb.GetValue(), UpdatedAt: decodeTime(pb.GetUpdatedAt())}, nil
}

// GetVariable implements ports.CISettingsStore.
func (s *Settings) GetVariable(ctx context.Context, scope domain.SettingsScope, name string) (domain.Variable, error) {
	snap := s.db.Snapshot(ctx)
	if err := s.existsIn(ctx, snap, scope); err != nil {
		return domain.Variable{}, err
	}
	item, ok := snap.Get(ctx, settingKey(variablePrefix, scope, name))
	if !ok {
		return domain.Variable{}, domain.ErrNotFound
	}
	return decodeVariable(name, item.Value)
}

// ListVariables implements ports.CISettingsStore.
func (s *Settings) ListVariables(ctx context.Context, scope domain.SettingsScope) ([]domain.Variable, error) {
	snap := s.db.Snapshot(ctx)
	if err := s.existsIn(ctx, snap, scope); err != nil {
		return nil, err
	}
	prefix := settingPrefix(variablePrefix, scope)
	var out []domain.Variable
	for key, item := range snap.Prefix(ctx, []byte(prefix)) {
		v, err := decodeVariable(strings.TrimPrefix(string(key), prefix), item.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// DeleteVariable implements ports.CISettingsStore.
func (s *Settings) DeleteVariable(ctx context.Context, scope domain.SettingsScope, name string) error {
	return s.deleteOne(ctx, scope, variablePrefix, name, "delete variable")
}

func decodeEnvironment(val []byte) (domain.Environment, error) {
	var pb storagepb.EnvironmentRecord
	if err := proto.Unmarshal(val, &pb); err != nil {
		return domain.Environment{}, fmt.Errorf("decode environment: %w", err)
	}
	repo, err := domain.ParseRepoName(pb.GetRepo())
	if err != nil {
		return domain.Environment{}, fmt.Errorf("decode environment: %w", err)
	}
	return domain.Environment{Repo: repo, Name: pb.GetName(), CreatedAt: decodeTime(pb.GetCreatedAt())}, nil
}

// CreateEnvironment implements ports.CISettingsStore.
func (s *Settings) CreateEnvironment(ctx context.Context, repo domain.RepoName, name string, at time.Time) (domain.Environment, error) {
	key := envKey(repo, name)
	for range updateAttempts {
		if item, ok := s.db.Get(ctx, key); ok {
			return decodeEnvironment(item.Value)
		}
		var b kv.Batch
		if err := guardRepoKey(ctx, s.db, &b, repo); err != nil {
			return domain.Environment{}, err
		}
		b.RequireAbsent(key)
		b.Put(key, mustMarshal(&storagepb.EnvironmentRecord{Repo: repo.String(), Name: name, CreatedAt: encodeTime(at)}))
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue // created or the repository written meanwhile: look again
		}
		if err != nil {
			return domain.Environment{}, fmt.Errorf("create environment: %w", err)
		}
		return domain.Environment{Repo: repo, Name: name, CreatedAt: at}, nil
	}
	return domain.Environment{}, fmt.Errorf("create environment: %w", domain.ErrConflict)
}

// ListEnvironments implements ports.CISettingsStore.
func (s *Settings) ListEnvironments(ctx context.Context, repo domain.RepoName) ([]domain.Environment, error) {
	snap := s.db.Snapshot(ctx)
	if _, ok := snap.Get(ctx, repoKey(repo)); !ok {
		return nil, domain.ErrNotFound
	}
	var out []domain.Environment
	for _, item := range snap.Prefix(ctx, []byte(envRepoPrefix(repo))) {
		e, err := decodeEnvironment(item.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// DeleteEnvironment implements ports.CISettingsStore.
func (s *Settings) DeleteEnvironment(ctx context.Context, repo domain.RepoName, name string) error {
	key := envKey(repo, name)
	scope := domain.EnvironmentScope(repo, name)
	for range updateAttempts {
		snap := s.db.Snapshot(ctx)
		// The repository record is read from the same snapshot as the keys
		// listed below, then required at that version and rewritten, as
		// Store.Delete does: a value written after the snapshot rewrote the
		// record (guardRepoKey), so this batch conflicts and lists again.
		repoItem, ok := snap.Get(ctx, repoKey(repo))
		if !ok {
			return domain.ErrNotFound
		}
		item, ok := snap.Get(ctx, key)
		if !ok {
			return domain.ErrNotFound
		}
		if afterSettingsSnapshot != nil {
			afterSettingsSnapshot()
		}
		var b kv.Batch
		b.RequireVersion(repoKey(repo), repoItem.Version)
		b.Put(repoKey(repo), repoItem.Value)
		b.RequireVersion(key, item.Version)
		b.Delete(key)
		for _, p := range []string{settingPrefix(secretPrefix, scope), settingPrefix(variablePrefix, scope)} {
			for k := range snap.Prefix(ctx, []byte(p)) {
				b.Delete(slices.Clone(k))
			}
		}
		_, err := s.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("delete environment: %w", err)
		}
		return nil
	}
	return fmt.Errorf("delete environment: %w", domain.ErrConflict)
}
