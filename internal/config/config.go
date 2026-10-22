// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config parses and validates the server configuration from the
// environment. It is a composition-root helper: only internal/app imports it, and it never
// reads the process environment itself, the caller passes a lookup function.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"bnema.dev/ermite/internal/mediapolicy"
)

// Variable names.
const (
	EnvVideoHosts     = "ERMITE_VIDEO_HOSTS"
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
	// EnvCIPollTimeout is how long FetchJob holds a runner's call. Lower it
	// when a reverse proxy cuts idle responses sooner than the default 50 s.
	EnvCIPollTimeout = "ERMITE_CI_POLL_TIMEOUT"
	// EnvSecretsKey is the master key that encrypts CI secrets at rest: 32
	// random bytes in base64, provided by the deployment secret store like
	// ERMITE_ADMIN_TOKEN. Without it secrets cannot be set and jobs get none.
	EnvSecretsKey = "ERMITE_SECRETS_KEY"
	// EnvCIArtifacts turns the CI artifacts (artifacts v4) off when "0" or
	// "false"; they are on by default.
	EnvCIArtifacts = "ERMITE_CI_ARTIFACTS"
	// EnvCIArtifactRetention is the default and longest retention of an
	// artifact, EnvCIArtifactQuota the space of the artifacts of one
	// repository, EnvCIArtifactMax the size of one artifact.
	EnvCIArtifactRetention = "ERMITE_CI_ARTIFACT_RETENTION"
	EnvCIArtifactQuota     = "ERMITE_CI_ARTIFACT_QUOTA"
	EnvCIArtifactMax       = "ERMITE_CI_ARTIFACT_MAX"
	EnvS3Bucket            = "ERMITE_S3_BUCKET"
	EnvS3Endpoint          = "ERMITE_S3_ENDPOINT"
	EnvS3Region            = "ERMITE_S3_REGION"
	EnvS3AccessKeyID       = "ERMITE_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey   = "ERMITE_S3_SECRET_ACCESS_KEY"
	EnvS3ForcePathStyle    = "ERMITE_S3_FORCE_PATH_STYLE"
	EnvS3Prefix            = "ERMITE_S3_PREFIX"
	EnvReleaseAssetMax     = "ERMITE_RELEASE_ASSET_MAX"
	EnvReleaseAssetsMax    = "ERMITE_RELEASE_ASSETS_MAX"
	EnvReleaseQuota        = "ERMITE_RELEASE_QUOTA"
)

// SecretsKeyLen is the length of the secrets key in bytes (AES-256).
const SecretsKeyLen = 32

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
)

// Values of ERMITE_GIT_READER.
const (
	// GitReaderCLI reads repositories with the git binary only.
	GitReaderCLI = "cli"
	// GitReaderErmiteGit reads them with the ermite-git library where it is
	// proven faster, and with the git binary elsewhere.
	GitReaderErmiteGit = "ermite-git"
)

// Bounds of ERMITE_CI_POLL_TIMEOUT. The upper bound keeps the poll under the 60 s
// that the runner waits for one FetchJob; the lower one stops runners from
// polling in a tight loop.
const (
	MinCIPollTimeout = time.Second
	MaxCIPollTimeout = 55 * time.Second
)

// MinAdminTokenLen is the shortest accepted admin token, in characters.
const MinAdminTokenLen = 32

// Config is the validated server configuration.
type Config struct {
	// VideoHosts are exact external hosts allowed to load README videos.
	VideoHosts []string
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
	// CIRetention is how long CI data is kept; 0 (variable unset) leaves the
	// default to the CI use case.
	CIRetention time.Duration
	// CIPollTimeout is how long FetchJob holds a call; 0 (variable unset)
	// leaves the default to the CI use case.
	CIPollTimeout time.Duration
	// SecretsKey is the decoded ERMITE_SECRETS_KEY (SecretsKeyLen bytes); nil
	// when the variable is unset. It is a secret: never log it or put it in an
	// error.
	SecretsKey []byte
	// CIArtifacts is false when ERMITE_CI_ARTIFACTS turns the artifacts off.
	CIArtifacts bool
	// CIArtifactRetention, CIArtifactQuota and CIArtifactMax are the limits of
	// the artifacts; 0 (variable unset) leaves the default to the CI use case.
	CIArtifactRetention time.Duration
	CIArtifactQuota     int64
	CIArtifactMax       int64
	// S3Bucket selects S3; empty selects local disk. Credentials are secrets.
	S3Bucket, S3Endpoint, S3Region, S3AccessKeyID, S3SecretAccessKey, S3Prefix string
	S3ForcePathStyle                                                           bool
	// Zero release limits leave defaults to the release use case.
	ReleaseAssetMax, ReleaseQuota int64
	ReleaseAssetsMax              int
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
	get = resolveFiles(get, &problems)
	var c Config
	var hostErr error
	c.VideoHosts, hostErr = mediapolicy.Parse(get(EnvVideoHosts))
	if hostErr != nil {
		bad(EnvVideoHosts, "%v", hostErr)
	}

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

	// The name rules of the owner are those of repository names (the domain);
	// config imports nothing, so internal/app checks them (see loadConfig).
	c.Owner = strings.TrimSpace(get(EnvOwner))
	if c.Owner == "" {
		c.Owner = DefaultOwner
	}

	if raw := strings.TrimSpace(get(EnvCILogRetention)); raw != "" {
		d, err := parseRetention(raw)
		if err != nil {
			bad(EnvCILogRetention, "%v", err)
		} else {
			c.CIRetention = d
		}
	}

	if raw := strings.TrimSpace(get(EnvCIPollTimeout)); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			bad(EnvCIPollTimeout, "must be a duration such as 25s (got %q)", raw)
		case d < MinCIPollTimeout || d > MaxCIPollTimeout:
			bad(EnvCIPollTimeout, "must be between %v and %v (got %q)", MinCIPollTimeout, MaxCIPollTimeout, raw)
		default:
			c.CIPollTimeout = d
		}
	}

	if raw := strings.TrimSpace(get(EnvSecretsKey)); raw != "" {
		// The message never repeats the value.
		if key, ok := decodeSecretsKey(raw); !ok {
			bad(EnvSecretsKey, "must be %d random bytes in base64 (for example `head -c 32 /dev/urandom | base64`)", SecretsKeyLen)
		} else {
			c.SecretsKey = key
		}
	}

	switch raw := strings.ToLower(strings.TrimSpace(get(EnvCIArtifacts))); raw {
	case "", "1", "true":
		c.CIArtifacts = true
	case "0", "false":
	default:
		bad(EnvCIArtifacts, "must be 0, 1, false or true (got %q)", raw)
	}
	if raw := strings.TrimSpace(get(EnvCIArtifactRetention)); raw != "" {
		d, err := parseRetention(raw)
		switch {
		case err != nil:
			bad(EnvCIArtifactRetention, "%v", err)
		case d < 24*time.Hour:
			bad(EnvCIArtifactRetention, "must be at least one day (got %q)", raw)
		default:
			c.CIArtifactRetention = d
		}
	}
	for _, v := range []struct {
		env string
		dst *int64
	}{{EnvCIArtifactQuota, &c.CIArtifactQuota}, {EnvCIArtifactMax, &c.CIArtifactMax}} {
		if raw := strings.TrimSpace(get(v.env)); raw != "" {
			n, err := parseSize(raw)
			if err != nil {
				bad(v.env, "%v", err)
			} else {
				*v.dst = n
			}
		}
	}

	loadStorage(get, &c, &problems)
	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return c, nil
}

// resolveFiles resolves only the four supported secrets, once at startup.
// Neither file paths, file errors nor secret contents enter diagnostics.
func resolveFiles(get func(string) string, problems *[]string) func(string) string {
	values := make(map[string]string)
	for _, key := range []string{EnvAdminToken, EnvSecretsKey, EnvS3AccessKeyID, EnvS3SecretAccessKey} {
		value, file := get(key), get(key+"_FILE")
		if value != "" && file != "" {
			*problems = append(*problems, key+": cannot set both variable and _FILE")
		} else if file != "" {
			data, err := os.ReadFile(file)
			if err != nil {
				*problems = append(*problems, key+"_FILE: cannot read secret file")
			} else {
				value = strings.TrimRight(string(data), "\r\n")
				if value == "" {
					*problems = append(*problems, key+"_FILE: secret file is empty")
				}
			}
		}
		values[key] = value
	}
	return func(key string) string {
		if value, ok := values[key]; ok {
			return value
		}
		return get(key)
	}
}

func loadStorage(get func(string) string, c *Config, problems *[]string) {
	bad := func(key, message string) { *problems = append(*problems, key+": "+message) }
	c.S3Bucket = get(EnvS3Bucket)
	c.S3Endpoint = get(EnvS3Endpoint)
	c.S3Region = get(EnvS3Region)
	c.S3AccessKeyID = get(EnvS3AccessKeyID)
	c.S3SecretAccessKey = get(EnvS3SecretAccessKey)
	c.S3Prefix = strings.Trim(get(EnvS3Prefix), "/")
	if c.S3Prefix != "" {
		valid := len(c.S3Prefix) <= 512
		for _, part := range strings.Split(c.S3Prefix, "/") {
			if part == "" || part == "." || part == ".." {
				valid = false
			}
		}
		for _, r := range c.S3Prefix {
			if r < 0x21 || r > 0x7e || r == '\\' {
				valid = false
			}
		}
		if !valid {
			bad(EnvS3Prefix, "must be an ASCII key prefix without empty, dot or dot-dot segments")
		}
		c.S3Prefix += "/"
	}
	if c.S3Endpoint != "" {
		if endpoint, err := parseURL(c.S3Endpoint, false); err != nil {
			bad(EnvS3Endpoint, "must be an http(s) URL without credentials, path, query or fragment")
		} else {
			c.S3Endpoint = endpoint
		}
	}
	switch get(EnvS3ForcePathStyle) {
	case "", "0", "false":
	case "1", "true":
		c.S3ForcePathStyle = true
	default:
		bad(EnvS3ForcePathStyle, "must be 0, 1, false or true")
	}
	if c.S3Bucket != "" {
		for _, v := range []struct{ key, value string }{{EnvS3Region, c.S3Region}, {EnvS3AccessKeyID, c.S3AccessKeyID}, {EnvS3SecretAccessKey, c.S3SecretAccessKey}} {
			if strings.TrimSpace(v.value) == "" {
				bad(v.key, "required when ERMITE_S3_BUCKET is set")
			}
		}
	}
	for _, v := range []struct {
		key string
		dst *int64
	}{{EnvReleaseAssetMax, &c.ReleaseAssetMax}, {EnvReleaseQuota, &c.ReleaseQuota}} {
		if raw := strings.TrimSpace(get(v.key)); raw != "" {
			n, err := parseSize(raw)
			if err != nil {
				bad(v.key, err.Error())
			} else {
				*v.dst = n
			}
		}
	}
	if raw := get(EnvReleaseAssetsMax); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			bad(EnvReleaseAssetsMax, "must be a positive integer")
		} else {
			c.ReleaseAssetsMax = n
		}
	}
}

// sizeUnits are the suffixes of parseSize, longest first.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

// parseSize reads a size in bytes: a number with an optional suffix B, KiB,
// MiB, GiB or TiB ("10GiB", "512 MiB"), from 1 MiB to 1 PiB.
func parseSize(raw string) (int64, error) {
	num, mult := raw, int64(1)
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(raw, u.suffix); ok {
			num, mult = strings.TrimSpace(n), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 1 || n > (1<<50)/mult || n*mult < 1<<20 {
		return 0, fmt.Errorf("must be a size from 1MiB to 1024TiB such as 10GiB (got %q)", raw)
	}
	return n * mult, nil
}

// decodeSecretsKey reads 32 bytes in standard or URL-safe base64, with or
// without padding.
func decodeSecretsKey(raw string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(raw); err == nil {
			return key, len(key) == SecretsKeyLen
		}
	}
	return nil, false
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
