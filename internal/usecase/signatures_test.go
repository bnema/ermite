// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/ports/mocks"
	"bnema.dev/ermite/internal/usecase"
)

const (
	sshArmor  = "-----BEGIN SSH SIGNATURE-----\n AAAA\n -----END SSH SIGNATURE-----\n"
	gpgArmor  = "-----BEGIN PGP SIGNATURE-----\n\n iQ==\n -----END PGP SIGNATURE-----\n"
	x509Armor = "-----BEGIN SIGNED MESSAGE-----\n MIIB\n -----END SIGNED MESSAGE-----\n"
)

var sigNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

type sigFixture struct {
	store     *mocks.TrustedKeyStore
	inspector *mocks.KeyInspector
	ssh, gpg  *mocks.SignatureVerifier
	objects   *mocks.CommitObjectReader
	clock     *mocks.Clock
	sigs      *usecase.Signatures
}

var builtinKey = domain.TrustedKey{
	ID: "SHA256:builtin", Format: domain.FormatSSH, Title: "instance", Emails: []string{"bot@example.org"}, Builtin: true,
}

func newSigFixture(t *testing.T, stored ...domain.TrustedKey) *sigFixture {
	t.Helper()
	f := &sigFixture{
		store: mocks.NewTrustedKeyStore(t), inspector: mocks.NewKeyInspector(t),
		ssh: mocks.NewSignatureVerifier(t), gpg: mocks.NewSignatureVerifier(t),
		objects: mocks.NewCommitObjectReader(t), clock: mocks.NewClock(t),
	}
	f.clock.EXPECT().Now().Return(sigNow).Maybe()
	f.store.EXPECT().ListTrustedKeys(mock.Anything).Return(stored, nil)
	var err error
	f.sigs, err = usecase.NewSignatures(f.store, f.inspector, map[domain.SignatureFormat]ports.SignatureVerifier{
		domain.FormatSSH: f.ssh, domain.FormatGPG: f.gpg,
	}, f.objects, builtinKey, f.clock)
	require.NoError(t, err)
	return f
}

func sigToken(scopes []domain.Scope, repos ...domain.RepoName) domain.Principal {
	return domain.TokenPrincipal(domain.Token{ID: "t", Scopes: scopes, Repos: repos})
}

func TestNewSignaturesLoadError(t *testing.T) {
	t.Parallel()
	store := mocks.NewTrustedKeyStore(t)
	store.EXPECT().ListTrustedKeys(mock.Anything).Return(nil, errors.New("boom"))
	_, err := usecase.NewSignatures(store, nil, nil, nil, domain.TrustedKey{}, nil)
	require.Error(t, err)
}

func TestTrustedKeyAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reader := sigToken([]domain.Scope{domain.ScopeRepoRead})
	owner := sigToken(domain.AllScopes())

	t.Run("list", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		_, err := f.sigs.ListTrustedKeys(ctx, domain.Anonymous())
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		for _, p := range []domain.Principal{reader, owner, domain.Root(), domain.Owner()} {
			_, err := f.sigs.ListTrustedKeys(ctx, p)
			assert.NoError(t, err)
		}
	})

	t.Run("add and remove", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		_, err := f.sigs.AddTrustedKey(ctx, domain.Anonymous(), domain.FormatSSH, "t", "k", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)
		err = f.sigs.RemoveTrustedKey(ctx, domain.Anonymous(), "x")
		assert.ErrorIs(t, err, domain.ErrUnauthenticated)

		_, err = f.sigs.AddTrustedKey(ctx, reader, domain.FormatSSH, "t", "k", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrForbidden)
		assert.Contains(t, err.Error(), "managing trusted keys needs")
		err = f.sigs.RemoveTrustedKey(ctx, reader, "x")
		assert.ErrorIs(t, err, domain.ErrForbidden)

		limited := sigToken(domain.AllScopes(), name(t, "r"))
		_, err = f.sigs.AddTrustedKey(ctx, limited, domain.FormatSSH, "t", "k", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrForbidden)

		for _, p := range []domain.Principal{owner, domain.Root(), domain.Owner()} {
			id := "SHA256:" + p.Label()
			f.inspector.EXPECT().Inspect(domain.FormatSSH, "k").Return(id, "ssh-ed25519 K", nil).Once()
			f.store.EXPECT().CreateTrustedKey(ctx, mock.Anything).Return(nil).Once()
			_, err := f.sigs.AddTrustedKey(ctx, p, domain.FormatSSH, "t", "k", []string{"a@b.c"})
			assert.NoError(t, err)
			f.store.EXPECT().DeleteTrustedKey(ctx, id).Return(nil).Once()
			assert.NoError(t, f.sigs.RemoveTrustedKey(ctx, p, id))
		}
	})
}

func TestAddTrustedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owner := domain.Root()

	t.Run("stores the canonical key and lists it after the builtin", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		f.inspector.EXPECT().Inspect(domain.FormatSSH, "ssh-ed25519 K comment").Return("SHA256:k1", "ssh-ed25519 K", nil)
		want := domain.TrustedKey{
			ID: "SHA256:k1", Format: domain.FormatSSH, Title: "laptop", PublicKey: "ssh-ed25519 K",
			Emails: []string{"a@b.c", "d@e.f"}, CreatedAt: sigNow,
		}
		f.store.EXPECT().CreateTrustedKey(ctx, want).Return(nil)
		got, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "  laptop ", "ssh-ed25519 K comment", []string{"A@B.c", " d@e.f", "a@b.c"})
		require.NoError(t, err)
		assert.Equal(t, want, got)

		keys, err := f.sigs.ListTrustedKeys(ctx, owner)
		require.NoError(t, err)
		require.Len(t, keys, 2)
		assert.Equal(t, builtinKey, keys[0])
		assert.Equal(t, want, keys[1])
	})

	t.Run("validation", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		for name, call := range map[string]func() error{
			"empty title": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, " ", "k", []string{"a@b.c"})
				return err
			},
			"no email": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "k", nil)
				return err
			},
			"bad email": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "k", []string{"not an email"})
				return err
			},
			"x509": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatX509, "t", "k", []string{"a@b.c"})
				return err
			},
			"no format": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatNone, "t", "k", []string{"a@b.c"})
				return err
			},
			"key too big": func() error {
				_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatGPG, "t", strings.Repeat("x", domain.MaxTrustedKeyBytes+1), []string{"a@b.c"})
				return err
			},
		} {
			assert.ErrorIs(t, call(), domain.ErrInvalid, name)
		}
	})

	t.Run("inspector error is returned", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		f.inspector.EXPECT().Inspect(domain.FormatGPG, "bad").Return("", "", fmt.Errorf("%w: nope", domain.ErrInvalid))
		_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatGPG, "t", "bad", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrInvalid)
	})

	t.Run("builtin and duplicates are refused", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, domain.TrustedKey{ID: "SHA256:old", Format: domain.FormatSSH})
		f.inspector.EXPECT().Inspect(domain.FormatSSH, "b").Return(builtinKey.ID, "ssh-ed25519 B", nil)
		_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "b", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrAlreadyExists)

		f.inspector.EXPECT().Inspect(domain.FormatSSH, "o").Return("SHA256:old", "ssh-ed25519 O", nil)
		_, err = f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "o", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	})

	t.Run("limit", func(t *testing.T) {
		t.Parallel()
		var stored []domain.TrustedKey
		for i := range domain.MaxTrustedKeys {
			stored = append(stored, domain.TrustedKey{ID: fmt.Sprintf("SHA256:%d", i), Format: domain.FormatSSH})
		}
		f := newSigFixture(t, stored...)
		f.inspector.EXPECT().Inspect(domain.FormatSSH, "n").Return("SHA256:new", "ssh-ed25519 N", nil)
		_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "n", []string{"a@b.c"})
		assert.ErrorIs(t, err, domain.ErrResourceExhausted)
	})

	t.Run("store error leaves the list unchanged", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		f.inspector.EXPECT().Inspect(domain.FormatSSH, "k").Return("SHA256:k", "ssh-ed25519 K", nil)
		f.store.EXPECT().CreateTrustedKey(ctx, mock.Anything).Return(errors.New("down"))
		_, err := f.sigs.AddTrustedKey(ctx, owner, domain.FormatSSH, "t", "k", []string{"a@b.c"})
		require.Error(t, err)
		keys, _ := f.sigs.ListTrustedKeys(ctx, owner)
		assert.Len(t, keys, 1)
	})
}

func TestRemoveTrustedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("builtin is refused without touching the store", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		err := f.sigs.RemoveTrustedKey(ctx, domain.Root(), builtinKey.ID)
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	})

	t.Run("removes from the store and the list", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, domain.TrustedKey{ID: "SHA256:a"}, domain.TrustedKey{ID: "SHA256:b"})
		f.store.EXPECT().DeleteTrustedKey(ctx, "SHA256:a").Return(nil)
		require.NoError(t, f.sigs.RemoveTrustedKey(ctx, domain.Root(), "SHA256:a"))
		keys, _ := f.sigs.ListTrustedKeys(ctx, domain.Root())
		require.Len(t, keys, 2)
		assert.Equal(t, "SHA256:b", keys[1].ID)
	})

	t.Run("store errors are returned and the key stays", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, domain.TrustedKey{ID: "SHA256:a"})
		f.store.EXPECT().DeleteTrustedKey(ctx, "SHA256:a").Return(domain.ErrNotFound)
		assert.ErrorIs(t, f.sigs.RemoveTrustedKey(ctx, domain.Root(), "SHA256:a"), domain.ErrNotFound)
		keys, _ := f.sigs.ListTrustedKeys(ctx, domain.Root())
		assert.Len(t, keys, 2)
	})
}

func TestListWithoutBuiltin(t *testing.T) {
	t.Parallel()
	store := mocks.NewTrustedKeyStore(t)
	store.EXPECT().ListTrustedKeys(mock.Anything).Return([]domain.TrustedKey{{ID: "SHA256:a"}}, nil)
	s, err := usecase.NewSignatures(store, nil, nil, nil, domain.TrustedKey{}, nil)
	require.NoError(t, err)
	keys, err := s.ListTrustedKeys(context.Background(), domain.Root())
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "SHA256:a", keys[0].ID)
}

// rawCommit builds a commit object with the signature armor in a gpgsig header.
func rawCommit(armor string) []byte {
	var b strings.Builder
	b.WriteString("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor A <a@b.c> 1 +0000\ncommitter A <a@b.c> 1 +0000\n")
	if armor != "" {
		lines := strings.Split(strings.TrimSuffix(armor, "\n"), "\n")
		b.WriteString("gpgsig " + strings.TrimPrefix(lines[0], " ") + "\n")
		for _, l := range lines[1:] {
			b.WriteString(" " + strings.TrimPrefix(l, " ") + "\n")
		}
	}
	b.WriteString("\nmessage\n")
	return []byte(b.String())
}

func sigCommit(c string) domain.Commit {
	return domain.Commit{ID: oid(c), Committer: domain.Signature{Email: "a@b.c", Time: time.Unix(1700000000, 0)}}
}

func TestVerifyCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := name(t, "r")
	stored := []domain.TrustedKey{
		{ID: "SHA256:s1", Format: domain.FormatSSH, Emails: []string{"a@b.c"}},
		{ID: "ABCD", Format: domain.FormatGPG, Emails: []string{"a@b.c"}},
	}

	t.Run("unsigned, x509 and unknown armor need no verifier", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		cs := []domain.Commit{sigCommit("1"), sigCommit("2"), sigCommit("3")}
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("1"), oid("2"), oid("3")}).
			Return(map[domain.ObjectID][]byte{
				oid("1"): rawCommit(""),
				oid("2"): rawCommit(x509Armor),
				oid("3"): rawCommit("-----BEGIN WHAT-----\n x\n -----END WHAT-----\n"),
			}, nil).Once()
		got := f.sigs.VerifyCommits(ctx, repo, cs)
		assert.Equal(t, map[domain.ObjectID]domain.CommitVerification{
			oid("1"): {Status: domain.StatusUnsigned},
			oid("2"): {Format: domain.FormatX509, Status: domain.StatusUnsupported},
			oid("3"): {Format: domain.FormatNone, Status: domain.StatusBadSignature},
		}, got)
	})

	t.Run("ssh gets the builtin and the ssh keys only", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		c := sigCommit("1")
		raw := rawCommit(sshArmor)
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{c.ID}).Return(map[domain.ObjectID][]byte{c.ID: raw}, nil)
		want := domain.CommitVerification{Format: domain.FormatSSH, Status: domain.StatusVerified, Fingerprint: "SHA256:s1", KeyID: "SHA256:s1"}
		payload, sig, ok := domain.SplitCommitSignature(raw)
		require.True(t, ok)
		f.ssh.EXPECT().Verify(payload, sig, "a@b.c", int64(1700000000), []domain.TrustedKey{builtinKey, stored[0]}).Return(want)
		assert.Equal(t, want, f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c})[c.ID])
	})

	t.Run("gpg gets the gpg keys only", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		c := sigCommit("1")
		raw := rawCommit(gpgArmor)
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{c.ID}).Return(map[domain.ObjectID][]byte{c.ID: raw}, nil)
		want := domain.CommitVerification{Format: domain.FormatGPG, Status: domain.StatusExpired}
		payload, sig, _ := domain.SplitCommitSignature(raw)
		f.gpg.EXPECT().Verify(payload, sig, "a@b.c", int64(1700000000), []domain.TrustedKey{stored[1]}).Return(want)
		assert.Equal(t, want, f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c})[c.ID])
	})

	t.Run("results are cached per repository", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		c := sigCommit("1")
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{c.ID}).
			Return(map[domain.ObjectID][]byte{c.ID: rawCommit("")}, nil).Once()
		first := f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c})
		second := f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c})
		assert.Equal(t, first, second)

		// Another repository is another entry.
		other := name(t, "other")
		f.objects.EXPECT().ReadCommitObjects(ctx, other, []domain.ObjectID{c.ID}).
			Return(map[domain.ObjectID][]byte{c.ID: rawCommit("")}, nil).Once()
		f.sigs.VerifyCommits(ctx, other, []domain.Commit{c})
	})

	t.Run("only the misses are read", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("1")}).
			Return(map[domain.ObjectID][]byte{oid("1"): rawCommit("")}, nil).Once()
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1")})
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("2")}).
			Return(map[domain.ObjectID][]byte{oid("2"): rawCommit("")}, nil).Once()
		got := f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1"), sigCommit("2")})
		assert.Len(t, got, 2)
	})

	t.Run("adding or removing a key invalidates the cache", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		c := sigCommit("1")
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{c.ID}).
			Return(map[domain.ObjectID][]byte{c.ID: rawCommit("")}, nil).Times(3)
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c})

		f.inspector.EXPECT().Inspect(domain.FormatSSH, "k").Return("SHA256:new", "ssh-ed25519 N", nil)
		f.store.EXPECT().CreateTrustedKey(ctx, mock.Anything).Return(nil)
		_, err := f.sigs.AddTrustedKey(ctx, domain.Root(), domain.FormatSSH, "t", "k", []string{"a@b.c"})
		require.NoError(t, err)
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c}) // read 2
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c}) // cached

		f.store.EXPECT().DeleteTrustedKey(ctx, "SHA256:new").Return(nil)
		require.NoError(t, f.sigs.RemoveTrustedKey(ctx, domain.Root(), "SHA256:new"))
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{c}) // read 3
	})

	t.Run("a read error returns the cached results only", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("1")}).
			Return(map[domain.ObjectID][]byte{oid("1"): rawCommit("")}, nil).Once()
		f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1")})

		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("2")}).Return(nil, errors.New("disk")).Once()
		got := f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1"), sigCommit("2")})
		assert.Equal(t, map[domain.ObjectID]domain.CommitVerification{oid("1"): {Status: domain.StatusUnsigned}}, got)

		// A failure is not cached.
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("2")}).
			Return(map[domain.ObjectID][]byte{oid("2"): rawCommit("")}, nil).Once()
		assert.Len(t, f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("2")}), 1)
	})

	t.Run("a commit missing from the raw map is left out and not cached", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t, stored...)
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, []domain.ObjectID{oid("1")}).Return(map[domain.ObjectID][]byte{}, nil).Twice()
		assert.Empty(t, f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1")}))
		assert.Empty(t, f.sigs.VerifyCommits(ctx, repo, []domain.Commit{sigCommit("1")}))
	})

	t.Run("no commits, no read", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		assert.Empty(t, f.sigs.VerifyCommits(ctx, repo, nil))
	})

	t.Run("the cache stays bounded", func(t *testing.T) {
		t.Parallel()
		f := newSigFixture(t)
		read := 0 // ids asked to the reader
		f.objects.EXPECT().ReadCommitObjects(ctx, repo, mock.Anything).
			RunAndReturn(func(_ context.Context, _ domain.RepoName, ids []domain.ObjectID) (map[domain.ObjectID][]byte, error) {
				read += len(ids)
				m := make(map[domain.ObjectID][]byte, len(ids))
				for _, id := range ids {
					m[id] = rawCommit("")
				}
				return m, nil
			})
		mk := func(from, n int) []domain.Commit {
			cs := make([]domain.Commit, n)
			for i := range cs {
				cs[i] = sigCommit("a")
				cs[i].ID = numID(from + i)
			}
			return cs
		}
		const total = 2 * usecase.MaxSignatureCacheEntries
		for from := 0; from < total; from += 1000 {
			assert.Len(t, f.sigs.VerifyCommits(ctx, repo, mk(from, 1000)), 1000)
		}
		assert.Equal(t, total, read)
		// The latest page is still cached.
		f.sigs.VerifyCommits(ctx, repo, mk(total-1000, 1000))
		assert.Equal(t, total, read)
		// At most MaxSignatureCacheEntries of the total are kept.
		read = 0
		assert.Len(t, f.sigs.VerifyCommits(ctx, repo, mk(0, total)), total)
		assert.GreaterOrEqual(t, read, total-usecase.MaxSignatureCacheEntries)
	})
}

func numID(n int) domain.ObjectID {
	id, err := domain.ParseObjectID(fmt.Sprintf("%040x", n+1))
	if err != nil {
		panic(err)
	}
	return id
}
