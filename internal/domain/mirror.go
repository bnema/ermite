// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	MirrorMinInterval     = 5 * time.Minute  // densest cron allowed
	MirrorManualInterval  = 60 * time.Second // min gap between manual syncs (not --force)
	MirrorRepoPlaceholder = "{repo}"
)

// MirrorGlobalConfig is the server-wide mirror policy.
type MirrorGlobalConfig struct {
	Enabled     bool
	URLTemplate string // ssh URL containing MirrorRepoPlaceholder; "" allowed while disabled
	Cron        string // 5-field cron, UTC; "" allowed while disabled
	AllBranches bool
	UpdatedAt   time.Time
}

// MirrorRepoConfig is the per-repository entry: an override (URL != "") or an exclusion.
type MirrorRepoConfig struct {
	Repo        RepoName
	URL         string // "" means no override
	Cron        string // "" means inherit global cron
	AllBranches *bool  // nil means inherit
	Excluded    bool   // opt out of the global template; ignored when URL != ""
	UpdatedAt   time.Time
}
type MirrorSource string

const (
	MirrorSourceNone     MirrorSource = "disabled" // nothing applies
	MirrorSourceGlobal   MirrorSource = "global"
	MirrorSourceOverride MirrorSource = "override"
	MirrorSourceExcluded MirrorSource = "excluded"
)

// MirrorTarget is what one repository is mirrored to after resolution.
type MirrorTarget struct {
	Repo        RepoName
	Enabled     bool
	URL         string
	Cron        Cron // zero when !Enabled
	CronSpec    string
	AllBranches bool
	Source      MirrorSource
}

func mirrorURLParts(s string) (host, path string, err error) {
	bad := func() (string, string, error) {
		return "", "", fmt.Errorf("%w: mirror url must be an SSH host and repository path without credentials", ErrInvalid)
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return bad()
	}
	if strings.HasPrefix(s, "ssh://") {
		u, e := url.Parse(s)
		if e != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
			return bad()
		}
		if u.User != nil {
			if _, ok := u.User.Password(); ok {
				return bad()
			}
			if u.User.Username() == "" {
				return bad()
			}
		}
		if p := u.Port(); p != "" {
			n, e := strconv.Atoi(p)
			if e != nil || n < 1 || n > 65535 {
				return bad()
			}
		}
		host = u.Host
		path = strings.TrimPrefix(u.Path, "/")
	} else {
		if strings.Contains(s, "://") {
			return bad()
		}
		var ok bool
		host, path, ok = strings.Cut(s, ":")
		if !ok {
			return bad()
		}
		if strings.Contains(host, "@") {
			user, h, _ := strings.Cut(host, "@")
			if user == "" || strings.ContainsAny(user, ":/@") {
				return bad()
			}
			host = h
		}
		if strings.ContainsAny(host, "/@:") {
			return bad()
		}
	}
	if host == "" || strings.HasPrefix(host, "-") || path == "" || strings.HasPrefix(path, "-") || strings.Contains(path, "@") {
		return bad()
	}
	// Reject malformed bracketed addresses as well as empty hosts.
	if strings.HasPrefix(host, "[") {
		h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if x, _, e := net.SplitHostPort(host); e == nil {
			h = strings.Trim(x, "[]")
		}
		if net.ParseIP(h) == nil {
			return bad()
		}
	}
	return host, path, nil
}

// ValidateMirrorURL accepts "git@host:path" (scp-like, user optional, no ':' port) and
// "ssh://[user@]host[:port]/path". Anything else, an empty host, control chars, or a path
// starting with "-" is "%w: mirror url ..." with ErrInvalid. URL is never logged in full
// elsewhere: use RedactMirrorURL.
func ValidateMirrorURL(s string) error { _, _, err := mirrorURLParts(s); return err }
func RedactMirrorURL(s string) string {
	h, p, e := mirrorURLParts(s)
	if e != nil {
		return "[invalid mirror URL]"
	}
	return h + ":" + p
}

// ExpandMirrorURL replaces MirrorRepoPlaceholder with repo (as a path segment, not escaped
// beyond RepoName's own charset). Returns ErrInvalid when the template has no placeholder.
func ExpandMirrorURL(template string, repo RepoName) (string, error) {
	if !strings.Contains(template, MirrorRepoPlaceholder) {
		return "", fmt.Errorf("%w: mirror url template needs {repo}", ErrInvalid)
	}
	return strings.ReplaceAll(template, MirrorRepoPlaceholder, repo.String()), nil
}
func mirrorCron(spec string) (Cron, error) {
	c, e := ParseCron(spec)
	if e != nil {
		return c, e
	}
	if gap := CronMinGap(c); gap > 0 && gap < MirrorMinInterval {
		return Cron{}, fmt.Errorf("%w: cron interval must be at least five minutes", ErrInvalid)
	}
	return c, nil
}

// ValidateMirrorGlobalConfig / ValidateMirrorRepoConfig return InvalidFieldsError with fields
// "url_template", "cron", "url": cron parsed with ParseCron and checked by CronMinGap >= MirrorMinInterval;
// url_template must contain the placeholder; when Enabled both are required.
func ValidateMirrorGlobalConfig(g MirrorGlobalConfig) error {
	var fields []FieldViolation
	if g.Enabled || g.URLTemplate != "" {
		repo, _ := ParseRepoName("repo")
		u, e := ExpandMirrorURL(g.URLTemplate, repo)
		if e == nil {
			e = ValidateMirrorURL(u)
		}
		if e != nil {
			fields = append(fields, FieldViolation{"url_template", "must be an SSH URL template containing {repo}"})
		}
	}
	if g.Enabled || g.Cron != "" {
		if _, e := mirrorCron(g.Cron); e != nil {
			fields = append(fields, FieldViolation{"cron", e.Error()})
		}
	}
	if len(fields) > 0 {
		return &InvalidFieldsError{Fields: fields}
	}
	return nil
}
func ValidateMirrorRepoConfig(r MirrorRepoConfig) error {
	var fields []FieldViolation
	if r.URL != "" {
		if e := ValidateMirrorURL(r.URL); e != nil {
			fields = append(fields, FieldViolation{"url", e.Error()})
		}
	}
	if r.Cron != "" {
		if _, e := mirrorCron(r.Cron); e != nil {
			fields = append(fields, FieldViolation{"cron", e.Error()})
		}
	}
	if len(fields) > 0 {
		return &InvalidFieldsError{Fields: fields}
	}
	return nil
}

// ResolveMirror: override wins even when global is disabled; else global, unless excluded.
func ResolveMirror(g MirrorGlobalConfig, r *MirrorRepoConfig, repo RepoName) (MirrorTarget, error) {
	t := MirrorTarget{Repo: repo, Source: MirrorSourceNone}
	if r != nil && r.URL != "" {
		t.URL = r.URL
		t.Source = MirrorSourceOverride
	} else if r != nil && r.Excluded {
		t.Source = MirrorSourceExcluded
		return t, nil
	} else if g.Enabled {
		var e error
		t.URL, e = ExpandMirrorURL(g.URLTemplate, repo)
		if e != nil {
			return t, e
		}
		t.Source = MirrorSourceGlobal
	} else {
		return t, nil
	}
	t.CronSpec = g.Cron
	t.AllBranches = g.AllBranches
	if r != nil {
		if r.Cron != "" {
			t.CronSpec = r.Cron
		}
		if r.AllBranches != nil {
			t.AllBranches = *r.AllBranches
		}
	}
	if e := ValidateMirrorURL(t.URL); e != nil {
		return t, e
	}
	// Mutation boundaries validate the minimum gap; resolution only parses.
	c, e := ParseCron(t.CronSpec)
	if e != nil {
		return t, e
	}
	t.Cron = c
	t.Enabled = true
	return t, nil
}
