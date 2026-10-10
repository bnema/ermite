// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zerowrap"
	"golang.org/x/sys/unix"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

var _ ports.GitRepos = (*Git)(nil)

const gitSuffix = ".git"

func (g *Git) reposDir() string   { return filepath.Join(g.dataDir, "repos") }
func (g *Git) stagingDir() string { return filepath.Join(g.reposDir(), ".staging") }
func (g *Git) trashDir() string   { return filepath.Join(g.dataDir, "trash") }

// Stage runs `git init --bare` in a new directory under repos/.staging/ and
// sets the repository defaults. The initial branch is main.
func (g *Git) Stage(ctx context.Context) (ports.StagedID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("random staging name: %w", err)
	}
	id := ports.StagedID(hex.EncodeToString(raw[:]))
	dir := filepath.Join(g.stagingDir(), string(id))

	_, err := g.run(ctx, "init", "--quiet", "--bare", "--object-format=sha1", "--ref-format=files", "--initial-branch=main", dir)
	if err == nil {
		for _, kv := range [][2]string{{"repack.writeBitmaps", "true"}, {"core.commitGraph", "true"}} {
			if _, err = g.run(ctx, "-C", dir, "config", "--local", kv[0], kv[1]); err != nil {
				break
			}
		}
	}
	if err != nil {
		if rerr := os.RemoveAll(dir); rerr != nil {
			zerowrap.Ctx(ctx).Error().Err(rerr).Str("dir", dir).Msg("remove failed staging directory")
		}
		return "", fmt.Errorf("stage repository: %w", err)
	}
	zerowrap.Ctx(ctx).Debug().Str("staged", string(id)).Msg("repository staged")
	return id, nil
}

// Publish renames the staged repository to repos/<name>.git without
// replacing anything.
func (g *Git) Publish(ctx context.Context, id ports.StagedID, name domain.RepoName) error {
	src, err := g.stagedPath(id)
	if err != nil {
		return err
	}
	dst := filepath.Join(g.reposDir(), name.String()+gitSuffix)
	if err := renameNoReplace(src, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return domain.ErrAlreadyExists
		}
		return fmt.Errorf("publish repository %q: %w", name, err)
	}
	if err := syncDir(g.reposDir()); err != nil {
		return fmt.Errorf("publish repository %q: %w", name, err)
	}
	zerowrap.Ctx(ctx).Debug().Str("repo", name.String()).Msg("repository published")
	return nil
}

// Discard removes a staged repository.
func (g *Git) Discard(_ context.Context, id ports.StagedID) error {
	dir, err := g.stagedPath(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("discard staged repository %s: %w", id, err)
	}
	return nil
}

// PurgeStaging removes everything under repos/.staging/.
//
//hexcheck:ignore suspicious-business-logic-in-adapter port method name only
func (g *Git) PurgeStaging(ctx context.Context) error {
	entries, err := os.ReadDir(g.stagingDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read staging directory: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(g.stagingDir(), e.Name())); err != nil {
			return fmt.Errorf("remove staging leftover %s: %w", e.Name(), err)
		}
		zerowrap.Ctx(ctx).Info().Str("entry", e.Name()).Msg("removed staging leftover")
	}
	return nil
}

// List returns the repository directories. Entries that are not
// "<valid name>.git" directories are reported as invalid; .staging is not an
// entry.
func (g *Git) List(_ context.Context) (ports.RepoDirs, error) {
	entries, err := os.ReadDir(g.reposDir())
	if err != nil {
		return ports.RepoDirs{}, fmt.Errorf("read repositories directory: %w", err)
	}
	var out ports.RepoDirs
	for _, e := range entries {
		if e.Name() == ".staging" {
			continue
		}
		base, ok := strings.CutSuffix(e.Name(), gitSuffix)
		if !ok || !e.IsDir() {
			out.Invalid = append(out.Invalid, e.Name())
			continue
		}
		name, err := domain.ParseRepoName(base)
		if err != nil {
			out.Invalid = append(out.Invalid, e.Name())
			continue
		}
		out.Names = append(out.Names, name)
	}
	return out, nil
}

// Trash moves repos/<name>.git to trash/<name>-<unix>.git.
func (g *Git) Trash(ctx context.Context, name domain.RepoName) error {
	src := filepath.Join(g.reposDir(), name.String()+gitSuffix)
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("trash repository %q: %w", name, err)
	}
	stamp := time.Now().Unix()
	for i := range 100 {
		dst := filepath.Join(g.trashDir(), fmt.Sprintf("%s-%d%s", name, stamp+int64(i), gitSuffix))
		err := renameNoReplace(src, dst)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("trash repository %q: %w", name, err)
		}
		if err := syncDir(g.reposDir()); err != nil {
			return fmt.Errorf("trash repository %q: %w", name, err)
		}
		zerowrap.Ctx(ctx).Info().Str("repo", name.String()).Str("trash", filepath.Base(dst)).Msg("repository moved to trash")
		return nil
	}
	return fmt.Errorf("trash repository %q: no free name in trash", name)
}

// stagedPath returns the directory of a staged repository, rejecting
// identifiers that are not the hex names Stage produces.
func (g *Git) stagedPath(id ports.StagedID) (string, error) {
	if _, err := hex.DecodeString(string(id)); err != nil || len(id) != 32 {
		return "", fmt.Errorf("invalid staged repository id %q", id)
	}
	return filepath.Join(g.stagingDir(), string(id)), nil
}

// renameNoReplace renames src to dst and fails with fs.ErrExist if dst
// exists, even as an empty directory. Filesystems without RENAME_NOREPLACE
// fall back to a check before the rename.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fs.ErrExist
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP):
		if _, serr := os.Lstat(dst); serr == nil {
			return fs.ErrExist
		}
		return os.Rename(src, dst)
	}
	return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
}

// syncDir flushes a directory so a rename in it survives a crash.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
