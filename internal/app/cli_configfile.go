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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/kvconf"
	"github.com/spf13/cobra"
	"golang.org/x/term"

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
	trust = checkPrivate(path, info, os.Geteuid())
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

// urlSource says where the server address in use comes from, for error messages.
func (e *cliEnv) urlSource() string {
	if strings.TrimSpace(e.getenv(client.EnvURL)) != "" {
		return client.EnvURL
	}
	return fmt.Sprintf("url in %s (fix it with `ermite config set url <url>`)", e.filePath)
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
	out, err := e.runTokenCmd(ctx, line)
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

// runTokenShell is the default runTokenCmd: the system shell, for at most
// tokenCmdTimeout.
func runTokenShell(ctx context.Context, line string) (string, error) {
	return runShell(ctx, line, tokenCmdTimeout)
}

// runShell runs line with the system shell. Standard error is the terminal's,
// and so is standard input when it is a terminal, so a pinentry or a biometric
// prompt works; a script or an agent gets no input. Standard output is
// captured, up to tokenCmdMaxOutput bytes. The environment is the process's.
// An interrupt or SIGTERM sent to ermite, or the end of ctx, stops the command
// like a timeout does (see shellCommand for what is killed).
func runShell(ctx context.Context, line string, timeout time.Duration) (string, error) {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithTimeout(sigCtx, timeout)
	defer cancel()

	foreground := term.IsTerminal(int(os.Stdin.Fd()))
	c := shellCommand(runCtx, line, foreground)
	out := &cappedBuffer{max: tokenCmdMaxOutput}
	c.Stdout, c.Stderr = out, os.Stderr
	if foreground {
		c.Stdin = os.Stdin
	} // else nil: the command reads os.DevNull
	// Do not wait forever for a background process that kept the pipe open.
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	if err != nil {
		switch {
		case sigCtx.Err() != nil:
			return "", errors.New("interrupted")
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("timed out after %s", timeout)
		}
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			return "", errors.New(exit.String())
		}
		return "", err
	}
	if out.over {
		return "", fmt.Errorf("printed more than %d bytes", tokenCmdMaxOutput)
	}
	return out.buf.String(), nil
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
