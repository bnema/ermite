// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the merge requests. Repository names hold no '/', so a prefix
// ending in "/" never matches another repository; numbers and comment ids are
// zero-padded so that keys sort numerically.
const (
	mrPrefix      = "mr/"     // mr/<repo>/<number:010d>
	commentPrefix = "mrc/"    // mrc/<repo>/<number:010d>/<id:010d>
	readPrefix    = "mrread/" // mrread/<repo>/<number:010d>
	seqPrefix     = "mrseq/"  // mrseq/<repo>
	numberFormat  = "%010d"
	createTries   = 10
)

// MergeRequests is the ermite-kv backed ports.MergeRequestStore. It shares the
// database of the Store it comes from.
type MergeRequests struct {
	db *kv.DB
}

var _ ports.MergeRequestStore = (*MergeRequests)(nil)

// MergeRequests returns the merge request store over the same database. It is
// valid until the Store is closed.
func (s *Store) MergeRequests() *MergeRequests { return &MergeRequests{db: s.db} }

func mrRepoPrefix(repo domain.RepoName) string { return mrPrefix + repo.String() + "/" }
func mrKey(repo domain.RepoName, n int64) []byte {
	return []byte(mrRepoPrefix(repo) + fmt.Sprintf(numberFormat, n))
}
func commentsPrefix(repo domain.RepoName, n int64) string {
	return commentPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n) + "/"
}
func commentKey(repo domain.RepoName, n, id int64) []byte {
	return []byte(commentsPrefix(repo, n) + fmt.Sprintf(numberFormat, id))
}
func readKey(repo domain.RepoName, n int64) []byte {
	return []byte(readPrefix + repo.String() + "/" + fmt.Sprintf(numberFormat, n))
}
func seqKey(repo domain.RepoName) []byte { return []byte(seqPrefix + repo.String()) }

// mergeRequestKeys lists every key of the snapshot that belongs to the
// repository's merge requests.
func mergeRequestKeys(ctx context.Context, snap *kv.Snapshot, repo domain.RepoName) [][]byte {
	var keys [][]byte
	for _, p := range []string{mrRepoPrefix(repo), commentPrefix + repo.String() + "/", readPrefix + repo.String() + "/"} {
		for k := range snap.Prefix(ctx, []byte(p)) {
			keys = append(keys, slices.Clone(k))
		}
	}
	if _, ok := snap.Get(ctx, seqKey(repo)); ok {
		keys = append(keys, seqKey(repo))
	}
	return keys
}

// guardRepo is guardRepoKey on the database of the merge requests; see there.
func (m *MergeRequests) guardRepo(ctx context.Context, b *kv.Batch, repo domain.RepoName) error {
	return guardRepoKey(ctx, m.db, b, repo)
}

// Create takes the next number of the repository from mrseq/<repo> and stores
// the merge request under it, in one batch that requires the sequence to be
// unchanged since it was read: two concurrent creations never share a number.
// The batch also requires the repository to be unchanged since it was read and
// rewrites it (see guardRepo), so that a repository deleted meanwhile is not
// left with a stray merge request (a push recording pushed_at makes it retry). A missing repository gives
// domain.ErrNotFound.
//
//hexcheck:ignore suspicious-business-logic-in-adapter takes the next number under a version precondition: a storage concern, no policy
func (m *MergeRequests) Create(ctx context.Context, mr domain.MergeRequest) (domain.MergeRequest, error) {
	for range createTries {
		repoItem, repoOK := m.db.Get(ctx, repoKey(mr.Repo))
		if !repoOK {
			return domain.MergeRequest{}, domain.ErrNotFound
		}
		var last int64
		item, ok := m.db.Get(ctx, seqKey(mr.Repo))
		if ok {
			var seq storagepb.Sequence
			if err := proto.Unmarshal(item.Value, &seq); err != nil {
				return domain.MergeRequest{}, fmt.Errorf("decode merge request sequence of %q: %w", mr.Repo, err)
			}
			last = seq.GetLast()
		}
		mr.Number = last + 1
		val, err := encodeMergeRequest(mr)
		if err != nil {
			return domain.MergeRequest{}, err
		}
		var b kv.Batch
		b.RequireVersion(repoKey(mr.Repo), repoItem.Version)
		b.Put(repoKey(mr.Repo), repoItem.Value) // see guardRepo
		if ok {
			b.RequireVersion(seqKey(mr.Repo), item.Version)
		} else {
			b.RequireAbsent(seqKey(mr.Repo))
		}
		b.Put(seqKey(mr.Repo), mustMarshal(&storagepb.Sequence{Last: mr.Number}))
		b.RequireAbsent(mrKey(mr.Repo, mr.Number))
		b.Put(mrKey(mr.Repo, mr.Number), val)
		_, err = m.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return domain.MergeRequest{}, fmt.Errorf("create merge request of %q: %w", mr.Repo, err)
		}
		return m.Get(ctx, mr.Repo, mr.Number)
	}
	return domain.MergeRequest{}, fmt.Errorf("create merge request of %q: %w", mr.Repo, domain.ErrConflict)
}

// Get reads mr/<repo>/<number>.
func (m *MergeRequests) Get(ctx context.Context, repo domain.RepoName, number int64) (domain.MergeRequest, error) {
	if number < 1 {
		return domain.MergeRequest{}, domain.ErrNotFound
	}
	item, ok := m.db.Get(ctx, mrKey(repo, number))
	if !ok {
		return domain.MergeRequest{}, domain.ErrNotFound
	}
	return decodeMergeRequest(repo, item)
}

// List returns the merge requests of the repository, highest number first.
func (m *MergeRequests) List(ctx context.Context, repo domain.RepoName) ([]domain.MergeRequest, error) {
	var out []domain.MergeRequest
	for key, item := range m.db.Snapshot(ctx).Prefix(ctx, []byte(mrRepoPrefix(repo))) {
		mr, err := decodeMergeRequest(repo, item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, mr)
	}
	slices.Reverse(out)
	return out, nil
}

// Update replaces the merge request with a version precondition.
//
//hexcheck:ignore suspicious-business-logic-in-adapter port method name only
func (m *MergeRequests) Update(ctx context.Context, mr domain.MergeRequest) error {
	val, err := encodeMergeRequest(mr)
	if err != nil {
		return err
	}
	key := mrKey(mr.Repo, mr.Number)
	if _, ok := m.db.Get(ctx, key); !ok {
		return domain.ErrNotFound
	}
	if mr.Version == 0 {
		return fmt.Errorf("update merge request %d of %q: read it first: %w", mr.Number, mr.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	b.RequireVersion(key, mr.Version)
	b.Put(key, val)
	if _, err := m.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("update merge request %d of %q: %w", mr.Number, mr.Repo, err)
	}
	return nil
}

// ListComments returns the comments of the merge request, oldest first.
func (m *MergeRequests) ListComments(ctx context.Context, repo domain.RepoName, number int64) ([]domain.Comment, error) {
	var out []domain.Comment
	for key, item := range m.db.Snapshot(ctx).Prefix(ctx, []byte(commentsPrefix(repo, number))) {
		c, err := decodeComment(item)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// GetComment reads one comment.
func (m *MergeRequests) GetComment(ctx context.Context, repo domain.RepoName, number, id int64) (domain.Comment, error) {
	if number < 1 || id < 1 {
		return domain.Comment{}, domain.ErrNotFound
	}
	item, ok := m.db.Get(ctx, commentKey(repo, number, id))
	if !ok {
		return domain.Comment{}, domain.ErrNotFound
	}
	return decodeComment(item)
}

// SaveComment writes the comment and the merge request in one batch, each with
// its version precondition (absent for a new comment), and requires the
// repository to exist (see guardRepo): domain.ErrNotFound otherwise.
func (m *MergeRequests) SaveComment(ctx context.Context, mr domain.MergeRequest, c domain.Comment) error {
	mrVal, err := encodeMergeRequest(mr)
	if err != nil {
		return err
	}
	cVal, err := encodeComment(c)
	if err != nil {
		return err
	}
	if mr.Version == 0 {
		return fmt.Errorf("save comment on %d of %q: read the merge request first: %w", mr.Number, mr.Repo, domain.ErrConflict)
	}
	var b kv.Batch
	if err := m.guardRepo(ctx, &b, mr.Repo); err != nil {
		return err
	}
	b.RequireVersion(mrKey(mr.Repo, mr.Number), mr.Version)
	b.Put(mrKey(mr.Repo, mr.Number), mrVal)
	if c.Version == 0 {
		b.RequireAbsent(commentKey(mr.Repo, mr.Number, c.ID))
	} else {
		b.RequireVersion(commentKey(mr.Repo, mr.Number, c.ID), c.Version)
	}
	b.Put(commentKey(mr.Repo, mr.Number, c.ID), cVal)
	if _, err := m.db.Apply(ctx, &b); err != nil {
		if errors.Is(err, kv.ErrConflict) {
			return domain.ErrConflict
		}
		return fmt.Errorf("save comment on %d of %q: %w", mr.Number, mr.Repo, err)
	}
	return nil
}

// GetRead reads mrread/<repo>/<number>: the id of a commit, or nothing.
func (m *MergeRequests) GetRead(ctx context.Context, repo domain.RepoName, number int64) (domain.ObjectID, error) {
	item, ok := m.db.Get(ctx, readKey(repo, number))
	if !ok {
		return domain.ObjectID{}, nil
	}
	id, err := domain.ParseObjectID(string(item.Value))
	if err != nil {
		return domain.ObjectID{}, fmt.Errorf("decode read position of %d in %q: %w", number, repo, err)
	}
	return id, nil
}

// SetRead writes mrread/<repo>/<number>. The repository must exist (see
// guardRepo): domain.ErrNotFound otherwise.
//
//hexcheck:ignore suspicious-business-logic-in-adapter retries a write that lost a race with the repository key: a storage concern, no policy
func (m *MergeRequests) SetRead(ctx context.Context, repo domain.RepoName, number int64, head domain.ObjectID) error {
	if head.IsZero() {
		return fmt.Errorf("%w: empty read position", domain.ErrInvalidObjectID)
	}
	for range createTries {
		var b kv.Batch
		if err := m.guardRepo(ctx, &b, repo); err != nil {
			return err
		}
		b.Put(readKey(repo, number), []byte(head.String()))
		_, err := m.db.Apply(ctx, &b)
		if errors.Is(err, kv.ErrConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("record read position of %d in %q: %w", number, repo, err)
		}
		return nil
	}
	return fmt.Errorf("record read position of %d in %q: %w", number, repo, domain.ErrConflict)
}

func encodeMergeRequest(mr domain.MergeRequest) ([]byte, error) {
	if mr.Repo.IsZero() {
		return nil, fmt.Errorf("%w: empty name", domain.ErrInvalidName)
	}
	if !utf8.ValidString(mr.Author) {
		return nil, errors.New("merge request author is not valid UTF-8")
	}
	state, err := encodeState(mr.State)
	if err != nil {
		return nil, err
	}
	mode, err := encodeMode(mr.MergeMode)
	if err != nil {
		return nil, err
	}
	return mustMarshal(&storagepb.MergeRequest{
		Number:         mr.Number,
		Title:          mr.Title.String(),
		Description:    mr.Description.String(),
		SourceBranch:   mr.Source,
		TargetBranch:   mr.Target,
		State:          state,
		Author:         mr.Author,
		CreatedAt:      encodeTime(mr.CreatedAt),
		UpdatedAt:      encodeTime(mr.UpdatedAt),
		HeadId:         mr.HeadID.String(),
		BaseId:         mr.BaseID.String(),
		MergedCommitId: mr.MergedCommitID.String(),
		MergeMode:      mode,
		CommentSeq:     mr.CommentSeq,
	}), nil
}

//hexcheck:ignore suspicious-business-logic-in-adapter schema decoding, not a policy
func decodeMergeRequest(repo domain.RepoName, item kv.Item) (domain.MergeRequest, error) {
	var pb storagepb.MergeRequest
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.MergeRequest{}, fmt.Errorf("decode merge request: %w", err)
	}
	fail := func(err error) (domain.MergeRequest, error) {
		return domain.MergeRequest{}, fmt.Errorf("decode merge request %d of %q: %w", pb.GetNumber(), repo, err)
	}
	title, err := domain.ParseMRTitle(pb.GetTitle())
	if err != nil {
		return fail(err)
	}
	desc, err := domain.ParsePlainText(pb.GetDescription())
	if err != nil {
		return fail(err)
	}
	state, err := decodeState(pb.GetState())
	if err != nil {
		return fail(err)
	}
	mr := domain.MergeRequest{
		Repo: repo, Number: pb.GetNumber(), Title: title, Description: desc,
		Source: pb.GetSourceBranch(), Target: pb.GetTargetBranch(), State: state,
		Author:    pb.GetAuthor(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), UpdatedAt: decodeTime(pb.GetUpdatedAt()),
		MergeMode:  decodeMode(pb.GetMergeMode()),
		CommentSeq: pb.GetCommentSeq(),
		Version:    item.Version,
	}
	for _, f := range []struct {
		dst *domain.ObjectID
		src string
	}{{&mr.HeadID, pb.GetHeadId()}, {&mr.BaseID, pb.GetBaseId()}, {&mr.MergedCommitID, pb.GetMergedCommitId()}} {
		if f.src == "" {
			continue
		}
		if *f.dst, err = domain.ParseObjectID(f.src); err != nil {
			return fail(err)
		}
	}
	return mr, nil
}

func encodeComment(c domain.Comment) ([]byte, error) {
	side, err := encodeSide(c.Side)
	if err != nil {
		return nil, err
	}
	return mustMarshal(&storagepb.Comment{
		Id:        c.ID,
		Author:    c.Author,
		Path:      c.Path,
		Line:      int32(c.Line), //nolint:gosec // validated against the diff, far below 2^31
		Side:      side,
		CommitId:  c.CommitID.String(),
		Text:      c.Text.String(),
		Resolved:  c.Resolved,
		CreatedAt: encodeTime(c.CreatedAt),
	}), nil
}

//hexcheck:ignore suspicious-business-logic-in-adapter schema decoding, not a policy
func decodeComment(item kv.Item) (domain.Comment, error) {
	var pb storagepb.Comment
	if err := proto.Unmarshal(item.Value, &pb); err != nil {
		return domain.Comment{}, fmt.Errorf("decode comment: %w", err)
	}
	text, err := domain.ParsePlainText(pb.GetText())
	if err != nil {
		return domain.Comment{}, fmt.Errorf("decode comment %d: %w", pb.GetId(), err)
	}
	c := domain.Comment{
		ID: pb.GetId(), Author: pb.GetAuthor(), Path: pb.GetPath(), Line: int(pb.GetLine()),
		Side: decodeSide(pb.GetSide()), Text: text, Resolved: pb.GetResolved(),
		CreatedAt: decodeTime(pb.GetCreatedAt()), Version: item.Version,
	}
	if pb.GetCommitId() != "" {
		if c.CommitID, err = domain.ParseObjectID(pb.GetCommitId()); err != nil {
			return domain.Comment{}, fmt.Errorf("decode comment %d: %w", c.ID, err)
		}
	}
	return c, nil
}

func encodeState(s domain.MRState) (storagepb.MergeRequestState, error) {
	switch s {
	case domain.MRStateOpen:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN, nil
	case domain.MRStateMerged:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_MERGED, nil
	case domain.MRStateClosed:
		return storagepb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED, nil
	}
	return 0, fmt.Errorf("%w: merge request state %q", domain.ErrInvalid, s)
}

func decodeState(s storagepb.MergeRequestState) (domain.MRState, error) {
	switch s {
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_OPEN:
		return domain.MRStateOpen, nil
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_MERGED:
		return domain.MRStateMerged, nil
	case storagepb.MergeRequestState_MERGE_REQUEST_STATE_CLOSED:
		return domain.MRStateClosed, nil
	}
	return "", fmt.Errorf("unknown merge request state %d", s)
}

func encodeMode(m domain.MergeMode) (storagepb.MergeMode, error) {
	switch m {
	case "":
		return storagepb.MergeMode_MERGE_MODE_UNSPECIFIED, nil
	case domain.MergeModeFastForward:
		return storagepb.MergeMode_MERGE_MODE_FAST_FORWARD, nil
	case domain.MergeModeSquash:
		return storagepb.MergeMode_MERGE_MODE_SQUASH, nil
	}
	return 0, fmt.Errorf("%w: merge mode %q", domain.ErrInvalid, m)
}

// decodeMode maps an unknown mode to none: it only labels a merged request.
func decodeMode(m storagepb.MergeMode) domain.MergeMode {
	switch m {
	case storagepb.MergeMode_MERGE_MODE_FAST_FORWARD:
		return domain.MergeModeFastForward
	case storagepb.MergeMode_MERGE_MODE_SQUASH:
		return domain.MergeModeSquash
	}
	return ""
}

func encodeSide(s domain.DiffSide) (storagepb.DiffSide, error) {
	switch s {
	case domain.SideNone:
		return storagepb.DiffSide_DIFF_SIDE_UNSPECIFIED, nil
	case domain.SideOld:
		return storagepb.DiffSide_DIFF_SIDE_OLD, nil
	case domain.SideNew:
		return storagepb.DiffSide_DIFF_SIDE_NEW, nil
	}
	return 0, fmt.Errorf("%w: diff side %q", domain.ErrInvalid, s)
}

func decodeSide(s storagepb.DiffSide) domain.DiffSide {
	switch s {
	case storagepb.DiffSide_DIFF_SIDE_OLD:
		return domain.SideOld
	case storagepb.DiffSide_DIFF_SIDE_NEW:
		return domain.SideNew
	}
	return domain.SideNone
}

func encodeSettings(s domain.RepoSettings) (*storagepb.RepoSettings, error) {
	pb := &storagepb.RepoSettings{FailingChecksBlock: s.FailingChecksBlock}
	for _, m := range s.AllowedMergeModes {
		mode, err := encodeMode(m)
		if err != nil || mode == storagepb.MergeMode_MERGE_MODE_UNSPECIFIED {
			return nil, fmt.Errorf("%w: merge mode %q in the settings", domain.ErrInvalid, m)
		}
		pb.AllowedMergeModes = append(pb.AllowedMergeModes, mode)
	}
	return pb, nil
}

// decodeSettings returns the defaults for a repository stored before merge
// requests existed, and for settings left with no mode that this build knows.
//
//hexcheck:ignore suspicious-business-logic-in-adapter schema decoding with the documented defaults
func decodeSettings(pb *storagepb.RepoSettings) domain.RepoSettings {
	if pb == nil {
		return domain.DefaultRepoSettings()
	}
	var modes []domain.MergeMode
	for _, m := range pb.GetAllowedMergeModes() {
		if mode := decodeMode(m); mode != "" {
			modes = append(modes, mode)
		}
	}
	if len(modes) == 0 {
		return domain.DefaultRepoSettings()
	}
	normalized, err := domain.NormalizeMergeModes(modes)
	if err != nil {
		return domain.DefaultRepoSettings()
	}
	return domain.RepoSettings{AllowedMergeModes: normalized, FailingChecksBlock: pb.GetFailingChecksBlock()}
}
