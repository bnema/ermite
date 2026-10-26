// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/adapters/client"
)

var errNoReleases = errors.New("the server has no releases (it lacks the releases feature)")

func requireReleases(ctx context.Context, c *client.Client) error {
	res, err := c.Server().GetVersion(ctx, connect.NewRequest(&ermite.GetVersionRequest{}))
	if err != nil {
		return c.Describe(err)
	}
	if !slices.Contains(res.Msg.GetFeatures(), client.FeatureReleases) {
		return errNoReleases
	}
	return nil
}

func releaseClient(env *cliEnv, cmd *cobra.Command) (*client.Client, error) {
	c, err := env.client(cmd)
	if err != nil {
		return nil, err
	}
	if err := requireReleases(env.ctx(cmd), c); err != nil {
		return nil, err
	}
	return c, nil
}

// jsonRelease and jsonReleaseAsset keep the CLI shape independent of protobuf JSON.
type jsonReleaseAsset struct {
	ID          int64   `json:"id"`
	ReleaseID   int64   `json:"release_id"`
	Name        string  `json:"name"`
	Label       string  `json:"label"`
	ContentType string  `json:"content_type"`
	Size        int64   `json:"size"`
	Digest      string  `json:"digest"`
	Uploader    string  `json:"uploader"`
	CreatedAt   *string `json:"created_at"`
	UpdatedAt   *string `json:"updated_at"`
}
type jsonRelease struct {
	ID              int64              `json:"id"`
	Repo            string             `json:"repo"`
	Tag             string             `json:"tag"`
	Title           string             `json:"title"`
	Notes           string             `json:"notes"`
	Draft           bool               `json:"draft"`
	Prerelease      bool               `json:"prerelease"`
	Latest          bool               `json:"latest"`
	TargetCommitish string             `json:"target_commitish"`
	Author          string             `json:"author"`
	CreatedAt       *string            `json:"created_at"`
	PublishedAt     *string            `json:"published_at"`
	UpdatedAt       *string            `json:"updated_at"`
	Assets          []jsonReleaseAsset `json:"assets"`
}

func toJSONRelease(r *ermite.Release) jsonRelease {
	out := jsonRelease{ID: r.GetId(), Repo: r.GetRepo(), Tag: r.GetTag(), Title: r.GetTitle(), Notes: r.GetNotes(), Draft: r.GetDraft(), Prerelease: r.GetPrerelease(), Latest: r.GetLatest(), TargetCommitish: r.GetTargetCommitish(), Author: r.GetAuthor(), CreatedAt: jsonTime(r.GetCreatedAt()), PublishedAt: jsonTime(r.GetPublishedAt()), UpdatedAt: jsonTime(r.GetUpdatedAt()), Assets: []jsonReleaseAsset{}}
	for _, a := range r.GetAssets() {
		out.Assets = append(out.Assets, jsonReleaseAsset{a.GetId(), a.GetReleaseId(), a.GetName(), a.GetLabel(), a.GetContentType(), a.GetSize(), a.GetDigest(), a.GetUploader(), jsonTime(a.GetCreatedAt()), jsonTime(a.GetUpdatedAt())})
	}
	return out
}

const releaseJSONHelp = "\nJSON fields: id, repo, tag, title, notes, draft, prerelease, latest, target_commitish, author,\ncreated_at, published_at, updated_at (RFC 3339 UTC or null), assets (id, release_id, name, label,\ncontent_type, size, digest, uploader, created_at, updated_at). list prints an array."

func newReleaseCmd(env *cliEnv) *cobra.Command {
	cmd := &cobra.Command{Use: "release", Short: "Manage releases and their assets", Long: "Manage tagged releases. Reads follow repository visibility; writes need release:write. The server must support releases. Select by tag, numeric id, or latest (view/download)." + releaseJSONHelp}
	cmd.AddCommand(newReleaseCreateCmd(env), newReleaseListCmd(env), newReleaseViewCmd(env), newReleaseEditCmd(env), newReleaseDeleteCmd(env), newReleaseUploadCmd(env), newReleaseDownloadCmd(env), newReleasePendingCmd(env))
	return cmd
}
func releaseCommand(env *cliEnv, use, short string, n int) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(n), ValidArgsFunction: completeRepoNames(env)}
}

func getRelease(ctx context.Context, c *client.Client, repo, selector string, latest bool) (*ermite.Release, error) {
	req := &ermite.GetReleaseRequest{Repo: repo}
	if selector == "latest" && latest {
		req.Selector = &ermite.GetReleaseRequest_Latest{Latest: true}
	} else if selector != "" && strings.Trim(selector, "0123456789") == "" {
		id, err := strconv.ParseInt(selector, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid release id %q", selector)
		}
		req.Selector = &ermite.GetReleaseRequest_Id{Id: id}
	} else {
		req.Selector = &ermite.GetReleaseRequest_Tag{Tag: selector}
	}
	res, err := c.Releases().GetRelease(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, c.Describe(err)
	}
	return res.Msg.GetRelease(), nil
}
func releaseState(r *ermite.Release) string {
	if r.GetDraft() {
		return "draft"
	}
	if r.GetPrerelease() {
		return "prerelease"
	}
	return "published"
}
func releaseURL(base, repo, tag string) string {
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(repo) + "/releases/tag/" + url.PathEscape(tag)
}
func outputRelease(cmd *cobra.Command, c *client.Client, r *ermite.Release, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), toJSONRelease(r))
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\nid: %d\n", releaseURL(c.URL(), r.GetRepo(), r.GetTag()), r.GetId())
	return err
}

type releaseFields struct {
	title, notes, notesFile, target, tag             string
	draft, publish, prerelease, noPrerelease, latest bool
}

func (f *releaseFields) flags(cmd *cobra.Command, edit bool) {
	cmd.Flags().StringVar(&f.title, "title", "", "release title")
	cmd.Flags().StringVar(&f.notes, "notes", "", "Markdown release notes")
	cmd.Flags().StringVar(&f.notesFile, "notes-file", "", "read Markdown release notes from a file (- for stdin)")
	cmd.MarkFlagsMutuallyExclusive("notes", "notes-file")
	cmd.Flags().StringVar(&f.target, "target", "", "target commit or branch for a missing tag")
	cmd.Flags().BoolVar(&f.draft, "draft", false, "keep as a draft")
	cmd.Flags().BoolVar(&f.prerelease, "prerelease", false, "mark as a prerelease")
	cmd.Flags().BoolVar(&f.latest, "latest", false, "pin as latest")
	if edit {
		cmd.Flags().StringVar(&f.tag, "tag", "", "new tag")
		cmd.Flags().BoolVar(&f.publish, "publish", false, "publish a draft")
		cmd.Flags().BoolVar(&f.noPrerelease, "no-prerelease", false, "clear the prerelease flag")
		cmd.MarkFlagsMutuallyExclusive("draft", "publish")
		cmd.MarkFlagsMutuallyExclusive("prerelease", "no-prerelease")
	}
}
func (f *releaseFields) readNotes(cmd *cobra.Command) error {
	if !cmd.Flags().Changed("notes-file") {
		return nil
	}
	var data []byte
	var err error
	if f.notesFile == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(f.notesFile)
	}
	f.notes = string(data)
	return err
}
func newReleaseCreateCmd(env *cliEnv) *cobra.Command {
	var f releaseFields
	var assets []string
	cmd := releaseCommand(env, "create <repo> <tag>", "Create a release", 2)
	cmd.Long = "Create a release, then upload repeatable --asset FILE[#LABEL]. The shell expands globs.\nJSON: a release; see `ermite release --help`."
	f.flags(cmd, false)
	cmd.Flags().StringArrayVar(&assets, "asset", nil, "upload FILE[#LABEL] (repeatable)")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.readNotes(cmd); err != nil {
			return err
		}
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		res, err := c.Releases().CreateRelease(ctx, connect.NewRequest(&ermite.CreateReleaseRequest{Repo: args[0], Tag: args[1], Title: f.title, Notes: f.notes, TargetCommitish: f.target, Draft: f.draft, Prerelease: f.prerelease, MakeLatest: f.latest}))
		if err != nil {
			return c.Describe(err)
		}
		r := res.Msg.GetRelease()
		for _, spec := range assets {
			a, err := uploadReleaseFile(ctx, c, args[0], r.GetId(), spec, "", false)
			if err != nil {
				return fmt.Errorf("release %d was created, but asset upload failed: %w", r.GetId(), err)
			}
			r.Assets = append(r.Assets, a)
		}
		return outputRelease(cmd, c, r, *asJSON)
	}
	return cmd
}
func newReleaseListCmd(env *cliEnv) *cobra.Command {
	var limit int32
	cmd := releaseCommand(env, "list <repo>", "List releases, newest first", 1)
	cmd.Long = "List releases, including drafts visible to writers." + "\nJSON: an array of releases; see `ermite release --help`."
	cmd.Flags().Int32Var(&limit, "limit", 30, "maximum number of releases")
	asJSON := addJSONFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if limit <= 0 {
			return errors.New("--limit must be positive")
		}
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		all := []*ermite.Release{}
		page := ""
		for int32(len(all)) < limit {
			res, err := c.Releases().ListReleases(env.ctx(cmd), connect.NewRequest(&ermite.ListReleasesRequest{Repo: args[0], PageSize: limit - int32(len(all)), PageToken: page}))
			if err != nil {
				return c.Describe(err)
			}
			all = append(all, res.Msg.GetReleases()...)
			next := res.Msg.GetNextPageToken()
			if next == "" {
				break
			}
			if next == page {
				return errors.New("the server repeated a release page token")
			}
			page = next
		}
		if int32(len(all)) > limit {
			all = all[:limit]
		}
		if *asJSON {
			out := make([]jsonRelease, 0, len(all))
			for _, r := range all {
				out = append(out, toJSONRelease(r))
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}
		rows := [][]string{}
		for _, r := range all {
			rows = append(rows, []string{strconv.FormatInt(r.GetId(), 10), r.GetTag(), r.GetTitle(), releaseState(r), shortTime(r.GetPublishedAt(), "-"), strconv.Itoa(len(r.GetAssets()))})
		}
		return table(cmd.OutOrStdout(), []string{"ID", "TAG", "TITLE", "STATE", "PUBLISHED", "ASSETS"}, rows)
	}
	return cmd
}
func newReleaseViewCmd(env *cliEnv) *cobra.Command {
	var notes bool
	cmd := releaseCommand(env, "view <repo> <tag|id|latest>", "View a release and its assets", 2)
	cmd.Long = "View release fields, Markdown notes and assets. --notes prints only Markdown." + "\nJSON: a release; see `ermite release --help`."
	asJSON := addJSONFlag(cmd)
	cmd.Flags().BoolVar(&notes, "notes", false, "print only Markdown notes")
	cmd.MarkFlagsMutuallyExclusive("json", "notes")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		r, err := getRelease(env.ctx(cmd), c, args[0], args[1], true)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(cmd.OutOrStdout(), toJSONRelease(r))
		}
		if notes {
			_, err := fmt.Fprint(cmd.OutOrStdout(), r.GetNotes())
			return err
		}
		if err := fields(cmd.OutOrStdout(), [][2]string{{"id", strconv.FormatInt(r.GetId(), 10)}, {"tag", r.GetTag()}, {"title", r.GetTitle()}, {"state", releaseState(r)}, {"latest", strconv.FormatBool(r.GetLatest())}, {"target", r.GetTargetCommitish()}, {"author", r.GetAuthor()}, {"created", shortTime(r.GetCreatedAt(), "-")}, {"published", shortTime(r.GetPublishedAt(), "-")}, {"updated", shortTime(r.GetUpdatedAt(), "-")}}); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), clean(r.GetNotes())); err != nil {
			return err
		}
		rows := [][]string{}
		for _, a := range r.GetAssets() {
			rows = append(rows, []string{a.GetName(), humanBytes(a.GetSize()), a.GetContentType()})
		}
		return table(cmd.OutOrStdout(), []string{"NAME", "SIZE", "TYPE"}, rows)
	}
	return cmd
}
func newReleaseEditCmd(env *cliEnv) *cobra.Command {
	var f releaseFields
	cmd := releaseCommand(env, "edit <repo> <tag|id>", "Edit a release", 2)
	f.flags(cmd, true)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := f.readNotes(cmd); err != nil {
			return err
		}
		paths := []string{}
		for _, p := range [][2]string{{"title", "title"}, {"notes", "notes"}, {"notes-file", "notes"}, {"tag", "tag"}, {"target", "target_commitish"}, {"draft", "draft"}, {"publish", "draft"}, {"prerelease", "prerelease"}, {"no-prerelease", "prerelease"}, {"latest", "make_latest"}} {
			if cmd.Flags().Changed(p[0]) {
				paths = append(paths, p[1])
			}
		}
		if len(paths) == 0 {
			return errors.New("give at least one field to edit")
		}
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		r, err := getRelease(ctx, c, args[0], args[1], false)
		if err != nil {
			return err
		}
		res, err := c.Releases().UpdateRelease(ctx, connect.NewRequest(&ermite.UpdateReleaseRequest{Repo: args[0], Id: r.GetId(), Tag: f.tag, Title: f.title, Notes: f.notes, TargetCommitish: f.target, Draft: f.draft && !f.publish, Prerelease: f.prerelease && !f.noPrerelease, MakeLatest: f.latest, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}))
		if err != nil {
			return c.Describe(err)
		}
		return outputRelease(cmd, c, res.Msg.GetRelease(), false)
	}
	return cmd
}
func newReleaseDeleteCmd(env *cliEnv) *cobra.Command {
	var yes bool
	cmd := releaseCommand(env, "delete <repo> <tag|id>", "Delete a release and its assets (keeps the tag)", 2)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		r, err := getRelease(ctx, c, args[0], args[1], false)
		if err != nil {
			return err
		}
		if in, ok := cmd.InOrStdin().(*os.File); !yes && ok && term.IsTerminal(int(in.Fd())) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Delete release %q and all its assets? [y/N] ", clean(r.GetTag()))
			answer, err := bufio.NewReader(in).ReadString('\n')
			if err != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") {
				return errors.New("aborted")
			}
		}
		_, err = c.Releases().DeleteRelease(ctx, connect.NewRequest(&ermite.DeleteReleaseRequest{Repo: args[0], Id: r.GetId()}))
		if err != nil {
			return c.Describe(err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted release %d\n", r.GetId())
		return err
	}
	return cmd
}
func newReleaseUploadCmd(env *cliEnv) *cobra.Command {
	var clobber bool
	var contentType string
	cmd := releaseCommand(env, "upload <repo> <tag|id> FILE[#LABEL]...", "Upload release assets", 3)
	cmd.Args = cobra.MinimumNArgs(3)
	cmd.Flags().BoolVar(&clobber, "clobber", false, "replace assets with the same name")
	cmd.Flags().StringVar(&contentType, "content-type", "", "override detected media type")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		r, err := getRelease(ctx, c, args[0], args[1], false)
		if err != nil {
			return err
		}
		for _, spec := range args[2:] {
			a, err := uploadReleaseFile(ctx, c, args[0], r.GetId(), spec, contentType, clobber)
			if err != nil {
				return err
			}
			noticef(cmd, "uploaded %s (%s)", clean(a.GetName()), humanBytes(a.GetSize()))
		}
		return nil
	}
	return cmd
}
func uploadReleaseFile(ctx context.Context, c *client.Client, repo string, id int64, spec, contentType string, replace bool) (*ermite.ReleaseAsset, error) {
	filename, label, _ := strings.Cut(spec, "#")
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", filename)
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
		if contentType == "" {
			buf := make([]byte, 512)
			n, err := f.Read(buf)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			contentType = http.DetectContentType(buf[:n])
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
		}
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return nil, fmt.Errorf("invalid --content-type: %w", err)
	}
	name := filepath.Base(filename)
	stream := c.Releases().UploadReleaseAsset(ctx)
	// Always close the stream, including a local read or Send failure.
	defer func() {
		if conn, err := stream.Conn(); err == nil {
			_ = conn.CloseRequest()
			_ = conn.CloseResponse()
		}
	}()
	err = stream.Send(&ermite.UploadReleaseAssetRequest{Repo: repo, ReleaseId: id, Name: name, Label: label, ContentType: contentType, Size: info.Size(), Replace: replace})
	buf := make([]byte, 1<<20)
	for err == nil {
		n, rerr := f.Read(buf)
		if n > 0 {
			err = stream.Send(&ermite.UploadReleaseAssetRequest{Data: buf[:n]})
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return nil, rerr
			}
			break
		}
	}
	// CloseAndReceive exposes a server error even when Send returned EOF.
	res, closeErr := stream.CloseAndReceive()
	if closeErr != nil {
		err = closeErr
	}
	if err != nil {
		if connect.CodeOf(err) == connect.CodeAlreadyExists && !replace {
			return nil, fmt.Errorf("asset %q already exists; use --clobber to replace it", clean(name))
		}
		return nil, c.Describe(err)
	}
	return res.Msg.GetAsset(), nil
}
func newReleaseDownloadCmd(env *cliEnv) *cobra.Command {
	var names, patterns []string
	var dir string
	cmd := releaseCommand(env, "download <repo> <tag|id|latest>", "Download assets without overwriting existing files", 2)
	cmd.Flags().StringArrayVar(&names, "name", nil, "asset name (repeatable)")
	cmd.Flags().StringArrayVar(&patterns, "pattern", nil, "asset name glob (repeatable)")
	cmd.Flags().StringVarP(&dir, "dir", "D", ".", "download directory")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		for _, p := range patterns {
			if _, err := filepath.Match(p, ""); err != nil {
				return fmt.Errorf("--pattern: %w", err)
			}
		}
		c, err := releaseClient(env, cmd)
		if err != nil {
			return err
		}
		ctx := env.ctx(cmd)
		r, err := getRelease(ctx, c, args[0], args[1], true)
		if err != nil {
			return err
		}
		picked, err := pickReleaseAssets(r.GetAssets(), names, patterns)
		if err != nil {
			return err
		}
		if len(picked) == 0 {
			return errors.New("no matching release assets")
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
			if err := downloadReleaseAsset(ctx, c, args[0], a, root); err != nil {
				return fmt.Errorf("asset %q: %w", clean(a.GetName()), err)
			}
			noticef(cmd, "downloaded %s to %s", clean(a.GetName()), clean(filepath.Join(dir, a.GetName())))
		}
		return nil
	}
	return cmd
}
func pickReleaseAssets(all []*ermite.ReleaseAsset, names, patterns []string) ([]*ermite.ReleaseAsset, error) {
	for _, name := range names {
		if !slices.ContainsFunc(all, func(a *ermite.ReleaseAsset) bool { return a.GetName() == name }) {
			return nil, fmt.Errorf("the release has no asset %q", clean(name))
		}
	}
	out := []*ermite.ReleaseAsset{}
	for _, a := range all {
		match := len(names) == 0 && len(patterns) == 0 || slices.Contains(names, a.GetName())
		for _, p := range patterns {
			ok, err := filepath.Match(p, a.GetName())
			if err != nil {
				return nil, err
			}
			match = match || ok
		}
		if match {
			out = append(out, a)
		}
	}
	return out, nil
}

// downloadReleaseAsset owns the destination exclusively; errors remove it. Only
// transport failures resume, once, using the immutable asset id and kept offset.
func downloadReleaseAsset(ctx context.Context, c *client.Client, repo string, a *ermite.ReleaseAsset, root *os.Root) (err error) {
	f, err := root.OpenFile(a.GetName(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			_ = root.Remove(a.GetName())
		}
	}()
	hash := sha256.New()
	w := io.MultiWriter(f, hash)
	var kept int64
	for attempt := 0; attempt < 2; attempt++ {
		stream, openErr := c.Releases().DownloadReleaseAsset(ctx, connect.NewRequest(&ermite.DownloadReleaseAssetRequest{Repo: repo, Selector: &ermite.DownloadReleaseAssetRequest_AssetId{AssetId: a.GetId()}, Offset: kept}))
		if openErr != nil {
			err = openErr
		} else {
			described := false
			for stream.Receive() {
				msg := stream.Msg()
				if !described {
					meta := msg.GetAsset()
					if meta == nil || meta.GetId() != a.GetId() || meta.GetSize() != a.GetSize() || meta.GetDigest() != a.GetDigest() {
						_ = stream.Close()
						return errors.New("the server did not describe the expected asset")
					}
					described = true
				}
				chunk := msg.GetChunk()
				if kept+int64(len(chunk)) > a.GetSize() {
					_ = stream.Close()
					return errors.New("the server sent more than the asset size")
				}
				n, werr := w.Write(chunk)
				kept += int64(n)
				if werr != nil {
					_ = stream.Close()
					return werr
				}
			}
			err = stream.Err()
			_ = stream.Close()
			if err == nil && (!described || kept != a.GetSize()) {
				err = io.ErrUnexpectedEOF
			}
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code := connect.CodeOf(err)
		if attempt == 1 || (code != connect.CodeUnavailable && code != connect.CodeInternal && !errors.Is(err, io.ErrUnexpectedEOF)) {
			return c.Describe(err)
		}
	}
	if err != nil {
		return c.Describe(err)
	}
	if a.GetDigest() != "sha256:"+hex.EncodeToString(hash.Sum(nil)) {
		return errors.New("asset digest mismatch")
	}
	return f.Sync()
}
