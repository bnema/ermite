// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
)

// WorkflowDir is the only directory the workflows of a repository are read
// from (ADR 0019: no ermite dialect).
const WorkflowDir = ".github/workflows"

// maxWorkflowFileName bounds the file name of a workflow, as a file name of
// Git trees is bounded on the usual file systems.
const maxWorkflowFileName = 255

// ErrWorkflowDisabled is returned by the store when a run is created for a
// workflow the repository disabled. It is checked in the very transaction that
// creates the run, so no disable can slip in between a check and the creation.
var ErrWorkflowDisabled = errors.New("workflow is disabled")

// ParseWorkflowPath normalizes the name of a workflow file, as `gh workflow`
// takes it: either its file name ("release.yml") or its path
// (".github/workflows/release.yml"). It returns the canonical path, and
// refuses every other spelling of it ("./.github/workflows/x.yml",
// ".github/workflows//x.yml", ".github/workflows/sub/../x.yml"), so one file has
// one key. Only the shape is checked, never whether the file exists: the state
// of a workflow can be set on a repository that is empty or that does not hold
// the file yet. A file in a subdirectory, a name that does not end in .yml or
// .yaml, and a name with a control character or a backslash are ErrInvalid.
func ParseWorkflowPath(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: the workflow file is required", ErrInvalid)
	}
	if !strings.Contains(s, "/") {
		s = WorkflowDir + "/" + s
	}
	name, ok := strings.CutPrefix(s, WorkflowDir+"/")
	ext := strings.ToLower(path.Ext(name))
	switch {
	case !ok,
		strings.Contains(name, "/"),
		ext != ".yml" && ext != ".yaml",
		strings.TrimSuffix(name, path.Ext(name)) == "",
		len(name) > maxWorkflowFileName,
		strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || r == '\\' }):
		return "", fmt.Errorf("%w: %q is not a workflow file of %s (a file name or the path %s/<file>, ending in .yml or .yaml)", ErrInvalid, s, WorkflowDir, WorkflowDir)
	}
	return s, nil
}
