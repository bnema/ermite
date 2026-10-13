// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bnema.dev/ermite/internal/domain"
)

func an(level domain.AnnotationLevel, step int32, msg string) domain.CIAnnotation {
	return domain.CIAnnotation{Level: level, Step: step, Message: msg}
}

func TestCheckAnnotationsLimits(t *testing.T) {
	t.Parallel()
	n, w, f := domain.AnnotationNotice, domain.AnnotationWarning, domain.AnnotationFailure
	same := func(level domain.AnnotationLevel, step int32, count int) []domain.CIAnnotation {
		var out []domain.CIAnnotation
		for range count {
			out = append(out, an(level, step, "m"))
		}
		return out
	}
	valid := map[string][]domain.CIAnnotation{
		"none":                     nil,
		"ten of a level":           same(n, 1, 10),
		"ten of each level":        append(append(same(n, 1, 10), same(w, 1, 10)...), same(f, 1, 10)...),
		"ten per level, two steps": append(same(n, 1, 10), same(n, 2, 10)...),
		"4096 characters, 8 KiB":   {an(n, 1, strings.Repeat("é", 4096))},
		"title 255, file 1024":     {{Level: n, Step: 1, Message: "m", Title: strings.Repeat("é", 255), File: strings.Repeat("é", 1024)}},
		"step 1000":                {an(n, 1000, "m")},
		"end line equal to start":  {{Level: n, Step: 1, Message: "m", StartLine: 3, EndLine: 3}},
		"end line without start":   {{Level: n, Step: 1, Message: "m", EndLine: 3}},
	}
	for name, anns := range valid {
		assert.NoError(t, domain.CheckAnnotations(anns), name)
	}
	invalid := map[string][]domain.CIAnnotation{
		"eleven of a level":  same(w, 4, 11),
		"no level":           {{Step: 1, Message: "m"}},
		"unknown level":      {{Level: "debug", Step: 1, Message: "m"}},
		"no message":         {an(n, 1, "")},
		"message 4097":       {an(n, 1, strings.Repeat("é", 4097))},
		"title 256":          {{Level: n, Step: 1, Message: "m", Title: strings.Repeat("t", 256)}},
		"file 1025":          {{Level: n, Step: 1, Message: "m", File: strings.Repeat("f", 1025)}},
		"step 0":             {an(n, 0, "m")},
		"step 1001":          {an(n, 1001, "m")},
		"negative line":      {{Level: n, Step: 1, Message: "m", StartLine: -1}},
		"negative column":    {{Level: n, Step: 1, Message: "m", EndColumn: -1}},
		"end before start":   {{Level: n, Step: 1, Message: "m", StartLine: 4, EndLine: 3}},
		"invalid utf-8":      {an(n, 1, "a\xffb")},
		"one bad among good": append(same(n, 1, 3), an(n, 1, "")),
	}
	for name, anns := range invalid {
		err := domain.CheckAnnotations(anns)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, domain.ErrInvalid, name)
	}
}

func TestGroupAnnotationsKeepsTheOrderOfAStep(t *testing.T) {
	t.Parallel()
	by, steps := domain.GroupAnnotations([]domain.CIAnnotation{
		an(domain.AnnotationNotice, 10, "ten-a"), an(domain.AnnotationNotice, 2, "two-a"), an(domain.AnnotationFailure, 10, "ten-b"),
	})
	assert.Equal(t, []int32{2, 10}, steps)
	assert.Equal(t, "ten-a", by[10][0].Message)
	assert.Equal(t, "ten-b", by[10][1].Message)
}
