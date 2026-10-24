// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsInvalid(t *testing.T) {
	t.Parallel()
	var named *InvalidNameError
	_, parseErr := ParseRepoName("Bad Name")
	assert.ErrorAs(t, parseErr, &named)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"invalid", ErrInvalid, true},
		{"wrapped invalid", fmt.Errorf("x: %w", ErrInvalidRevision), true},
		{"invalid name from the parser", parseErr, true},
		{"invalid fields", &InvalidFieldsError{Fields: []FieldViolation{{Field: "title", Reason: "is empty"}}}, true},
		{"page token", ErrInvalidPageToken, true},
		{"not a file", ErrNotFile, true},
		{"invalid text", ErrInvalidText, true},
		{"not found is not invalid", ErrNotFound, false},
		{"forbidden is not invalid", ErrForbidden, false},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsInvalid(tt.err))
		})
	}
}

func TestErrArchived(t *testing.T) {
	assert.ErrorIs(t, ErrArchived, ErrPreconditionFailed)
	assert.Contains(t, ErrArchived.Error(), "repository is archived")
}
