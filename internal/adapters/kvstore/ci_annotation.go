// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	kv "bnema.dev/ermite-kv"
	"google.golang.org/protobuf/proto"

	storagepb "bnema.dev/ermite/gen/ermite/storage"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Key layout of the annotations, written by CI.Apply with the job they belong
// to, under the lock of its run:
//
//	annot/<run:020d>/<attempt:010d>/<job id>/<step:010d>   CIStepAnnotations
//
// The attempt is the one in which the job executed (CIJob.Attempt, the one of
// its log), not the attempt of the run that keeps the record. One value holds
// all the annotations of one step, so that a report replaces them whole; a
// step without annotation has no key.
const annotPrefix = "annot/"

func annotRunPrefix(runID int64) string { return fmt.Sprintf("%s%020d/", annotPrefix, runID) }
func annotJobPrefix(runID int64, attempt int32, jobID string) string {
	return fmt.Sprintf("%s%010d/%s/", annotRunPrefix(runID), attempt, jobID)
}
func annotKey(runID int64, attempt int32, jobID string, step int32) []byte {
	return []byte(fmt.Sprintf("%s%010d", annotJobPrefix(runID, attempt, jobID), step))
}

var annotationLevels = []struct {
	d domain.AnnotationLevel
	p storagepb.CIAnnotationLevel
}{
	{domain.AnnotationNotice, storagepb.CIAnnotationLevel_CI_ANNOTATION_LEVEL_NOTICE},
	{domain.AnnotationWarning, storagepb.CIAnnotationLevel_CI_ANNOTATION_LEVEL_WARNING},
	{domain.AnnotationFailure, storagepb.CIAnnotationLevel_CI_ANNOTATION_LEVEL_FAILURE},
}

func encodeAnnotationLevel(l domain.AnnotationLevel) storagepb.CIAnnotationLevel {
	for _, e := range annotationLevels {
		if e.d == l {
			return e.p
		}
	}
	return storagepb.CIAnnotationLevel_CI_ANNOTATION_LEVEL_UNSPECIFIED
}

func decodeAnnotationLevel(l storagepb.CIAnnotationLevel) domain.AnnotationLevel {
	for _, e := range annotationLevels {
		if e.p == l {
			return e.d
		}
	}
	return domain.AnnotationNotice
}

func encodeAnnotations(items []domain.CIAnnotation) []byte {
	pb := &storagepb.CIStepAnnotations{Annotations: make([]*storagepb.CIAnnotation, 0, len(items))}
	for _, a := range items {
		pb.Annotations = append(pb.Annotations, &storagepb.CIAnnotation{
			Level: encodeAnnotationLevel(a.Level), Title: a.Title, Message: a.Message, File: a.File,
			StartLine: a.StartLine, EndLine: a.EndLine, StartColumn: a.StartColumn, EndColumn: a.EndColumn,
		})
	}
	return mustMarshal(pb)
}

func decodeAnnotations(val []byte, step int32) ([]domain.CIAnnotation, error) {
	var pb storagepb.CIStepAnnotations
	if err := proto.Unmarshal(val, &pb); err != nil {
		return nil, fmt.Errorf("decode annotations: %w", err)
	}
	out := make([]domain.CIAnnotation, 0, len(pb.GetAnnotations()))
	for _, a := range pb.GetAnnotations() {
		out = append(out, domain.CIAnnotation{
			Level: decodeAnnotationLevel(a.GetLevel()), Title: a.GetTitle(), Message: a.GetMessage(), File: a.GetFile(),
			StartLine: a.GetStartLine(), EndLine: a.GetEndLine(), StartColumn: a.GetStartColumn(), EndColumn: a.GetEndColumn(),
			Step: step,
		})
	}
	return out, nil
}

// stageAnnotations stages the deletion of the annotations of the executions to
// clear, then the replacement of the annotations of the steps to set.
//
// Annotations normally travel with the write of their job, whose version
// precondition fails once the job is gone; guarding the repository key as well
// would make the jobs of one repository conflict with one another at every
// report. Only a batch without a job guards it. A report that races the
// deletion of its repository can leave its annotation keys behind, under a run
// id that is never reused: harmless, and a few kilobytes at most.
func (c *CI) stageAnnotations(ctx context.Context, b *kv.Batch, w ports.CIWrite, guarded map[domain.RepoName]bool) {
	if len(w.ClearAnnotations) > 0 {
		snap := c.db.Snapshot(ctx)
		for _, e := range w.ClearAnnotations {
			for key := range snap.Prefix(ctx, []byte(annotJobPrefix(e.RunID, e.Attempt, e.JobID))) {
				b.Delete(slices.Clone(key))
			}
		}
	}
	for _, s := range w.Annotations {
		key := annotKey(s.Exec.RunID, s.Exec.Attempt, s.Exec.JobID, s.Step)
		if len(s.Items) == 0 {
			b.Delete(key)
			continue
		}
		b.Put(key, encodeAnnotations(s.Items))
		if len(w.Jobs) == 0 {
			guarded[s.Repo] = true
		}
	}
}

// JobAnnotations returns the annotations of one execution of a job, ordered by
// step then by the order the step produced them.
func (c *CI) JobAnnotations(ctx context.Context, runID int64, attempt int32, jobID string) ([]domain.CIAnnotation, error) {
	prefix := annotJobPrefix(runID, attempt, jobID)
	var out []domain.CIAnnotation
	for key, item := range c.db.Snapshot(ctx).Prefix(ctx, []byte(prefix)) {
		n, err := strconv.ParseInt(strings.TrimPrefix(string(key), prefix), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		items, err := decodeAnnotations(item.Value, int32(n)) //nolint:gosec // parsed with a 32-bit size
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		out = append(out, items...)
	}
	return out, nil
}

// annotationKeys lists the keys of the annotations of a run, for its deletion.
func annotationKeys(ctx context.Context, snap *kv.Snapshot, runID int64) [][]byte {
	var keys [][]byte
	for key := range snap.Prefix(ctx, []byte(annotRunPrefix(runID))) {
		keys = append(keys, slices.Clone(key))
	}
	return keys
}
