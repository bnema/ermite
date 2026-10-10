// SPDX-License-Identifier: AGPL-3.0-or-later

// Package client is the Connect client of the remote CLI: it turns the
// ERMITE_URL and ERMITE_TOKEN settings into authenticated service clients for
// the ermite-api contract, maps Connect errors to one-line messages and keeps
// a Watch stream alive across reconnections.
//
// It is a driving adapter: it never imports another adapter and never reads
// the process environment itself, the caller passes a lookup function.
package client

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Variable names.
const (
	// EnvURL is the base URL of the ermite server.
	EnvURL = "ERMITE_URL"
	// EnvToken is the bearer token sent with every request. Optional: without
	// it the calls are anonymous.
	EnvToken = "ERMITE_TOKEN"
)

const (
	// APIPath is where the server mounts the Connect handlers.
	APIPath = "/-/api"
	// DefaultTimeout bounds one unary call.
	DefaultTimeout = 30 * time.Second
)

// Config configures a Client.
type Config struct {
	// URL is the server address, http or https, without credentials. A path
	// prefix is kept, for a server behind a sub-path proxy.
	URL string
	// Token is the bearer token. Empty means anonymous.
	Token string
	// Timeout bounds each unary call. Streams are not bounded. Zero means
	// DefaultTimeout.
	Timeout time.Duration
	// UserAgent identifies the CLI, for example "ermite/v0.1.0 (cli)".
	UserAgent string
}

// ConfigFromEnv reads ERMITE_URL and ERMITE_TOKEN through getenv and validates
// them. Timeout and UserAgent are left for the caller to set.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	raw := strings.TrimSpace(getenv(EnvURL))
	if raw == "" {
		return Config{}, fmt.Errorf("%s is not set: set it to the address of your ermite server, for example https://git.example.com", EnvURL)
	}
	base, err := ParseURL(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvURL, err)
	}
	return Config{URL: base.String(), Token: strings.TrimSpace(getenv(EnvToken))}, nil
}

// ParseURL validates a server address: an absolute http(s) URL with a host,
// without credentials, query or fragment. The result has no trailing slash.
// Errors never echo a URL that may carry credentials.
func ParseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("not a valid URL")
	}
	switch {
	case u.User != nil:
		return nil, errors.New("must not contain credentials: use ERMITE_TOKEN for the token")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("not an http(s) URL: it must start with http:// or https://")
	case u.Hostname() == "":
		return nil, errors.New("the URL has no host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("the URL must not contain a query or a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// InsecureTokenWarning returns a warning when the token would travel over
// plain http to a host that is not on the loopback interface, and "" otherwise.
//
// It warns instead of refusing: ermite does not terminate TLS itself and is
// commonly reached over a VPN or a private network, where http is a legitimate
// deployment; a refusal would make those setups unusable. The warning keeps
// the risk visible on every call.
func (c Config) InsecureTokenWarning() string {
	if c.Token == "" {
		return ""
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "http" || isLoopback(u.Hostname()) {
		return ""
	}
	return fmt.Sprintf("%s is sent over plain http to %s: anyone on the network path can read it; use an https:// URL unless the network is trusted", EnvToken, u.Hostname())
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
