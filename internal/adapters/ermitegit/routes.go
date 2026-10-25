// SPDX-License-Identifier: AGPL-3.0-or-later

package ermitegit

// DefaultRoutes returns the routes of production.
//
// The rule: a method is routed to ermite-git when it passes the shared
// conformance table (TestConformance, TestCompareWithCLI) and its benchmark
// beats the CLI adapter WARM in time AND in bytes per call, on the generated
// fixture AND on lazygit (BenchmarkRepoReader, ERMITE_GIT_BENCH_REPO), and its
// COLD run does not lose in time by more than the open itself (about 65 µs,
// paid once per open, see below). Allocation counts are reported but do not
// decide: the repository cache (cache.go) keeps up to 64 repositories open for
// ten minutes, so nearly every request is warm, and the CLI's own cost (a
// process per call, absent from the Go counters) is not in its numbers. Where
// a method has several shapes that behave differently, each has its own
// constant: GetTree, GetTreeLastCommits (root), GetTreeSubdirLastCommits,
// ResolveRevisionAbbrev and ListCommitsPath. A method that does not win stays
// on the CLI. GetCommit is not routable: the diff stays on git. ListPaths has
// one shape: the fixture lists 131 files and lazygit 2,180.
//
// Warm is a repository that is open; cold closes it before every call, the
// price of the first read after the idle TTL, an eviction or a restart (a
// cold ermite-git call opens the packs and the commit-graph: about 65 µs and
// 130 allocations before the call itself). The CLI keeps nothing open, so its
// cold run is its warm run. Only the allocations of the Go process are
// counted, which favours the CLI (its work happens in git processes). The
// parallel runs (b.RunParallel) were also made: they agree with the warm runs
// and widen the time gap.
//
// The numbers are the median of six runs of `go test -bench RepoReader
// -benchmem -benchtime 500ms` on an AMD Ryzen 9 7900X3D, git 2.56, ermite-git
// 5228465, as "time bytes allocations per call", CLI against ermite-git (K and
// M are KiB and MiB). The fixture has 220 commits, 8 directories of 40 files
// and a 10 MiB blob; lazygit has 7,550 commits. "loses" names what the
// library does not win: t time, b bytes, a allocations.
//
//	shape                   repo     mode  cli                  ermite-git           verdict
//	ListRefs                fixture  warm  905µs 116.3K 125     24µs 19.0K 73        win
//	ListRefs                fixture  cold  904µs 116.3K 125     88µs 35.2K 203       loses a
//	ListRefs                lazygit  warm  901µs 105.2K 88      15µs 2.2K 27         win
//	ListRefs                lazygit  cold  908µs 105.4K 88      88µs 14.2K 163       loses a
//	ResolveRevision/branch  fixture  warm  22µs 0.9K 19         4µs 0.4K 8           win
//	ResolveRevision/branch  fixture  cold  22µs 0.9K 19         68µs 16.6K 138       loses tba
//	ResolveRevision/branch  lazygit  warm  22µs 1.0K 19         4µs 0.5K 8           win
//	ResolveRevision/branch  lazygit  cold  23µs 1.0K 19         72µs 12.4K 144       loses tba
//	ResolveRevision/fullid  fixture  warm  9µs 1.3K 22          2µs 0.4K 6           win
//	ResolveRevision/fullid  fixture  cold  9µs 1.3K 22          65µs 16.6K 136       loses tba
//	ResolveRevision/fullid  lazygit  warm  9µs 1.3K 22          2µs 0.4K 6           win
//	ResolveRevision/fullid  lazygit  cold  9µs 1.3K 22          70µs 12.3K 142       loses tba
//	ResolveRevision/abbrev  fixture  warm  63µs 1.5K 31         9µs 0.7K 11          win
//	ResolveRevision/abbrev  fixture  cold  64µs 1.5K 31         71µs 16.8K 141       loses tba
//	ResolveRevision/abbrev  lazygit  warm  63µs 1.5K 31         9µs 0.6K 11          win
//	ResolveRevision/abbrev  lazygit  cold  63µs 1.5K 31         76µs 12.6K 147       loses tba
//	ResolveRefPath          fixture  warm  104µs 2.4K 59        15µs 0.8K 22         win
//	ResolveRefPath          fixture  cold  105µs 2.4K 59        78µs 17.0K 152       loses ba
//	ResolveRefPath          lazygit  warm  150µs 4.1K 79        22µs 1.4K 29         win
//	ResolveRefPath          lazygit  cold  148µs 4.1K 79        89µs 13.4K 165       loses ba
//	GetTree/root            fixture  warm  717µs 108.4K 114     13µs 5.8K 24         win
//	GetTree/root            fixture  cold  725µs 108.4K 114     78µs 22.4K 154       loses a
//	GetTree/root            lazygit  warm  827µs 121.4K 167     29µs 18.2K 56        win
//	GetTree/root            lazygit  cold  831µs 121.5K 167     376µs 1.6M 200       loses ba
//	GetTree/dir             fixture  warm  742µs 109.5K 141     16µs 9.2K 26         win
//	GetTree/dir             fixture  cold  745µs 109.5K 141     109µs 75.8K 158      loses a
//	GetTree/dir             lazygit  warm  1.4ms 267.9K 1058    193µs 318.4K 73      loses b
//	GetTree/dir             lazygit  cold  1.4ms 268.0K 1058    4.2ms 2.0M 2490      loses tba
//	GetTree/root+last       fixture  warm  4.0ms 1.2M 1023      70µs 108.3K 107      win
//	GetTree/root+last       fixture  cold  4.0ms 1.2M 1024      165µs 127.8K 249     win
//	GetTree/root+last       lazygit  warm  20.2ms 3.8M 3072     6.1ms 367.0K 510     win
//	GetTree/root+last       lazygit  cold  20.3ms 3.8M 3073     8.5ms 3.4M 4204      loses a
//	GetTree/dir+last        fixture  warm  5.3ms 1.8M 1454      190µs 166.1K 142     win
//	GetTree/dir+last        fixture  cold  5.4ms 1.8M 1455      316µs 247.6K 323     win
//	GetTree/dir+last        lazygit  warm  195.2ms 20.5M 17116  7.9ms 1.5M 855       win
//	GetTree/dir+last        lazygit  cold  195.9ms 20.5M 17107  14.8ms 5.1M 6989     win
//	ListPaths               fixture  warm  760µs 118.4K 225     41µs 37.9K 30         win
//	ListPaths               fixture  cold  771µs 118.4K 225     107µs 54.7K 160       win
//	ListPaths               lazygit  warm  1.8ms 432.9K 2280    842µs 250.3K 477      win
//	ListPaths               lazygit  cold  1.8ms 432.9K 2280    1.2ms 298.7K 749      win
//	OpenBlob/small          fixture  warm  46µs 6.1K 25         17µs 5.8K 14         win
//	OpenBlob/small          fixture  cold  47µs 6.1K 25         87µs 27.7K 147       loses tba
//	OpenBlob/small          lazygit  warm  51µs 5.4K 25         29µs 7.0K 42         loses ba
//	OpenBlob/small          lazygit  cold  50µs 5.4K 25         131µs 25.1K 184      loses tba
//	OpenBlob/largest        fixture  warm  21.4ms 69.7K 64      52.5ms 9.0K 13       loses t
//	OpenBlob/largest        fixture  cold  21.6ms 70.0K 64      51.4ms 26.2K 143     loses ta
//	OpenBlob/largest        lazygit  warm  3.3ms 69.9K 64       5.7ms 16.3K 123      loses ta
//	OpenBlob/largest        lazygit  cold  3.3ms 69.9K 64       5.8ms 31.6K 283      loses ta
//	ListCommits/page1       fixture  warm  1.0ms 122.5K 404     61µs 19.1K 108       win
//	ListCommits/page1       fixture  cold  1.0ms 122.5K 404     134µs 36.9K 244      win
//	ListCommits/page1       lazygit  warm  1.3ms 157.2K 415     361µs 120.1K 224     win
//	ListCommits/page1       lazygit  cold  1.3ms 157.2K 415     503µs 136.2K 377     win
//	ListCommits/deep        fixture  warm  1.0ms 122.1K 404     71µs 19.3K 108       win
//	ListCommits/deep        fixture  cold  1.0ms 122.1K 404     136µs 36.1K 238      win
//	ListCommits/deep        lazygit  warm  2.0ms 123.2K 404     424µs 23.9K 112      win
//	ListCommits/deep        lazygit  cold  2.0ms 123.2K 404     544µs 36.3K 248      win
//	ListCommits/path        fixture  warm  1.6ms 108.8K 138     443µs 25.4K 42       win
//	ListCommits/path        fixture  cold  1.6ms 108.9K 138     594µs 93.6K 360      loses a
//	ListCommits/path        lazygit  warm  12.9ms 108.7K 118    6.0ms 29.2K 58       win
//	ListCommits/path        lazygit  cold  12.8ms 107.7K 118    9.5ms 2.4M 6498      loses ba
//	ListCommits/dirpath     fixture  warm  1.3ms 123.0K 403     199µs 33.8K 113      win
//	ListCommits/dirpath     fixture  cold  1.3ms 123.0K 403     277µs 55.4K 268      win
//	ListCommits/dirpath     lazygit  warm  6.7ms 127.8K 403     2.0ms 50.2K 181      win
//	ListCommits/dirpath     lazygit  cold  6.7ms 127.8K 403     3.3ms 887.1K 2019    loses ba
//
// The "verdict" column is the strict reading, every line of the table won;
// the routes follow the rule above. What decides them, with ermite-git 5228465
// (zlib reader with a reused adler32, header-size cache, Refresh and
// LookupRefID without allocations):
//
//   - ListRefs (every repository page, through loadRepo): 905 µs against 24 µs
//     warm, 120 KiB against 19 KiB (fixture), 960 against 16 µs and 108 KiB
//     against 2 KiB (lazygit). Cold it is still 4 to 10 times faster
//     (93 µs); only its allocation count loses. Routed.
//   - ResolveRevision (name, full id, abbreviation) and ResolveRefPath: 4 to 8
//     times faster warm, with fewer bytes. Cold they lose 45 µs of time to the
//     open, but the ListRefs of the same page has opened the repository
//     already, so the open is paid by a call that wins it back. Routed.
//   - GetTree with last commits: root listing 4.1 ms against 68 µs on the
//     fixture, 20.5 ms against 6.2 ms on lazygit (cold 8.7 ms; its 4,206
//     allocations against 3,082 are the only line lost); a subdirectory of
//     lazygit 195-199 ms against 8-9 ms warm, 15-17 ms cold, 20 MiB against
//     1.5 MiB. They are two constants so that either can be flipped back on
//     its own. Both routed.
//   - GetTree without last commits wins warm in time everywhere (13 µs against
//     0.7 ms at the root), but a large directory of lazygit loses warm bytes
//     (325 KiB against 275 KiB) and, cold, 4.0 ms against 1.4 ms in time. It
//     stays on the CLI.
//   - ListCommits of a revision wins every line and stays routed.
//     ListCommitsPath wins warm and cold in time (lazygit file: 12.9 ms against
//     6.0 ms warm), only its cold bytes lose (2.4 MiB): it is a candidate, not
//     flipped here, to be decided with its own conformance review.
//   - OpenBlob by size: below 1 MiB the small blob loses warm bytes and
//     allocations on lazygit; above, it loses time (inflate speed: 52 ms
//     against 21 ms). No size makes it win, so there is no size-based route.
//   - ListPaths (the recursive path list of the finder) wins every line, even
//     cold, because one walk replaces a process and the parsing of its output
//     (the bytes of the paths are kept in 32 KiB blocks, which is what brought
//     the lazygit bytes under the CLI's: a single growing buffer lost them,
//     699K against 433K). Routed.
//   - TagDistance (the inbox: the commits since the nearest tag, one call per
//     repository) peels the tags and walks with IDOnly from the commit-graph:
//     on the fixture 1.5 ms 113K 158 against 11µs 5.1K 36 warm and 84µs
//     21.7K 166 cold (ermite-git 0.1.1, same machine). Only its cold
//     allocation count loses, as for ListRefs. Routed.
func DefaultRoutes() Routes {
	return Routes{
		ListRefs:                 ErmiteGit,
		ResolveRevision:          ErmiteGit,
		ResolveRevisionAbbrev:    ErmiteGit,
		ResolveRefPath:           ErmiteGit,
		GetTree:                  CLI,
		GetTreeLastCommits:       ErmiteGit,
		GetTreeSubdirLastCommits: ErmiteGit,
		OpenBlob:                 CLI,
		ListCommits:              ErmiteGit,
		ListCommitsPath:          CLI,
		ListPaths:                ErmiteGit,
		TagDistance:              ErmiteGit,
	}
}
