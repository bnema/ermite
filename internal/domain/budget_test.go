// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"testing"
)

func budgetID(t *testing.T, s string) ObjectID {
	t.Helper()
	id, err := ParseObjectID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestValidateCommitObjects(t *testing.T) {
	id := budgetID(t, "1111111111111111111111111111111111111111")
	for _, tt := range []struct {
		name string
		ids  []ObjectID
		max  int
		want error
	}{
		{"empty", nil, 1, nil}, {"boundary", []ObjectID{id}, 1, nil},
		{"too many", []ObjectID{id, id}, 1, ErrInvalid}, {"zero", []ObjectID{{}}, 1, ErrInvalidObjectID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCommitObjects(tt.ids, tt.max); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCommitObjectBudget(t *testing.T) {
	id := budgetID(t, "1111111111111111111111111111111111111111")
	for _, tt := range []struct {
		name             string
		commit           bool
		size, total      int64
		seen, take, stop bool
	}{
		{"non commit", false, 1, 0, false, false, false},
		{"oversize", true, MaxCommitObjectBytes + 1, 0, false, false, false},
		{"exact object", true, MaxCommitObjectBytes, 0, false, true, false},
		{"exact total", true, 1, MaxCommitObjectsBytes - 1, false, true, false},
		{"over total", true, 2, MaxCommitObjectsBytes - 1, false, false, true},
		{"duplicate", true, 1, MaxCommitObjectsBytes, true, false, false},
		{"negative", true, -1, 0, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := CommitObjectBudget{total: tt.total}
			if tt.seen {
				b.seen = map[ObjectID]bool{id: true}
			}
			take, stop := b.Admit(id, tt.commit, tt.size)
			if take != tt.take || stop != tt.stop {
				t.Fatalf("got %v,%v", take, stop)
			}
			if take {
				b.Charge(id, tt.size)
				if !b.Seen(id) || b.total != tt.total+tt.size {
					t.Fatal("charge not recorded")
				}
			}
		})
	}
}

func TestDiffBudgetWindow(t *testing.T) {
	for _, tt := range []struct {
		name           string
		limits         DiffLimits
		kept, from, to int
		truncated      bool
	}{
		{"unlimited", DiffLimits{}, 5, 0, 5, false},
		{"cap", DiffLimits{MaxFiles: 3}, 3, 0, 3, true},
		{"window", DiffLimits{From: 1, To: 4}, 5, 1, 4, false},
		{"clamped", DiffLimits{From: -1, To: 9, MaxFiles: 3}, 3, 0, 3, true},
		{"past end", DiffLimits{From: 8}, 5, 8, 5, false},
		{"reversed", DiffLimits{From: 4, To: 2}, 5, 4, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewDiffBudget(tt.limits)
			k, f, to, tr := b.Window(5)
			if k != tt.kept || f != tt.from || to != tt.to || tr != tt.truncated {
				t.Fatalf("got %d,%d,%d,%v", k, f, to, tr)
			}
		})
	}
}

func TestDiffBudgetHunks(t *testing.T) {
	for _, tt := range []struct {
		name            string
		binary          bool
		n, lines, bytes int
		want            bool
	}{
		{"fits", false, 3, 3, 1, true}, {"binary", true, 3, 3, 1, false},
		{"no changes", false, 0, 3, 1, false}, {"file limit", false, 4, 5, 1, false},
		{"total lines", false, 3, 2, 1, false}, {"no bytes", false, 1, 3, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewDiffBudget(DiffLimits{MaxFileLines: 3, MaxTotalLines: tt.lines, MaxTotalBytes: tt.bytes})
			if got := b.WantsHunks(tt.binary, tt.n); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestDiffBudgetCharge(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bytes int
		fits  bool
	}{{"exact", 3, true}, {"over", 4, false}} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewDiffBudget(DiffLimits{MaxFileLines: 2, MaxTotalLines: 1, MaxTotalBytes: 3})
			before := b
			if b.FileLines() != 1 {
				t.Fatal("wrong file limit")
			}
			if b.ChargeLine(tt.bytes) != tt.fits {
				t.Fatal("wrong charge result")
			}
			if b.ChargeLine(0) {
				t.Fatal("line budget exceeded")
			}
			b = before
			if !b.WantsHunks(false, 1) {
				t.Fatal("discarded file consumed budget")
			}
		})
	}
}
