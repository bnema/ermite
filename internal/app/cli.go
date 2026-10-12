// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"bnema.dev/ermite/internal/adapters/client"
	"bnema.dev/ermite/internal/config"
)

// cliEnv is what the remote commands read from the outside: the environment,
// the clock and the build version. Tests replace it.
type cliEnv struct {
	getenv  func(string) string
	now     func() time.Time
	version string // bare build version, for example "v0.1.0" or "(devel)"
	// backoff and sleep tune the reconnections of watch; zero values mean the
	// defaults.
	backoff client.Backoff
	sleep   func(context.Context, time.Duration) error

	// configPath returns the CLI config file path; nil means the default
	// (ERMITE_CONFIG, else ~/.config/ermite/config).
	configPath func() (string, error)
	// runTokenCmd runs the token.cmd command line and returns its raw standard
	// output; nil means running it with the system shell. Tests replace it.
	runTokenCmd func(ctx context.Context, cmd string) (string, error)

	// The config file is read at most once and token.cmd run at most once per
	// process, and only when a command needs them (see cli_configfile.go).
	fileOnce  sync.Once
	file      *cliFile
	filePath  string
	fileErr   error
	tokenOnce sync.Once
	token     string
	tokenErr  error
}

func defaultCLIEnv() *cliEnv {
	return &cliEnv{getenv: os.Getenv, now: time.Now, version: buildVersion()}
}

// userAgent identifies this CLI to the server.
func (e *cliEnv) userAgent() string {
	return "ermite/" + strings.Trim(e.version, "()") + " (cli)"
}

// ctx is the command context carrying a logger when ERMITE_LOG_LEVEL is set:
// the remote commands log nothing by default (their output is the product).
func (e *cliEnv) ctx(cmd *cobra.Command) context.Context {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if lvl := e.getenv(config.EnvLogLevel); lvl != "" {
		ctx = zerowrap.WithCtx(ctx, zerowrap.New(zerowrap.Config{Level: lvl, Format: "console", Output: cmd.ErrOrStderr()}))
	}
	return ctx
}

// config builds the client configuration: ERMITE_URL and ERMITE_TOKEN win,
// then the config file (url, and the output of token.cmd for the token).
func (e *cliEnv) config(cmd *cobra.Command, warn bool) (client.Config, error) {
	return e.buildConfig(cmd, warn, true)
}

// buildConfig is config; runTokenCmd false leaves token.cmd alone (ERMITE_TOKEN
// is still used), for the calls that need no authentication.
func (e *cliEnv) buildConfig(cmd *cobra.Command, warn, runTokenCmd bool) (client.Config, error) {
	rawURL, err := e.resolveURL(cmd, warn)
	if err != nil {
		return client.Config{}, err
	}
	// A missing or invalid URL is reported before token.cmd gets to prompt.
	if _, err := client.NewConfig(rawURL, ""); err != nil {
		return client.Config{}, err
	}
	token, err := e.resolveToken(cmd, warn, runTokenCmd)
	if err != nil {
		return client.Config{}, err
	}
	cfg, err := client.NewConfig(rawURL, token)
	if err != nil {
		return cfg, err
	}
	cfg.UserAgent = e.userAgent()
	return cfg, nil
}

// client builds the API client. Commands warn when the token would travel in
// the clear; completion stays silent.
func (e *cliEnv) client(cmd *cobra.Command, warn bool) (*client.Client, error) {
	cfg, err := e.config(cmd, warn)
	if err != nil {
		return nil, err
	}
	return newClient(cmd, cfg, warn)
}

// clientNoTokenCmd is client without running token.cmd, for GetVersion, which
// is anonymous.
func (e *cliEnv) clientNoTokenCmd(cmd *cobra.Command, warn bool) (*client.Client, error) {
	cfg, err := e.buildConfig(cmd, warn, false)
	if err != nil {
		return nil, err
	}
	return newClient(cmd, cfg, warn)
}

func newClient(cmd *cobra.Command, cfg client.Config, warn bool) (*client.Client, error) {
	if w := cfg.InsecureTokenWarning(); w != "" && warn {
		warnf(cmd, "%s", w)
	}
	return client.New(cfg)
}

// warnf prints "ermite: warning: …" on stderr.
func warnf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ermite: warning: %s\n", fmt.Sprintf(format, args...))
}

// noticef prints "ermite: …" on stderr.
func noticef(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ermite: %s\n", fmt.Sprintf(format, args...))
}

// addJSONFlag registers --json and returns its value.
func addJSONFlag(cmd *cobra.Command) *bool {
	return cmd.Flags().Bool("json", false, "print machine-readable JSON (fields are listed in --help)")
}

// writeNDJSON prints v as one compact JSON line.
func writeNDJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// writeJSON prints v as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// clean makes a server-provided string safe for a terminal.
func clean(s string) string { return client.Clean(s) }

// collect reads every page: fetch gets the page token and returns the items
// and the next page token ("" at the end).
func collect[T any](ctx context.Context, fetch func(token string) ([]T, string, error)) ([]T, error) {
	var all []T
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items, next, err := fetch(token)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		token = next
	}
}

// complete builds a shell completion: it opens a client (silently giving up
// on any failure), lists with a 3 s timeout and keeps the entries whose
// first element starts with the prefix.
func complete[T any](env *cliEnv, cmd *cobra.Command, prefix string,
	list func(context.Context, *client.Client) ([]T, error), entry func(T) (key, text string),
) []string {
	c, err := env.client(cmd, false)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(env.ctx(cmd), 3*time.Second)
	defer cancel()
	items, err := list(ctx, c)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		if k, text := entry(it); strings.HasPrefix(k, prefix) {
			out = append(out, k+"\t"+text)
		}
	}
	return out
}

// table prints aligned columns under upper-case headers.
func table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = clean(c)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// fields prints "name  value" pairs aligned on the names.
func fields(w io.Writer, pairs [][2]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range pairs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p[0], clean(p[1]))
	}
	return tw.Flush()
}

// timeFormat is how tables show instants, in UTC.
const timeFormat = "2006-01-02 15:04"

// jsonTime is an RFC 3339 UTC string, or nil (JSON null) for an unset timestamp.
func jsonTime(t *timestamppb.Timestamp) *string {
	if !t.IsValid() {
		return nil
	}
	s := t.AsTime().UTC().Format(time.RFC3339)
	return &s
}

// shortTime is a table cell for a timestamp, or fallback when unset.
func shortTime(t *timestamppb.Timestamp, fallback string) string {
	if !t.IsValid() {
		return fallback
	}
	return t.AsTime().UTC().Format(timeFormat)
}
