// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit

// DefaultRoutes returns the routes of production.
//
// A method is routed to ermite-git only when it passes the shared conformance
// table (TestConformance, TestCompareWithCLI) and its benchmark beats the CLI
// adapter in time AND in allocations, on the generated fixture and on lazygit
// (BenchmarkRepoReader, ERMITE_GIT_BENCH_REPO). A method that does not stays
// on the CLI, with the numbers that decided it. GetCommit is not routable: the
// diff stays on git.
//
// The numbers are the median of three runs of `go test -bench RepoReader
// -benchmem` on an AMD Ryzen 9 7900X3D, git 2.56, ermite-git ea9bcf1, as
// "time / allocations per call", CLI against ermite-git. The fixture has 220
// commits, 8 directories of 40 files and a 10 MiB blob; lazygit has 7,550
// commits. Only the allocations of the Go process are counted, which favours
// the CLI (its work happens in git processes).
//
//	                        fixture                          lazygit
//	ListRefs                900 µs/126 → 29 µs/89            900 µs/89 → 20 µs/43       win
//	ResolveRevision branch   24 µs/19  →  5 µs/14             26 µs/19 →  5 µs/14       win
//	ResolveRevision full id   10 µs/22  →  2 µs/6               9 µs/22 →  2 µs/6        win
//	ResolveRevision abbrev   65 µs/31  → 12 µs/33             65 µs/31 → 13 µs/33       allocs +2
//	ResolveRefPath          108 µs/59  → 20 µs/72            159 µs/79 → 28 µs/101      allocs +13
//	GetTree dir             736 µs/141 → 26 µs/51            1.3 ms/1059 → 3.5 ms/2634  loses on lazygit
//	GetTree root            717 µs/114 → 14 µs/36            812 µs/167 → 50 µs/78      win
//	GetTree root+last       4.0 ms/1022 → 73 µs/139          20 ms/3084 → 6.2 ms/4470   allocs +45%
//	GetTree dir+last        5.3 ms/1452 → 176 µs/247         196 ms/17143 → 11 ms/8058  win
//	OpenBlob small           46 µs/25  → 17 µs/28             53 µs/25 → 29 µs/57       allocs +2.3x
//	OpenBlob 10 MiB          21 ms/64  → 49 ms/28            3.4 ms/64 → 5.6 ms/140     loses (inflate speed)
//	ListCommits page 1        1.0 ms/404 → 59 µs/149         1.3 ms/415 → 368 µs/265    win
//	ListCommits skip 5000     1.0 ms/404 → 74 µs/149         2.0 ms/404 → 433 µs/153    win
//	ListCommits -- file       1.6 ms/138 → 453 µs/355         12.7 ms/117 → 6.1 ms/6870 allocs +58x
//	ListCommits -- dir        1.3 ms/403 → 202 µs/239         6.6 ms/403 → 2.0 ms/1988  allocs +5x
//
// Where a method has several shapes, every shape must win to be routed: the
// constants GetTree, GetTreeLastCommits and ListCommitsPath cover directories
// of any size and files of any history. The losses in allocations come from
// the inflate of tree and commit objects (compress/zlib allocates an adler32
// digest and the flate tables on every Reset): once the library avoids that,
// re-run the benchmark and flip the constants it wins.
func DefaultRoutes() Routes {
	return Routes{
		ListRefs:              ErmiteGit,
		ResolveRevision:       ErmiteGit,
		ResolveRevisionAbbrev: CLI,
		ResolveRefPath:        CLI,
		GetTree:               CLI,
		GetTreeLastCommits:    CLI,
		OpenBlob:              CLI,
		ListCommits:           ErmiteGit,
		ListCommitsPath:       CLI,
	}
}
