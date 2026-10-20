package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestFindReferences(t *testing.T) {
	tests := []struct {
		text             string
		mentions, closes []int64
	}{
		{"abc#3 a_b#3 1#3 &#39; ##3 #3abc #03 #0", nil, nil},
		{"(#3) see #3, \"#3\"", []int64{3}, nil},
		{"fixes #3.", []int64{3}, []int64{3}},
		{"Fixes: #3", []int64{3}, []int64{3}},
		{"fixes:#3", []int64{3}, []int64{3}},
		{"Closes #3", []int64{3}, []int64{3}},
		{"RESOLVED #3", []int64{3}, []int64{3}},
		{"fix #3", []int64{3}, []int64{3}},
		{"prefixes #3", []int64{3}, nil},
		{"fixes 3 fixes#3", nil, nil},
		{"fixes #3, #4 and #5", []int64{3, 4, 5}, []int64{3, 4, 5}},
		{"fixes #3 #4", []int64{3, 4}, []int64{3}},
		{"#5,#3,#5", []int64{3, 5}, nil},
		{"#1234567890 #12345678901 !3 owner/repo#3", []int64{1234567890}, nil},
		{"`fixes #3`\n```\ncloses #4\n```", []int64{3, 4}, []int64{3, 4}},
		{"close #1 closed #2 fixes #3 fixed #4 resolve #5 resolves #6", []int64{1, 2, 3, 4, 5, 6}, []int64{1, 2, 3, 4, 5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := FindReferences(tt.text); !reflect.DeepEqual(got, tt.mentions) {
				t.Errorf("mentions = %v, want %v", got, tt.mentions)
			}
			if got := FindClosingReferences(tt.text); !reflect.DeepEqual(got, tt.closes) {
				t.Errorf("closes = %v, want %v", got, tt.closes)
			}
		})
	}
}

func TestScanMergeRequest(t *testing.T) {
	got := ScanMergeRequest("fixes #3", "#2", []string{"closes #4"}, []string{"resolved #5", strings.Repeat(" ", MaxReferenceTextBytes) + "fixes #6"})
	want := MergeRequestReferences{Mentions: []int64{2, 3, 4, 5}, Closes: []int64{3, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, scan := range []func(string) MergeRequestReferences{
		func(s string) MergeRequestReferences { return ScanMergeRequest(s, "", nil, nil) },
		func(s string) MergeRequestReferences { return ScanMergeRequest("", s, nil, nil) },
		func(s string) MergeRequestReferences { return ScanMergeRequest("", "", []string{s}, nil) },
		func(s string) MergeRequestReferences { return ScanMergeRequest("", "", nil, []string{s}) },
	} {
		if got := scan(strings.Repeat(" ", MaxReferenceTextBytes) + "#7"); len(got.Mentions) != 0 || len(got.Closes) != 0 {
			t.Errorf("text beyond limit scanned: %+v", got)
		}
	}
}

func TestScanMergeRequestTruncationDropsPartialToken(t *testing.T) {
	prefix := strings.Repeat(" ", MaxReferenceTextBytes-len("fixes #123"))
	got := ScanMergeRequest("", "", nil, []string{prefix + "fixes #12345"})
	if len(got.Mentions) != 0 || len(got.Closes) != 0 {
		t.Errorf("split number reported: %+v", got)
	}
	// Fully before the limit, followed by a split token.
	full := "fixes #7 " + strings.Repeat("a", MaxReferenceTextBytes)
	got = ScanMergeRequest("", "", nil, []string{full})
	if !reflect.DeepEqual(got, MergeRequestReferences{Mentions: []int64{7}, Closes: []int64{7}}) {
		t.Errorf("got %+v", got)
	}
	// No whitespace at all: nothing scanned.
	got = ScanMergeRequest("", "", nil, []string{strings.Repeat("a", MaxReferenceTextBytes-3) + " #1" + "2"})
	if len(got.Mentions) != 0 {
		t.Errorf("got %+v", got)
	}
}
