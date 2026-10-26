// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// jsonPointer matches the line that sends a reader to the help holding the JSON fields.
var jsonPointer = regexp.MustCompile("JSON: [^`]*`ermite ([a-z -]+) --help`")

// TestHelpJSONPointersResolve checks every "JSON: … see `ermite X --help`" pointer:
// X exists and its long help lists JSON fields.
func TestHelpJSONPointersResolve(t *testing.T) {
	root := newRootCmd()
	found := 0
	walkCommands(root, func(cmd *cobra.Command) {
		for _, m := range jsonPointer.FindAllStringSubmatch(cmd.Long, -1) {
			found++
			target, _, err := root.Find(strings.Fields(m[1]))
			if !assert.NoError(t, err, "%s points at %q", cmd.CommandPath(), m[1]) {
				continue
			}
			assert.Contains(t, target.Long, "(--json)", "%s points at %s, which lists no JSON fields", cmd.CommandPath(), target.CommandPath())
		}
	})
	assert.Greater(t, found, 20, "the pointers are found")
}

// TestHelpLinesFit checks that the long help is wrapped for a terminal.
func TestHelpLinesFit(t *testing.T) {
	walkCommands(newRootCmd(), func(cmd *cobra.Command) {
		for _, line := range strings.Split(cmd.Long, "\n") {
			if strings.HasPrefix(line, " ") || !strings.Contains(line, " ") {
				continue
			}
			assert.LessOrEqual(t, len(line), helpWidth, "%s: %q", cmd.CommandPath(), line)
		}
	})
}

func TestWrapHelp(t *testing.T) {
	assert.Equal(t, "aa bb\ncc", wrapHelp("aa bb cc", 5))
	assert.Equal(t, "  indented line stays", wrapHelp("  indented line stays", 5), "examples keep their layout")
	assert.Equal(t, "short\n\nnext", wrapHelp("short\n\nnext", 5), "blank lines stay")
	assert.Equal(t, "see\n`ermite mr --help`", wrapHelp("see `ermite mr --help`", 12), "a code span is not split")
}

func walkCommands(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, sub := range cmd.Commands() {
		walkCommands(sub, fn)
	}
}
