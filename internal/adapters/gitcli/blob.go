// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"bnema.dev/ermite/internal/domain"
)

// smallBlob is the largest blob read whole, through the long-lived cat-file
// process. A larger one streams from a process of its own, so that a slow
// reader never holds the shared process.
const smallBlob = 256 << 10

// OpenBlob describes the file at path and returns its content as a stream.
//
// A blob up to 256 KiB is read through the repository's long-lived cat-file
// process. A larger one gets a `git cat-file blob` process that lives until
// the reader is closed, or ctx ends, or the adapter is closed; the memory
// used is a few buffers, whatever the size. The reader fails with
// io.ErrUnexpectedEOF if git stops before the announced size.
//
//hexcheck:ignore suspicious-business-logic-in-adapter builds domain values from git output; size and binary rules are the port contract
func (g *Git) OpenBlob(ctx context.Context, repo domain.RepoName, rev domain.Revision, p domain.TreePath) (domain.BlobInfo, io.ReadCloser, error) {
	dir, err := g.repoDir(repo)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	if p.IsRoot() {
		return domain.BlobInfo{}, nil, fmt.Errorf("blob of %q: %w", repo, domain.ErrNotFile)
	}
	var (
		h    catInfo
		data []byte
	)
	err = g.cat.do(ctx, dir, func(c *catConn) error {
		h, data = catInfo{}, nil
		res, err := resolve(c, rev)
		if err != nil {
			return err
		}
		h, err = c.info(res.CommitID.String() + ":" + p.String())
		if notFound(err) {
			return fmt.Errorf("path %q: %w", p, domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if h.Type != "blob" || h.Size > smallBlob {
			return nil
		}
		_, data, err = c.contents(h.ID, smallBlob)
		return err
	})
	if err != nil {
		return domain.BlobInfo{}, nil, fmt.Errorf("blob %q of %q: %w", p, repo, err)
	}
	if h.Type != "blob" {
		return domain.BlobInfo{}, nil, fmt.Errorf("blob %q of %q: %w", p, repo, domain.ErrNotFile)
	}
	id, err := domain.ParseObjectID(h.ID)
	if err != nil {
		return domain.BlobInfo{}, nil, err
	}
	info := domain.BlobInfo{ID: id, Path: p.String(), Size: h.Size, Mode: h.Mode}

	if data != nil || h.Size == 0 {
		info.SniffContent(data)
		return info, io.NopCloser(bytes.NewReader(data)), nil
	}

	s, err := g.startBlobStream(ctx, dir, id, h.Size)
	if err != nil {
		return domain.BlobInfo{}, nil, fmt.Errorf("blob %q of %q: %w", p, repo, err)
	}
	head, err := s.r.Peek(min(int(h.Size), domain.BinarySniffLen))
	if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
		_ = s.Close()
		return domain.BlobInfo{}, nil, fmt.Errorf("blob %q of %q: %w", p, repo, ctxOr(ctx, err))
	}
	info.SniffContent(head)
	return info, s, nil
}

func ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// blobStream is the content of a blob read from its own git process.
type blobStream struct {
	pool *catPool
	cmd  *exec.Cmd
	pipe *os.File
	r    *bufio.Reader
	size int64
	read int64
	stop func() bool // detaches the ctx watcher

	done    chan struct{} // closed when the process has been collected
	once    sync.Once
	release func() // gives the stream slot back
}

// startBlobStream starts the process of a blob stream. The process is counted
// against Config.MaxBlobStreams from its start until the stream is closed (or
// the process is collected); there is no queue: a full table is refused with
// domain.ErrResourceExhausted.
func (g *Git) startBlobStream(ctx context.Context, dir string, id domain.ObjectID, size int64) (*blobStream, error) {
	select {
	case g.blobs <- struct{}{}:
	default:
		return nil, fmt.Errorf("too many blob streams open: %w", domain.ErrResourceExhausted)
	}
	release := func() { <-g.blobs }
	pr, pw, err := os.Pipe()
	if err != nil {
		release()
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}
	cmd := exec.Command(g.bin, readArgs(dir, "cat-file", "blob", id.String())...)
	cmd.Env = g.env
	cmd.Dir = g.dataDir
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		release()
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}
	_ = pw.Close()
	s := &blobStream{
		pool: g.cat, cmd: cmd, pipe: pr, r: bufio.NewReaderSize(pr, 64*1024),
		size: size, done: make(chan struct{}), release: release,
	}
	go s.reap()
	if err := g.cat.addStream(s); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.stop = context.AfterFunc(ctx, s.kill)
	return s, nil
}

// reap collects the process and, with it, the stream slot: a process that is
// gone no longer counts, whether or not the reader was closed.
func (s *blobStream) reap() {
	_ = s.cmd.Wait()
	s.release()
	close(s.done)
}

func (s *blobStream) kill() { _ = s.cmd.Process.Kill() }

// Read implements io.Reader.
func (s *blobStream) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.read += int64(n)
	if errors.Is(err, io.EOF) && s.read < s.size {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// Close stops the process, waits for it and releases the pipe. It is safe to
// call more than once.
func (s *blobStream) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
		s.kill()
		<-s.done
		_ = s.pipe.Close()
		s.pool.removeStream(s)
	})
	return nil
}
