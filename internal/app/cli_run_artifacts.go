// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

// artifactZip is the media type of an artifact that is a zip archive.
const artifactZip = "application/zip"

// maxExtractBytes bounds what the extraction of one archive writes: a small
// archive can declare a huge content.
var maxExtractBytes int64 = 32 << 30

// maxZipEntries bounds the entries of one archive.
const maxZipEntries = 100_000

// jsonArtifact is the stable JSON shape of an artifact.
type jsonArtifact struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Digest    string  `json:"digest"`
	MediaType string  `json:"media_type"`
	RunID     int64   `json:"run_id"`
	Attempt   int32   `json:"attempt"`
	JobID     string  `json:"job_id"`
	CreatedAt *string `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
}

type jsonArtifactList struct {
	Artifacts  []jsonArtifact `json:"artifacts"`
	UsedBytes  int64          `json:"used_bytes"`
	QuotaBytes int64          `json:"quota_bytes"`
}

func toJSONArtifact(a *ermite.Artifact) jsonArtifact {
	return jsonArtifact{
		ID: a.GetId(), Name: a.GetName(), Size: a.GetSize(), Digest: a.GetDigest(), MediaType: a.GetMediaType(),
		RunID: a.GetRunId(), Attempt: a.GetAttempt(), JobID: a.GetJobId(),
		CreatedAt: jsonTime(a.GetCreatedAt()), ExpiresAt: jsonTime(a.GetExpiresAt()),
	}
}

// humanBytes is a size for people, in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// listRunArtifacts reads the artifacts of a run. A server without them
// answers unimplemented, said in words here.
func listRunArtifacts(ctx context.Context, c *client.Client, repo string, runID int64) (*ermite.ListArtifactsResponse, error) {
	res, err := c.Artifacts().ListArtifacts(ctx, connect.NewRequest(&ermite.ListArtifactsRequest{Repo: repo, RunId: runID}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return nil, fmt.Errorf("the server does not keep artifacts (it lacks the ci.artifacts feature)")
	}
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func newRunArtifactsCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifacts <repo> <run id>",
		Short: "List the artifacts of a run",
		Long: "List a run's job artifacts (actions/upload-artifact) and repository artifact usage.\n" +
			"Access needs a repository-read token, even for public repositories.\n" +
			"JSON fields (--json): artifacts (id, name, size, digest, media_type, run_id, attempt, job_id,\n" +
			"created_at, expires_at), used_bytes and quota_bytes.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		res, err := listRunArtifacts(env.ctx(cmd), c, args[0], id)
		if err != nil {
			return err
		}
		if *asJSON {
			out := jsonArtifactList{Artifacts: []jsonArtifact{}, UsedBytes: res.GetUsedBytes(), QuotaBytes: res.GetQuotaBytes()}
			for _, a := range res.GetArtifacts() {
				out.Artifacts = append(out.Artifacts, toJSONArtifact(a))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		w := cmd.OutOrStdout()
		if len(res.GetArtifacts()) == 0 {
			noticef(cmd, "run %d has no artifacts", id)
		} else {
			rows := make([][]string, 0, len(res.GetArtifacts()))
			for _, a := range res.GetArtifacts() {
				rows = append(rows, []string{
					strconv.FormatInt(a.GetId(), 10), a.GetName(), humanBytes(a.GetSize()), shortTime(a.GetExpiresAt(), "-"),
				})
			}
			if err := table(w, []string{"ID", "NAME", "SIZE", "EXPIRES"}, rows); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintln(w, quotaLine(res.GetUsedBytes(), res.GetQuotaBytes()))
		return nil
	}
	return cmd
}

// quotaLine says how much of its space the artifacts of the repository use.
func quotaLine(used, quota int64) string {
	if quota <= 0 {
		return "artifacts of the repository: " + humanBytes(used) + " used"
	}
	return "artifacts of the repository: " + humanBytes(used) + " of " + humanBytes(quota) + " used"
}

func newRunDownloadCmd(env *cliEnv) *cobra.Command {
	var names []string
	var dir string
	cmd := &cobra.Command{
		Use:               "download <repo> <run id>",
		Short:             "Download the artifacts of a run",
		Long:              "Download all run artifacts, or select names with repeatable --name.\nThe command extracts zips into --dir for one downloaded artifact, or <dir>/<name> for several. Non-zips (archive: false uploads) are saved as <dir>/<name>. Existing files are never overwritten. Access needs a repository-read token, even for public repositories.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeRepoNames(env),
	}
	cmd.Flags().StringArrayVarP(&names, "name", "n", nil, "download the artifact of this name (repeatable)")
	cmd.Flags().StringVarP(&dir, "dir", "D", ".", "the directory to download into")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := parseRunID(args[1])
		if err != nil {
			return err
		}
		c, err := env.client(cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		res, err := listRunArtifacts(ctx, c, args[0], id)
		if err != nil {
			return err
		}
		picked, err := pickArtifacts(res.GetArtifacts(), names)
		if err != nil {
			return err
		}
		if len(picked) == 0 {
			return fmt.Errorf("run %d has no artifacts", id)
		}
		for _, a := range picked {
			if err := checkArtifactName(a.GetName()); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		for _, a := range picked {
			to, err := downloadArtifact(ctx, c, args[0], a, root, len(picked) > 1)
			if err != nil {
				return fmt.Errorf("artifact %q: %w", clean(a.GetName()), err)
			}
			noticef(cmd, "downloaded %s to %s", clean(a.GetName()), clean(filepath.Join(dir, to)))
		}
		return nil
	}
	return cmd
}

// pickArtifacts selects the artifacts of the names, every one when there is
// none. A name that no artifact has is an error.
func pickArtifacts(all []*ermite.Artifact, names []string) ([]*ermite.Artifact, error) {
	if len(names) == 0 {
		return all, nil
	}
	var out []*ermite.Artifact
	for _, n := range names {
		i := slices.IndexFunc(all, func(a *ermite.Artifact) bool { return a.GetName() == n })
		if i < 0 {
			return nil, fmt.Errorf("the run has no artifact %q", clean(n))
		}
		if !slices.Contains(out, all[i]) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// checkArtifactName refuses a name that is not a plain file name: the server's
// rules exclude separators, but the name comes from a job.
func checkArtifactName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || !filepath.IsLocal(name) {
		return fmt.Errorf("artifact %q: the name is not usable as a file name", clean(name))
	}
	return nil
}

// downloadArtifact streams an artifact into a temporary file of root, then
// extracts it (a zip) or renames it to its name. It returns the path written,
// relative to root.
func downloadArtifact(ctx context.Context, c *client.Client, repo string, a *ermite.Artifact, root *os.Root, several bool) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".ermite-download-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		_ = root.Remove(tmp)
	}()
	got, err := streamArtifact(ctx, c, repo, a.GetId(), f)
	if err != nil {
		return "", err
	}
	if got.GetMediaType() != artifactZip {
		if err := f.Close(); err != nil {
			return "", err
		}
		if _, err := root.Lstat(a.GetName()); err == nil {
			return "", fmt.Errorf("%s already exists", a.GetName())
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		return a.GetName(), root.Rename(tmp, a.GetName())
	}
	into, to := root, "."
	if several {
		to = a.GetName()
		if err := root.MkdirAll(to, 0o755); err != nil {
			return "", err
		}
		if into, err = root.OpenRoot(to); err != nil {
			return "", err
		}
		defer func() { _ = into.Close() }()
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if err := extractZip(f, size, into, maxExtractBytes); err != nil {
		return "", err
	}
	return to, nil
}

// streamArtifact writes the content of an artifact to w and returns the
// artifact, checking that the content has its size.
func streamArtifact(ctx context.Context, c *client.Client, repo string, id int64, w io.Writer) (*ermite.Artifact, error) {
	stream, err := c.Artifacts().DownloadArtifact(ctx, connect.NewRequest(&ermite.DownloadArtifactRequest{Repo: repo, ArtifactId: id}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var a *ermite.Artifact
	var n int64
	for stream.Receive() {
		msg := stream.Msg()
		if a == nil {
			if a = msg.GetArtifact(); a == nil {
				return nil, errors.New("the server did not describe the artifact")
			}
		}
		chunk := msg.GetChunk()
		if n+int64(len(chunk)) > a.GetSize() {
			return nil, errors.New("the server sent more than the size of the artifact")
		}
		if _, err := w.Write(chunk); err != nil {
			return nil, err
		}
		n += int64(len(chunk))
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if a == nil || n != a.GetSize() {
		return nil, errors.New("the download ended before the end of the artifact")
	}
	return a, nil
}

// extractZip writes the files of the archive into root. Every entry must be
// a local path (no absolute path, no "..") and a directory or a regular file:
// a symbolic link or any other kind fails the whole extraction. Files that
// exist are not overwritten, and the extraction writes at most limit bytes.
// os.Root keeps every write inside the directory, whatever links it holds.
func extractZip(r io.ReaderAt, size int64, root *os.Root, limit int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return fmt.Errorf("the archive has more than %d entries", maxZipEntries)
	}
	var total uint64
	for _, f := range zr.File {
		if err := checkZipEntry(f); err != nil {
			return err
		}
		total += f.UncompressedSize64
	}
	if limit < 0 || total > uint64(limit) {
		return fmt.Errorf("the archive expands to more than %s", humanBytes(limit))
	}
	remaining := limit
	for _, f := range zr.File {
		name := path.Clean(f.Name)
		if f.FileInfo().IsDir() {
			if err := root.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
				return err
			}
			continue
		}
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
				return err
			}
		}
		n, err := extractZipFile(f, root, filepath.FromSlash(name), remaining)
		if err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

// checkZipEntry refuses an entry that is not a local directory or regular file.
func checkZipEntry(f *zip.File) error {
	name := f.Name
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) ||
		slices.Contains(strings.Split(name, "/"), "..") || !filepath.IsLocal(filepath.FromSlash(path.Clean(name))) {
		return fmt.Errorf("the archive has an unsafe path %q", clean(name))
	}
	mode := f.Mode()
	if !mode.IsDir() && !mode.IsRegular() {
		return fmt.Errorf("the archive has %q, which is not a regular file or a directory", clean(name))
	}
	return nil
}

// extractZipFile writes one file of the archive, at most limit bytes.
func extractZipFile(f *zip.File, root *os.Root, name string, limit int64) (int64, error) {
	perm := os.FileMode(0o644)
	if f.Mode()&0o100 != 0 {
		perm = 0o755
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("the archive expands to more than declared")
	}
	return n, err
}
