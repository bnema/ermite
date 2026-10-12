// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
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
// on stderr, printed once, and only when warn is set.
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
		f, warnings, err := kvconf.Load[cliFile](p, kvconf.Options{})
		if err != nil {
			e.fileErr = fmt.Errorf("config: %w", err)
			return
		}
		if warn {
			for _, w := range warnings {
				warnf(cmd, "config: %s", w)
			}
		}
		e.file = f
	})
	return e.file, e.filePath, e.fileErr
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

// tokenFromCmd is the output of token.cmd, or "" when the file sets none.
func (e *cliEnv) tokenFromCmd(cmd *cobra.Command, warn bool) (string, error) {
	f, path, err := e.readFile(cmd, warn)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(f.TokenCmd)
	if line == "" {
		return "", nil
	}
	e.tokenOnce.Do(func() { e.token, e.tokenErr = e.fetchToken(e.ctx(cmd), path, line) })
	return e.token, e.tokenErr
}

// fetchToken runs token.cmd. Neither its output nor its error text reach the
// caller's messages: the output is the secret.
func (e *cliEnv) fetchToken(ctx context.Context, path, line string) (string, error) {
	if err := requirePrivate(path); err != nil {
		return "", err
	}
	run := e.runTokenCmd
	if run == nil {
		run = runShell
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

// requirePrivate refuses a config file other users can write: they could
// change token.cmd into any command. Windows has no such mode bits.
func requirePrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file %s is writable by others: token.cmd not run", path)
	}
	return nil
}

// runShell runs line with the system shell. Standard input and error are the
// terminal's, so a pinentry or a biometric prompt works; standard output is
// captured, up to tokenCmdMaxOutput bytes. The environment is the process's.
func runShell(ctx context.Context, line string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenCmdTimeout)
	defer cancel()
	c := shellCommand(ctx, line)
	out := &cappedBuffer{max: tokenCmdMaxOutput}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	// Do not wait forever for a background process that kept the pipe open.
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("timed out after %s", tokenCmdTimeout)
	case err != nil:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", errors.New(exit.ProcessState.String())
		}
		return "", err
	case out.over:
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
