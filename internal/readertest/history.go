// SPDX-License-Identifier: AGPL-3.0-or-later

package readertest

import "testing"

// History is the repository of the read tests: three commits on main, a
// branch with a slash, a merge, lightweight and annotated tags.
type History struct {
	*Fixture
	C1, C2, C3, Side, Merge string
}

func NewHistory(t *testing.T, data string) *History {
	t.Helper()
	f := NewFixture(t, data, "hist")
	h := &History{Fixture: f}
	f.Write("a.txt", "one\ntwo\nthree\n").
		WriteBytes("bin", []byte{0, 1}).
		Write("d/f", "hi\n").
		Write("d/sub/g", "deep\n").
		Write("we:ird*.txt", "x").
		Write("README.md", "# hello\n").
		Write("empty", "")
	h.C1 = f.Commit("first\n\nbody of the first commit\n")

	f.Mv("a.txt", "b.txt").Write("b.txt", "one\ntwo\nthree\nfour\n").
		WriteBytes("bin", []byte{0, 2}).
		Chmod("d/f", 0o755).
		Write("n.txt", "new\nnonl").
		Remove("README.md")
	h.C2 = f.Commit("second")

	f.Remove("n.txt").Symlink("b.txt", "n.txt")
	h.C3 = f.Commit("third: type change")

	f.CheckoutNew("feature/x")
	f.Write("side.txt", "s\n")
	h.Side = f.Commit("side work")
	f.Checkout("main")
	f.Write("main.txt", "m\n")
	f.Commit("main work")
	h.Merge = f.Merge("feature/x")

	f.Git("tag", "v1", h.C1)
	f.Git("tag", "-a", "-m", "release two", "v2", h.C2)
	f.Git("tag", "-a", "-m", "tag of a tag", "v2-again", "v2")
	f.Git("tag", "-a", "-m", "tag of a tree", "tree-tag", h.Merge+"^{tree}")
	f.Git("branch", "release/1.0", h.C2)
	f.Git("branch", "rel/1", h.C1)
	f.Git("tag", "rel/1/x", h.C3)
	return h
}
