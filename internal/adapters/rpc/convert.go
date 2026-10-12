// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"encoding/base64"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// This file maps domain values to the messages of the contract and back.
// Messages are DTOs: the domain never leaks into them, nor they into the
// domain.

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func goTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func visibilityToPB(v domain.Visibility) pb.Visibility {
	switch v {
	case domain.VisibilityPublic:
		return pb.Visibility_VISIBILITY_PUBLIC
	case domain.VisibilityPrivate:
		return pb.Visibility_VISIBILITY_PRIVATE
	}
	return pb.Visibility_VISIBILITY_UNSPECIFIED
}

// visibilityFromPB maps a visibility of a request. Unspecified is the empty
// domain.Visibility, which the use cases treat as "default".
func visibilityFromPB(v pb.Visibility) (domain.Visibility, error) {
	switch v {
	case pb.Visibility_VISIBILITY_UNSPECIFIED:
		return "", nil
	case pb.Visibility_VISIBILITY_PUBLIC:
		return domain.VisibilityPublic, nil
	case pb.Visibility_VISIBILITY_PRIVATE:
		return domain.VisibilityPrivate, nil
	}
	return "", fmt.Errorf("%w: %v", domain.ErrInvalidVisibility, v)
}

func repoToPB(v ports.RepoView) *pb.Repo {
	settings := v.Settings
	if settings.IsZero() {
		settings = domain.DefaultRepoSettings()
	}
	return &pb.Repo{
		Name:          v.Name.String(),
		Description:   v.Description,
		Visibility:    visibilityToPB(v.Visibility),
		DefaultBranch: v.DefaultBranch,
		CreatedAt:     timestamp(v.CreatedAt),
		PushedAt:      timestamp(v.PushedAt),
		Settings:      settingsToPB(settings),
	}
}

func settingsToPB(s domain.RepoSettings) *pb.RepoSettings {
	out := &pb.RepoSettings{FailingChecksBlock: s.FailingChecksBlock}
	for _, m := range s.AllowedMergeModes {
		out.AllowedMergeModes = append(out.AllowedMergeModes, mergeModeToPB(m))
	}
	return out
}

func mergeModeToPB(m domain.MergeMode) pb.MergeMode {
	switch m {
	case domain.MergeModeFastForward:
		return pb.MergeMode_MERGE_MODE_FAST_FORWARD
	case domain.MergeModeSquash:
		return pb.MergeMode_MERGE_MODE_SQUASH
	}
	return pb.MergeMode_MERGE_MODE_UNSPECIFIED
}

// mergeModeFromPB maps a mode of a merge request: unspecified is the empty
// domain.MergeMode, which the use case reads as fast-forward.
func mergeModeFromPB(m pb.MergeMode) (domain.MergeMode, error) {
	switch m {
	case pb.MergeMode_MERGE_MODE_UNSPECIFIED:
		return "", nil
	case pb.MergeMode_MERGE_MODE_FAST_FORWARD:
		return domain.MergeModeFastForward, nil
	case pb.MergeMode_MERGE_MODE_SQUASH:
		return domain.MergeModeSquash, nil
	}
	return "", fmt.Errorf("%w: merge mode %v", domain.ErrInvalid, m)
}

// mergeModesFromPB maps the allowed modes of the settings, where unspecified
// is rejected.
func mergeModesFromPB(in []pb.MergeMode) ([]domain.MergeMode, error) {
	out := make([]domain.MergeMode, 0, len(in))
	for _, m := range in {
		mode, err := mergeModeFromPB(m)
		if err != nil {
			return nil, err
		}
		if mode == "" {
			return nil, fmt.Errorf("%w: MERGE_MODE_UNSPECIFIED is not a merge mode", domain.ErrInvalid)
		}
		out = append(out, mode)
	}
	return out, nil
}

var scopeToPB = map[domain.Scope]pb.Scope{
	domain.ScopeRepoRead:       pb.Scope_SCOPE_REPO_READ,
	domain.ScopeRepoWrite:      pb.Scope_SCOPE_REPO_WRITE,
	domain.ScopeRepoAdmin:      pb.Scope_SCOPE_REPO_ADMIN,
	domain.ScopeMRWrite:        pb.Scope_SCOPE_MR_WRITE,
	domain.ScopeMRMerge:        pb.Scope_SCOPE_MR_MERGE,
	domain.ScopeCIWrite:        pb.Scope_SCOPE_CI_WRITE,
	domain.ScopeSecretsWrite:   pb.Scope_SCOPE_SECRETS_WRITE,
	domain.ScopeRunnerRegister: pb.Scope_SCOPE_RUNNER_REGISTER,
	domain.ScopeTokenAdmin:     pb.Scope_SCOPE_TOKEN_ADMIN,
}

func scopeFromPB(s pb.Scope) (domain.Scope, error) {
	for d, p := range scopeToPB {
		if p == s {
			return d, nil
		}
	}
	return "", fmt.Errorf("%w: %v", domain.ErrInvalidScope, s)
}

func tokenToPB(t domain.Token) *pb.Token {
	out := &pb.Token{
		Id:         t.ID,
		Label:      t.Label,
		ExpiresAt:  timestamp(t.ExpiresAt),
		CreatedAt:  timestamp(t.CreatedAt),
		LastUsedAt: timestamp(t.LastUsedAt),
	}
	for _, s := range t.Scopes {
		out.Scopes = append(out.Scopes, scopeToPB[s])
	}
	for _, r := range t.Repos {
		out.Repos = append(out.Repos, r.String())
	}
	return out
}

func refKindToPB(k domain.RefKind) pb.RefKind {
	switch k {
	case domain.RefKindBranch:
		return pb.RefKind_REF_KIND_BRANCH
	case domain.RefKindTag:
		return pb.RefKind_REF_KIND_TAG
	case domain.RefKindHead:
		return pb.RefKind_REF_KIND_HEAD
	}
	return pb.RefKind_REF_KIND_UNSPECIFIED
}

func refKindFromPB(k pb.RefKind) (domain.RefKind, error) {
	switch k {
	case pb.RefKind_REF_KIND_BRANCH:
		return domain.RefKindBranch, nil
	case pb.RefKind_REF_KIND_TAG:
		return domain.RefKindTag, nil
	case pb.RefKind_REF_KIND_HEAD:
		return domain.RefKindHead, nil
	}
	return "", fmt.Errorf("%w: %v", domain.ErrInvalidRefKind, k)
}

func refToPB(r domain.Ref) *pb.Ref {
	return &pb.Ref{
		Name:      r.Name,
		ShortName: r.ShortName,
		Kind:      refKindToPB(r.Kind),
		Id:        r.ID.String(),
		CommitId:  r.CommitID.String(),
	}
}

func entryKindToPB(k domain.EntryKind) pb.EntryKind {
	switch k {
	case domain.EntryFile:
		return pb.EntryKind_ENTRY_KIND_FILE
	case domain.EntryExecutable:
		return pb.EntryKind_ENTRY_KIND_EXECUTABLE
	case domain.EntryDirectory:
		return pb.EntryKind_ENTRY_KIND_DIRECTORY
	case domain.EntrySymlink:
		return pb.EntryKind_ENTRY_KIND_SYMLINK
	case domain.EntrySubmodule:
		return pb.EntryKind_ENTRY_KIND_SUBMODULE
	}
	return pb.EntryKind_ENTRY_KIND_UNSPECIFIED
}

func signatureToPB(s domain.Signature) *pb.Signature {
	return &pb.Signature{Name: s.Name, Email: s.Email, Time: timestamp(s.Time)}
}

func commitToPB(c domain.Commit) *pb.Commit {
	out := &pb.Commit{
		Id:        c.ID.String(),
		Author:    signatureToPB(c.Author),
		Committer: signatureToPB(c.Committer),
		Summary:   c.Summary,
		Message:   c.Message,
	}
	for _, p := range c.Parents {
		out.ParentIds = append(out.ParentIds, p.String())
	}
	return out
}

func treeEntryToPB(e domain.TreeEntry) *pb.TreeEntry {
	out := &pb.TreeEntry{
		Name: e.Name,
		Path: e.Path,
		Kind: entryKindToPB(e.Kind),
		Mode: e.Mode,
		Id:   e.ID.String(),
		Size: e.Size,
	}
	if e.LastCommit != nil {
		out.LastCommit = commitToPB(*e.LastCommit)
	}
	return out
}

func blobInfoToPB(b domain.BlobInfo) *pb.BlobInfo {
	return &pb.BlobInfo{
		Id:        b.ID.String(),
		Path:      b.Path,
		Size:      b.Size,
		Binary:    b.Binary,
		MediaType: b.MediaType,
		Mode:      b.Mode,
	}
}

func fileStatusToPB(s domain.FileStatus) pb.FileStatus {
	switch s {
	case domain.FileAdded:
		return pb.FileStatus_FILE_STATUS_ADDED
	case domain.FileDeleted:
		return pb.FileStatus_FILE_STATUS_DELETED
	case domain.FileModified:
		return pb.FileStatus_FILE_STATUS_MODIFIED
	case domain.FileRenamed:
		return pb.FileStatus_FILE_STATUS_RENAMED
	case domain.FileCopied:
		return pb.FileStatus_FILE_STATUS_COPIED
	case domain.FileTypeChanged:
		return pb.FileStatus_FILE_STATUS_TYPE_CHANGED
	}
	return pb.FileStatus_FILE_STATUS_UNSPECIFIED
}

func lineKindToPB(k domain.LineKind) pb.LineKind {
	switch k {
	case domain.LineContext:
		return pb.LineKind_LINE_KIND_CONTEXT
	case domain.LineAddition:
		return pb.LineKind_LINE_KIND_ADDITION
	case domain.LineDeletion:
		return pb.LineKind_LINE_KIND_DELETION
	}
	return pb.LineKind_LINE_KIND_UNSPECIFIED
}

func hunkToPB(h domain.Hunk) *pb.Hunk {
	out := &pb.Hunk{
		OldStart: int32(h.OldStart), OldLines: int32(h.OldLines),
		NewStart: int32(h.NewStart), NewLines: int32(h.NewLines),
		Header: h.Header,
		Lines:  make([]*pb.Line, 0, len(h.Lines)),
	}
	for _, l := range h.Lines {
		out.Lines = append(out.Lines, &pb.Line{
			Kind:           lineKindToPB(l.Kind),
			OldNumber:      int32(l.OldNumber),
			NewNumber:      int32(l.NewNumber),
			Text:           l.Text,
			NoNewlineAtEof: l.NoNewlineAtEOF,
		})
	}
	return out
}

func fileDiffToPB(f domain.FileDiff) *pb.FileDiff {
	out := &pb.FileDiff{
		OldPath: f.OldPath, NewPath: f.NewPath,
		Status:  fileStatusToPB(f.Status),
		OldMode: f.OldMode, NewMode: f.NewMode,
		OldId: f.OldID.String(), NewId: f.NewID.String(),
		Binary:     f.Binary,
		Additions:  int32(f.Additions),
		Deletions:  int32(f.Deletions),
		Similarity: int32(f.Similarity),
		Truncated:  f.Truncated,
	}
	for _, h := range f.Hunks {
		out.Hunks = append(out.Hunks, hunkToPB(h))
	}
	return out
}

func diffStatsToPB(s domain.DiffStats) *pb.DiffStats {
	return &pb.DiffStats{
		FilesChanged: int32(s.FilesChanged),
		Additions:    int32(s.Additions),
		Deletions:    int32(s.Deletions),
	}
}

func diffToPB(d domain.Diff) *pb.Diff {
	out := &pb.Diff{Truncated: d.Truncated}
	for _, f := range d.Files {
		out.Files = append(out.Files, fileDiffToPB(f))
	}
	return out
}

func commitDetailToPB(d domain.CommitDetail) *pb.GetCommitResponse {
	return &pb.GetCommitResponse{
		Commit: commitToPB(d.Commit),
		Stats:  diffStatsToPB(d.Stats),
		Diff:   diffToPB(d.Diff),
	}
}

// oidToPB turns the zero object id of a ref update (no object) into "".
func oidToPB(oid string) string {
	if oid == domain.ZeroOID {
		return ""
	}
	return oid
}

func refUpdatesToPB(updates []domain.RefUpdate) []*pb.RefUpdate {
	out := make([]*pb.RefUpdate, 0, len(updates))
	for _, u := range updates {
		out = append(out, &pb.RefUpdate{Ref: u.Ref, OldId: oidToPB(u.Old), NewId: oidToPB(u.New)})
	}
	return out
}

// watchEventToPB maps one item of a watch; nil when the event has no
// representation in the contract.
func watchEventToPB(we ports.WatchEvent) *pb.Event {
	if we.Gap {
		return &pb.Event{Kind: &pb.Event_Gap{Gap: &pb.Gap{}}}
	}
	out := &pb.Event{Id: we.ID}
	switch ev := we.Event.(type) {
	case domain.RepoCreated:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_RepoCreated{RepoCreated: &pb.RepoCreated{Repo: repoOrName(we, ev.Repo, ev.Visibility)}}
	case domain.RepoUpdated:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_RepoUpdated{RepoUpdated: &pb.RepoUpdated{Repo: repoOrName(we, ev.Repo, "")}}
	case domain.RepoDeleted:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_RepoDeleted{RepoDeleted: &pb.RepoDeleted{Repo: ev.Repo.String()}}
	case domain.PushEvent:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_Pushed{Pushed: &pb.Pushed{Repo: ev.Repo.String(), RefUpdates: refUpdatesToPB(ev.Updates)}}
	case domain.MergeRequestOpened:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestOpened{MergeRequestOpened: &pb.MergeRequestOpened{MergeRequest: mergeRequestToPB(ev.MergeRequest)}}
	case domain.MergeRequestMerged:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestMerged{MergeRequestMerged: &pb.MergeRequestMerged{MergeRequest: mergeRequestToPB(ev.MergeRequest)}}
	case domain.MergeRequestClosed:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestClosed{MergeRequestClosed: &pb.MergeRequestClosed{MergeRequest: mergeRequestToPB(ev.MergeRequest)}}
	case domain.CommentAdded:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_CommentAdded{CommentAdded: &pb.CommentAdded{Repo: ev.Repo.String(), Number: ev.Number, Comment: commentToPB(ev.Comment)}}
	default:
		return nil
	}
	return out
}

func repoOrName(we ports.WatchEvent, name domain.RepoName, vis domain.Visibility) *pb.Repo {
	if we.Repo != nil {
		return repoToPB(*we.Repo)
	}
	return &pb.Repo{Name: name.String(), Visibility: visibilityToPB(vis)}
}

// passkeysToPB converts passkeys; the credential id is base64url without
// padding, the form RemovePasskey takes back. Key material never leaves.
func passkeysToPB(list []domain.Passkey) []*pb.Passkey {
	out := make([]*pb.Passkey, 0, len(list))
	for _, p := range list {
		out = append(out, &pb.Passkey{
			Id:         base64.RawURLEncoding.EncodeToString(p.ID),
			Name:       p.Name,
			CreatedAt:  timestamp(p.CreatedAt),
			LastUsedAt: timestamp(p.LastUsedAt),
			Synced:     p.BackupState,
		})
	}
	return out
}
