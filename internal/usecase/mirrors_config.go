// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"
	"errors"
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
	// One failing reset must not leave the others stale: the policy is saved.
	var errs []error
	for _, name := range changed {
		if err := m.reset(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}
	return g, errors.Join(errs...)
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

// reset lifts the incident of a repository whose effective target changed and marks it dirty.
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
	g := m.gens[repo]
	g.dirty++
	m.gens[repo] = g
	m.mu.Unlock()
	return m.cfg.Store.PutStatus(ctx, s)
}

// resetIfChanged resets repo when its effective target is no longer before. configMu is held.
func (m *Mirrors) resetIfChanged(ctx context.Context, repo domain.RepoName, before domain.MirrorTarget, beforeErr error) (domain.MirrorTarget, error) {
	after, err := m.target(ctx, repo)
	if err != nil {
		return after, err
	}
	if beforeErr == nil && sameMirrorTarget(before, after) {
		return after, nil
	}
	return after, m.reset(ctx, repo)
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
	before, beforeErr := m.target(ctx, c.Repo)
	c.UpdatedAt = m.cfg.Clock.Now().UTC()
	if err := m.cfg.Store.PutRepoConfig(ctx, c); err != nil {
		return domain.MirrorTarget{}, err
	}
	return m.resetIfChanged(ctx, c.Repo, before, beforeErr)
}

// RemoveRepoMirror restores inherited policy.
func (m *Mirrors) RemoveRepoMirror(ctx context.Context, p domain.Principal, repo domain.RepoName) error {
	if err := m.admin(ctx, p, repo); err != nil {
		return err
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	before, beforeErr := m.target(ctx, repo)
	if err := m.cfg.Store.DeleteRepoConfig(ctx, repo); err != nil {
		return err
	}
	_, err := m.resetIfChanged(ctx, repo, before, beforeErr)
	return err
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
	return m.statusView(ctx, p, repo, true)
}

// statusView builds the view of one repository; withNext computes the next scheduled run.
func (m *Mirrors) statusView(ctx context.Context, p domain.Principal, repo domain.RepoName, withNext bool) (ports.MirrorStatusView, error) {
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
	return m.view(p, t, s, withNext), nil
}

func (m *Mirrors) view(p domain.Principal, t domain.MirrorTarget, s domain.MirrorStatus, withNext bool) ports.MirrorStatusView {
	var next time.Time
	if withNext && t.Enabled && s.State != domain.MirrorPaused {
		if s.State == domain.MirrorFailing {
			next = s.NextRetryAt
		} else {
			next, _ = domain.NextCron(domain.CronSchedule{Cron: t.Cron, Loc: time.UTC}, m.cfg.Clock.Now())
		}
	}
	if !p.Allows(domain.ScopeRepoAdmin, t.Repo) {
		s.Detail, s.Reason = "", ""
		t.URL = ""
	}
	return ports.MirrorStatusView{Target: t, Status: s, NextRunAt: next}
}

// ListStatus returns all statuses to an owner, from three reads instead of three per repository.
func (m *Mirrors) ListStatus(ctx context.Context, p domain.Principal) ([]ports.MirrorStatusView, error) {
	if err := mirrorOwner(p); err != nil {
		return nil, err
	}
	repos, err := m.cfg.Meta.List(ctx)
	if err != nil {
		return nil, err
	}
	g, err := m.cfg.Store.GetGlobal(ctx)
	if err != nil {
		return nil, err
	}
	configs, err := m.cfg.Store.ListRepoConfigs(ctx)
	if err != nil {
		return nil, err
	}
	byRepo := make(map[domain.RepoName]domain.MirrorRepoConfig, len(configs))
	for _, c := range configs {
		byRepo[c.Repo] = c
	}
	statuses, err := m.cfg.Store.ListStatus(ctx)
	if err != nil {
		return nil, err
	}
	stored := make(map[domain.RepoName]domain.MirrorStatus, len(statuses))
	for _, s := range statuses {
		stored[s.Repo] = s
	}
	out := make([]ports.MirrorStatusView, 0, len(repos))
	for _, r := range repos {
		var cp *domain.MirrorRepoConfig
		if c, ok := byRepo[r.Name]; ok {
			cp = &c
		}
		t, err := m.resolve(g, cp, r.Name)
		if err != nil {
			return nil, err
		}
		s, ok := stored[r.Name]
		if !ok {
			s = domain.MirrorStatus{Repo: r.Name, State: domain.MirrorDisabled}
		}
		out = append(out, m.view(p, t, s, true))
	}
	return out, nil
}

// Warning provides the post-receive diagnostic of a failing or paused mirror to the admins of
// the repository only: it names the remote and the reason.
func (m *Mirrors) Warning(ctx context.Context, p domain.Principal, repo domain.RepoName) string {
	if !p.Allows(domain.ScopeRepoAdmin, repo) {
		return ""
	}
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
	v, err := m.statusView(ctx, p, repo, false)
	if err != nil {
		return domain.MirrorDisabled, ""
	}
	return v.Status.State, v.Status.Reason
}

func (m *Mirrors) validateGlobal(g domain.MirrorGlobalConfig) error {
	return domain.ValidateMirrorGlobalConfigWith(g, m.cfg.ValidateURL)
}
func (m *Mirrors) validateRepo(c domain.MirrorRepoConfig) error {
	return domain.ValidateMirrorRepoConfigWith(c, m.cfg.ValidateURL)
}
func (m *Mirrors) resolve(g domain.MirrorGlobalConfig, c *domain.MirrorRepoConfig, repo domain.RepoName) (domain.MirrorTarget, error) {
	return domain.ResolveMirrorWith(g, c, repo, m.cfg.ValidateURL)
}
