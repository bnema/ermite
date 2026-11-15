// SPDX-License-Identifier: AGPL-3.0-or-later

package domain_test

import (
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite/internal/domain"
)

// addParserSeeds includes hostile inputs shared by all untrusted-input parsers.
func addParserSeeds(f *testing.F, seeds ...string) {
	f.Helper()
	for _, s := range append(seeds, "", "\x00", strings.Repeat("a", 4096), "../a", "a//b", "\xff") {
		f.Add(s)
	}
}

func FuzzParseRepoName(f *testing.F) {
	addParserSeeds(f,
		"ermite",
		"a",
		"my-repo_1.0",
		"x.y",
		"0",
		strings.Repeat("a", 100),
		strings.Repeat("a", 101),
		"",
		"Ermite",
		"My-Repo",
		"my-repo",
		"UPPER.git",
		".hidden",
		"trailing.",
		"a..b",
		"tools.git",
		"-",
		"robots.txt",
		"favicon.ico",
		"-dash-ok",
		"has space",
		"a/b",
		"caf\u00e9",
		"a\x00b",
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseRepoName(s)
		if err != nil {
			return
		}
		again, err := domain.ParseRepoName(out.String())
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
	})
}

func FuzzParseRevision(f *testing.F) {
	addParserSeeds(f,
		"",
		"main",
		"feature/x",
		"refs/heads/feature/x",
		"v1.0.0",
		"HEAD",
		"abc1234",
		strings.Repeat("a", 40),
		"release-2026.10",
		"café",
		"-x",
		"--upload-pack=x",
		"/x",
		"x/",
		"x.",
		"a..b",
		"a@{1}",
		"@",
		"a b",
		"a~1",
		"a^",
		"a:b",
		"a?",
		"a*",
		"a[",
		"a\\b",
		"a\nb",
		"a\x00b",
		"a//b",
		".hidden",
		"a/.b",
		"x.lock",
		"a/x.lock/b",
		"\xff",
		strings.Repeat("a", 513),
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseRevision(s)
		if err != nil {
			return
		}
		again, err := domain.ParseRevision(out.String())
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
	})
}

func FuzzParseTreePath(f *testing.F) {
	addParserSeeds(f,
		strings.Repeat("a", 4096),
		"a",
		"a/b.txt",
		"dir/ünï.go",
		"a b/c",
		".github/ci.yml",
		"/a",
		"a//b",
		"./a",
		"a/..",
		"..",
		".",
		"a/../b",
		"tab\tname",
		"a\nb",
		"a\x00b",
		"a\x7fb",
		"\xff",
		"a/\xfe/b",
		"//",
		"",
		"a/",
		"/",
		"a/b/",
		strings.Repeat("a", 4097),
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseTreePath(s)
		if err != nil {
			return
		}
		again, err := domain.ParseTreePath(out.String())
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
	})
}

func FuzzParseObjectID(f *testing.F) {
	addParserSeeds(f,
		strings.Repeat("ab12", 10),
		"",
		"abc",
		"0",
		strings.Repeat("g", 40),
		"main",
		strings.Repeat("AB12", 10),
		strings.Repeat("ab12", 10)+"0",
		strings.Repeat("ab12", 10)[:39],
		strings.Repeat("0", 40),
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseObjectID(s)
		if err != nil {
			return
		}
		again, err := domain.ParseObjectID(out.String())
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
		if out.IsZero() && s != strings.Repeat("0", domain.ObjectIDLen) {
			t.Fatalf("nonzero input %q parsed as zero", s)
		}
	})
}

func FuzzParseBranchName(f *testing.F) {
	addParserSeeds(f,
		"main",
		"feat/x",
		"v1.2",
		"a-b_c",
		"",
		"refs/heads/main",
		"-x",
		"a..b",
		"a b",
		"x/",
		"a:b",
		"x.lock",
		"a\x00b",
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseBranchName(s)
		if err != nil {
			return
		}
		again, err := domain.ParseBranchName(out)
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
	})
}

func FuzzParsePlainText(f *testing.F) {
	addParserSeeds(f,
		"",
		"one\ntwo\n\nthree",
		"x\n",
		strings.Repeat("a\n", domain.MaxTextLen/2),
		strings.Repeat("€", domain.MaxTextLen),
		strings.Repeat("a", domain.MaxTextLen+1),
		"a\r\nb",
		"a\tb",
		"a\x1b]0;title\x07",
		"a\x07",
		"ok\n\xc3\x28",
		"x\u202ey",
		"x\u2066y",
		"x\u2068y",
		"x\u2028y",
		"x\u2029y",
		"x\u200ey",
		"x\u200fy",
		"x\u061cy",
		"x\u200dy",
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParsePlainText(s)
		if err != nil {
			return
		}
		again, err := domain.ParsePlainText(out.String())
		if err != nil || again != out {
			t.Fatalf("round trip of %q: got %v, want %v (error: %v)", s, again, out, err)
		}
	})
}

func FuzzParseCron(f *testing.F) {
	addParserSeeds(f,
		"* * * * *",
		"",
		"*/15 * * * *",
		"30 4 * * *",
		"0 0 1 1 *",
		"5,10-12 */6 * * *",
		"10/20 * * * *",
		"0 12 * JAN-mar mon-fri",
		"0 0 * * 0",
		"0 is Sunday",
		"0 0 13 * fri",
		"0 0 13 * *",
		"0 0 * * fri",
		"0 10 * * *",
		"0 0 29 2 *",
		"2028 is a leap year",
		"0 0 31 * *",
		"59/9223372036854775807 * * * *",
		"*/9223372036854775807 * * * *",
		"0-30/100 * * * *",
		"*/99999999999999999999 * * * *",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 7",
		"*/0 * * * *",
		"*/x * * * *",
		"5-3 * * * *",
		"1,,2 * * * *",
		"-1 * * * *",
		"1- * * * *",
		"foo * * * *",
		"* * * jun-foo *",
		"+5 * * * *",
		"0x5 * * * *",
		"0 9 * * *",
		"30 0 * * mon",
		"30 2 * * *",
		"59 1 * * *",
		"0 3 * * *",
		"*/15 2-3 * * *",
		"0 * * * *",
		"30 1 * * *",
		"0 2 * * *",
		"02:00 EST, once",
		"0 6 * * 1",
		"0 0 31 2 *",
		"5,40 3,17 * * *",
	)
	f.Fuzz(func(t *testing.T, s string) {
		out, err := domain.ParseCron(s)
		if err != nil {
			return
		}
		t0 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		if next, ok := out.Next(t0); ok && !next.After(t0) {
			t.Fatalf("Next(%v) = %v, not strictly after", t0, next)
		}
	})
}
