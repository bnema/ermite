// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

func mirrorOwner(p domain.Principal) error {
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	if !p.HasOwnerRights() {
		return domain.ErrForbidden
	}
	return nil
}
func (m *Mirrors) admin(ctx context.Context, p domain.Principal, repo domain.RepoName) error {
	if p.IsAnonymous() {
		return domain.ErrUnauthenticated
	}
	r, err := m.cfg.Meta.Get(ctx, repo)
	if err != nil {
		return err
	}
	if !p.Allows(domain.ScopeRepoAdmin, repo) {
		if !canRead(p, r) {
			return domain.ErrNotFound
		}
		return domain.ErrForbidden
	}
	return nil
}
func (m *Mirrors) target(ctx context.Context, repo domain.RepoName) (domain.MirrorTarget, error) {
	g, err := m.cfg.Store.GetGlobal(ctx)
	if err != nil {
		return domain.MirrorTarget{}, err
	}
	r, err := m.cfg.Store.GetRepoConfig(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) {
		return m.resolve(g, nil, repo)
	}
	if err != nil {
		return domain.MirrorTarget{}, err
	}
	return m.resolve(g, &r, repo)
}
func (m *Mirrors) status(ctx context.Context, repo domain.RepoName) (domain.MirrorStatus, error) {
	s, err := m.cfg.Store.GetStatus(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.MirrorStatus{Repo: repo, State: domain.MirrorDisabled}, nil
	}
	return s, err
}

// GetGlobalConfig returns server-wide policy to an owner.
func (m *Mirrors) GetGlobalConfig(ctx context.Context, p domain.Principal) (domain.MirrorGlobalConfig, error) {
	if err := mirrorOwner(p); err != nil {
		return domain.MirrorGlobalConfig{}, err
	}
	return m.cfg.Store.GetGlobal(ctx)
}

// SetGlobalConfig patches server-wide policy and resets affected mirror incidents.
func (m *Mirrors) SetGlobalConfig(ctx context.Context, p domain.Principal, patch ports.MirrorGlobalPatch) (domain.MirrorGlobalConfig, error) {
	if err := mirrorOwner(p); err != nil {
		return domain.MirrorGlobalConfig{}, err
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	old, err := m.cfg.Store.GetGlobal(ctx)
	if err != nil {
		return old, err
	}
	g := old
	if patch.Enabled != nil {
		g.Enabled = *patch.Enabled
	}
	if patch.URLTemplate != nil {
		g.URLTemplate = *patch.URLTemplate
	}
	if patch.Cron != nil {
		g.Cron = *patch.Cron
	}
	if patch.AllBranches != nil {
		g.AllBranches = *patch.AllBranches
	}
	if err := m.validateGlobal(g); err != nil {
		return old, err
	}
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return old, err
	}
	// Resolve every effective target before and after, and persist nothing
	// unless all of them stay valid.
	var changed []domain.RepoName
	for _, r := range repos {
		c, e := m.cfg.Store.GetRepoConfig(ctx, r.Name)
		var cp *domain.MirrorRepoConfig
		if e == nil {
			cp = &c
		} else if !errors.Is(e, domain.ErrNotFound) {
			return old, e
		}
		before, _ := m.resolve(old, cp, r.Name)
		after, e := m.resolve(g, cp, r.Name)
		if e != nil {
			return old, invalidCron(e)
		}
		if !sameMirrorTarget(before, after) {
			changed = append(changed, r.Name)
		}
	}
	g.UpdatedAt = m.cfg.Clock.Now().UTC()
	if err := m.cfg.Store.PutGlobal(ctx, g); err != nil {
		return old, err
	}
	for _, name := range changed {
		if err := m.reset(ctx, name); err != nil {
			return g, err
		}
	}
	return g, nil
}

func invalidCron(e error) error {
	var inv *domain.InvalidFieldsError
	if errors.As(e, &inv) {
		return e
	}
	return &domain.InvalidFieldsError{Fields: []domain.FieldViolation{{Field: "cron", Reason: e.Error()}}}
}

func sameMirrorTarget(a, b domain.MirrorTarget) bool {
	return a.Enabled == b.Enabled && a.URL == b.URL && a.CronSpec == b.CronSpec && a.AllBranches == b.AllBranches && a.Source == b.Source
}
func (m *Mirrors) reset(ctx context.Context, repo domain.RepoName) error {
	t, err := m.target(ctx, repo)
	if err != nil {
		return err
	}
	s, err := m.status(ctx, repo)
	if err != nil {
		return err
	}
	now := m.cfg.Clock.Now().UTC()
	if t.Enabled {
		s = domain.MirrorReset(s, now)
	} else {
		s = domain.MirrorDisable(s, now)
	}
	m.mu.Lock()
	m.dirty[repo]++
	m.mu.Unlock()
	return m.cfg.Store.PutStatus(ctx, s)
}

// SetRepoMirror sets a repository override.
func (m *Mirrors) SetRepoMirror(ctx context.Context, p domain.Principal, c domain.MirrorRepoConfig) (domain.MirrorTarget, error) {
	if err := m.admin(ctx, p, c.Repo); err != nil {
		return domain.MirrorTarget{}, err
	}
	if err := m.validateRepo(c); err != nil {
		return domain.MirrorTarget{}, err
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	return m.setRepoLocked(ctx, c)
}

// setRepoLocked validates the prospective target, then persists. configMu must be held.
func (m *Mirrors) setRepoLocked(ctx context.Context, c domain.MirrorRepoConfig) (domain.MirrorTarget, error) {
	g, err := m.cfg.Store.GetGlobal(ctx)
	if err != nil {
		return domain.MirrorTarget{}, err
	}
	if _, err := m.resolve(g, &c, c.Repo); err != nil {
		return domain.MirrorTarget{}, invalidCron(err)
	}
	c.UpdatedAt = m.cfg.Clock.Now().UTC()
	if err := m.cfg.Store.PutRepoConfig(ctx, c); err != nil {
		return domain.MirrorTarget{}, err
	}
	if err := m.reset(ctx, c.Repo); err != nil {
		return domain.MirrorTarget{}, err
	}
	return m.target(ctx, c.Repo)
}

// RemoveRepoMirror restores inherited policy.
func (m *Mirrors) RemoveRepoMirror(ctx context.Context, p domain.Principal, repo domain.RepoName) error {
	if err := m.admin(ctx, p, repo); err != nil {
		return err
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	if err := m.cfg.Store.DeleteRepoConfig(ctx, repo); err != nil {
		return err
	}
	return m.reset(ctx, repo)
}

// SetRepoExcluded updates exclusion without discarding an override.
func (m *Mirrors) SetRepoExcluded(ctx context.Context, p domain.Principal, repo domain.RepoName, excluded bool) (domain.MirrorTarget, error) {
	if err := m.admin(ctx, p, repo); err != nil {
		return domain.MirrorTarget{}, err
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	c, err := m.cfg.Store.GetRepoConfig(ctx, repo)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.MirrorTarget{}, err
	}
	c.Repo, c.Excluded = repo, excluded
	return m.setRepoLocked(ctx, c)
}

// GetStatus returns a read-authorized status, redacting administrative diagnostics.
func (m *Mirrors) GetStatus(ctx context.Context, p domain.Principal, repo domain.RepoName) (ports.MirrorStatusView, error) {
	r, err := m.cfg.Meta.Get(ctx, repo)
	if err != nil {
		return ports.MirrorStatusView{}, err
	}
	if !canRead(p, r) {
		return ports.MirrorStatusView{}, domain.ErrNotFound
	}
	t, err := m.target(ctx, repo)
	if err != nil {
		return ports.MirrorStatusView{}, err
	}
	s, err := m.status(ctx, repo)
	if err != nil {
		return ports.MirrorStatusView{}, err
	}
	var next time.Time
	if t.Enabled && s.State != domain.MirrorPaused {
		if s.State == domain.MirrorFailing {
			next = s.NextRetryAt
		} else {
			next, _ = domain.NextCron(domain.CronSchedule{Cron: t.Cron, Loc: time.UTC}, m.cfg.Clock.Now())
		}
	}
	if !p.Allows(domain.ScopeRepoAdmin, repo) {
		s.Detail, s.Reason = "", ""
		t.URL = ""
	}
	return ports.MirrorStatusView{Target: t, Status: s, NextRunAt: next}, nil
}

// ListStatus returns all statuses to an owner.
func (m *Mirrors) ListStatus(ctx context.Context, p domain.Principal) ([]ports.MirrorStatusView, error) {
	if err := mirrorOwner(p); err != nil {
		return nil, err
	}
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ports.MirrorStatusView, 0, len(repos))
	for _, r := range repos {
		v, err := m.GetStatus(ctx, p, r.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Warning provides an administrative post-receive diagnostic.
func (m *Mirrors) Warning(ctx context.Context, repo domain.RepoName) string {
	s, err := m.status(ctx, repo)
	if err != nil || (s.State != domain.MirrorFailing && s.State != domain.MirrorPaused) {
		return ""
	}
	t, err := m.target(ctx, repo)
	if err != nil {
		return ""
	}
	return "ermite: mirror to " + domain.RedactMirrorURL(t.URL) + " is " + string(s.State) + ": " + s.Reason
}

// Badge returns a viewer-safe state and optional administrative reason.
func (m *Mirrors) Badge(ctx context.Context, p domain.Principal, repo domain.RepoName) (domain.MirrorState, string) {
	v, err := m.GetStatus(ctx, p, repo)
	if err != nil {
		return domain.MirrorDisabled, ""
	}
	return v.Status.State, v.Status.Reason
}

// testURL substitutes only during validation/resolution; stored and pushed URLs remain intact.
func (m *Mirrors) testURL(url string) string {
	if m.cfg.AllowAnyURL && strings.HasPrefix(url, "file://") {
		return "git@mirror-test:" + strings.TrimPrefix(url, "file://")
	}
	return url
}
func (m *Mirrors) validateGlobal(g domain.MirrorGlobalConfig) error {
	g.URLTemplate = m.testURL(g.URLTemplate)
	return domain.ValidateMirrorGlobalConfig(g)
}
func (m *Mirrors) validateRepo(c domain.MirrorRepoConfig) error {
	c.URL = m.testURL(c.URL)
	return domain.ValidateMirrorRepoConfig(c)
}
func (m *Mirrors) resolve(g domain.MirrorGlobalConfig, c *domain.MirrorRepoConfig, repo domain.RepoName) (domain.MirrorTarget, error) {
	originalGlobal := g.URLTemplate
	originalRepo := ""
	g.URLTemplate = m.testURL(g.URLTemplate)
	if c != nil {
		copyConfig := *c
		originalRepo = c.URL
		copyConfig.URL = m.testURL(c.URL)
		c = &copyConfig
	}
	t, err := domain.ResolveMirror(g, c, repo)
	if err == nil && t.Enabled && m.cfg.AllowAnyURL {
		if originalRepo != "" {
			t.URL = originalRepo
		} else if strings.HasPrefix(originalGlobal, "file://") {
			t.URL, err = domain.ExpandMirrorURL(originalGlobal, repo)
		}
	}
	return t, err
}
