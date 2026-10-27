// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"encoding/base64"
	"fmt"
	"slices"
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
	settings := v.EffectiveSettings()
	return &pb.Repo{
		Name:          v.Name.String(),
		Description:   v.Description,
		Topics:        v.Topics,
		Visibility:    visibilityToPB(v.Visibility),
		DefaultBranch: v.DefaultBranch,
		Archived:      v.Archived,
		CreatedAt:     timestamp(v.CreatedAt),
		PushedAt:      timestamp(v.PushedAt),
		Settings:      settingsToPB(settings),
	}
}

func settingsToPB(s domain.RepoSettings) *pb.RepoSettings {
	out := &pb.RepoSettings{CiAutoCancel: !s.CIAutoCancelDisabled, AllowAutoMerge: s.AllowAutoMerge, Issues: issuesModeToPB(s.Issues)}
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
	case domain.MergeModeMerge:
		return pb.MergeMode_MERGE_MODE_MERGE_COMMIT
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
	case pb.MergeMode_MERGE_MODE_MERGE_COMMIT:
		return domain.MergeModeMerge, nil
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
	domain.ScopeMRReview:       pb.Scope_SCOPE_MR_REVIEW,
	domain.ScopeRepoRead:       pb.Scope_SCOPE_REPO_READ,
	domain.ScopeRepoWrite:      pb.Scope_SCOPE_REPO_WRITE,
	domain.ScopeRepoAdmin:      pb.Scope_SCOPE_REPO_ADMIN,
	domain.ScopeMRWrite:        pb.Scope_SCOPE_MR_WRITE,
	domain.ScopeMRMerge:        pb.Scope_SCOPE_MR_MERGE,
	domain.ScopeMRForceMerge:   pb.Scope_SCOPE_MR_FORCE_MERGE,
	domain.ScopeCIWrite:        pb.Scope_SCOPE_CI_WRITE,
	domain.ScopeSecretsWrite:   pb.Scope_SCOPE_SECRETS_WRITE,
	domain.ScopeRunnerRegister: pb.Scope_SCOPE_RUNNER_REGISTER,
	domain.ScopeTokenAdmin:     pb.Scope_SCOPE_TOKEN_ADMIN,
	domain.ScopeIssueWrite:     pb.Scope_SCOPE_ISSUE_WRITE,
	domain.ScopeReleaseWrite:   pb.Scope_SCOPE_RELEASE_WRITE,
	domain.ScopeJournalRead:    pb.Scope_SCOPE_JOURNAL_READ,
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
	out.BranchPrefixes = slices.Clone(t.BranchPrefixes)
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
	if c.Verification.Status != domain.StatusUnchecked {
		out.Verification = verificationToPB(c.Verification)
	}
	return out
}

func verificationToPB(v domain.CommitVerification) *pb.CommitVerification {
	return &pb.CommitVerification{
		Format:      signatureFormatToPB(v.Format),
		Status:      signatureStatusToPB(v.Status),
		Fingerprint: v.Fingerprint,
		KeyId:       v.KeyID,
		KeyTitle:    v.KeyTitle,
	}
}

func signatureFormatToPB(f domain.SignatureFormat) pb.SignatureFormat {
	switch f {
	case domain.FormatSSH:
		return pb.SignatureFormat_SIGNATURE_FORMAT_SSH
	case domain.FormatGPG:
		return pb.SignatureFormat_SIGNATURE_FORMAT_GPG
	case domain.FormatX509:
		return pb.SignatureFormat_SIGNATURE_FORMAT_X509
	case domain.FormatNone:
		return pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED
	}
	return pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED
}

// signatureFormatFromPB maps the format of a key to add: only SSH and GPG keys
// can be trusted.
func signatureFormatFromPB(f pb.SignatureFormat) (domain.SignatureFormat, error) {
	switch f {
	case pb.SignatureFormat_SIGNATURE_FORMAT_SSH:
		return domain.FormatSSH, nil
	case pb.SignatureFormat_SIGNATURE_FORMAT_GPG:
		return domain.FormatGPG, nil
	case pb.SignatureFormat_SIGNATURE_FORMAT_UNSPECIFIED, pb.SignatureFormat_SIGNATURE_FORMAT_X509:
	}
	return domain.FormatNone, fmt.Errorf("%w: key format must be ssh or gpg", domain.ErrInvalid)
}

func signatureStatusToPB(s domain.SignatureStatus) pb.SignatureStatus {
	switch s {
	case domain.StatusUnsigned:
		return pb.SignatureStatus_SIGNATURE_STATUS_UNSIGNED
	case domain.StatusVerified:
		return pb.SignatureStatus_SIGNATURE_STATUS_VERIFIED
	case domain.StatusUnknownKey:
		return pb.SignatureStatus_SIGNATURE_STATUS_UNKNOWN_KEY
	case domain.StatusBadSignature:
		return pb.SignatureStatus_SIGNATURE_STATUS_BAD_SIGNATURE
	case domain.StatusEmailMismatch:
		return pb.SignatureStatus_SIGNATURE_STATUS_EMAIL_MISMATCH
	case domain.StatusExpired:
		return pb.SignatureStatus_SIGNATURE_STATUS_EXPIRED
	case domain.StatusRevoked:
		return pb.SignatureStatus_SIGNATURE_STATUS_REVOKED
	case domain.StatusUnsupported:
		return pb.SignatureStatus_SIGNATURE_STATUS_UNSUPPORTED
	case domain.StatusUnchecked:
		return pb.SignatureStatus_SIGNATURE_STATUS_UNSPECIFIED
	}
	return pb.SignatureStatus_SIGNATURE_STATUS_UNSPECIFIED
}

func trustedKeyToPB(k domain.TrustedKey) *pb.TrustedKey {
	return &pb.TrustedKey{
		Id:        k.ID,
		Format:    signatureFormatToPB(k.Format),
		Title:     k.Title,
		PublicKey: k.PublicKey,
		Emails:    slices.Clone(k.Emails),
		CreatedAt: timestamp(k.CreatedAt),
		Builtin:   k.Builtin,
	}
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
		Truncated:  f.Omitted != domain.OmitNone,
		Omitted:    fileOmissionToPB(f.Omitted),
	}
	for _, h := range f.Hunks {
		out.Hunks = append(out.Hunks, hunkToPB(h))
	}
	return out
}

func fileOmissionToPB(o domain.FileOmission) pb.FileOmission {
	switch o {
	case domain.OmitGenerated:
		return pb.FileOmission_FILE_OMISSION_GENERATED
	case domain.OmitLarge:
		return pb.FileOmission_FILE_OMISSION_LARGE
	case domain.OmitBudget:
		return pb.FileOmission_FILE_OMISSION_BUDGET
	case domain.OmitUnavailable:
		return pb.FileOmission_FILE_OMISSION_UNAVAILABLE
	}
	return pb.FileOmission_FILE_OMISSION_UNSPECIFIED
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
		out.Truncated = out.Truncated || f.Omitted != domain.OmitNone
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
		out.Kind = &pb.Event_MergeRequestMerged{MergeRequestMerged: &pb.MergeRequestMerged{MergeRequest: mergeRequestToPB(ev.MergeRequest), Auto: ev.Auto}}
	case domain.MergeRequestAutoMergeEnabled:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestAutoMergeEnabled{MergeRequestAutoMergeEnabled: &pb.MergeRequestAutoMergeEnabled{MergeRequest: mergeRequestToPB(ev.MergeRequest)}}
	case domain.MergeRequestAutoMergeCancelled:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestAutoMergeCancelled{MergeRequestAutoMergeCancelled: &pb.MergeRequestAutoMergeCancelled{MergeRequest: mergeRequestToPB(ev.MergeRequest), Reason: ev.Reason}}
	case domain.MergeRequestClosed:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestClosed{MergeRequestClosed: &pb.MergeRequestClosed{MergeRequest: mergeRequestToPB(ev.MergeRequest)}}
	case domain.CommentAdded:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_CommentAdded{CommentAdded: &pb.CommentAdded{Repo: ev.Repo.String(), Number: ev.Number, Comment: commentToPB(ev.Comment)}}
	case domain.IssueOpened:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueOpened{IssueOpened: &pb.IssueOpened{Issue: issueToPB(ev.Issue)}}
	case domain.IssueResolved:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueResolved{IssueResolved: &pb.IssueResolved{Issue: issueToPB(ev.Issue)}}
	case domain.IssueClosed:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueClosed{IssueClosed: &pb.IssueClosed{Issue: issueToPB(ev.Issue)}}
	case domain.IssueReopened:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueReopened{IssueReopened: &pb.IssueReopened{Issue: issueToPB(ev.Issue)}}
	case domain.IssueEdited:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueEdited{IssueEdited: &pb.IssueEdited{Issue: issueToPB(ev.Issue), ChangedFields: ev.ChangedFields}}
	case domain.MergeRequestEdited:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestEdited{MergeRequestEdited: &pb.MergeRequestEdited{MergeRequest: mergeRequestToPB(ev.MergeRequest), ChangedFields: ev.ChangedFields}}
	case domain.IssueCommented:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueCommented{IssueCommented: &pb.IssueCommented{Repo: ev.Repo.String(), Number: ev.Number, Comment: issueCommentToPB(ev.Comment)}}
	case domain.IssueSubmitted:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_IssueSubmitted{IssueSubmitted: &pb.IssueSubmitted{Repo: ev.Repo.String(), Id: ev.ID.String()}}
	case domain.DependencyUpdatesFound:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_DependencyUpdatesFound{DependencyUpdatesFound: &pb.DependencyUpdatesFound{Repo: ev.Repo.String(), Count: int32(ev.Count)}}
	case domain.CommentEdited:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_CommentEdited{CommentEdited: &pb.CommentEdited{Repo: ev.Repo.String(), Number: ev.Number, Comment: commentToPB(ev.Comment)}}
	case domain.CommentDeleted:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_CommentDeleted{CommentDeleted: &pb.CommentDeleted{Repo: ev.Repo.String(), Number: ev.Number, Comment: commentToPB(ev.Comment)}}
	case domain.CommentResolved:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_CommentResolved{CommentResolved: &pb.CommentResolved{Repo: ev.Repo.String(), Number: ev.Number, Comment: commentToPB(ev.Comment)}}
	case domain.MergeRequestMergeability:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MergeRequestMergeability{MergeRequestMergeability: &pb.MergeRequestMergeability{Repo: ev.Repo.String(), Number: ev.Number, Mergeability: mergeabilityToPB(ev.Mergeability)}}
	case domain.RunUpdated:
		out.Time = timestamp(ev.At)
		out.Kind = runEventToPB(ev)
	case domain.JobUpdated:
		out.Time = timestamp(ev.At)
		out.Kind = jobEventToPB(ev)
	case domain.MirrorFailed:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MirrorFailed{MirrorFailed: &pb.MirrorFailed{Repo: ev.Repo.String(), State: mirrorStateToPB(ev.State), FailureClass: string(ev.Class)}}
	case domain.MirrorRecovered:
		out.Time = timestamp(ev.At)
		out.Kind = &pb.Event_MirrorRecovered{MirrorRecovered: &pb.MirrorRecovered{Repo: ev.Repo.String()}}
	default:
		// Release lifecycle conversions live alongside the event service.
		// Web-only events have no representation in the API.
		return releaseEventToPB(we)
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

func releaseToPB(r domain.Release) *pb.Release {
	return &pb.Release{
		Id: r.ID, Repo: r.Repo.String(), Tag: r.Tag, Title: r.Title, Notes: r.Notes,
		Draft: r.Draft, Prerelease: r.Prerelease, TargetCommitish: r.TargetCommitish, Author: r.Author,
		CreatedAt: timestamp(r.CreatedAt), PublishedAt: timestamp(r.PublishedAt), UpdatedAt: timestamp(r.UpdatedAt),
	}
}

func assetToPB(a domain.ReleaseAsset) *pb.ReleaseAsset {
	return &pb.ReleaseAsset{
		Id: a.ID, ReleaseId: a.ReleaseID, Name: a.Name, Label: a.Label, ContentType: a.ContentType,
		Size: a.Size, Digest: a.Digest, Uploader: a.Uploader,
		CreatedAt: timestamp(a.CreatedAt), UpdatedAt: timestamp(a.UpdatedAt),
	}
}

func assetsToPB(assets []domain.ReleaseAsset) []*pb.ReleaseAsset {
	out := make([]*pb.ReleaseAsset, 0, len(assets))
	for _, a := range assets {
		out = append(out, assetToPB(a))
	}
	return out
}
