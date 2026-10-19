// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math/bits"
	"strconv"
	"strings"
	"sync"
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

const testBits = 8

type subFixture struct {
	t       *testing.T
	meta    *mocks.RepoMetaStore
	pending *mocks.PendingIssueStore
	bans    *mocks.IssueBanStore
	limiter *mocks.SubmissionLimiter
	bus     *mocks.EventBus
	uc      *usecase.IssueSubmissions
	repo    domain.Repo
	addr    string

	mu  sync.Mutex
	now time.Time
}

func (f *subFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func newSubFixture(t *testing.T) *subFixture {
	t.Helper()
	f := &subFixture{
		t: t, meta: mocks.NewRepoMetaStore(t), pending: mocks.NewPendingIssueStore(t),
		bans: mocks.NewIssueBanStore(t), limiter: mocks.NewSubmissionLimiter(t), bus: mocks.NewEventBus(t),
		addr: "ip:203.0.113.7", now: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
	}
	f.repo = domain.Repo{Name: name(t, "proj"), Visibility: domain.VisibilityPublic, CreatedAt: f.now, Settings: withIssues(domain.IssuesPublic)}
	f.meta.EXPECT().Get(mock.Anything, f.repo.Name).Return(f.repo, nil).Maybe()
	clock := mocks.NewClock(t)
	clock.EXPECT().Now().RunAndReturn(func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}).Maybe()
	keyStore := mocks.NewIssueKeyStore(t)
	keyStore.EXPECT().Key(mock.Anything).Return([]byte(strings.Repeat("k", 32)), nil)
	keys, err := usecase.NewIssueKeys(context.Background(), keyStore)
	require.NoError(t, err)
	f.uc = usecase.NewIssueSubmissions(usecase.IssueSubmissionsConfig{
		Meta: f.meta, Pending: f.pending, Bans: f.bans, Limiter: f.limiter, Keys: keys, Bus: f.bus, Clock: clock,
		BaseDifficulty: testBits, MaxDifficulty: testBits + 3,
	})
	return f
}

// open expects the checks before the token: not banned, allowed, room.
func (f *subFixture) open(pendingCount int) {
	f.bans.EXPECT().Banned(mock.Anything, mock.Anything, mock.Anything).Return(false, nil).Maybe()
	f.limiter.EXPECT().Allow(mock.Anything, mock.Anything).Return(true, time.Duration(0)).Maybe()
	f.pending.EXPECT().Count(mock.Anything).Return(pendingCount, nil).Maybe()
}

func (f *subFixture) challenge(addr string) ports.IssueChallenge {
	f.t.Helper()
	f.limiter.EXPECT().Recent(mock.Anything, addr).Return(0, 0).Once()
	ch, err := f.uc.Challenge(context.Background(), f.repo.Name, addr)
	require.NoError(f.t, err)
	return ch
}

func solve(t *testing.T, ch ports.IssueChallenge) string {
	t.Helper()
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	require.NoError(t, err)
	for i := 0; ; i++ {
		s := strconv.Itoa(i)
		sum := sha256.Sum256(append(append(append([]byte(nil), nonce...), ':'), s...))
		if zeroBits(sum[:]) >= ch.Difficulty {
			return s
		}
	}
}

func zeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(x)
	}
	return n
}

// valid is a submission that passes every check once the fill time is over.
func (f *subFixture) valid(ch ports.IssueChallenge) ports.IssueSubmission {
	return ports.IssueSubmission{
		Token: ch.Token, Solution: solve(f.t, ch), FormToken: ch.FormToken,
		Title: "A bug", Body: "It breaks.", Author: "me",
	}
}

func TestIssueChallenge(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	ch := f.challenge(f.addr)
	assert.Equal(t, testBits, ch.Difficulty)
	assert.Equal(t, f.now, ch.IssuedAt)
	assert.Equal(t, f.now.Add(usecase.MinFillTime), ch.MinSubmitAt)
	assert.Equal(t, f.now.Add(usecase.ChallengeTTL), ch.ExpiresAt)
	assert.Contains(t, ch.Token, ".")
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	require.NoError(t, err)
	assert.Len(t, nonce, 16)
	assert.NotEmpty(t, ch.FormToken)

	f.limiter.EXPECT().Recent(mock.Anything, f.addr).Return(4, 40).Once() // +2 +2
	ch, err = f.uc.Challenge(context.Background(), f.repo.Name, f.addr)
	require.NoError(t, err)
	assert.Equal(t, testBits+3, ch.Difficulty) // capped at base+3
}

func TestIssueSubmissionRepoAccess(t *testing.T) {
	t.Parallel()
	for name_, repo := range map[string]domain.Repo{
		"private":      {Visibility: domain.VisibilityPrivate, Settings: withIssues(domain.IssuesPublic)},
		"owner issues": {Visibility: domain.VisibilityPublic, Settings: withIssues(domain.IssuesOwner)},
		"issues off":   {Visibility: domain.VisibilityPublic, Settings: withIssues(domain.IssuesOff)},
	} {
		t.Run(name_, func(t *testing.T) {
			t.Parallel()
			f := newSubFixture(t)
			meta := mocks.NewRepoMetaStore(t)
			repo.Name = f.repo.Name
			meta.EXPECT().Get(mock.Anything, repo.Name).Return(repo, nil)
			f.uc = usecase.NewIssueSubmissions(usecase.IssueSubmissionsConfig{Meta: meta, Pending: f.pending, Bans: f.bans, Limiter: f.limiter, Bus: f.bus, Clock: mocks.NewClock(t)})
			_, err := f.uc.Challenge(context.Background(), repo.Name, f.addr)
			assert.ErrorIs(t, err, domain.ErrNotFound)
			err = f.uc.Submit(context.Background(), repo.Name, f.addr, ports.IssueSubmission{})
			assert.ErrorIs(t, err, domain.ErrNotFound)
		})
	}
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		f := newSubFixture(t)
		other := name(t, "other")
		f.meta.EXPECT().Get(mock.Anything, other).Return(domain.Repo{}, domain.ErrNotFound)
		assert.ErrorIs(t, f.uc.Submit(context.Background(), other, f.addr, ports.IssueSubmission{}), domain.ErrNotFound)
		_, err := f.uc.Challenge(context.Background(), other, f.addr)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestIssueSubmitSuccess(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	ch := f.challenge(f.addr)
	sub := f.valid(ch)
	f.advance(usecase.MinFillTime)
	f.limiter.EXPECT().Record(mock.Anything, f.addr).Once()
	var put domain.PendingIssue
	f.pending.EXPECT().Put(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p domain.PendingIssue) error {
		put = p
		return nil
	}).Once()
	f.bus.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(ev domain.Event) bool {
		e, ok := ev.(domain.IssueSubmitted)
		return ok && e.Repo == f.repo.Name && e.ID == put.ID
	})).Once()
	require.NoError(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub))
	assert.Equal(t, "A bug", put.Title.String())
	assert.Equal(t, "It breaks.", put.Body.String())
	assert.Equal(t, "me", put.Author)
	assert.Equal(t, f.repo.Name, put.Repo)
	assert.Len(t, put.IPHash, domain.IPHashLen)
	assert.False(t, strings.Contains(string(put.IPHash), f.addr))
	assert.Equal(t, f.now, put.CreatedAt)
}

// TestIssueSubmitRefusals: each failing check returns its sentinel; the
// strict mocks fail the test on any Put, Record or Publish.
func TestIssueSubmitRefusals(t *testing.T) {
	t.Parallel()
	type tc struct {
		name    string
		want    error
		arrange func(f *subFixture, ch *ports.IssueChallenge, sub *ports.IssueSubmission) (addr string)
		wait    time.Duration
		setup   func(f *subFixture) // replaces f.open
	}
	wait := usecase.MinFillTime
	cases := []tc{
		{name: "banned", want: domain.ErrSubmissionInvalid, wait: wait, setup: func(f *subFixture) {
			f.bans.EXPECT().Banned(mock.Anything, mock.Anything, mock.Anything).Return(true, nil)
		}},
		{name: "rate limited", want: domain.ErrSubmissionRateLimited, wait: wait, setup: func(f *subFixture) {
			f.bans.EXPECT().Banned(mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
			f.limiter.EXPECT().Allow(mock.Anything, mock.Anything).Return(false, 5*time.Minute)
		}},
		{name: "paused at 200", want: domain.ErrSubmissionsPaused, wait: wait, setup: func(f *subFixture) {
			f.bans.EXPECT().Banned(mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
			f.limiter.EXPECT().Allow(mock.Anything, mock.Anything).Return(true, time.Duration(0))
			f.pending.EXPECT().Count(mock.Anything).Return(domain.MaxPendingIssues, nil)
		}},
		{name: "honeypot", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			s.Honeypot = "http://spam"
			return ""
		}},
		{name: "forged signature", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			p, sig, _ := strings.Cut(s.Token, ".")
			b := []byte(sig)
			b[0] ^= 1 // still base64url? flip between 'A'/'B'
			if b[0] > 'z' || b[0] < '0' {
				b[0] = 'A'
			}
			if string(b) == sig {
				b[0] = 'B'
			}
			s.Token = p + "." + string(b)
			return ""
		}},
		{name: "forged payload", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			_, sig, _ := strings.Cut(s.Token, ".")
			s.Token = base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"d":1}`)) + "." + sig
			return ""
		}},
		{name: "token of another repo", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(f *subFixture, ch *ports.IssueChallenge, s *ports.IssueSubmission) string {
			// a valid token for "other", sent to "proj"
			other := name(f.t, "other")
			f.meta.EXPECT().Get(mock.Anything, other).Return(domain.Repo{Name: other, Visibility: domain.VisibilityPublic, Settings: withIssues(domain.IssuesPublic)}, nil)
			f.limiter.EXPECT().Recent(mock.Anything, f.addr).Return(0, 0).Once()
			c2, err := f.uc.Challenge(context.Background(), other, f.addr)
			require.NoError(f.t, err)
			*s = f.valid(c2)
			return ""
		}},
		{name: "expired", want: domain.ErrSubmissionInvalid, wait: usecase.ChallengeTTL + time.Second},
		{name: "other ip", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(f *subFixture, _ *ports.IssueChallenge, _ *ports.IssueSubmission) string {
			return "ip:198.51.100.9"
		}},
		{name: "too fast", want: domain.ErrSubmissionInvalid, wait: wait - time.Second},
		{name: "wrong form token", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			s.FormToken = "AAAA"
			return ""
		}},
		{name: "insufficient pow", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, ch *ports.IssueChallenge, s *ports.IssueSubmission) string {
			// a counter that does not solve it
			nonce, _ := base64.RawURLEncoding.DecodeString(ch.Nonce)
			for i := 0; ; i++ {
				sum := sha256.Sum256(append(append(nonce, ':'), strconv.Itoa(i)...))
				if zeroBits(sum[:]) < ch.Difficulty {
					s.Solution = strconv.Itoa(i)
					return ""
				}
			}
		}},
		{name: "non digit solution", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			s.Solution = "12a"
			return ""
		}},
		{name: "empty solution", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			s.Solution = ""
			return ""
		}},
		{name: "huge solution", want: domain.ErrSubmissionInvalid, wait: wait, arrange: func(_ *subFixture, _ *ports.IssueChallenge, s *ports.IssueSubmission) string {
			s.Solution = strings.Repeat("1", 21)
			return ""
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newSubFixture(t)
			if c.setup != nil {
				c.setup(f)
			} else {
				f.open(0)
			}
			ch := f.challenge(f.addr)
			sub := f.valid(ch)
			addr := f.addr
			if c.arrange != nil {
				if a := c.arrange(f, &ch, &sub); a != "" {
					addr = a
				}
			}
			f.advance(c.wait)
			err := f.uc.Submit(context.Background(), f.repo.Name, addr, sub)
			require.ErrorIs(t, err, c.want)
			assert.ErrorIs(t, err, domain.ErrSubmissionRefused)
			if c.want == domain.ErrSubmissionRateLimited {
				var re *domain.SubmissionRetryError
				require.ErrorAs(t, err, &re)
				assert.Equal(t, 5*time.Minute, re.After)
			}
		})
	}
}

func TestIssueSubmitReplayedNonce(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	ch := f.challenge(f.addr)
	sub := f.valid(ch)
	f.advance(usecase.MinFillTime)
	// first: text refused, nonce is spent anyway
	bad := sub
	bad.Title = ""
	var te *domain.TextError
	require.ErrorAs(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, bad), &te)
	assert.Equal(t, 1, f.uc.SpentLen())
	err := f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub)
	assert.ErrorIs(t, err, domain.ErrSubmissionInvalid)
	assert.False(t, errors.Is(err, domain.ErrInvalidText))
}

func TestIssueSubmitReplayAfterSuccess(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	ch := f.challenge(f.addr)
	sub := f.valid(ch)
	f.advance(usecase.MinFillTime)
	f.limiter.EXPECT().Record(mock.Anything, f.addr).Once()
	f.pending.EXPECT().Put(mock.Anything, mock.Anything).Return(nil).Once()
	f.bus.EXPECT().Publish(mock.Anything, mock.Anything).Once()
	require.NoError(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub))
	assert.ErrorIs(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub), domain.ErrSubmissionInvalid)
}

func TestIssueSubmitTextErrors(t *testing.T) {
	t.Parallel()
	for name_, mod := range map[string]func(*ports.IssueSubmission){
		"title":  func(s *ports.IssueSubmission) { s.Title = "two\nlines" },
		"body":   func(s *ports.IssueSubmission) { s.Body = strings.Repeat("x", domain.MaxIssueBodyLen+1) },
		"author": func(s *ports.IssueSubmission) { s.Author = strings.Repeat("x", domain.MaxAuthorLabelLen+1) },
	} {
		t.Run(name_, func(t *testing.T) {
			t.Parallel()
			f := newSubFixture(t)
			f.open(0)
			ch := f.challenge(f.addr)
			sub := f.valid(ch)
			mod(&sub)
			f.advance(usecase.MinFillTime)
			err := f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub)
			var te *domain.TextError
			require.ErrorAs(t, err, &te)
			assert.NotErrorIs(t, err, domain.ErrSubmissionRefused)
		})
	}
}

func TestIssueSubmitSpentSetFull(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	ch := f.challenge(f.addr)
	sub := f.valid(ch)
	f.advance(usecase.MinFillTime)
	f.uc.FillSpent(usecase.MaxSpent, f.now.Add(time.Hour))
	assert.ErrorIs(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub), domain.ErrSubmissionsPaused)
	assert.Equal(t, usecase.MaxSpent, f.uc.SpentLen()) // nothing live evicted
}

func TestIssueSubmitSpentSetExpiry(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	f.limiter.EXPECT().Record(mock.Anything, mock.Anything).Maybe()
	f.pending.EXPECT().Put(mock.Anything, mock.Anything).Return(nil).Maybe()
	f.bus.EXPECT().Publish(mock.Anything, mock.Anything).Maybe()
	ch := f.challenge(f.addr)
	sub := f.valid(ch)
	f.advance(usecase.MinFillTime)
	require.NoError(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub))
	assert.Equal(t, 1, f.uc.SpentLen())
	f.advance(usecase.ChallengeTTL + 2*time.Minute)
	ch2 := f.challenge(f.addr)
	sub2 := f.valid(ch2)
	f.advance(usecase.MinFillTime)
	require.NoError(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub2))
	assert.Equal(t, 1, f.uc.SpentLen(), "the expired nonce was swept")

	// a full set of expired nonces is swept, not a reason to pause
	f.uc.FillSpent(usecase.MaxSpent, f.now.Add(-time.Second))
	ch3 := f.challenge(f.addr)
	sub3 := f.valid(ch3)
	f.advance(usecase.MinFillTime)
	require.NoError(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, sub3))
	assert.Equal(t, 2, f.uc.SpentLen(), "expired fillers swept, live nonces kept")
}

func TestIssueSubmitGarbageDoesNotPanic(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	f.open(0)
	ch := f.challenge(f.addr)
	p, sig, _ := strings.Cut(ch.Token, ".")
	huge := strings.Repeat("A", 1<<20)
	tokens := []string{
		"", ".", "..", "a", "a.b", "!!!.???", huge, huge + "." + huge, p, p + ".", "." + sig,
		p + "." + sig + "." + sig, p + "." + sig[:len(sig)-3], p[:len(p)/2] + "." + sig,
		base64.RawURLEncoding.EncodeToString([]byte("null")) + "." + sig,
		base64.StdEncoding.EncodeToString([]byte("{}")) + "==." + sig, "\x00\xff.\x00",
	}
	f.advance(usecase.MinFillTime)
	for _, tok := range tokens {
		for _, s := range []ports.IssueSubmission{
			{Token: tok},
			{Token: tok, Solution: huge, FormToken: huge, Title: huge, Body: huge, Author: huge},
			{Token: tok, Solution: "0", FormToken: "\xff"},
		} {
			err := f.uc.Submit(context.Background(), f.repo.Name, f.addr, s)
			assert.ErrorIs(t, err, domain.ErrSubmissionInvalid)
		}
	}
	// the genuine token with garbage around it
	for _, s := range []ports.IssueSubmission{
		{Token: ch.Token, FormToken: huge, Solution: "1"},
		{Token: ch.Token, FormToken: ch.FormToken, Solution: huge},
	} {
		assert.ErrorIs(t, f.uc.Submit(context.Background(), f.repo.Name, f.addr, s), domain.ErrSubmissionInvalid)
	}
}

func TestIssueSubmissionHonoursContext(t *testing.T) {
	t.Parallel()
	f := newSubFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, f.uc.Submit(ctx, f.repo.Name, f.addr, ports.IssueSubmission{}), context.Canceled)
	_, err := f.uc.Challenge(ctx, f.repo.Name, f.addr)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDifficulty(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ perAddr, global, want int }{
		{0, 0, 18}, {1, 0, 18}, {2, 0, 19}, {3, 0, 19}, {4, 0, 20}, {6, 0, 21}, {100, 0, 21},
		{0, 19, 18}, {0, 20, 19}, {0, 40, 20}, {0, 60, 21}, {0, 1000, 21},
		{2, 20, 20}, {4, 20, 21}, {-5, -5, 18},
	} {
		assert.Equal(t, c.want, usecase.DifficultyFor(c.perAddr, c.global), "%d/%d", c.perAddr, c.global)
	}
	assert.Equal(t, 18, usecase.BaseDifficulty)
	assert.Equal(t, 21, usecase.MaxDifficulty)
}
