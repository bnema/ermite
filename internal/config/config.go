// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config parses and validates the server configuration from the
// environment. It is an entrypoint helper: only cmd imports it, and it never
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
	EnvSourceURL      = "ERMITE_SOURCE_URL"
	EnvLogLevel       = "ERMITE_LOG_LEVEL"
	EnvLogFormat      = "ERMITE_LOG_FORMAT"
)

// Defaults.
const (
	DefaultAddr      = ":7341"
	DefaultGit       = "git"
	DefaultLogLevel  = "info"
	DefaultLogFormat = "json"
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
	// SourceURL is the link to the source of the running version.
	SourceURL string
	// LogLevel is one of trace, debug, info, warn, error.
	LogLevel string
	// LogFormat is json or console.
	LogFormat string
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
// variable and whether it is set (os.LookupEnv fits). An empty value counts as
// unset. All problems are reported together in an *Error. Messages name the
// variable and never contain the admin token.
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key string) string {
		v, _ := lookup(key)
		return v
	}
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

	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return c, nil
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
