// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// fakeArtifactAPI serves the artifacts of run 1 of proj.
type fakeArtifactAPI struct {
	ermiteconnect.UnimplementedArtifactServiceHandler
	arts    []*ermite.Artifact
	content map[int64][]byte
}

func (f *fakeArtifactAPI) ListArtifacts(_ context.Context, r *connect.Request[ermite.ListArtifactsRequest]) (*connect.Response[ermite.ListArtifactsResponse], error) {
	if r.Msg.GetRepo() != "proj" || r.Msg.GetRunId() != 1 {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&ermite.ListArtifactsResponse{Artifacts: f.arts, UsedBytes: 3 << 20, QuotaBytes: 10 << 30}), nil
}

func (f *fakeArtifactAPI) DownloadArtifact(_ context.Context, r *connect.Request[ermite.DownloadArtifactRequest], st *connect.ServerStream[ermite.DownloadArtifactResponse]) error {
	for _, a := range f.arts {
		if a.GetId() != r.Msg.GetArtifactId() {
			continue
		}
		if err := st.Send(&ermite.DownloadArtifactResponse{Artifact: a}); err != nil {
			return err
		}
		data := f.content[a.GetId()]
		for len(data) > 0 {
			n := min(len(data), 7)
			if err := st.Send(&ermite.DownloadArtifactResponse{Chunk: data[:n]}); err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}
	return connect.NewError(connect.CodeNotFound, nil)
}

func (f *fakeArtifactAPI) add(name, mediaType string, data []byte) {
	if f.content == nil {
		f.content = map[int64][]byte{}
	}
	id := int64(len(f.arts) + 1)
	f.arts = append(f.arts, &ermite.Artifact{
		Id: id, Repo: "proj", RunId: 1, Attempt: 1, JobId: "1-0-0", Name: name, MediaType: mediaType,
		Size: int64(len(data)), ExpiresAt: ts("2027-01-08T12:00:00Z"),
	})
	f.content[id] = data
}

func (f *fakeArtifactAPI) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	p, h := ermiteconnect.NewArtifactServiceHandler(f)
	mux.Handle("/-/api"+p, http.StripPrefix("/-/api", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

type zipEntry struct {
	name, body string
	mode       os.FileMode
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		_, err = w.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return b.Bytes()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

// noTemp checks that no temporary file of a download is left in dir.
func noTemp(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, ".ermite-download-*"))
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestRunArtifactsList(t *testing.T) {
	f := &fakeArtifactAPI{}
	f.add("dist", "application/zip", []byte("PK"))
	f.add("report.txt", "text/plain", bytes.Repeat([]byte("x"), 1536))
	url := f.serve(t)
	out := runCLI(t, envFor(url), "", "run", "artifacts", "proj", "1").ok(t)
	assert.Contains(t, out.stdout, "ID")
	assert.Contains(t, out.stdout, "EXPIRES")
	assert.Regexp(t, `1\s+dist\s+2 B\s+2027-01-08 12:00`, out.stdout)
	assert.Regexp(t, `2\s+report\.txt\s+1\.5 KiB`, out.stdout)
	assert.Contains(t, out.stdout, "3.0 MiB of 10.0 GiB used")

	js := decode[jsonArtifactList](t, runCLI(t, envFor(url), "", "run", "artifacts", "proj", "1", "--json").ok(t).stdout)
	require.Len(t, js.Artifacts, 2)
	assert.Equal(t, "report.txt", js.Artifacts[1].Name)
	assert.Equal(t, int64(10<<30), js.QuotaBytes)

	assert.Error(t, runCLI(t, envFor(url), "", "run", "artifacts", "proj", "2").err)
}

func TestRunDownloadOneZip(t *testing.T) {
	f := &fakeArtifactAPI{}
	f.add("dist", "application/zip", zipOf(t,
		zipEntry{name: "bin/"}, zipEntry{name: "bin/app", body: "binary", mode: 0o755}, zipEntry{name: "README", body: "read me"}))
	dir := filepath.Join(t.TempDir(), "out")
	runCLI(t, envFor(f.serve(t)), "", "run", "download", "proj", "1", "--dir", dir).ok(t)
	assert.Equal(t, "binary", readFile(t, filepath.Join(dir, "bin", "app")))
	assert.Equal(t, "read me", readFile(t, filepath.Join(dir, "README")))
	st, err := os.Stat(filepath.Join(dir, "bin", "app"))
	require.NoError(t, err)
	assert.NotZero(t, st.Mode()&0o100, "the executable bit")
	noTemp(t, dir)
}

func TestRunDownloadSeveral(t *testing.T) {
	f := &fakeArtifactAPI{}
	f.add("linux", "application/zip", zipOf(t, zipEntry{name: "app", body: "linux"}))
	f.add("darwin", "application/zip", zipOf(t, zipEntry{name: "app", body: "darwin"}))
	f.add("notes.txt", "text/plain", []byte("plain file"))
	url := f.serve(t)

	dir := t.TempDir()
	runCLI(t, envFor(url), "", "run", "download", "proj", "1", "--dir", dir).ok(t)
	assert.Equal(t, "linux", readFile(t, filepath.Join(dir, "linux", "app")))
	assert.Equal(t, "darwin", readFile(t, filepath.Join(dir, "darwin", "app")))
	assert.Equal(t, "plain file", readFile(t, filepath.Join(dir, "notes.txt")), "saved as is")
	noTemp(t, dir)

	// Again into the same directory: nothing is overwritten.
	assert.Error(t, runCLI(t, envFor(url), "", "run", "download", "proj", "1", "--dir", dir).err)
	noTemp(t, dir)

	// --name chooses; two names are several, one is extracted into the dir.
	two := t.TempDir()
	runCLI(t, envFor(url), "", "run", "download", "proj", "1", "--dir", two, "--name", "darwin", "-n", "notes.txt").ok(t)
	assert.Equal(t, "darwin", readFile(t, filepath.Join(two, "darwin", "app")))
	assert.Equal(t, "plain file", readFile(t, filepath.Join(two, "notes.txt")))
	assert.NoDirExists(t, filepath.Join(two, "linux"))

	one := t.TempDir()
	runCLI(t, envFor(url), "", "run", "download", "proj", "1", "--dir", one, "--name", "linux").ok(t)
	assert.Equal(t, "linux", readFile(t, filepath.Join(one, "app")))

	unknown := runCLI(t, envFor(url), "", "run", "download", "proj", "1", "--dir", t.TempDir(), "--name", "nope")
	require.Error(t, unknown.err)
	assert.Contains(t, unknown.err.Error(), `no artifact "nope"`)
}

func TestRunDownloadRefusesUnsafeArchives(t *testing.T) {
	for name, entries := range map[string][]zipEntry{
		"dot-dot":   {{name: "ok", body: "x"}, {name: "../escape", body: "x"}},
		"deep":      {{name: "a/../../escape", body: "x"}},
		"absolute":  {{name: "/tmp/escape", body: "x"}},
		"symlink":   {{name: "link", body: "/etc/passwd", mode: os.ModeSymlink | 0o777}},
		"backslash": {{name: `a\..\..\escape`, body: "x"}},
		"dir-link":  {{name: "d", body: "/", mode: os.ModeSymlink | 0o777}, {name: "d/escape", body: "x"}},
		"device":    {{name: "dev", body: "", mode: os.ModeDevice | 0o666}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeArtifactAPI{}
			f.add("evil", "application/zip", zipOf(t, entries...))
			parent := t.TempDir()
			dir := filepath.Join(parent, "out")
			res := runCLI(t, envFor(f.serve(t)), "", "run", "download", "proj", "1", "--dir", dir)
			require.Error(t, res.err)
			assert.NoFileExists(t, filepath.Join(parent, "escape"))
			assert.NoFileExists(t, filepath.Join(dir, "link"))
			assert.NoFileExists(t, filepath.Join(dir, "ok"), "nothing is written from a refused archive")
			noTemp(t, dir)
		})
	}
}

func TestExtractZipBoundsTheExpansion(t *testing.T) {
	data := zipOf(t, zipEntry{name: "big", body: string(bytes.Repeat([]byte("0"), 1<<20))})
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	err = extractZip(bytes.NewReader(data), int64(len(data)), root, 1<<10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expands to more than")
	assert.NoFileExists(t, filepath.Join(dir, "big"))
}

// A setuid or setgid bit and other mode bits of an entry are dropped: files
// are 0644, or 0755 when the owner could execute them.
func TestExtractZipKeepsOnlyPlainModes(t *testing.T) {
	data := zipOf(t, zipEntry{name: "suid", body: "x", mode: os.ModeSetuid | os.ModeSetgid | 0o4777}, zipEntry{name: "plain", body: "y", mode: 0o666})
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.NoError(t, extractZip(bytes.NewReader(data), int64(len(data)), root, 1<<20))
	for name, want := range map[string]os.FileMode{"suid": 0o755, "plain": 0o644} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Zero(t, info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky), name)
		assert.Equal(t, want, info.Mode().Perm()&want, name)
		assert.Zero(t, info.Mode().Perm()&^0o755, name)
	}
}

// An entry whose header understates its size (a zip bomb that lies) is cut at
// the limit: the declared sizes are not trusted alone.
func TestExtractZipStopsALyingEntry(t *testing.T) {
	var deflated bytes.Buffer
	fw, err := flate.NewWriter(&deflated, flate.BestCompression)
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte{0}, 4<<20))
	require.NoError(t, err)
	require.NoError(t, fw.Close())
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "bomb", Method: zip.Deflate, CompressedSize64: uint64(deflated.Len()), UncompressedSize64: 10})
	require.NoError(t, err)
	_, err = w.Write(deflated.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.Error(t, extractZip(bytes.NewReader(b.Bytes()), int64(b.Len()), root, 1<<20))
	if info, err := os.Stat(filepath.Join(dir, "bomb")); err == nil {
		assert.LessOrEqual(t, info.Size(), int64(1<<20)+1, "never more than the limit written")
	}
}

func TestExtractZipBoundsTheEntries(t *testing.T) {
	entries := make([]zipEntry, maxZipEntries+1)
	for i := range entries {
		entries[i] = zipEntry{name: fmt.Sprint("f", i)}
	}
	data := zipOf(t, entries...)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	err = extractZip(bytes.NewReader(data), int64(len(data)), root, 1<<30)
	require.ErrorContains(t, err, "entries")
	assert.NoFileExists(t, filepath.Join(dir, "f0"))
}

func TestCheckArtifactName(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, "/etc/passwd", "../x", "a\x00b"} {
		assert.Error(t, checkArtifactName(n), n)
	}
	assert.NoError(t, checkArtifactName("dist-linux.tar"))
	assert.Equal(t, "1.5 KiB", humanBytes(1536))
	assert.Equal(t, "10.0 GiB", humanBytes(10<<30))
}
