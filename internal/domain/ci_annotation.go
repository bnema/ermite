// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// AnnotationLevel is the severity of an annotation, with the names of GitHub's
// check run annotation levels.
type AnnotationLevel string

// Annotation levels.
const (
	AnnotationNotice  AnnotationLevel = "notice"
	AnnotationWarning AnnotationLevel = "warning"
	AnnotationFailure AnnotationLevel = "failure"
)

// Valid reports whether the level is one of the three.
func (l AnnotationLevel) Valid() bool {
	return l == AnnotationNotice || l == AnnotationWarning || l == AnnotationFailure
}

// Limits of annotations and summaries (runner.proto, run.proto). Lengths count
// characters (code points), not bytes, except the summary size.
const (
	// CIMaxAnnotationsPerLevel bounds the annotations of one level in one step.
	CIMaxAnnotationsPerLevel = 10
	// CIMaxAnnotationMessage, CIMaxAnnotationTitle and CIMaxAnnotationFile bound
	// the text fields of an annotation, in characters.
	CIMaxAnnotationMessage = 4096
	CIMaxAnnotationTitle   = 255
	CIMaxAnnotationFile    = 1024
	// CIMaxListedAnnotations is the most annotations ListAnnotations returns.
	CIMaxListedAnnotations = 1000
	// CIMaxSummaryBytes bounds the summary of one step (1 MiB).
	CIMaxSummaryBytes = 1 << 20
	// CIMaxListedSummaries is the most summaries GetStepSummaries returns for a
	// job, as GitHub does.
	CIMaxListedSummaries = 20
)

// CIAnnotation is a message attached to a step, and optionally to a place in a
// file, like an annotation of a GitHub check run.
type CIAnnotation struct {
	Level                                      AnnotationLevel
	Title, Message, File                       string
	StartLine, EndLine, StartColumn, EndColumn int32
	// Step is the number of the step that produced the annotation.
	Step int32
}

// CIStepSummary is the job summary (Markdown) of one step.
type CIStepSummary struct {
	Step     int32
	Markdown string
}

// CheckAnnotations validates the annotations of a request: the limits of the
// contract, whole or not at all.
func CheckAnnotations(anns []CIAnnotation) error {
	type key struct {
		step  int32
		level AnnotationLevel
	}
	count := map[key]int{}
	for i, a := range anns {
		switch {
		case !a.Level.Valid():
			return fmt.Errorf("%w: annotation %d has no level", ErrInvalid, i)
		case a.Step < 1 || a.Step > CIMaxSteps:
			return fmt.Errorf("%w: annotation %d: step number %d is not between 1 and %d", ErrInvalid, i, a.Step, CIMaxSteps)
		case a.Message == "":
			return fmt.Errorf("%w: annotation %d has no message", ErrInvalid, i)
		case utf8.RuneCountInString(a.Message) > CIMaxAnnotationMessage:
			return fmt.Errorf("%w: annotation %d: the message is longer than %d characters", ErrInvalid, i, CIMaxAnnotationMessage)
		case utf8.RuneCountInString(a.Title) > CIMaxAnnotationTitle:
			return fmt.Errorf("%w: annotation %d: the title is longer than %d characters", ErrInvalid, i, CIMaxAnnotationTitle)
		case utf8.RuneCountInString(a.File) > CIMaxAnnotationFile:
			return fmt.Errorf("%w: annotation %d: the file is longer than %d characters", ErrInvalid, i, CIMaxAnnotationFile)
		case !utf8.ValidString(a.Message) || !utf8.ValidString(a.Title) || !utf8.ValidString(a.File):
			return fmt.Errorf("%w: annotation %d is not valid UTF-8", ErrInvalid, i)
		case a.StartLine < 0 || a.EndLine < 0 || a.StartColumn < 0 || a.EndColumn < 0:
			return fmt.Errorf("%w: annotation %d: lines and columns are not negative", ErrInvalid, i)
		case a.EndLine != 0 && a.EndLine < a.StartLine:
			return fmt.Errorf("%w: annotation %d: end_line is before start_line", ErrInvalid, i)
		}
		k := key{a.Step, a.Level}
		if count[k]++; count[k] > CIMaxAnnotationsPerLevel {
			return fmt.Errorf("%w: step %d has more than %d annotations of level %s", ErrInvalid, a.Step, CIMaxAnnotationsPerLevel, a.Level)
		}
	}
	return nil
}

// GroupAnnotations groups annotations by step number, each group in the order
// of the input, and returns the step numbers in ascending order.
func GroupAnnotations(anns []CIAnnotation) (map[int32][]CIAnnotation, []int32) {
	by := map[int32][]CIAnnotation{}
	var steps []int32
	for _, a := range anns {
		if _, ok := by[a.Step]; !ok {
			steps = append(steps, a.Step)
		}
		by[a.Step] = append(by[a.Step], a)
	}
	slices.Sort(steps)
	return by, steps
}
