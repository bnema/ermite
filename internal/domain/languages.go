// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"cmp"
	"path"
	"slices"
	"strings"
	"time"
)

// LanguagesVersion is the version of the counting algorithm and its table. A
// stored count with another version is recomputed.
const LanguagesVersion = 1

// MaxLanguageFiles bounds the regular files counted in one repository.
const MaxLanguageFiles = MaxPathEntries

// MaxLanguageBlob is the largest file counted: bigger ones are likely
// generated or bundled.
const MaxLanguageBlob = 1 << 20

// FileSize is a regular file of a tree with its size in bytes.
type FileSize struct {
	Path string
	Size int64
}

// FileSizes is the list of regular files of a tree.
type FileSizes struct {
	Files []FileSize
	// Truncated is true when the tree has more files than were listed.
	Truncated bool
}

// LanguageBytes is the bytes counted for one language.
type LanguageBytes struct {
	Name  string
	Bytes int64
}

// LanguageShare is the share of one language in the bytes counted.
type LanguageShare struct {
	Name    string
	Bytes   int64
	Percent float64 // 0-100
}

// RepoLanguages is the language count of a repository head.
type RepoLanguages struct {
	Repo RepoName
	// RepoCreatedAt is the creation time of the repository it was computed
	// for: a repository created again under the name does not inherit it.
	RepoCreatedAt time.Time
	Commit        ObjectID
	Version       int
	ComputedAt    time.Time
	// Languages are ordered by bytes descending, then name.
	Languages []LanguageBytes
	// Truncated is true when the file limit was hit: the count is partial.
	Truncated bool
}

// languageByName maps an exact file base name to its language.
var languageByName = map[string]string{
	"Makefile":       "Makefile",
	"makefile":       "Makefile",
	"GNUmakefile":    "Makefile",
	"Dockerfile":     "Dockerfile",
	"CMakeLists.txt": "CMake",
	"Jenkinsfile":    "Groovy",
	"Rakefile":       "Ruby",
	"Gemfile":        "Ruby",
}

// languageByExt maps a lowercased extension, dot included, to its language.
// Only programming and markup languages are listed: data and prose (.json,
// .yaml, .toml, .xml, .csv, .md, .txt, .rst, .lock) are left out on purpose.
// Ambiguous extensions have one fixed answer: .h is C, .m Objective-C, .pl
// Perl, .v Verilog.
var languageByExt = map[string]string{
	".go": "Go", ".rs": "Rust",
	".c": "C", ".h": "C",
	".cc": "C++", ".cpp": "C++", ".cxx": "C++", ".hpp": "C++", ".hh": "C++", ".hxx": "C++",
	".cs": "C#", ".java": "Java", ".kt": "Kotlin", ".kts": "Kotlin", ".scala": "Scala",
	".swift": "Swift", ".m": "Objective-C", ".mm": "Objective-C++",
	".py": "Python", ".pyi": "Python", ".rb": "Ruby", ".php": "PHP", ".pl": "Perl", ".pm": "Perl",
	".lua": "Lua",
	".js":  "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".jsx": "JavaScript",
	".ts": "TypeScript", ".tsx": "TypeScript", ".mts": "TypeScript", ".cts": "TypeScript",
	".vue": "Vue", ".svelte": "Svelte",
	".ex": "Elixir", ".exs": "Elixir", ".erl": "Erlang", ".hrl": "Erlang",
	".hs": "Haskell", ".ml": "OCaml", ".mli": "OCaml", ".fs": "F#", ".fsx": "F#",
	".clj": "Clojure", ".cljs": "Clojure", ".cljc": "Clojure",
	".dart": "Dart", ".zig": "Zig", ".nim": "Nim", ".r": "R", ".jl": "Julia",
	".sh": "Shell", ".bash": "Shell", ".zsh": "Shell", ".fish": "Fish",
	".ps1": "PowerShell", ".psm1": "PowerShell",
	".mk": "Makefile", ".dockerfile": "Dockerfile", ".cmake": "CMake",
	".nix": "Nix", ".tf": "HCL", ".hcl": "HCL", ".templ": "Templ", ".proto": "Protocol Buffer",
	".sql": "SQL", ".html": "HTML", ".htm": "HTML",
	".css": "CSS", ".scss": "SCSS", ".less": "Less",
	".tex": "TeX", ".s": "Assembly", ".asm": "Assembly",
	".vim": "Vim Script", ".el": "Emacs Lisp", ".groovy": "Groovy", ".gradle": "Gradle",
	".v": "Verilog", ".sv": "SystemVerilog", ".vhd": "VHDL", ".coffee": "CoffeeScript",
	".sol": "Solidity", ".cr": "Crystal", ".d": "D", ".pas": "Pascal", ".f90": "Fortran",
	".lisp": "Common Lisp", ".scm": "Scheme", ".rkt": "Racket", ".gleam": "Gleam",
	".odin": "Odin", ".astro": "Astro", ".graphql": "GraphQL", ".gql": "GraphQL",
}

var languageExcludedSegments = map[string]struct{}{
	"third_party": {}, "thirdparty": {}, "Godeps": {}, "bower_components": {},
	"dist": {}, ".git": {},
}

var languageExcludedRootDirs = map[string]struct{}{
	"docs": {}, "doc": {}, "Documentation": {},
}

// languageExcluded reports whether the path alone keeps a file out of the
// count: generated and vendored files, documentation at the root, and
// dot-directories other than .github.
func languageExcluded(p string) bool {
	if IsGeneratedPath(p) {
		return true
	}
	segs := strings.Split(p, "/")
	dirs := segs[:len(segs)-1]
	for i, seg := range dirs {
		if _, ok := languageExcludedSegments[seg]; ok {
			return true
		}
		if i == 0 {
			if _, ok := languageExcludedRootDirs[seg]; ok {
				return true
			}
		}
		if strings.HasPrefix(seg, ".") && seg != ".github" {
			return true
		}
	}
	return false
}

// LanguageOf returns the language of a file by its base name, then by its
// lowercased extension. It does not apply the path exclusions.
func LanguageOf(p string) (string, bool) {
	base := path.Base(p)
	if lang, ok := languageByName[base]; ok {
		return lang, true
	}
	lang, ok := languageByExt[strings.ToLower(path.Ext(base))]
	return lang, ok
}

// LanguageCounted reports whether a file of that size counts: it is not
// excluded by its path, is not empty and is at most MaxLanguageBlob.
func LanguageCounted(p string, size int64) bool {
	return size > 0 && size <= MaxLanguageBlob && !languageExcluded(p)
}

// CountLanguages sums the bytes of the counted files by language. Repo,
// RepoCreatedAt, Commit, Version and ComputedAt are left to the caller.
// Languages is nil when nothing counts.
func CountLanguages(files FileSizes) RepoLanguages {
	sums := map[string]int64{}
	for _, f := range files.Files {
		if !LanguageCounted(f.Path, f.Size) {
			continue
		}
		if lang, ok := LanguageOf(f.Path); ok {
			sums[lang] += f.Size
		}
	}
	out := RepoLanguages{Truncated: files.Truncated}
	for name, n := range sums {
		out.Languages = append(out.Languages, LanguageBytes{Name: name, Bytes: n})
	}
	slices.SortFunc(out.Languages, func(a, b LanguageBytes) int {
		if c := cmp.Compare(b.Bytes, a.Bytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

// Shares returns the languages with their percentage of the bytes counted, in
// the order of Languages. It is nil when there are no bytes.
func (r RepoLanguages) Shares() []LanguageShare {
	var total int64
	for _, l := range r.Languages {
		total += l.Bytes
	}
	if total == 0 {
		return nil
	}
	out := make([]LanguageShare, 0, len(r.Languages))
	for _, l := range r.Languages {
		out = append(out, LanguageShare{Name: l.Name, Bytes: l.Bytes, Percent: 100 * float64(l.Bytes) / float64(total)})
	}
	return out
}
