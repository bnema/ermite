// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// A focused version of cli_fake_test's Connect rig, without changing the shared fake.
type fakeReleaseAPI struct {
	ermiteconnect.UnimplementedReleaseServiceHandler
	mu        sync.Mutex
	create    *ermite.CreateReleaseRequest
	gets      []*ermite.GetReleaseRequest
	lists     []*ermite.ListReleasesRequest
	update    *ermite.UpdateReleaseRequest
	deleted   *ermite.DeleteReleaseRequest
	uploads   []*ermite.UploadReleaseAssetRequest
	offsets   []int64
	release   *ermite.Release
	data      []byte
	uploadErr error
	download  func(*ermite.DownloadReleaseAssetRequest, *connect.ServerStream[ermite.DownloadReleaseAssetResponse]) error
}

func (f *fakeReleaseAPI) serve(t *testing.T, features ...string) string {
	t.Helper()
	mux := http.NewServeMux()
	p, h := ermiteconnect.NewReleaseServiceHandler(f)
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	v := &fakeAPI{getVersion: func(*ermite.GetVersionRequest) (*ermite.GetVersionResponse, error) {
		return &ermite.GetVersionResponse{Features: features}, nil
	}}
	p, h = ermiteconnect.NewServerServiceHandler(v)
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}
func (f *fakeReleaseAPI) CreateRelease(_ context.Context, r *connect.Request[ermite.CreateReleaseRequest]) (*connect.Response[ermite.CreateReleaseResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.create = proto.Clone(r.Msg).(*ermite.CreateReleaseRequest)
	return connect.NewResponse(&ermite.CreateReleaseResponse{Release: f.release}), nil
}
func (f *fakeReleaseAPI) GetRelease(_ context.Context, r *connect.Request[ermite.GetReleaseRequest]) (*connect.Response[ermite.GetReleaseResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, proto.Clone(r.Msg).(*ermite.GetReleaseRequest))
	return connect.NewResponse(&ermite.GetReleaseResponse{Release: f.release}), nil
}
func (f *fakeReleaseAPI) ListReleases(_ context.Context, r *connect.Request[ermite.ListReleasesRequest]) (*connect.Response[ermite.ListReleasesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, proto.Clone(r.Msg).(*ermite.ListReleasesRequest))
	next := ""
	if r.Msg.GetPageToken() == "" {
		next = "next"
	}
	return connect.NewResponse(&ermite.ListReleasesResponse{Releases: []*ermite.Release{f.release}, NextPageToken: next}), nil
}
func (f *fakeReleaseAPI) UpdateRelease(_ context.Context, r *connect.Request[ermite.UpdateReleaseRequest]) (*connect.Response[ermite.UpdateReleaseResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.update = proto.Clone(r.Msg).(*ermite.UpdateReleaseRequest)
	return connect.NewResponse(&ermite.UpdateReleaseResponse{Release: f.release}), nil
}
func (f *fakeReleaseAPI) DeleteRelease(_ context.Context, r *connect.Request[ermite.DeleteReleaseRequest]) (*connect.Response[ermite.DeleteReleaseResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = proto.Clone(r.Msg).(*ermite.DeleteReleaseRequest)
	return connect.NewResponse(&ermite.DeleteReleaseResponse{}), nil
}
func (f *fakeReleaseAPI) UploadReleaseAsset(_ context.Context, st *connect.ClientStream[ermite.UploadReleaseAssetRequest]) (*connect.Response[ermite.UploadReleaseAssetResponse], error) {
	var got []*ermite.UploadReleaseAssetRequest
	for st.Receive() {
		got = append(got, proto.Clone(st.Msg()).(*ermite.UploadReleaseAssetRequest))
	}
	if err := st.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, got...)
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	return connect.NewResponse(&ermite.UploadReleaseAssetResponse{Asset: &ermite.ReleaseAsset{Id: 9, ReleaseId: 7, Name: got[0].GetName(), Size: got[0].GetSize()}}), nil
}
func (f *fakeReleaseAPI) DownloadReleaseAsset(_ context.Context, r *connect.Request[ermite.DownloadReleaseAssetRequest], st *connect.ServerStream[ermite.DownloadReleaseAssetResponse]) error {
	f.mu.Lock()
	f.offsets = append(f.offsets, r.Msg.GetOffset())
	fn := f.download
	f.mu.Unlock()
	if fn != nil {
		return fn(r.Msg, st)
	}
	if err := st.Send(&ermite.DownloadReleaseAssetResponse{Asset: f.release.Assets[0]}); err != nil {
		return err
	}
	return st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: f.data[r.Msg.GetOffset():]})
}
func releaseFixture() *ermite.Release {
	return &ermite.Release{Id: 7, Repo: "proj", Tag: "v1", Title: "First", Notes: "# Notes\n", PublishedAt: ts("2026-10-10T12:00:00Z"), Assets: []*ermite.ReleaseAsset{}}
}

func TestCLIReleaseCreateAndUpload(t *testing.T) {
	data := make([]byte, 5<<19)
	file := filepath.Join(t.TempDir(), "asset.bin")
	require.NoError(t, os.WriteFile(file, data, 0o600))
	notes := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(notes, []byte("## hello"), 0o600))
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t, "releases"))
	res := runCLI(t, vars, "", "release", "create", "proj", "v1", "--title", "Hello", "--notes-file", notes, "--target", "main", "--draft", "--prerelease", "--latest", "--asset", file+"#Binary", "--json").ok(t)
	out := decode[jsonRelease](t, res.stdout)
	assert.Equal(t, int64(7), out.ID)
	require.Len(t, out.Assets, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.True(t, proto.Equal(&ermite.CreateReleaseRequest{Repo: "proj", Tag: "v1", Title: "Hello", Notes: "## hello", TargetCommitish: "main", Draft: true, Prerelease: true, MakeLatest: true}, f.create))
	require.Len(t, f.uploads, 4)
	assert.Equal(t, "Binary", f.uploads[0].GetLabel())
	assert.Equal(t, int64(len(data)), f.uploads[0].GetSize())
	assert.Empty(t, f.uploads[0].GetData())
	assert.False(t, f.uploads[0].GetReplace())
	assert.NotEmpty(t, f.uploads[0].GetContentType())
	for i, n := range []int{1 << 20, 1 << 20, 1 << 19} {
		assert.Len(t, f.uploads[i+1].GetData(), n)
		assert.Empty(t, f.uploads[i+1].GetRepo())
		assert.Empty(t, f.uploads[i+1].GetName())
	}
}
func TestCLIReleaseUpload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o600))
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t, "releases"))
	runCLI(t, vars, "", "release", "upload", "proj", "7", file+"#A", "--clobber", "--content-type", "application/test").ok(t)
	f.mu.Lock()
	assert.True(t, f.uploads[0].GetReplace())
	assert.Equal(t, "application/test", f.uploads[0].GetContentType())
	assert.Equal(t, int64(7), f.gets[0].GetId())
	f.uploadErr = connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("taken"))
	f.mu.Unlock()
	res := runCLI(t, vars, "", "release", "upload", "proj", "v1", file)
	require.ErrorContains(t, res.err, "--clobber")
	assert.Contains(t, res.err.Error(), `"x"`)
}
func TestCLIReleaseList(t *testing.T) {
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t, "releases"))
	res := runCLI(t, vars, "", "release", "list", "proj", "--limit", "1").ok(t)
	assert.Equal(t, "ID  TAG  TITLE  STATE      PUBLISHED         ASSETS\n7   v1   First  published  2026-10-10 12:00  0\n", res.stdout)
	res = runCLI(t, vars, "", "release", "list", "proj", "--limit", "2", "--json").ok(t)
	assert.Len(t, decode[[]jsonRelease](t, res.stdout), 2)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.lists, 3)
	assert.Equal(t, int32(2), f.lists[1].GetPageSize())
	assert.Equal(t, "next", f.lists[2].GetPageToken())
	assert.Equal(t, int32(1), f.lists[2].GetPageSize())
}
func TestCLIReleaseViewSelectors(t *testing.T) {
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t, "releases"))
	for _, sel := range []string{"v1", "7", "latest"} {
		runCLI(t, vars, "", "release", "view", "proj", sel, "--json").ok(t)
	}
	assert.Equal(t, "# Notes\n", runCLI(t, vars, "", "release", "view", "proj", "latest", "--notes").ok(t).stdout)
	res := runCLI(t, vars, "", "release", "view", "proj", "v1").ok(t)
	assert.Contains(t, res.stdout, "NAME  SIZE  TYPE")
	assert.Contains(t, res.stdout, "First")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, "v1", f.gets[0].GetTag())
	assert.Equal(t, int64(7), f.gets[1].GetId())
	assert.True(t, f.gets[2].GetLatest())
}
func TestCLIReleaseEdit(t *testing.T) {
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t, "releases"))
	runCLI(t, vars, "", "release", "edit", "proj", "v1", "--title=", "--notes-file", "-", "--tag", "v2", "--target", "main", "--publish", "--no-prerelease", "--latest=false").ok(t)
	f.mu.Lock()
	assert.ElementsMatch(t, []string{"title", "notes", "tag", "target_commitish", "draft", "prerelease", "make_latest"}, f.update.GetUpdateMask().GetPaths())
	assert.False(t, f.update.GetDraft())
	assert.False(t, f.update.GetPrerelease())
	assert.False(t, f.update.GetMakeLatest())
	assert.Empty(t, f.update.GetTitle())
	assert.Equal(t, "v2", f.update.GetTag())
	f.mu.Unlock()
	runCLI(t, vars, "", "release", "edit", "proj", "7", "--draft", "--prerelease", "--notes", "text").ok(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.True(t, f.update.GetDraft())
	assert.True(t, f.update.GetPrerelease())
	assert.Equal(t, "text", f.update.GetNotes())
}
func TestCLIReleaseDelete(t *testing.T) {
	for _, flags := range [][]string{nil, {"--yes"}} {
		f := &fakeReleaseAPI{release: releaseFixture()}
		vars := envFor(f.serve(t, "releases"))
		res := runCLI(t, vars, "", append([]string{"release", "delete", "proj", "v1"}, flags...)...).ok(t)
		assert.Equal(t, "deleted release 7\n", res.stdout)
		f.mu.Lock()
		assert.Equal(t, int64(7), f.deleted.GetId())
		assert.Equal(t, "proj", f.deleted.GetRepo())
		f.mu.Unlock()
	}
}
func TestCLIReleaseNoFeature(t *testing.T) {
	f := &fakeReleaseAPI{release: releaseFixture()}
	vars := envFor(f.serve(t))
	for _, args := range [][]string{{"create", "proj", "v1"}, {"list", "proj"}, {"view", "proj", "latest"}, {"edit", "proj", "v1", "--title", "x"}, {"delete", "proj", "v1"}, {"upload", "proj", "v1", "x"}, {"download", "proj", "latest"}} {
		res := runCLI(t, vars, "", append([]string{"release"}, args...)...)
		require.ErrorIs(t, res.err, errNoReleases)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Nil(t, f.create)
	assert.Empty(t, f.gets)
	assert.Nil(t, f.update)
	assert.Nil(t, f.deleted)
	assert.Empty(t, f.uploads)
}
func TestCLIReleaseFlagValidation(t *testing.T) {
	for _, args := range [][]string{{"create", "proj", "v1", "--notes", "x", "--notes-file", "f"}, {"edit", "proj", "v1", "--draft", "--publish"}, {"edit", "proj", "v1", "--prerelease", "--no-prerelease"}, {"edit", "proj", "v1"}, {"list", "proj", "--limit", "0"}, {"download", "proj", "v1", "--pattern", "["}, {"view", "proj", "v1", "--notes", "--json"}} {
		require.Error(t, runCLI(t, nil, "", append([]string{"release"}, args...)...).err)
	}
}
func TestCLIReleaseDownload(t *testing.T) {
	for _, mode := range []string{"normal", "resume", "twice broken", "digest mismatch", "unsafe", "overwrite", "empty"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("hello world")
			if mode == "empty" {
				data = nil
			}
			a := &ermite.ReleaseAsset{Id: 9, ReleaseId: 7, Name: "asset.bin", Size: int64(len(data)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))}
			if mode == "unsafe" {
				a.Name = "../escape"
			}
			if mode == "digest mismatch" {
				a.Digest = "sha256:bad"
			}
			r := releaseFixture()
			r.Assets = []*ermite.ReleaseAsset{a}
			f := &fakeReleaseAPI{release: r, data: data}
			if mode == "resume" || mode == "twice broken" {
				f.download = func(req *ermite.DownloadReleaseAssetRequest, st *connect.ServerStream[ermite.DownloadReleaseAssetResponse]) error {
					if err := st.Send(&ermite.DownloadReleaseAssetResponse{Asset: a}); err != nil {
						return err
					}
					if req.GetOffset() == 0 {
						if err := st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: data[:4]}); err != nil {
							return err
						}
						return connect.NewError(connect.CodeUnavailable, fmt.Errorf("broken"))
					}
					if mode == "twice broken" {
						return connect.NewError(connect.CodeUnavailable, fmt.Errorf("broken"))
					}
					return st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: data[req.GetOffset():]})
				}
			}
			dir := t.TempDir()
			path := filepath.Join(dir, a.GetName())
			if mode == "overwrite" {
				require.NoError(t, os.WriteFile(path, []byte("keep"), 0o600))
			}
			res := runCLI(t, envFor(f.serve(t, "releases")), "", "release", "download", "proj", "latest", "--dir", dir)
			switch mode {
			case "unsafe", "twice broken", "digest mismatch":
				require.Error(t, res.err)
				if mode != "unsafe" {
					_, err := os.Stat(path)
					assert.True(t, os.IsNotExist(err))
				}
			case "overwrite":
				require.Error(t, res.err)
				got, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, "keep", string(got))
			default:
				res.ok(t)
				got, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, string(data), string(got))
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if mode == "resume" || mode == "twice broken" {
				assert.Equal(t, []int64{0, 4}, f.offsets)
			}
			if mode == "normal" {
				assert.True(t, f.gets[0].GetLatest())
			}
		})
	}
}
func TestReleaseAssetSelection(t *testing.T) {
	all := []*ermite.ReleaseAsset{{Name: "a.zip"}, {Name: "b.txt"}, {Name: "c.zip"}}
	picked, err := pickReleaseAssets(all, []string{"b.txt"}, []string{"*.zip"})
	require.NoError(t, err)
	assert.Len(t, picked, 3)
	picked, err = pickReleaseAssets(all, nil, []string{"*.zip"})
	require.NoError(t, err)
	assert.Len(t, picked, 2)
	_, err = pickReleaseAssets(all, []string{"missing"}, nil)
	require.Error(t, err)
}

func TestCLIReleaseDownloadProtocolFailures(t *testing.T) {
	for _, mode := range []string{"missing metadata", "wrong metadata", "too much data", "short data", "permission denied"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("abc")
			a := &ermite.ReleaseAsset{Id: 9, Name: "a", Size: 3, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))}
			r := releaseFixture()
			r.Assets = []*ermite.ReleaseAsset{a}
			f := &fakeReleaseAPI{release: r, download: func(_ *ermite.DownloadReleaseAssetRequest, st *connect.ServerStream[ermite.DownloadReleaseAssetResponse]) error {
				switch mode {
				case "missing metadata":
					return st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: data})
				case "wrong metadata":
					return st.Send(&ermite.DownloadReleaseAssetResponse{Asset: &ermite.ReleaseAsset{Id: 10}})
				case "permission denied":
					return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("denied"))
				}
				if err := st.Send(&ermite.DownloadReleaseAssetResponse{Asset: a}); err != nil {
					return err
				}
				if mode == "too much data" {
					return st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: []byte("abcd")})
				}
				return st.Send(&ermite.DownloadReleaseAssetResponse{Chunk: []byte("a")})
			}}
			dir := t.TempDir()
			res := runCLI(t, envFor(f.serve(t, "releases")), "", "release", "download", "proj", "v1", "--dir", dir)
			require.Error(t, res.err)
			_, err := os.Stat(filepath.Join(dir, "a"))
			assert.True(t, os.IsNotExist(err))
			f.mu.Lock()
			defer f.mu.Unlock()
			if mode == "short data" {
				assert.Len(t, f.offsets, 2)
			} else {
				assert.Len(t, f.offsets, 1)
			}
		})
	}
}

func TestCLIReleaseUploadContentTypeAndEmpty(t *testing.T) {
	for _, data := range []string{"hello world", ""} {
		t.Run(fmt.Sprintf("size %d", len(data)), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "unknown-extension")
			require.NoError(t, os.WriteFile(file, []byte(data), 0o600))
			f := &fakeReleaseAPI{release: releaseFixture()}
			vars := envFor(f.serve(t, "releases"))
			runCLI(t, vars, "", "release", "upload", "proj", "v1", file).ok(t)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Equal(t, int64(len(data)), f.uploads[0].GetSize())
			assert.Equal(t, "text/plain; charset=utf-8", f.uploads[0].GetContentType())
			if data == "" {
				assert.Len(t, f.uploads, 1)
			} else {
				require.Len(t, f.uploads, 2)
				assert.Equal(t, data, string(f.uploads[1].GetData()))
			}
		})
	}
}
