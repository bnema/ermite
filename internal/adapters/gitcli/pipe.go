// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// errStop is returned by a pipe consumer that has all it needs: the process is
// killed and pipe returns nil.
var errStop = errors.New("stop")

// readArgs returns the arguments of a read-only git command on the bare
// repository in dir: no replace refs, no optional locks, literal pathspecs and
// no signature verification, whatever the repository configuration says.
func readArgs(dir string, args ...string) []string {
	base := []string{
		"--no-replace-objects", "--no-optional-locks", "--literal-pathspecs",
		"-c", "log.showSignature=false",
		"--git-dir=" + dir,
	}
	return append(base, args...)
}

// pipe runs git and hands its standard output to fn. When fn returns nil the
// rest of the output is discarded and a failing exit status is an error; when
// fn returns errStop the process is killed and pipe returns nil; any other
// error kills the process and is returned. Without a deadline in ctx the
// process gets commandTimeout.
func (g *Git) pipe(ctx context.Context, args []string, fn func(*bufio.Reader) error) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	base := ctx
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.bin, args...)
	cmd.Env = g.env
	cmd.Dir = g.dataDir
	cmd.WaitDelay = 5 * time.Second
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	r := bufio.NewReaderSize(stdout, 64*1024)
	ferr := fn(r)
	if ferr == nil {
		_, ferr = io.Copy(io.Discard, r)
	}
	if ferr != nil {
		cancel()
	}
	werr := cmd.Wait()

	switch {
	case errors.Is(ferr, errStop):
		return nil
	case base.Err() != nil:
		return fmt.Errorf("git %s: %w", subcommand(args), base.Err())
	case ferr != nil:
		return ferr
	case werr != nil:
		if msg := stderr.String(); msg != "" {
			return fmt.Errorf("git %s: %w: %s", subcommand(args), werr, msg)
		}
		return fmt.Errorf("git %s: %w", subcommand(args), werr)
	}
	return nil
}

// subcommand returns the git subcommand of args for error messages: the first
// argument that is neither an option nor the value of -c or -C.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return "?"
}

// readToken reads up to the next NUL and returns the text before it.
func readToken(r *bufio.Reader) (string, error) {
	s, err := r.ReadString(0)
	if err != nil {
		if errors.Is(err, io.EOF) && s != "" {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	return s[:len(s)-1], nil
}

// lineReader reads text lines, keeping at most max bytes of each: the rest of
// a longer line is dropped, so a pathological line cannot use unbounded memory.
type lineReader struct {
	r   *bufio.Reader
	max int
	buf []byte
}

func newLineReader(r *bufio.Reader, max int) *lineReader {
	return &lineReader{r: r, max: max}
}

// next returns the next line without its "\n", or io.EOF. cut is true when
// the line was longer than max and the end of it was dropped. The slice is
// valid until the next call.
func (l *lineReader) next() (line []byte, cut bool, err error) {
	l.buf = l.buf[:0]
	got := false
	for {
		chunk, err := l.r.ReadSlice('\n')
		if len(chunk) > 0 {
			got = true
		}
		data := chunk
		if err == nil {
			data = chunk[:len(chunk)-1]
		}
		room := l.max - len(l.buf)
		if len(data) > room {
			cut = true
		}
		if room > 0 {
			l.buf = append(l.buf, data[:min(room, len(data))]...)
		}
		switch {
		case err == nil:
			return l.buf, cut, nil
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			if !got {
				return nil, false, io.EOF
			}
			return l.buf, cut, nil
		default:
			return nil, false, err
		}
	}
}
