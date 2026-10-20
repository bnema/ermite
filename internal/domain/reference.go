package domain

import (
	"regexp"
	"slices"
	"strconv"
)

const maxReferenceDigits = 10

// MaxReferenceTextBytes bounds each title, description, note and commit message scanned.
const MaxReferenceTextBytes = 16 << 10

var numberRef = regexp.MustCompile(`(^|[^0-9A-Za-z_&#])#([1-9][0-9]{0,9})\b`)
var closingRef = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z_])(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)(?::[ \t]*|[ \t]+)#([1-9][0-9]{0,9})\b((?:[ \t]*,[ \t]*#[1-9][0-9]{0,9}\b|[ \t]+and[ \t]+#[1-9][0-9]{0,9}\b)*)`)

// FindReferences returns the positive #N numbers in text, sorted and unique.
// Numbers glued to words, entities or another # are excluded, as are zero,
// leading zeroes and numbers longer than ten digits. Code is scanned as text;
// cross-repository references and !N are not supported.
func FindReferences(text string) []int64 {
	numbers := make(map[int64]struct{})
	// Resume at the end of the number, retaining its following delimiter for the
	// next match (for example, both references in "#1,#2").
	for offset := 0; offset < len(text); {
		match := numberRef.FindStringSubmatchIndex(text[offset:])
		if match == nil {
			break
		}
		start, end := offset+match[4], offset+match[5]
		// A sliced search must not manufacture a beginning-of-text boundary.
		if start < 2 || !referenceWord(text[start-2]) {
			n, _ := strconv.ParseInt(text[start:end], 10, 64)
			numbers[n] = struct{}{}
		}
		offset = end
	}
	return sortedReferences(numbers)
}

func referenceWord(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_' || b == '&' || b == '#'
}

// FindClosingReferences returns sorted, unique references following close,
// closes, closed, fix, fixes, fixed, resolve, resolves or resolved (case
// insensitive). A colon is optional; lists use commas or "and", not spaces.
// Like FindReferences, this scans code spans and fences as ordinary text.
func FindClosingReferences(text string) []int64 {
	numbers := make(map[int64]struct{})
	for _, match := range closingRef.FindAllString(text, -1) {
		for _, n := range FindReferences(match) {
			numbers[n] = struct{}{}
		}
	}
	return sortedReferences(numbers)
}

func sortedReferences(numbers map[int64]struct{}) []int64 {
	var result []int64
	for n := range numbers {
		result = append(result, n)
	}
	slices.Sort(result)
	return result
}

// MergeRequestReferences describes the issues named by a merge request.
type MergeRequestReferences struct {
	Mentions []int64
	Closes   []int64
}

// ScanMergeRequest scans titles, descriptions, notes and commit messages for
// references. Notes can mention issues but cannot close them. Each individual
// text is truncated to MaxReferenceTextBytes before scanning; no Markdown or
// cross-repository interpretation is performed.
func ScanMergeRequest(title, description string, notes, commitMessages []string) MergeRequestReferences {
	mentions, closes := make(map[int64]struct{}), make(map[int64]struct{})
	scan := func(text string, closing bool) {
		if len(text) > MaxReferenceTextBytes {
			text = text[:MaxReferenceTextBytes]
		}
		for _, n := range FindReferences(text) {
			mentions[n] = struct{}{}
		}
		if closing {
			for _, n := range FindClosingReferences(text) {
				closes[n] = struct{}{}
			}
		}
	}
	scan(title, true)
	scan(description, true)
	for _, text := range notes {
		scan(text, false)
	}
	for _, text := range commitMessages {
		scan(text, true)
	}
	return MergeRequestReferences{Mentions: sortedReferences(mentions), Closes: sortedReferences(closes)}
}
