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

func TestDiffBudgetAdmit(t *testing.T) {
	for _, tt := range []struct {
		name     string
		limits   DiffLimits
		binary   bool
		path     string
		n        int
		wantRead bool
		wantWhy  FileOmission
	}{
		{"fits", DiffLimits{MaxFileLines: 3, MaxTotalLines: 3, MaxTotalBytes: 1}, false, "a.go", 3, true, OmitNone},
		{"binary", DiffLimits{MaxTotalBytes: 1}, true, "a.bin", 3, false, OmitNone},
		{"no changes", DiffLimits{MaxTotalBytes: 1}, false, "a.go", 0, false, OmitNone},
		{"file limit", DiffLimits{MaxFileLines: 3, MaxTotalLines: 5, MaxTotalBytes: 1}, false, "a.go", 4, false, OmitLarge},
		{"total lines", DiffLimits{MaxFileLines: 3, MaxTotalLines: 2, MaxTotalBytes: 1}, false, "a.go", 3, false, OmitBudget},
		{"no bytes", DiffLimits{MaxFileLines: 3, MaxTotalLines: 3}, false, "a.go", 1, false, OmitBudget},
		{"generated", DiffLimits{SkipGenerated: true, MaxTotalBytes: 1}, false, "go.sum", 1, false, OmitGenerated},
		{"generated not skipped", DiffLimits{MaxTotalBytes: 1}, false, "go.sum", 1, true, OmitNone},
		{"generated before large", DiffLimits{SkipGenerated: true, MaxFileLines: 1, MaxTotalBytes: 1}, false, "go.sum", 9, false, OmitGenerated},
		{"unlimited lines", DiffLimits{MaxTotalBytes: 1}, false, "a.go", 1 << 20, true, OmitNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewDiffBudget(tt.limits)
			read, why := b.Admit(tt.binary, tt.path, tt.n)
			if read != tt.wantRead || why != tt.wantWhy {
				t.Fatalf("got %v,%v", read, why)
			}
		})
	}
}

func TestDiffBudgetCharge(t *testing.T) {
	for _, tt := range []struct {
		name  string
		bytes int
		want  FileOmission
	}{{"exact", 3, OmitNone}, {"over", 4, OmitUnavailable}} {
		t.Run(tt.name, func(t *testing.T) {
			b := NewDiffBudget(DiffLimits{MaxFileLines: 2, MaxTotalLines: 1, MaxTotalBytes: 3})
			before := b
			if b.FileLines() != 1 {
				t.Fatal("wrong file limit")
			}
			if got := b.ChargeLine(tt.bytes, true); got != tt.want {
				t.Fatalf("wrong charge result %v", got)
			}
			if b.ChargeLine(0, true) == OmitNone {
				t.Fatal("line budget exceeded")
			}
			b = before
			if read, _ := b.Admit(false, "a", 1); !read {
				t.Fatal("discarded file consumed budget")
			}
		})
	}
}

func TestDiffBudgetChargeReasons(t *testing.T) {
	b := NewDiffBudget(DiffLimits{MaxTotalLines: 1, MaxTotalBytes: 3})
	if b.ChargeLine(2, false) != OmitNone || b.FileLines() != 1 {
		t.Fatal("context consumes bytes only")
	}
	if b.ChargeLine(1, true) != OmitNone || b.FileLines() != 0 {
		t.Fatal("first changed line fits")
	}
	if got := b.ChargeLine(0, true); got != OmitBudget {
		t.Fatalf("lines ran out: %v", got)
	}
	b = NewDiffBudget(DiffLimits{MaxTotalLines: 9, MaxTotalBytes: 1})
	if got := b.ChargeLine(2, false); got != OmitUnavailable {
		t.Fatalf("bytes ran out: %v", got)
	}
	free := NewDiffBudget(DiffLimits{MaxTotalBytes: 5})
	for range 100 {
		if free.ChargeLine(0, true) != OmitNone {
			t.Fatal("0 means no line limit")
		}
	}
	if free.FileLines() < 1<<30 {
		t.Fatal("unlimited FileLines")
	}
}

func TestFileOmissionCollapsed(t *testing.T) {
	for o, want := range map[FileOmission]bool{OmitNone: false, OmitGenerated: true, OmitLarge: true, OmitBudget: true, OmitUnavailable: false} {
		if o.Collapsed() != want {
			t.Errorf("%d: got %v", o, !want)
		}
	}
}

func TestOneFileDiffLimits(t *testing.T) {
	l := OneFileDiffLimits(3)
	if l.From != 3 || l.To != 4 || l.MaxFileLines != 0 || l.MaxTotalLines != 0 || l.MaxFiles != 0 || l.MaxTotalBytes != DefaultDiffLimits().MaxTotalBytes {
		t.Fatalf("got %+v", l)
	}
	// A negative index (an unchecked IndexFunc) selects no file, not the whole diff.
	if _, from, to, _ := NewDiffBudget(OneFileDiffLimits(-1)).Window(10); from < to {
		t.Fatalf("OneFileDiffLimits(-1) selects files [%d, %d)", from, to)
	}
}

func TestDiffBudgetBytesOfEarlierFiles(t *testing.T) {
	b := NewDiffBudget(DiffLimits{MaxTotalBytes: 10})
	b.StartFile()
	if b.ChargeLine(4, true) != OmitNone {
		t.Fatal("the first file fits")
	}
	b.StartFile()
	if got := b.ChargeLine(7, true); got != OmitBudget {
		t.Fatalf("a file meeting the bytes of earlier files: got %v, want OmitBudget", got)
	}
	alone := NewDiffBudget(DiffLimits{MaxTotalBytes: 10})
	alone.StartFile()
	if got := alone.ChargeLine(11, true); got != OmitUnavailable {
		t.Fatalf("a file over the bytes on its own: got %v, want OmitUnavailable", got)
	}
}

func TestDiffBudgetContext(t *testing.T) {
	b := NewDiffBudget(DiffLimits{MaxFileLines: 2, MaxTotalLines: 2, MaxTotalBytes: 6})
	if b.ChargeLine(2, false) != OmitNone || b.FileLines() != 2 {
		t.Fatal("context consumed changed-line budget")
	}
	first := b.ChargeLine(1, true)
	second := b.ChargeLine(1, true)
	if first != OmitNone || second != OmitNone || b.FileLines() != 0 {
		t.Fatal("additions and deletions must consume changed-line budget")
	}
	if b.ChargeLine(2, false) != OmitNone {
		t.Fatal("context at the byte boundary must fit even without changed lines left")
	}
	if b.ChargeLine(1, false) != OmitUnavailable {
		t.Fatal("context must not exceed the byte budget")
	}
}
