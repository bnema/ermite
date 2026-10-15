// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"strconv"
	"strings"
	"time"
)

// A merge request is a "pull request" for the workflows that run on it (CI-3,
// D5): ermite keeps two refs per request, as GitHub does.
//
//	refs/pull/<n>/head   the head of the source branch
//	refs/pull/<n>/merge  a test merge of the head into the target, when it merges cleanly
//
// They are written by ermite only: a push to them is refused (see IsPullRef),
// and they are not branches nor tags, so no listing shows them. Git serves them
// to fetch like any ref.

// PullRefPrefix starts the refs of the merge requests.
const PullRefPrefix = "refs/pull/"

// PullHeadRef is the ref that holds the head of merge request number.
func PullHeadRef(number int64) string { return PullRefPrefix + strconv.FormatInt(number, 10) + "/head" }

// PullMergeRef is the ref that holds the test merge of merge request number.
func PullMergeRef(number int64) string {
	return PullRefPrefix + strconv.FormatInt(number, 10) + "/merge"
}

// IsPullRefName reports whether ref is under refs/pull/, whatever follows.
// Pushes are refused on all of that namespace.
func IsPullRefName(ref string) bool { return strings.HasPrefix(ref, PullRefPrefix) }

// IsPullRef reports whether ref is exactly refs/pull/<n>/head or
// refs/pull/<n>/merge, the two refs ermite maintains.
func IsPullRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, PullRefPrefix)
	if !ok {
		return false
	}
	num, kind, ok := strings.Cut(rest, "/")
	if !ok || (kind != "head" && kind != "merge") {
		return false
	}
	n, err := strconv.ParseInt(num, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == num
}

// PullRequestActions are the activity types of the pull_request event that
// ermite can produce. A merge request cannot be reopened yet, so "reopened"
// never occurs, but a workflow may list it.
const (
	PullOpened      = "opened"
	PullSynchronize = "synchronize"
	PullReopened    = "reopened"
	PullClosed      = "closed"
)

// DefaultPullRequestTypes are the types of a pull_request that lists none.
var DefaultPullRequestTypes = []string{PullOpened, PullSynchronize, PullReopened}

// TestMergeCommit returns the text of the commit that tests the merge of head
// into base: the tree the merge produced, parents base then head, and GitHub's
// message. It is not signed.
func TestMergeCommit(tree, base, head ObjectID, who Identity, at time.Time) []byte {
	sig := signatureLine(Signature{Name: who.Name, Email: who.Email, Time: at.UTC()})
	var b strings.Builder
	b.WriteString("tree " + tree.String() + "\n")
	b.WriteString("parent " + base.String() + "\n")
	b.WriteString("parent " + head.String() + "\n")
	b.WriteString("author " + sig + "\n")
	b.WriteString("committer " + sig + "\n")
	b.WriteString("\nMerge " + head.String() + " into " + base.String() + "\n")
	return []byte(b.String())
}
