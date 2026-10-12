// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bnema/kvconf"
	"github.com/spf13/cobra"

	"bnema.dev/ermite/internal/adapters/client"
)

const (
	// envConfig selects the CLI config file instead of ~/.config/ermite/config.
	envConfig = "ERMITE_CONFIG"
	// Keys of the CLI config file.
	keyURL      = "url"
	keyTokenCmd = "token.cmd"

	// tokenCmdTimeout bounds one run of token.cmd, prompts included.
	tokenCmdTimeout = 60 * time.Second
	// tokenCmdMaxOutput bounds what is read from its standard output.
	tokenCmdMaxOutput = 4 << 10
)

// cliFile is the CLI config file. It holds no secret: the token is fetched
// from a secret manager by the token.cmd command line.
type cliFile struct {
	URL      string `kv:"url"`
	TokenCmd string `kv:"token.cmd"`
}

// path is the config file: $ERMITE_CONFIG, else <config dir>/ermite/config.
func (e *cliEnv) path() (string, error) {
	if e.configPath != nil {
		return e.configPath()
	}
	if p := strings.TrimSpace(e.getenv(envConfig)); p != "" {
		return p, nil
	}
	p, err := kvconf.DefaultPath("ermite", "config")
	if err != nil {
		return "", fmt.Errorf("no home directory to find the config file: set %s", envConfig)
	}
	return p, nil
}

// readFile reads the config file, once per process. A missing file, or no
// place to look for it, gives an empty one. Problems in the file are warnings
// on stderr, printed once, and only when warn is set. The path is returned
// even when reading fails.
//
// The file is opened once: what is parsed and what checkPrivate vets (see
// fetchToken) are the same file, so token.cmd cannot come from a file that was
// swapped after the check.
func (e *cliEnv) readFile(cmd *cobra.Command, warn bool) (*cliFile, string, error) {
	e.fileOnce.Do(func() {
		p, err := e.path()
		if err != nil {
			// Reading is best effort when the location is unknown; the
			// `ermite config` commands report this error.
			e.file = &cliFile{}
			return
		}
		e.filePath = p
		f, warnings, trust, err := loadConfigFile(p)
		if err != nil {
			e.fileErr = fmt.Errorf("config: %w", err)
			return
		}
		e.fileTrust = trust
		if warn {
			for _, w := range warnings {
				warnf(cmd, "config: %s", w)
			}
		}
		e.file = f
	})
	return e.file, e.filePath, e.fileErr
}

// loadConfigFile parses the file at path (missing: empty). trust is nil when
// token.cmd may be run from this file, else the reason why not.
func loadConfigFile(path string) (f *cliFile, warnings []kvconf.Warning, trust, err error) {
	h, err := openConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &cliFile{}, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	defer h.Close() //nolint:errcheck // read only
	info, err := h.Stat()
	if err != nil {
		return nil, nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, kvconf.ErrNotRegular)
	}
	trust = checkPrivate(path, info)
	data, err := io.ReadAll(io.LimitReader(h, kvconf.DefaultMaxBytes+1))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(data) > kvconf.DefaultMaxBytes {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, kvconf.ErrTooLarge)
	}
	f = &cliFile{}
	warnings, err = kvconf.Unmarshal(data, f, kvconf.Options{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, warnings, trust, nil
}

// resolveURL is ERMITE_URL if set, else the url of the config file, else "".
func (e *cliEnv) resolveURL(cmd *cobra.Command, warn bool) (string, error) {
	if v := strings.TrimSpace(e.getenv(client.EnvURL)); v != "" {
		return v, nil
	}
	f, _, err := e.readFile(cmd, warn)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(f.URL), nil
}

// resolveToken is ERMITE_TOKEN if set, else (when runCmd is set) the output
// of token.cmd, else "" (anonymous). token.cmd runs at most once per process.
func (e *cliEnv) resolveToken(cmd *cobra.Command, warn, runCmd bool) (string, error) {
	if v := strings.TrimSpace(e.getenv(client.EnvToken)); v != "" {
		return v, nil
	}
	if !runCmd {
		return "", nil
	}
	return e.tokenFromCmd(cmd, warn)
}

// tokenFromCmd is the output of token.cmd, or "" when the file sets none or
// when ERMITE_URL points somewhere else than the url of the file: the token
// the command fetches belongs to the server of the file, and must not be sent
// to another one.
func (e *cliEnv) tokenFromCmd(cmd *cobra.Command, warn bool) (string, error) {
	f, _, err := e.readFile(cmd, warn)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(f.TokenCmd)
	if line == "" {
		return "", nil
	}
	e.tokenOnce.Do(func() {
		if !e.urlIsFromFile(f) {
			if warn {
				noticef(cmd, "%s differs from the config file url: token.cmd not run (set %s)", client.EnvURL, client.EnvToken)
			}
			return
		}
		e.token, e.tokenErr = e.fetchToken(e.ctx(cmd), line)
	})
	return e.token, e.tokenErr
}

// urlIsFromFile reports whether the server in use is the one of the config
// file: ERMITE_URL is unset, or names the same address as the file's url.
func (e *cliEnv) urlIsFromFile(f *cliFile) bool {
	envURL := strings.TrimSpace(e.getenv(client.EnvURL))
	if envURL == "" {
		return true
	}
	a, err := client.ParseURL(envURL)
	if err != nil {
		return false
	}
	b, err := client.ParseURL(strings.TrimSpace(f.URL))
	return err == nil && a.String() == b.String()
}

// fetchToken runs token.cmd. Neither its output nor its error text reach the
// caller's messages: the output is the secret.
func (e *cliEnv) fetchToken(ctx context.Context, line string) (string, error) {
	if e.fileTrust != nil {
		return "", e.fileTrust
	}
	run := e.runTokenCmd
	if run == nil {
		run = e.shell
	}
	out, err := run(ctx, line)
	if err != nil {
		return "", fmt.Errorf("token.cmd failed: %w", err)
	}
	out = strings.TrimSpace(out)
	switch {
	case out == "":
		return "", errors.New("token.cmd printed nothing")
	case strings.ContainsAny(out, "\r\n"):
		return "", errors.New("token.cmd printed more than one line")
	}
	return out, nil
}

// shell runs a token.cmd command line, with the timeout of the environment.
func (e *cliEnv) shell(ctx context.Context, line string) (string, error) {
	timeout := e.tokenTimeout
	if timeout <= 0 {
		timeout = tokenCmdTimeout
	}
	return runShell(ctx, line, timeout)
}

// runShell runs line with the system shell. Standard error is the terminal's,
// and so is standard input when it is a terminal, so a pinentry or a biometric
// prompt works; a script or an agent gets no input. Standard output is
// captured, up to tokenCmdMaxOutput bytes. The environment is the process's.
func runShell(ctx context.Context, line string, timeout time.Duration) (string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	c := shellCommand(ctx, line)
	out := &cappedBuffer{max: tokenCmdMaxOutput}
	c.Stdout, c.Stderr = out, os.Stderr
	if stdinIsTerminal() {
		c.Stdin = os.Stdin
	} // else nil: the command reads os.DevNull
	// Do not wait forever for a background process that kept the pipe open.
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	switch {
	case parent.Err() != nil:
		return "", errors.New("interrupted")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("timed out after %s", timeout)
	case err != nil:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", errors.New(exit.String())
		}
		return "", err
	case out.over:
		return "", fmt.Errorf("printed more than %d bytes", tokenCmdMaxOutput)
	}
	return out.buf.String(), nil
}

// stdinIsTerminal reports whether standard input is a terminal: a character
// device other than the null device.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// cappedBuffer keeps the first max bytes written and notes an overflow.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p) // report everything as written: a short write would abort the copy
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}

var _ io.Writer = (*cappedBuffer)(nil)
