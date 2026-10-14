// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Secrets, variables and environments (ADR 0019, CI-3 plan D2).

// Limits of names and values.
const (
	// MaxSettingValueBytes is the largest secret or variable: 48 KB as
	// docs.github.com writes it, taken as 48 KiB.
	MaxSettingValueBytes = 48 * 1024
	// MaxSettingNameLen is the longest name of a secret, variable or
	// environment, in characters.
	MaxSettingNameLen = 255
	// ReservedSettingPrefix starts no secret or variable name: GitHub keeps it
	// for its own.
	ReservedSettingPrefix = "GITHUB_"
)

// ErrSecretsKeyMismatch means the data was sealed with another
// ERMITE_SECRETS_KEY than the one the server runs with: it refuses to start
// rather than hand out values it cannot read or seal new ones with a key the
// old ones do not open with.
var ErrSecretsKeyMismatch = errors.New("ERMITE_SECRETS_KEY is not the key the stored secrets were sealed with")

// ParseSettingName checks the name of a secret or variable and returns it in
// upper case, the way it is stored: letters, digits and "_", not starting with
// a digit or with GITHUB_, 1 to 255 characters, case-insensitive.
func ParseSettingName(s string) (string, error) {
	if s == "" || len(s) > MaxSettingNameLen {
		return "", &TextError{Reason: "must be 1 to 255 characters"}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return "", &TextError{Reason: "may only hold letters, digits and underscores, and must not start with a digit"}
		}
	}
	up := strings.ToUpper(s)
	if strings.HasPrefix(up, ReservedSettingPrefix) {
		return "", &TextError{Reason: "must not start with " + ReservedSettingPrefix}
	}
	return up, nil
}

// CheckSecretValue checks the value of a secret: 1 byte to 48 KiB.
func CheckSecretValue(v string) error {
	switch {
	case v == "":
		return &TextError{Reason: "must not be empty"}
	case len(v) > MaxSettingValueBytes:
		return &TextError{Reason: "must be at most 49,152 bytes (48 KiB)"}
	}
	return nil
}

// CheckVariableValue checks the value of a variable: at most 48 KiB, may be
// empty.
func CheckVariableValue(v string) error {
	if len(v) > MaxSettingValueBytes {
		return &TextError{Reason: "must be at most 49,152 bytes (48 KiB)"}
	}
	return nil
}

// ParseEnvironmentName checks the name of an environment: 1 to 255 characters
// of valid UTF-8 without control characters or leading and trailing whitespace
// (the one rule for the API, the CLI and workflows: a name is never trimmed).
// Names are unique without regard to case; the case given is kept.
func ParseEnvironmentName(s string) (string, error) {
	if s == "" || utf8.RuneCountInString(s) > MaxSettingNameLen {
		return "", &TextError{Reason: "must be 1 to 255 characters"}
	}
	if !utf8.ValidString(s) {
		return "", &TextError{Reason: "must be valid UTF-8"}
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "", &TextError{Reason: "must not start or end with whitespace"}
	}
	for _, r := range s {
		if unicode.IsControl(r) || isBidiControl(r) {
			return "", &TextError{Reason: "must not hold control characters"}
		}
	}
	return s, nil
}

// SettingsScopeKind tells where secrets and variables live.
type SettingsScopeKind uint8

// Scope kinds.
const (
	// ScopeKindInstance is the whole server.
	ScopeKindInstance SettingsScopeKind = iota + 1
	// ScopeKindRepo is one repository.
	ScopeKindRepo
	// ScopeKindEnvironment is one environment of a repository.
	ScopeKindEnvironment
)

// SettingsScope says where a secret or a variable lives. Build it with
// InstanceScope, RepoScope or EnvironmentScope; the zero value is invalid.
type SettingsScope struct {
	Kind SettingsScopeKind
	Repo RepoName
	// Env is the environment name in the case it was given. Scopes compare by
	// Key, which folds the case.
	Env string
}

// InstanceScope is the scope of the whole server.
func InstanceScope() SettingsScope { return SettingsScope{Kind: ScopeKindInstance} }

// RepoScope is the scope of a repository.
func RepoScope(repo RepoName) SettingsScope { return SettingsScope{Kind: ScopeKindRepo, Repo: repo} }

// EnvironmentScope is the scope of an environment of a repository; the name
// must follow ParseEnvironmentName.
func EnvironmentScope(repo RepoName, env string) SettingsScope {
	return SettingsScope{Kind: ScopeKindEnvironment, Repo: repo, Env: env}
}

// Valid reports whether the scope is well formed.
func (s SettingsScope) Valid() bool {
	switch s.Kind {
	case ScopeKindInstance:
		return true
	case ScopeKindRepo:
		return !s.Repo.IsZero()
	case ScopeKindEnvironment:
		if s.Repo.IsZero() {
			return false
		}
		_, err := ParseEnvironmentName(s.Env)
		return err == nil
	}
	return false
}

// Key is the scope as the storage and the encryption name it: "i",
// "r/<repo>" or "e/<repo>/<environment>". The environment is lower-cased and
// escaped so that its key holds no "/" and two names that differ by case are
// one scope.
func (s SettingsScope) Key() string {
	switch s.Kind {
	case ScopeKindInstance:
		return "i"
	case ScopeKindRepo:
		return "r/" + s.Repo.String()
	case ScopeKindEnvironment:
		return "e/" + s.Repo.String() + "/" + EnvironmentKey(s.Env)
	}
	return ""
}

// EnvironmentKey is the part of a storage key that names an environment: its
// name lower-cased and path-escaped.
func EnvironmentKey(name string) string { return url.PathEscape(strings.ToLower(name)) }

// SecretAAD is the additional data a secret is sealed with. It binds the
// ciphertext to its scope and name, so a stored value moved to another secret
// does not open.
func SecretAAD(scope SettingsScope, name string) []byte {
	return []byte("ermite-secret\x00" + scope.Key() + "\x00" + name)
}

// Secret describes a stored secret; it never holds the value.
type Secret struct {
	Name      string
	UpdatedAt time.Time
}

// Variable is a configuration variable.
type Variable struct {
	Name      string
	Value     string
	UpdatedAt time.Time
}

// Environment is a named deployment target of a repository.
type Environment struct {
	Repo      RepoName
	Name      string
	CreatedAt time.Time
}
