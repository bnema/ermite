// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config parses and validates the server configuration from the
// environment. It is a composition-root helper: only internal/app imports it, and it never
// reads the process environment itself, the caller passes a lookup function.
package config

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Variable names.
const (
	EnvAddr           = "ERMITE_ADDR"
	EnvDataDir        = "ERMITE_DATA_DIR"
	EnvPublicURL      = "ERMITE_PUBLIC_URL"
	EnvAdminToken     = "ERMITE_ADMIN_TOKEN"
	EnvTrustedProxies = "ERMITE_TRUSTED_PROXIES"
	EnvGit            = "ERMITE_GIT"
	EnvGitReader      = "ERMITE_GIT_READER"
	EnvSourceURL      = "ERMITE_SOURCE_URL"
	EnvLogLevel       = "ERMITE_LOG_LEVEL"
	EnvLogFormat      = "ERMITE_LOG_FORMAT"
	// EnvCommitterName and EnvCommitterEmail name the committer of the
	// commits ermite creates (squash merges, ADR 0022).
	EnvCommitterName  = "ERMITE_COMMITTER_NAME"
	EnvCommitterEmail = "ERMITE_COMMITTER_EMAIL"
	// EnvOwner is the owner login CI shows in the github context
	// ("<owner>/<repo>"); it follows the rules of repository names.
	EnvOwner = "ERMITE_OWNER"
	// EnvCILogRetention is how long CI runs, jobs, checks and logs are kept.
	EnvCILogRetention = "ERMITE_CI_LOG_RETENTION"
)

// Defaults.
const (
	DefaultAddr      = ":7341"
	DefaultGit       = "git"
	DefaultGitReader = GitReaderErmiteGit
	DefaultLogLevel  = "info"
	DefaultLogFormat = "json"
	// DefaultCommitterName is the committer name of the commits ermite creates.
	DefaultCommitterName = "ermite"
	// DefaultOwner is the owner login when ERMITE_OWNER is not set.
	DefaultOwner = "owner"
	// DefaultCIRetention is the retention of CI data.
	DefaultCIRetention = 90 * 24 * time.Hour
)

// Values of ERMITE_GIT_READER.
const (
	// GitReaderCLI reads repositories with the git binary only.
	GitReaderCLI = "cli"
	// GitReaderErmiteGit reads them with the ermite-git library where it is
	// proven faster, and with the git binary elsewhere.
	GitReaderErmiteGit = "ermite-git"
)

// MinAdminTokenLen is the shortest accepted admin token, in characters.
const MinAdminTokenLen = 32

// Config is the validated server configuration.
type Config struct {
	// Addr is the listen address, "host:port".
	Addr string
	// DataDir is the absolute, clean data root.
	DataDir string
	// PublicURL is the base URL without a trailing slash.
	PublicURL string
	// AdminTokenHash is the SHA-256 of the admin token. The token itself is
	// dropped; compare with a constant-time function.
	AdminTokenHash [sha256.Size]byte
	// TrustedProxies are the networks allowed to set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// Git is the git executable, a name or a path.
	Git string
	// GitReader is GitReaderCLI or GitReaderErmiteGit.
	GitReader string
	// SourceURL is the link to the source of the running version.
	SourceURL string
	// LogLevel is one of trace, debug, info, warn, error.
	LogLevel string
	// LogFormat is json or console.
	LogFormat string
	// CommitterName and CommitterEmail are recorded as the committer of the
	// commits ermite creates. The email defaults to "ermite@<host of
	// PublicURL>".
	CommitterName, CommitterEmail string
	// Owner is the owner login of the github context of CI runs.
	Owner string
	// CIRetention is how long CI data is kept.
	CIRetention time.Duration
}

// Error lists every configuration problem found, in variable order.
type Error struct {
	Problems []string
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Load reads the configuration through lookup, which returns the value of a
// variable, or "" when it is unset (os.Getenv fits). An empty value counts as
// unset. All problems are reported together in an *Error. Messages name the
// variable and never contain the admin token.
func Load(get func(string) string) (Config, error) {
	var problems []string
	bad := func(key, format string, args ...any) {
		problems = append(problems, key+": "+fmt.Sprintf(format, args...))
	}
	var c Config

	c.Addr = get(EnvAddr)
	if c.Addr == "" {
		c.Addr = DefaultAddr
	} else if err := validateAddr(c.Addr); err != nil {
		bad(EnvAddr, "%v", err)
	}

	switch dir := get(EnvDataDir); {
	case dir == "":
		bad(EnvDataDir, "required")
	case !filepath.IsAbs(dir):
		bad(EnvDataDir, "must be an absolute path")
	default:
		c.DataDir = filepath.Clean(dir)
	}

	publicOK := false
	if raw := get(EnvPublicURL); raw == "" {
		bad(EnvPublicURL, "required")
	} else if u, err := parseURL(raw, false); err != nil {
		bad(EnvPublicURL, "%v", err)
	} else {
		c.PublicURL = u
		publicOK = true
	}

	if token := get(EnvAdminToken); token == "" {
		bad(EnvAdminToken, "required")
	} else if n := utf8.RuneCountInString(token); n < MinAdminTokenLen {
		bad(EnvAdminToken, "must be at least %d characters (got %d)", MinAdminTokenLen, n)
	} else {
		c.AdminTokenHash = sha256.Sum256([]byte(token))
	}

	if raw := get(EnvTrustedProxies); raw != "" {
		prefixes, err := parseCIDRs(raw)
		if err != nil {
			bad(EnvTrustedProxies, "%v", err)
		}
		c.TrustedProxies = prefixes
	}

	c.Git = get(EnvGit)
	if c.Git == "" {
		c.Git = DefaultGit
	}

	c.GitReader = strings.ToLower(strings.TrimSpace(get(EnvGitReader)))
	switch c.GitReader {
	case "":
		c.GitReader = DefaultGitReader
	case GitReaderCLI, GitReaderErmiteGit:
	default:
		bad(EnvGitReader, "must be %s or %s (got %q)", GitReaderCLI, GitReaderErmiteGit, get(EnvGitReader))
	}

	switch raw := get(EnvSourceURL); {
	case raw != "":
		if u, err := parseURL(raw, true); err != nil {
			bad(EnvSourceURL, "%v", err)
		} else {
			c.SourceURL = u
		}
	case publicOK:
		c.SourceURL = c.PublicURL + "/ermite"
	}

	c.LogLevel = strings.ToLower(get(EnvLogLevel))
	switch c.LogLevel {
	case "":
		c.LogLevel = DefaultLogLevel
	case "trace", "debug", "info", "warn", "error":
	default:
		bad(EnvLogLevel, "must be one of trace, debug, info, warn, error (got %q)", get(EnvLogLevel))
	}

	c.LogFormat = strings.ToLower(get(EnvLogFormat))
	switch c.LogFormat {
	case "":
		c.LogFormat = DefaultLogFormat
	case "json", "console":
	default:
		bad(EnvLogFormat, "must be json or console (got %q)", get(EnvLogFormat))
	}

	c.CommitterName = strings.TrimSpace(get(EnvCommitterName))
	if c.CommitterName == "" {
		c.CommitterName = DefaultCommitterName
	} else if err := validateIdentity(c.CommitterName); err != nil {
		bad(EnvCommitterName, "%v", err)
	}
	c.CommitterEmail = strings.TrimSpace(get(EnvCommitterEmail))
	switch {
	case c.CommitterEmail != "":
		if err := validateIdentity(c.CommitterEmail); err != nil {
			bad(EnvCommitterEmail, "%v", err)
		} else if !strings.Contains(c.CommitterEmail, "@") || strings.ContainsAny(c.CommitterEmail, " \t") {
			bad(EnvCommitterEmail, "must be an email address such as ermite@example.org")
		}
	case publicOK:
		u, _ := url.Parse(c.PublicURL)
		c.CommitterEmail = "ermite@" + u.Hostname()
	}

	c.Owner = strings.TrimSpace(get(EnvOwner))
	if c.Owner == "" {
		c.Owner = DefaultOwner
	} else if err := validateOwner(c.Owner); err != nil {
		bad(EnvOwner, "%v", err)
	}

	c.CIRetention = DefaultCIRetention
	if raw := strings.TrimSpace(get(EnvCILogRetention)); raw != "" {
		d, err := parseRetention(raw)
		if err != nil {
			bad(EnvCILogRetention, "%v", err)
		} else {
			c.CIRetention = d
		}
	}

	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return c, nil
}

// validateOwner applies the rules of repository names (lowercase letters,
// digits, '.', '_' and '-', at most 100 characters, no leading or trailing
// '.'), which config cannot import from the domain.
func validateOwner(s string) error {
	if len(s) > 100 {
		return fmt.Errorf("must be at most 100 characters")
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return fmt.Errorf("must follow the rules of repository names: a-z, 0-9, '.', '_', '-' (got %q)", s)
		}
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return fmt.Errorf("must not start or end with '.' nor contain '..' (got %q)", s)
	}
	return nil
}

// parseRetention reads a duration in days ("90d") or in Go's syntax ("720h"),
// at least one hour.
func parseRetention(raw string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > 36500 {
			return 0, fmt.Errorf("must be a number of days such as 90d (got %q)", raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return 0, fmt.Errorf("must be a duration such as 90d or 720h (got %q)", raw)
		}
	}
	if d < time.Hour {
		return 0, fmt.Errorf("must be at least one hour (got %q)", raw)
	}
	return d, nil
}

// validateIdentity accepts a name or an email address for a Git identity: valid
// UTF-8 without control characters or the angle brackets and line breaks that
// Git does not allow in an identity.
func validateIdentity(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return fmt.Errorf("must not contain control characters, '<' or '>'")
		}
	}
	return nil
}

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port, such as %q (%v)", DefaultAddr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be a number from 1 to 65535 (got %q)", port)
	}
	return nil
}

// parseURL accepts an http(s) URL with a host and no credentials, query or
// fragment, and returns it without a trailing slash. A path is accepted only
// with allowPath.
func parseURL(raw string, allowPath bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %v", err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	case u.Host == "" || u.Hostname() == "":
		return "", fmt.Errorf("host is missing")
	case u.User != nil:
		return "", fmt.Errorf("must not contain credentials")
	case !allowPath && u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("must not contain a path (got %q)", u.Path)
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("must not contain a query")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("must not contain a fragment")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func parseCIDRs(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty entry in the comma-separated list")
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR such as 10.0.0.0/8 or fd00::/8", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
