// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// archiveTimeout bounds one archive when ctx has no deadline: a large
// repository takes minutes, a stalled client must not hold a process forever.
const archiveTimeout = 15 * time.Minute

var _ ports.GitArchive = (*Git)(nil)

// Archive streams the tree of the commit as a gzip-compressed tar, every path
// below prefix ("<prefix>/..."), with `git archive`. The tar is compressed here,
// not by a gzip binary. A client that goes away ends ctx, which kills git.
// Nothing is written to w when the commit does not exist
// (domain.ErrNotFound).
func (g *Git) Archive(ctx context.Context, repo domain.RepoName, commit domain.ObjectID, prefix string, w io.Writer) error {
	dir, err := g.repoDir(repo)
	if err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, archiveTimeout)
		defer cancel()
	}
	release, err := g.acquireExec(ctx)
	if err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	defer release()

	cmd := exec.CommandContext(ctx, g.bin, readArgs(dir, "archive", "--format=tar", "--prefix="+prefix+"/", commit.String(), "--")...)
	cmd.Env = g.env
	cmd.Dir = g.dataDir
	cmd.WaitDelay = 5 * time.Second
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	// The first byte decides whether git found the commit: wait for it before
	// writing to w, so that a missing commit can still be answered.
	first := make([]byte, 1)
	if n, _ := io.ReadFull(stdout, first); n == 0 {
		werr := cmd.Wait()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case werr != nil:
			return fmt.Errorf("git archive %s %s: %w: %s", repo, commit, domain.ErrNotFound, strings.TrimSpace(stderr.String()))
		}
		return errors.New("git archive: empty output")
	}
	zw := gzip.NewWriter(w)
	_, cerr := io.Copy(zw, io.MultiReader(bytes.NewReader(first), stdout))
	if cerr == nil {
		cerr = zw.Close()
	}
	if cerr != nil {
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case cerr != nil:
		return fmt.Errorf("git archive %s: %w", repo, cerr)
	case werr != nil:
		return fmt.Errorf("git archive %s: %w: %s", repo, werr, strings.TrimSpace(stderr.String()))
	}
	return nil
}
