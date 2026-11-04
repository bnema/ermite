package domain

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// botDraftPrefix marks Bernard's merge request as a draft (see IsDraft).
const botDraftPrefix = "WIP: "

// maxBotErrorBytes bounds the failure text shown in a merge request body.
const maxBotErrorBytes = 8 << 10

// BotDraftTitle is the title of Bernard's merge request while it is a draft:
// the draft prefix followed by BotSubject.
func BotDraftTitle(b DepBatch) string {
	return botDraftPrefix + BotSubject(b)
}

// DepsRepro returns the shell lines that redo ups by hand, grouped by
// ecosystem then directory, in a deterministic order, from the repository
// root (each group after the first goes back to it). Every argument is
// single-quoted for a POSIX shell. Actions updates are applied by the server
// and yield no command. The npm lines are written for npm even though the
// runner picks the package manager from the lockfile.
func DepsRepro(ups []DepUpdate) []string {
	type group struct {
		eco DepsEcosystem
		dir string
	}
	byGroup := map[group][]DepUpdate{}
	for _, u := range ups {
		if u.Ecosystem != EcosystemGoMod && u.Ecosystem != EcosystemNPM {
			continue
		}
		g := group{u.Ecosystem, u.Directory}
		byGroup[g] = append(byGroup[g], u)
	}
	keys := make([]group, 0, len(byGroup))
	for g := range byGroup {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, func(a, b group) int {
		return cmp.Or(cmp.Compare(a.eco, b.eco), cmp.Compare(a.dir, b.dir))
	})
	var lines []string
	for i, g := range keys {
		us := slices.Clone(byGroup[g])
		slices.SortFunc(us, func(a, b DepUpdate) int { return cmp.Compare(a.Name, b.Name) })
		if i > 0 {
			lines = append(lines, "cd \"$(git rev-parse --show-toplevel)\"")
		}
		if g.dir != "" {
			lines = append(lines, "cd -- "+shellQuote(g.dir))
		}
		switch g.eco {
		case EcosystemGoMod:
			for _, u := range us {
				name := u.Name
				if u.NewName != "" {
					lines = append(lines, fmt.Sprintf("# rewrite imports %s -> %s", oneLine(u.Name), oneLine(u.NewName)))
					name = u.NewName
				}
				lines = append(lines, "go get "+shellQuote(name+"@"+u.Version))
			}
			lines = append(lines, "go mod tidy")
		case EcosystemNPM:
			args := make([]string, len(us))
			for j, u := range us {
				args[j] = shellQuote(u.Name + "@" + u.Version)
			}
			lines = append(lines, "npm install --ignore-scripts "+strings.Join(args, " "))
		}
	}
	return lines
}

// shellQuote is s as one POSIX shell word: single-quoted, each ' written as
// '\'', control characters dropped (a word stays on its line). Unicode is kept.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(oneLine(s), "'", `'\''`) + "'"
}

// oneLine drops the control characters of s.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

// BotMRBodyFor is BotMRBody for batch b, followed by a failure section when b
// failed with an error (with b.Repro as local commands when present), and a
// note when a human adopted the branch.
func BotMRBodyFor(b DepBatch, ups []DepUpdate, effects []DepSideEffect) string {
	var s strings.Builder
	s.WriteString(BotMRBody(ups, effects))
	if b.State == DepsFailed && strings.TrimSpace(b.Error) != "" {
		msg := b.Error
		if len(msg) > maxBotErrorBytes {
			msg = msg[len(msg)-maxBotErrorBytes:]
			if k := strings.IndexByte(msg, '\n'); k >= 0 && k < len(msg)-1 {
				msg = msg[k+1:]
			}
			msg = "…\n" + msg
		}
		s.WriteString("\n### Bernard: update failed\n\n")
		writeFenced(&s, "", strings.TrimRight(msg, "\n"))
		if len(b.Repro) != 0 {
			s.WriteString("\nReproduce locally:\n\n")
			writeFenced(&s, "sh", strings.Join(b.Repro, "\n"))
		}
	}
	if b.Adopted {
		s.WriteString("\nThis branch was changed by hand: Bernard no longer rewrites it.\n")
	}
	return s.String()
}

// writeFenced writes text in a code fence longer than any backtick run in it.
func writeFenced(s *strings.Builder, lang, text string) {
	run, longest := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	fmt.Fprintf(s, "%s%s\n%s\n%s\n", fence, lang, text, fence)
}
