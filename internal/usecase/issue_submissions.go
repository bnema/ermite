// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Parameters of the anonymous submission (ADR 0031).
const (
	// baseDifficulty is the proof-of-work of a client that has not submitted
	// lately, in leading zero bits of SHA-256: 2^18 hashes on average, about
	// one second for a SHA-256 in JavaScript on a mid-range phone.
	baseDifficulty = 18
	// maxDifficulty caps the adaptive proof-of-work: about 8 to 10 seconds.
	maxDifficulty = 21
	// perAddrStep and globalStep: one more bit per that many submissions of the
	// last hour from the client, and from everyone.
	perAddrStep = 2
	globalStep  = 20

	// challengeTTL is how long a challenge may be solved and sent.
	challengeTTL = 10 * time.Minute
	// minFillTime is the least time between the challenge and the submission:
	// a human cannot write an issue faster.
	minFillTime = 3 * time.Second
	// maxSpent bounds the nonces remembered as spent. Past it submissions are
	// paused rather than a live nonce forgotten.
	maxSpent = 100_000

	tokenVersion    = 1
	nonceLen        = 16
	maxChallengeLen = 1024
	maxSolutionLen  = 20
	spentSweep      = time.Minute
)

// difficultyFor is the proof-of-work for a client with perAddr submissions in
// the last hour among global ones: base, plus a bit per perAddrStep and per
// globalStep, at most limit. It decays as the sliding window empties.
func difficultyFor(base, limit, perAddr, global int) int {
	return min(base+max(perAddr, 0)/perAddrStep+max(global, 0)/globalStep, limit)
}

// leadingZeroBits counts the zero bits at the start of sum.
func leadingZeroBits(sum []byte) int {
	n := 0
	for _, b := range sum {
		if b == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(b)
	}
	return n
}

// powHash is SHA-256(nonce || ":" || solution).
func powHash(nonce []byte, solution string) []byte {
	h := sha256.New()
	h.Write(nonce)
	h.Write([]byte(":"))
	h.Write([]byte(solution))
	return h.Sum(nil)
}

// challengeClaims is the signed payload of a challenge token.
type challengeClaims struct {
	V          int    `json:"v"`
	Repo       string `json:"repo"`
	Nonce      []byte `json:"nonce"`
	Difficulty int    `json:"d"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
	IP         []byte `json:"ip"`
}

// IssueSubmissions is the anonymous submission of issues: a signed challenge
// with an adaptive proof of work, and every server-side check before the issue
// awaits moderation.
type IssueSubmissions struct {
	meta    ports.RepoMetaStore
	pending ports.PendingIssueStore
	bans    ports.IssueBanStore
	limiter ports.SubmissionLimiter
	keys    *IssueKeys
	bus     ports.EventBus
	clock   ports.Clock
	base    int
	limit   int
	minFill time.Duration

	admitMu sync.Mutex // serializes the pending cap check and the store

	mu        sync.Mutex
	spent     map[string]time.Time // nonce -> expiry of its challenge
	nextSweep time.Time
}

var _ ports.IssueSubmissionService = (*IssueSubmissions)(nil)

// IssueSubmissionsConfig holds what the submission needs. Every field but the
// difficulties is required.
type IssueSubmissionsConfig struct {
	Meta    ports.RepoMetaStore
	Pending ports.PendingIssueStore
	Bans    ports.IssueBanStore
	Limiter ports.SubmissionLimiter
	Keys    *IssueKeys
	Bus     ports.EventBus
	Clock   ports.Clock
	// BaseDifficulty and MaxDifficulty replace the production constants; for
	// tests. Zero keeps the constants.
	BaseDifficulty, MaxDifficulty int
	// MinFillTime replaces minFillTime when positive; for tests.
	MinFillTime time.Duration
}

// NewIssueSubmissions builds the submission use case.
func NewIssueSubmissions(cfg IssueSubmissionsConfig) *IssueSubmissions {
	s := &IssueSubmissions{
		meta: cfg.Meta, pending: cfg.Pending, bans: cfg.Bans, limiter: cfg.Limiter,
		keys: cfg.Keys, bus: cfg.Bus, clock: cfg.Clock,
		base: baseDifficulty, limit: maxDifficulty, minFill: minFillTime,
		spent: make(map[string]time.Time),
	}
	if cfg.BaseDifficulty > 0 {
		s.base = cfg.BaseDifficulty
	}
	if cfg.MaxDifficulty > 0 {
		s.limit = cfg.MaxDifficulty
	}
	if cfg.MinFillTime > 0 {
		s.minFill = cfg.MinFillTime
	}
	return s
}

// openRepo returns the repository if anyone may submit to it, else
// domain.ErrNotFound.
func (s *IssueSubmissions) openRepo(ctx context.Context, repo domain.RepoName) error {
	r, err := lookupRepo(ctx, s.meta, repo)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalidName) {
			return domain.ErrNotFound
		}
		return err
	}
	if !canRead(domain.Anonymous(), r) || r.Settings.Issues.OrDefault() != domain.IssuesPublic {
		return domain.ErrNotFound
	}
	return nil
}

func (s *IssueSubmissions) sign(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// Challenge issues a challenge for a client of the repository.
func (s *IssueSubmissions) Challenge(ctx context.Context, repo domain.RepoName, addr string) (ports.IssueChallenge, error) {
	if err := ctx.Err(); err != nil {
		return ports.IssueChallenge{}, err
	}
	if err := s.openRepo(ctx, repo); err != nil {
		return ports.IssueChallenge{}, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return ports.IssueChallenge{}, fmt.Errorf("random nonce: %w", err)
	}
	perAddr, global := s.limiter.Recent(ctx, addr)
	d := difficultyFor(s.base, s.limit, perAddr, global)
	now := s.clock.Now().UTC()
	claims := challengeClaims{
		V: tokenVersion, Repo: repo.String(), Nonce: nonce, Difficulty: d,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(challengeTTL).Unix(), IP: s.keys.IPHash(addr),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ports.IssueChallenge{}, fmt.Errorf("encode challenge: %w", err)
	}
	return ports.IssueChallenge{
		Token:       enc64(payload) + "." + enc64(s.sign(s.keys.PoWKey(), payload)),
		Nonce:       enc64(nonce),
		Difficulty:  d,
		FormToken:   enc64(s.sign(s.keys.FormTokenKey(), nonce)),
		IssuedAt:    time.Unix(claims.IssuedAt, 0).UTC(),
		MinSubmitAt: time.Unix(claims.IssuedAt, 0).UTC().Add(s.minFill),
		ExpiresAt:   time.Unix(claims.ExpiresAt, 0).UTC(),
	}, nil
}

// parseToken verifies the signature and returns the claims, or false.
func (s *IssueSubmissions) parseToken(tok string) (challengeClaims, bool) {
	var c challengeClaims
	if len(tok) > maxChallengeLen {
		return c, false
	}
	p, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return c, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return c, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(s.keys.PoWKey(), payload)) {
		return c, false
	}
	if json.Unmarshal(payload, &c) != nil {
		return c, false
	}
	return c, true
}

func validSolution(sol string) bool {
	if len(sol) < 1 || len(sol) > maxSolutionLen {
		return false
	}
	for i := 0; i < len(sol); i++ {
		if sol[i] < '0' || sol[i] > '9' {
			return false
		}
	}
	return true
}

// spend marks the nonce spent. It returns domain.ErrSubmissionInvalid for a
// nonce already spent and domain.ErrSubmissionsPaused when the set is full.
func (s *IssueSubmissions) spend(nonce []byte, exp, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spent) >= maxSpent || !now.Before(s.nextSweep) {
		for k, e := range s.spent {
			if now.After(e) { // an expired challenge is refused anyway
				delete(s.spent, k)
			}
		}
		s.nextSweep = now.Add(spentSweep)
	}
	key := string(nonce)
	if _, dup := s.spent[key]; dup {
		return domain.ErrSubmissionInvalid
	}
	if len(s.spent) >= maxSpent {
		return domain.ErrSubmissionsPaused
	}
	s.spent[key] = exp
	return nil
}

// Submit checks the submission and stores it as a pending issue. The checks
// run in a fixed order and stop at the first failure; every bot-related
// failure answers domain.ErrSubmissionInvalid, and the precise reason is only
// logged (without address, hash or text).
func (s *IssueSubmissions) Submit(ctx context.Context, repo domain.RepoName, addr string, f ports.IssueSubmission) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log := zerowrap.FromCtx(ctx)
	refuse := func(reason string, err error) error {
		log.Info().Str("repo", repo.String()).Str("reason", reason).Msg("issue submission refused")
		return err
	}
	if err := s.openRepo(ctx, repo); err != nil {
		return err
	}
	ipHash := s.keys.IPHash(addr)
	now := s.clock.Now().UTC()
	if f.Honeypot != "" {
		return refuse("honeypot", domain.ErrSubmissionInvalid)
	}
	c, ok := s.parseToken(f.Token)
	if !ok {
		return refuse("bad_token", domain.ErrSubmissionInvalid)
	}
	exp := time.Unix(c.ExpiresAt, 0)
	switch {
	case c.V != tokenVersion || len(c.Nonce) != nonceLen:
		return refuse("bad_token", domain.ErrSubmissionInvalid)
	case c.Repo != repo.String():
		return refuse("token_other_repo", domain.ErrSubmissionInvalid)
	case now.After(exp):
		return refuse("token_expired", domain.ErrSubmissionInvalid)
	case !hmac.Equal(c.IP, ipHash):
		return refuse("token_other_client", domain.ErrSubmissionInvalid)
	}
	if !hmac.Equal([]byte(f.FormToken), []byte(enc64(s.sign(s.keys.FormTokenKey(), c.Nonce)))) {
		return refuse("bad_form_token", domain.ErrSubmissionInvalid)
	}
	if now.Before(time.Unix(c.IssuedAt, 0).Add(s.minFill)) {
		return refuse("too_fast", domain.ErrSubmissionInvalid)
	}
	if !validSolution(f.Solution) || c.Difficulty < 1 || c.Difficulty > 8*sha256.Size ||
		leadingZeroBits(powHash(c.Nonce, f.Solution)) < c.Difficulty {
		return refuse("bad_pow", domain.ErrSubmissionInvalid)
	}
	if err := s.spend(c.Nonce, exp, now); err != nil {
		if errors.Is(err, domain.ErrSubmissionsPaused) {
			return refuse("spent_full", err)
		}
		return refuse("replayed", err)
	}
	banned, err := s.bans.Banned(ctx, ipHash, now)
	if err != nil {
		return fmt.Errorf("check ban: %w", err)
	}
	if banned {
		return refuse("banned", domain.ErrSubmissionInvalid)
	}
	title, err := domain.ParseIssueTitle(f.Title)
	if err != nil {
		return refuse("text_title", fmt.Errorf("title: %w", err))
	}
	body, err := domain.ParseIssueBody(f.Body)
	if err != nil {
		return refuse("text_body", fmt.Errorf("body: %w", err))
	}
	author, err := domain.ParseAuthorLabel(f.Author)
	if err != nil {
		return refuse("text_author", fmt.Errorf("author: %w", err))
	}
	id, err := domain.NewPendingIssueID()
	if err != nil {
		return fmt.Errorf("new pending issue id: %w", err)
	}
	if err := s.admit(ctx, addr, refuse, domain.PendingIssue{
		ID: id, Repo: repo, Title: title, Body: body, Author: author, CreatedAt: now, IPHash: ipHash,
	}); err != nil {
		return err
	}
	log.Info().Str("repo", repo.String()).Msg("issue submitted for moderation")
	s.bus.Publish(ctx, domain.IssueSubmitted{Repo: repo, ID: id, At: now})
	return nil
}

// admit reserves the client's quota, checks the global queue cap and stores
// the issue. Ermite is a single process (ADR 0001), so admitMu serializes the
// Count -> Put section: concurrent submissions cannot exceed
// domain.MaxPendingIssues. It would not hold with several processes.
func (s *IssueSubmissions) admit(ctx context.Context, addr string, refuse func(string, error) error, p domain.PendingIssue) error {
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	ok, after, release := s.limiter.Reserve(ctx, addr)
	if !ok {
		return refuse("rate_limited", &domain.SubmissionRetryError{After: after})
	}
	n, err := s.pending.Count(ctx)
	if err != nil {
		release()
		return fmt.Errorf("count pending issues: %w", err)
	}
	if n >= domain.MaxPendingIssues {
		release()
		return refuse("pending_full", domain.ErrSubmissionsPaused)
	}
	if err := s.pending.Put(ctx, p); err != nil {
		release()
		return fmt.Errorf("store pending issue of %q: %w", p.Repo, err)
	}
	return nil
}

func enc64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
