// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mediapolicy shares the external video host policy between HTTP and Markdown.
package mediapolicy

import (
	"fmt"
	"net/url"
	"strings"
)

// Defaults includes GitHub's user-attachment redirect destination.
const Defaults = "github.com,user-images.githubusercontent.com,private-user-images.githubusercontent.com,github-production-user-asset-6210df.s3.amazonaws.com"

// Parse accepts comma-separated exact DNS hostnames, never CSP expressions or URLs.
func Parse(value string) ([]string, error) {
	if value == "" {
		value = Defaults
	}
	var hosts []string
	for _, raw := range strings.Split(value, ",") {
		host := strings.ToLower(strings.TrimSpace(raw))
		if len(host) == 0 || len(host) > 253 {
			return nil, fmt.Errorf("must contain exact DNS hostnames")
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, fmt.Errorf("must contain exact DNS hostnames")
			}
			for _, c := range label {
				if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
					return nil, fmt.Errorf("must contain exact DNS hostnames")
				}
			}
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// Hosts supplies defaults for adapters constructed without explicit configuration.
func Hosts(hosts []string) []string {
	if hosts != nil {
		return hosts
	}
	defaults, _ := Parse("")
	return defaults
}

// Allows accepts only HTTP(S), without credentials or nonstandard ports.
func Allows(u *url.URL, hosts []string) bool {
	if u == nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	if port := u.Port(); port != "" && (u.Scheme != "https" || port != "443") && (u.Scheme != "http" || port != "80") {
		return false
	}
	for _, host := range Hosts(hosts) {
		if strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

// Sources returns explicit scheme sources, matching Allows and forbidding wildcards.
func Sources(hosts []string) string {
	var sources []string
	for _, host := range Hosts(hosts) {
		sources = append(sources, "https://"+host, "http://"+host)
	}
	return strings.Join(sources, " ")
}
