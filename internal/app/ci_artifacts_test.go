// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
	"bnema.dev/ermite/internal/adapters/blobdisk"
	"bnema.dev/ermite/internal/adapters/ciartifacts"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
	"bnema.dev/ermite/internal/usecase"
)

// CI-4 D3: the artifacts of the runs, as the runner and the owners call them.

const artifactWorkflow = `name: ci
on:
  push:
    branches: [main]
jobs:
  up:
    runs-on: ubuntu-latest
    steps:
      - run: echo up
  down:
    runs-on: ubuntu-latest
    steps:
      - run: echo down
`

// newArtifactHarness uses the production blob wiring.
func newArtifactHarness(t *testing.T, tweak func(*usecase.CIConfig)) *ciHarness {
	t.Helper()
	c := newCIHarness(t, tweak)
	c.artifactRoot = blobdisk.Dir(c.data)
	return c
}

// artifactAPI are the artifact clients of one caller.
type artifactAPI struct {
	job   ermiteconnect.JobArtifactServiceClient
	owner ermiteconnect.ArtifactServiceClient
}

func (c *ciHarness) artifacts(secret string) artifactAPI {
	hc := &http.Client{Transport: bearerTransport{secret: secret, next: http.DefaultTransport}}
	return artifactAPI{ermiteconnect.NewJobArtifactServiceClient(hc, c.url("/-/api")), ermiteconnect.NewArtifactServiceClient(hc, c.url("/-/api"))}
}

// strangerArtifacts are clients whose refused calls do not count against the
// test's client address.
func (c *ciHarness) strangerArtifacts(secret string) artifactAPI {
	hc := &http.Client{Transport: bearerTransport{secret: secret, freshClient: new(atomic.Uint32), next: http.DefaultTransport}}
	return artifactAPI{ermiteconnect.NewJobArtifactServiceClient(hc, c.url("/-/api")), ermiteconnect.NewArtifactServiceClient(hc, c.url("/-/api"))}
}

// jobArtifacts is the job side of one job.
type jobArtifacts struct {
	c   *ciHarness
	job *pb.Job
	api artifactAPI
}

func (c *ciHarness) jobArtifacts(j *pb.Job) *jobArtifacts {
	return &jobArtifacts{c: c, job: j, api: c.artifacts(j.GetJobToken())}
}

func (j *jobArtifacts) create(name string, expires *timestamppb.Timestamp) (*pb.CreateJobArtifactResponse, error) {
	res, err := j.api.job.CreateJobArtifact(j.c.ctx, connect.NewRequest(&pb.CreateJobArtifactRequest{JobId: j.job.GetJobId(), Name: name, ExpiresAt: expires}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (j *jobArtifacts) mustCreate(name string) int64 {
	j.c.t.Helper()
	res, err := j.create(name, nil)
	require.NoError(j.c.t, err)
	return res.GetArtifactId()
}

// block uploads data as one block, in messages of at most 1 MiB.
func (j *jobArtifacts) block(id int64, blockID string, data []byte) (int64, error) {
	return putBlock(j.c.ctx, j.api, j.job.GetJobId(), id, []byte(blockID), data)
}

func putBlock(ctx context.Context, api artifactAPI, jobID string, id int64, blockID, data []byte) (int64, error) {
	stream := api.job.UploadJobArtifactBlock(ctx)
	first := &pb.UploadJobArtifactBlockRequest{JobId: jobID, ArtifactId: id, BlockId: blockID}
	n := min(len(data), domain.CIArtifactChunk)
	first.Data, data = data[:n], data[n:]
	if err := stream.Send(first); err != nil {
		data = nil // the server answered early: CloseAndReceive has the error
	}
	for len(data) > 0 {
		n := min(len(data), domain.CIArtifactChunk)
		if err := stream.Send(&pb.UploadJobArtifactBlockRequest{Data: data[:n]}); err != nil {
			break // the server answered early: CloseAndReceive has the error
		}
		data = data[n:]
	}
	res, err := stream.CloseAndReceive()
	if err != nil {
		return 0, err
	}
	return res.Msg.GetSize(), nil
}

func (j *jobArtifacts) commit(id int64, blocks ...string) (*pb.CommitJobArtifactResponse, error) {
	ids := make([][]byte, 0, len(blocks))
	for _, b := range blocks {
		ids = append(ids, []byte(b))
	}
	res, err := j.api.job.CommitJobArtifact(j.c.ctx, connect.NewRequest(&pb.CommitJobArtifactRequest{JobId: j.job.GetJobId(), ArtifactId: id, BlockIds: ids}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (j *jobArtifacts) finalize(name, digest string) (*pb.Artifact, error) {
	res, err := j.api.job.FinalizeJobArtifact(j.c.ctx, connect.NewRequest(&pb.FinalizeJobArtifactRequest{JobId: j.job.GetJobId(), Name: name, Digest: digest}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetArtifact(), nil
}

func (j *jobArtifacts) list(name string) ([]*pb.Artifact, error) {
	res, err := j.api.job.ListJobArtifacts(j.c.ctx, connect.NewRequest(&pb.ListJobArtifactsRequest{JobId: j.job.GetJobId(), Name: name}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetArtifacts(), nil
}

func (j *jobArtifacts) download(id, offset int64) (*pb.Artifact, []byte, error) {
	stream, err := j.api.job.DownloadJobArtifact(j.c.ctx, connect.NewRequest(&pb.DownloadJobArtifactRequest{JobId: j.job.GetJobId(), ArtifactId: id, Offset: offset}))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = stream.Close() }()
	var a *pb.Artifact
	var buf bytes.Buffer
	for stream.Receive() {
		if m := stream.Msg(); m.GetArtifact() != nil {
			a = m.GetArtifact()
		}
		buf.Write(stream.Msg().GetChunk())
	}
	return a, buf.Bytes(), stream.Err()
}

func (j *jobArtifacts) remove(name string) error {
	_, err := j.api.job.DeleteJobArtifact(j.c.ctx, connect.NewRequest(&pb.DeleteJobArtifactRequest{JobId: j.job.GetJobId(), Name: name}))
	return err
}

// upload runs a whole upload of one block.
func (j *jobArtifacts) upload(name string, data []byte) *pb.Artifact {
	j.c.t.Helper()
	id := j.mustCreate(name)
	_, err := j.block(id, "only", data)
	require.NoError(j.c.t, err)
	_, err = j.commit(id, "only")
	require.NoError(j.c.t, err)
	a, err := j.finalize(name, "")
	require.NoError(j.c.t, err)
	return a
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

func (c *ciHarness) usage() int64 {
	c.t.Helper()
	used, _, err := c.forge.store.Artifacts().ArtifactUsage(c.ctx, c.name(c.repo))
	require.NoError(c.t, err)
	return used
}

// artifactFiles lists the new CI objects under the test blob root.
func (c *ciHarness) artifactFiles() []string {
	c.t.Helper()
	var out []string
	root := filepath.Join(c.artifactRoot, "objects", "ci")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func endedDetail(t *testing.T, err error) {
	t.Helper()
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
	var ce *connect.Error
	require.True(t, errors.As(err, &ce))
	found := false
	for _, d := range ce.Details() {
		if _, derr := d.Value(); derr == nil && strings.HasSuffix(d.Type(), "JobEnded") {
			found = true
		}
	}
	assert.True(t, found, "a JobEnded detail")
}

// newArtifactRun pushes the workflow and returns the jobs of the run, given and
// acknowledged.
func (c *ciHarness) newArtifactRun(r *fakeRunner, files map[string]string) (runID int64, up, down *pb.Job) {
	c.t.Helper()
	before := len(c.runs())
	c.push("main", files, "")
	c.waitFor("a run", func() bool { return len(c.runs()) == before+1 })
	runID = c.runs()[0].GetId()
	a, b := r.await(), r.await()
	r.heartbeat(a)
	r.heartbeat(b)
	if strings.HasSuffix(a.GetJobKey(), "down") {
		a, b = b, a
	}
	return runID, a, b
}

func TestCIArtifactsPassBetweenJobs(t *testing.T) {
	c := newArtifactHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, upJob, downJob := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up, down := c.jobArtifacts(upJob), c.jobArtifacts(downJob)

	ver, err := c.root.server.GetVersion(c.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.Contains(t, ver.Msg.GetFeatures(), "ci.artifacts")

	// Blocks of 1.5 MiB (two messages each), uploaded at once in any order.
	blocks := [][]byte{pattern(3<<19, 1), pattern(3<<19, 2), pattern(3<<19, 3), pattern(10, 4)}
	var content []byte
	for _, b := range blocks {
		content = append(content, b...)
	}
	created, err := up.create("dist", nil)
	require.NoError(t, err)
	id := created.GetArtifactId()
	retention := domain.CIRetentionDays * 24 * time.Hour
	assert.WithinDuration(t, time.Now().Add(retention), created.GetExpiresAt().AsTime(), time.Minute, "the default retention")

	t.Run("an upload in progress is invisible", func(t *testing.T) {
		got, err := down.list("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	var wg sync.WaitGroup
	for i := len(blocks) - 1; i >= 0; i-- {
		wg.Go(func() {
			n, err := up.block(id, fmt.Sprint("b", i), blocks[i])
			assert.NoError(t, err)
			assert.EqualValues(t, len(blocks[i]), n)
		})
	}
	wg.Wait()
	// A retried block replaces the first one, and is charged once.
	_, err = up.block(id, "b3", blocks[3])
	require.NoError(t, err)
	assert.EqualValues(t, len(content), c.usage(), "pending blocks are charged")

	commit, err := up.commit(id, "b0", "b1", "b2", "b3")
	require.NoError(t, err)
	assert.EqualValues(t, len(content), commit.GetSize())
	assert.Equal(t, digestOf(content), commit.GetDigest())
	again, err := up.commit(id, "b0", "b1", "b2", "b3")
	require.NoError(t, err, "a repeat returns the same result")
	assert.Equal(t, commit.GetDigest(), again.GetDigest())
	_, err = up.commit(id, "b0")
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "another list on a committed artifact")
	_, err = up.block(id, "b9", []byte("late"))
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "no block after the commit")
	assert.EqualValues(t, len(content), c.usage(), "the content replaces the charge of the blocks")
	for _, f := range c.artifactFiles() {
		assert.NotContains(t, f, ".blocks", "the blocks are dropped at the commit")
	}

	_, err = up.finalize("dist", digestOf([]byte("other")))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "a digest that differs")
	a, err := up.finalize("dist", strings.ToUpper(digestOf(content)[7:]))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "a digest without its prefix")
	require.Nil(t, a)
	a, err = up.finalize("dist", digestOf(content))
	require.NoError(t, err)
	assert.Equal(t, id, a.GetId())
	assert.Equal(t, upJob.GetJobId(), a.GetJobId())
	assert.Equal(t, domain.CIArtifactZip, a.GetMediaType())
	repeat, err := up.finalize("dist", "")
	require.NoError(t, err, "a repeat returns the same artifact")
	assert.Equal(t, id, repeat.GetId())

	t.Run("another job of the run lists and downloads it", func(t *testing.T) {
		got, err := down.list("")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "dist", got[0].GetName())
		assert.EqualValues(t, len(content), got[0].GetSize())
		meta, body, err := down.download(id, 0)
		require.NoError(t, err)
		assert.Equal(t, "dist", meta.GetName())
		assert.True(t, bytes.Equal(content, body))
		_, body, err = down.download(id, 3<<19+5)
		require.NoError(t, err, "a resumed download")
		assert.True(t, bytes.Equal(content[3<<19+5:], body))
		_, body, err = down.download(id, int64(len(content)))
		require.NoError(t, err)
		assert.Empty(t, body)
		_, _, err = down.download(id, int64(len(content))+1)
		assert.Equal(t, connect.CodeOutOfRange, connect.CodeOf(err))
	})

	t.Run("a name is unique in the attempt; overwrite deletes first", func(t *testing.T) {
		_, err := down.create("dist", nil)
		assert.Equal(t, connect.CodeAlreadyExists, connect.CodeOf(err))
		require.NoError(t, down.remove("dist"))
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(down.remove("dist")))
		assert.Zero(t, c.usage(), "deleting refunds")
		b := down.upload("dist", []byte("v2"))
		assert.Greater(t, b.GetId(), id, "ids increase, never reused")
		_, body, err := up.download(b.GetId(), 0)
		require.NoError(t, err)
		assert.Equal(t, "v2", string(body))
	})

	t.Run("names are checked, never used as paths", func(t *testing.T) {
		for _, name := range []string{"", "../x", "a/b", `a\b`, "a:b", "nul\x00", strings.Repeat("n", 257)} {
			_, err := up.create(name, nil)
			assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%q", name)
		}
	})

	t.Run("the owners: repo:read to list and download, ci:write to delete", func(t *testing.T) {
		res, err := c.artifacts(adminToken).owner.ListArtifacts(c.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: c.repo, RunId: a.GetRunId()}))
		require.NoError(t, err)
		require.Len(t, res.Msg.GetArtifacts(), 1)
		assert.EqualValues(t, 2, res.Msg.GetUsedBytes())
		assert.EqualValues(t, domain.DefaultCIArtifactQuota, res.Msg.GetQuotaBytes())

		// Anonymous: a private repository does not exist, a public one needs a
		// signed-in caller.
		anon := c.strangerArtifacts("")
		_, err = anon.owner.ListArtifacts(c.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: c.repo, RunId: a.GetRunId()}))
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
		public := domain.VisibilityPublic
		_, err = c.forge.Repos.Update(c.ctx, domain.Root(), c.name(c.repo), ports.UpdateRepoParams{Visibility: &public})
		require.NoError(t, err)
		_, err = anon.owner.ListArtifacts(c.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: c.repo, RunId: a.GetRunId()}))
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
		stream, err := anon.owner.DownloadArtifact(c.ctx, connect.NewRequest(&pb.DownloadArtifactRequest{Repo: c.repo, ArtifactId: res.Msg.GetArtifacts()[0].GetId()}))
		if err == nil {
			for stream.Receive() {
				t.Error("anonymous got content")
			}
			err = stream.Err()
		}
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

		// A token with ci:write only reads the repository (public) but not its
		// artifacts; it may delete them.
		tok, err := c.root.token.CreateToken(c.ctx, connect.NewRequest(&pb.CreateTokenRequest{Label: "ci", Scopes: []pb.Scope{pb.Scope_SCOPE_CI_WRITE}}))
		require.NoError(t, err)
		ciw := c.strangerArtifacts(tok.Msg.GetSecret())
		_, err = ciw.owner.ListArtifacts(c.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: c.repo, RunId: a.GetRunId()}))
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, err = anon.owner.DeleteArtifact(c.ctx, connect.NewRequest(&pb.DeleteArtifactRequest{Repo: c.repo, ArtifactId: res.Msg.GetArtifacts()[0].GetId()}))
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

		root := c.artifacts(adminToken)
		dl, err := root.owner.DownloadArtifact(c.ctx, connect.NewRequest(&pb.DownloadArtifactRequest{Repo: c.repo, ArtifactId: res.Msg.GetArtifacts()[0].GetId()}))
		require.NoError(t, err)
		var body []byte
		for dl.Receive() {
			body = append(body, dl.Msg().GetChunk()...)
		}
		require.NoError(t, dl.Err())
		assert.Equal(t, "v2", string(body))
		_, err = ciw.owner.DeleteArtifact(c.ctx, connect.NewRequest(&pb.DeleteArtifactRequest{Repo: c.repo, ArtifactId: res.Msg.GetArtifacts()[0].GetId()}))
		require.NoError(t, err)
		got, err := up.list("")
		require.NoError(t, err)
		assert.Empty(t, got)
		assert.Zero(t, c.usage())
	})
}

// AC3: a job token of another run reaches nothing of this run, and the calls
// are refused once the job ended.
func TestCIArtifactsAreIsolatedPerRunAndJob(t *testing.T) {
	c := newArtifactHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	runA, upA, downA := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	_, upB, _ := c.newArtifactRun(runner, map[string]string{"README": "two"})
	a, b := c.jobArtifacts(upA), c.jobArtifacts(upB)

	secret := a.upload("secret", []byte("of run A"))
	pendingID := a.mustCreate("wip")
	_, err := a.block(pendingID, "x", []byte("half"))
	require.NoError(t, err)

	t.Run("a job of run B", func(t *testing.T) {
		got, err := b.list("")
		require.NoError(t, err)
		assert.Empty(t, got, "lists its own run only")
		_, _, err = b.download(secret.GetId(), 0)
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(b.remove("secret")))
		_, err = b.block(pendingID, "y", []byte("intrusion"))
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
		_, err = b.commit(pendingID, "x")
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
		_, err = b.finalize("wip", "")
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	})

	t.Run("another job of the same run uploads nothing into the job's upload", func(t *testing.T) {
		d := c.jobArtifacts(downA)
		_, err := d.block(pendingID, "y", []byte("intrusion"))
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
		_, err = d.commit(pendingID, "x")
		assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	})

	t.Run("job ids in requests must be the token's", func(t *testing.T) {
		wrong := &jobArtifacts{c: c, job: &pb.Job{JobId: upA.GetJobId(), JobToken: upB.GetJobToken()}, api: c.strangerArtifacts(upB.GetJobToken())}
		_, err := wrong.list("")
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, _, err = wrong.download(secret.GetId(), 0)
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(wrong.remove("secret")))
		_, err = wrong.block(pendingID, "z", []byte("x"))
		assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	})

	t.Run("no token, a runner token, a repository token", func(t *testing.T) {
		for name, secret := range map[string]string{"none": "", "runner": runner.secret, "garbage": "ermitejob_nope", "admin": adminToken} {
			s := &jobArtifacts{c: c, job: upA, api: c.strangerArtifacts(secret)}
			_, err := s.list("")
			assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), name)
			_, err = s.block(pendingID, "z", []byte("x"))
			assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), name)
		}
	})

	t.Run("the end of the job discards its uploads and refuses its token", func(t *testing.T) {
		before := c.usage()
		runner.runJob(upA, pb.CiConclusion_CI_CONCLUSION_SUCCESS, nil)
		assert.EqualValues(t, before-int64(len("half")), c.usage(), "the unfinished upload is refunded")
		_, err := c.forge.store.Artifacts().GetArtifact(c.ctx, pendingID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		for _, f := range c.artifactFiles() {
			assert.NotContains(t, f, fmt.Sprintf("%020d", pendingID), "its files are gone")
		}
		_, err = a.list("")
		endedDetail(t, err)
		_, err = a.create("late", nil)
		endedDetail(t, err)
		_, _, err = a.download(secret.GetId(), 0)
		endedDetail(t, err)

		// The finalized artifact stays for the other jobs.
		got, err := c.jobArtifacts(downA).list("secret")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, runA, got[0].GetRunId())
	})
}

// shiftClock is the real time moved by an offset the test sets.
type shiftClock struct{ off atomic.Int64 }

func (s *shiftClock) Now() time.Time                  { return time.Now().Add(time.Duration(s.off.Load())) }
func (s *shiftClock) advance(d time.Duration)         { s.off.Add(int64(d)) }
func (s *shiftClock) since(t time.Time) time.Duration { return s.Now().Sub(t) }

func TestCIArtifactLimits(t *testing.T) {
	const mib = 1 << 20
	c := newArtifactHarness(t, func(cfg *usecase.CIConfig) {
		cfg.ArtifactLimits = ports.ArtifactLimits{Retention: 10 * 24 * time.Hour, Quota: 3 * mib, MaxSize: 2 * mib}
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, upJob, downJob := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up, down := c.jobArtifacts(upJob), c.jobArtifacts(downJob)

	t.Run("the retention is kept between one day and the longest", func(t *testing.T) {
		res, err := up.create("soon", timestamppb.New(time.Now().Add(time.Hour)))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(24*time.Hour), res.GetExpiresAt().AsTime(), time.Minute)
		res, err = up.create("late", timestamppb.New(time.Now().Add(1000*24*time.Hour)))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(10*24*time.Hour), res.GetExpiresAt().AsTime(), time.Minute)
		res, err = up.create("mid", timestamppb.New(time.Now().Add(5*24*time.Hour)))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(5*24*time.Hour), res.GetExpiresAt().AsTime(), time.Second)
	})

	id := up.mustCreate("big")
	t.Run("a block over the size of one artifact is refused and nothing is kept", func(t *testing.T) {
		_, err := up.block(id, "a", pattern(2*mib+1, 0))
		require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
		assert.Contains(t, err.Error(), "2 MiB")
		assert.Zero(t, c.usage())
		assert.Empty(t, c.artifactFiles())
	})

	t.Run("blocks add up against the size of one artifact", func(t *testing.T) {
		_, err := up.block(id, "a", pattern(3*mib/2, 0))
		require.NoError(t, err)
		_, err = up.block(id, "b", pattern(mib, 0))
		require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
		assert.EqualValues(t, 3*mib/2, c.usage())
		_, err = up.commit(id, "a", "a")
		require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err), "a repeated block counts twice")
		_, err = up.commit(id, "a", "nope")
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "a block never uploaded")
		_, err = up.finalize("big", "")
		assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "not committed")
	})

	t.Run("the quota of the repository counts the uploads in progress", func(t *testing.T) {
		other := down.mustCreate("other")
		_, err := down.block(other, "a", pattern(mib, 1))
		require.NoError(t, err)
		_, err = down.block(other, "b", pattern(mib, 1))
		require.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
		assert.Contains(t, err.Error(), "quota (3 MiB)")
		// A retry of a stored block within the quota replaces it.
		_, err = down.block(other, "a", pattern(mib/2, 2))
		require.NoError(t, err)
		assert.EqualValues(t, 2*mib, c.usage())
		_, err = down.block(other, "b", pattern(mib, 1))
		require.NoError(t, err)
		_, err = down.create("third", nil)
		assert.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err), "the space is used up")
	})
}

func TestCIArtifactsSweep(t *testing.T) {
	clk := &shiftClock{}
	legacyRoot := ciartifacts.Dir(t.TempDir())
	c := newArtifactHarness(t, func(cfg *usecase.CIConfig) {
		cfg.ArtifactFiles = ciartifacts.New(legacyRoot)
		cfg.Clock = clk
		cfg.Tick = 20 * time.Millisecond
		cfg.ArtifactLimits.Retention = 2 * 24 * time.Hour
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	runID, upJob, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(upJob)
	kept := up.upload("kept", []byte("content"))

	// A crash of the server between the end of the job and its cleanup: an
	// unfinished upload stays recorded with its block; a crash between a file
	// and its record: files that no record names.
	crashed := up.mustCreate("crashed")
	_, err := up.block(crashed, "x", []byte("stale"))
	require.NoError(t, err)
	stray := filepath.Join(legacyRoot, fmt.Sprintf("%020d", runID), fmt.Sprintf("%020d", int64(999999)))
	require.NoError(t, os.MkdirAll(filepath.Dir(stray), 0o700))
	require.NoError(t, os.WriteFile(stray, []byte("orphan"), 0o600))
	strayRun := filepath.Join(legacyRoot, fmt.Sprintf("%020d", int64(888888)))
	require.NoError(t, os.MkdirAll(strayRun, 0o700))
	start := clk.Now()

	// The day after: the upload is older than a day and swept, the stray files
	// go, the finalized artifact stays.
	clk.advance(25 * time.Hour)
	c.waitFor("the sweep", func() bool {
		_, err := c.forge.store.Artifacts().GetArtifact(c.ctx, crashed)
		return errors.Is(err, domain.ErrNotFound)
	})
	c.waitFor("the stray files", func() bool {
		_, err1 := os.Stat(stray)
		_, err2 := os.Stat(strayRun)
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
	assert.EqualValues(t, len("content"), c.usage(), "the unfinished upload is refunded")
	_, err = c.forge.store.Artifacts().GetArtifact(c.ctx, kept.GetId())
	require.NoError(t, err)
	for _, f := range c.artifactFiles() {
		assert.Equal(t, filepath.Join(fmt.Sprintf("%020d", runID), fmt.Sprintf("%020d", kept.GetId())), f)
	}

	// Past its expiry it is invisible at once, then swept.
	clk.advance(24 * time.Hour)
	res, err := c.artifacts(adminToken).owner.ListArtifacts(c.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: c.repo, RunId: runID}))
	require.NoError(t, err)
	assert.Empty(t, res.Msg.GetArtifacts(), "expired")
	c.waitFor("the expired artifact swept", func() bool { return c.usage() == 0 && len(c.artifactFiles()) == 0 })
	assert.Greater(t, clk.since(start), 48*time.Hour)
}

// Deleting the repository takes the records and the files of its artifacts.
func TestCIArtifactsGoWithTheRepository(t *testing.T) {
	c := newArtifactHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, upJob, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	a := c.jobArtifacts(upJob).upload("dist", []byte("x"))
	require.NotEmpty(t, c.artifactFiles())

	require.NoError(t, c.forge.Repos.Delete(c.ctx, domain.Root(), c.name(c.repo)))
	_, err := c.forge.store.Artifacts().GetArtifact(c.ctx, a.GetId())
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Empty(t, c.artifactFiles())
}

// ERMITE_CI_ARTIFACTS=0 serves neither service.
func TestCIArtifactsOff(t *testing.T) {
	h := newHarnessWith(t, harnessOpts{env: map[string]string{"ERMITE_CI_ARTIFACTS": "0"}}, nil)
	ver, err := h.api("").server.GetVersion(h.ctx, connect.NewRequest(&pb.GetVersionRequest{}))
	require.NoError(t, err)
	assert.NotContains(t, ver.Msg.GetFeatures(), "ci.artifacts")
	c := &ciHarness{harness: h}
	_, err = c.artifacts(adminToken).owner.ListArtifacts(h.ctx, connect.NewRequest(&pb.ListArtifactsRequest{Repo: "x", RunId: 1}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
	_, err = c.artifacts("ermitejob_x").job.ListJobArtifacts(h.ctx, connect.NewRequest(&pb.ListJobArtifactsRequest{JobId: "1-0-0"}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
}

// Concurrent writes never pass the limits: the bytes of a block are reserved
// before they are written, against the charges and the other writes.
func TestCIArtifactLimitsHoldUnderConcurrency(t *testing.T) {
	const kib, mib = 1 << 10, 1 << 20
	c := newArtifactHarness(t, func(cfg *usecase.CIConfig) {
		cfg.ArtifactLimits = ports.ArtifactLimits{Quota: 3 * mib, MaxSize: 2 * mib}
	})
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, upJob, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(upJob)

	blockFiles := func() (total int64) {
		for _, f := range c.artifactFiles() {
			if info, err := os.Stat(filepath.Join(c.artifactRoot, "objects", "ci", f)); err == nil {
				total += info.Size()
			}
		}
		return total
	}

	// peak samples the bytes on disk while fn runs.
	peak := func(fn func()) int64 {
		var top atomic.Int64
		done := make(chan struct{})
		go func() {
			for {
				if n := blockFiles(); n > top.Load() {
					top.Store(n)
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
		fn()
		close(done)
		return top.Load()
	}

	t.Run("one artifact: parallel blocks stop at the size of one artifact", func(t *testing.T) {
		id := up.mustCreate("one")
		var stored atomic.Int64
		var wg sync.WaitGroup
		top := peak(func() {
			for i := range 12 {
				wg.Go(func() {
					n, err := up.block(id, fmt.Sprint("b", i), pattern(512*kib, byte(i)))
					if err == nil {
						stored.Add(n)
						return
					}
					assert.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
				})
			}
			wg.Wait()
		})
		assert.LessOrEqual(t, top, int64(2*mib), "nothing beyond the limit ever reached the disk")
		assert.LessOrEqual(t, stored.Load(), int64(2*mib), "never past the size of one artifact")
		assert.Positive(t, stored.Load())
		assert.Equal(t, stored.Load(), c.usage(), "what is stored is what is charged")
		assert.Equal(t, stored.Load(), blockFiles(), "no refused block left on disk")
	})

	t.Run("several artifacts: parallel blocks stop at the quota", func(t *testing.T) {
		ids := []int64{up.mustCreate("two"), up.mustCreate("three")}
		before := c.usage()
		var stored atomic.Int64
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Go(func() {
				n, err := up.block(ids[i%2], fmt.Sprint("b", i), pattern(256*kib, byte(i)))
				if err == nil {
					stored.Add(n)
					return
				}
				assert.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
			})
		}
		wg.Wait()
		assert.LessOrEqual(t, c.usage(), int64(3*mib), "never past the quota")
		assert.Positive(t, stored.Load())
		assert.Equal(t, before+stored.Load(), c.usage())
		assert.Equal(t, c.usage(), blockFiles(), "no refused block left on disk")
	})
}

func TestCIArtifactCaps(t *testing.T) {
	c := newArtifactHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, upJob, downJob := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up, down := c.jobArtifacts(upJob), c.jobArtifacts(downJob)

	t.Run("a job has a bounded number of unfinished uploads", func(t *testing.T) {
		for i := range domain.CIArtifactMaxPendingPerJob {
			up.mustCreate(fmt.Sprint("p", i))
		}
		_, err := up.create("one-more", nil)
		assert.Equal(t, connect.CodeResourceExhausted, connect.CodeOf(err))
		_, err = up.create("p0", nil)
		require.NoError(t, err, "a new create of a pending name replaces it")
		down.mustCreate("other job")
	})

	t.Run("a commit repeated while it runs, or after its caller left, gives one result", func(t *testing.T) {
		content := pattern(6<<20, 9)
		id := down.mustCreate("big")
		_, err := down.block(id, "a", content[:3<<20])
		require.NoError(t, err)
		_, err = down.block(id, "b", content[3<<20:])
		require.NoError(t, err)

		// A caller that gives up at once: the commit still completes.
		gone, cancel := context.WithTimeout(c.ctx, time.Millisecond)
		_, _ = down.api.job.CommitJobArtifact(gone, connect.NewRequest(&pb.CommitJobArtifactRequest{JobId: downJob.GetJobId(), ArtifactId: id, BlockIds: [][]byte{[]byte("a"), []byte("b")}}))
		cancel()

		var wg sync.WaitGroup
		results := make([]*pb.CommitJobArtifactResponse, 4)
		for i := range results {
			wg.Go(func() {
				res, err := down.commit(id, "a", "b")
				assert.NoError(t, err)
				results[i] = res
			})
		}
		wg.Wait()
		for _, r := range results {
			require.NotNil(t, r)
			assert.Equal(t, digestOf(content), r.GetDigest())
			assert.EqualValues(t, len(content), r.GetSize())
		}
		_, body, err := func() (*pb.Artifact, []byte, error) {
			if _, err := down.finalize("big", ""); err != nil {
				return nil, nil, err
			}
			return up.download(id, 0)
		}()
		require.NoError(t, err)
		assert.True(t, bytes.Equal(content, body))
	})
}

// Both storage generations remain downloadable through the real RPC handlers.
func TestCIArtifactBlobAndLegacyDownload(t *testing.T) {
	// This test inserts legacy data after startup; supply its reader explicitly.
	legacyRoot := ciartifacts.Dir(t.TempDir())
	c := newArtifactHarness(t, func(cfg *usecase.CIConfig) { cfg.ArtifactFiles = ciartifacts.New(legacyRoot) })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, job, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(job)
	current := up.upload("new", []byte("new blob"))
	store := c.forge.store.Artifacts()
	a, err := store.GetArtifact(c.ctx, current.GetId())
	require.NoError(t, err)
	assert.Equal(t, domain.CIArtifactStoreBlob, a.Store)
	path := filepath.Join(c.artifactRoot, "objects", filepath.FromSlash(a.BlobKey()))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new blob", string(data))
	_, got, err := up.download(a.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Simulate an on-disk record written by the pre-upgrade server: no store
	// field and contents at the old id-only path, absent from the blob store.
	legacy := up.upload("old", []byte("old disk"))
	a, err = store.GetArtifact(c.ctx, legacy.GetId())
	require.NoError(t, err)
	oldPath := filepath.Join(legacyRoot, fmt.Sprintf("%020d", a.RunID), fmt.Sprintf("%020d", a.ID))
	require.NoError(t, os.MkdirAll(filepath.Dir(oldPath), 0o700))
	require.NoError(t, os.WriteFile(oldPath, []byte("old disk"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(c.artifactRoot, "objects", filepath.FromSlash(a.BlobKey()))))
	used, version, err := store.ArtifactUsage(c.ctx, a.Repo)
	require.NoError(t, err)
	a.Store = domain.CIArtifactStoreLegacy
	require.NoError(t, store.ApplyArtifacts(c.ctx, ports.CIArtifactWrite{Repo: a.Repo, Used: used, UsageVersion: version, Put: []domain.CIArtifact{a}}))
	_, got, err = up.download(a.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "old disk", string(got))
	require.NoError(t, up.remove("old"))
	_, err = os.Stat(oldPath)
	assert.True(t, os.IsNotExist(err))
}

func TestCIArtifactWithoutLegacyReader(t *testing.T) {
	c := newArtifactHarness(t, func(cfg *usecase.CIConfig) { cfg.ArtifactFiles = nil })
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, job, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(job)
	a := up.upload("blob only", []byte("contents"))
	_, got, err := up.download(a.GetId(), 0)
	require.NoError(t, err)
	assert.Equal(t, "contents", string(got))
	require.NoError(t, c.forge.Repos.Delete(c.ctx, domain.Root(), c.name(c.repo)))
	assert.Empty(t, c.artifactFiles())
}

func TestCIArtifactMultipartAndOversizedBlock(t *testing.T) {
	c := newArtifactHarness(t, nil)
	runner := c.registerRunner("alpha", "ubuntu-latest")
	_, job, _ := c.newArtifactRun(runner, map[string]string{".github/workflows/ci.yml": artifactWorkflow})
	up := c.jobArtifacts(job)
	id := up.mustCreate("multipart")
	first, second := pattern(6<<20, 1), pattern(3<<20, 2)
	_, err := up.block(id, "a", first)
	require.NoError(t, err)
	_, err = up.block(id, "b", second)
	require.NoError(t, err)
	committed, err := up.commit(id, "a", "b")
	require.NoError(t, err)
	_, err = up.finalize("multipart", committed.GetDigest())
	require.NoError(t, err)
	_, got, err := up.download(id, 0)
	require.NoError(t, err)
	assert.Equal(t, append(first, second...), got)
	before := c.usage()
	refused := up.mustCreate("too big")
	_, err = up.block(refused, "a", make([]byte, domain.CIArtifactMaxBlockSize+1))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	assert.Contains(t, err.Error(), "a block has at most 8 MiB")
	assert.Equal(t, before, c.usage())
	a, err := c.forge.store.Artifacts().GetArtifact(c.ctx, refused)
	require.NoError(t, err)
	assert.Zero(t, a.Charged)
	require.NoError(t, blobdisk.New(c.artifactRoot).List(c.ctx, a.BlobKey()+".blocks/", func(ports.BlobInfo) error { t.Error("refused block survived"); return nil }))
}
