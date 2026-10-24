// SPDX-License-Identifier: AGPL-3.0-or-later

// Package deps implements dependency ecosystem detection and bounded file edits.
package deps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// Actions handles GitHub Actions uses references.
type Actions struct{}

var _ ports.DepsEcosystem = Actions{}

var usesLine = regexp.MustCompile(`^\s*-?\s*uses:\s*["']?([\w.-]+)/([\w.-]+)(/[^@\s"']*)?@([^\s"'#]+)`)
var shaRef = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// Name identifies the ecosystem.
func (Actions) Name() domain.DepsEcosystem { return domain.EcosystemActions }

// ApplyInServer reports that action edits need no runner.
func (Actions) ApplyInServer() bool { return true }

// Manifests selects workflow and composite action files.
func (Actions) Manifests(paths domain.PathList) []domain.TreePath {
	var out []domain.TreePath
	for _, p := range paths.Paths {
		base := path.Base(p)
		workflow := path.Dir(p) == ".github/workflows" && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml"))
		if !workflow && base != "action.yml" && base != "action.yaml" {
			continue
		}
		if tp, err := domain.ParseTreePath(p); err == nil {
			out = append(out, tp)
		}
	}
	return out
}

// Detect returns one candidate per file, action and current ref.
//
//hexcheck:ignore suspicious-business-logic-in-adapter parses uses syntax and resolves tags through the registry port; update policy stays in domain.PickUpdate
func (Actions) Detect(ctx context.Context, files map[domain.TreePath][]byte, reg ports.DepsRegistry) ([]domain.DepCandidate, error) {
	var out []domain.DepCandidate
	seen := map[string]bool{}
	for _, p := range filePaths(files) {
		for _, line := range bytes.Split(files[p], []byte("\n")) {
			m := usesLine.FindSubmatch(line)
			if m == nil || shaRef.Match(m[4]) {
				continue
			}
			name, current := string(m[1])+"/"+string(m[2]), string(m[4])
			key := p.String() + "\x00" + name + "\x00" + current
			if seen[key] {
				continue
			}
			seen[key] = true
			tags, err := reg.ActionTags(ctx, string(m[1]), string(m[2]))
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			c := domain.DepCandidate{Ecosystem: domain.EcosystemActions, Directory: p.String(), Name: name, Current: current}
			for _, tag := range tags {
				c.Versions = append(c.Versions, domain.DepVersion{Version: tag.Name, SHA: tag.SHA})
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// Apply changes only matched uses version bytes, preserving quotes and comments.
func (Actions) Apply(files map[domain.TreePath][]byte, ups []domain.DepUpdate) (map[domain.TreePath][]byte, error) {
	out := make(map[domain.TreePath][]byte, len(files))
	for p, b := range files {
		out[p] = bytes.Clone(b)
	}
	for _, u := range ups {
		if u.Ecosystem != domain.EcosystemActions || !validActionRef(u.Current) || !validActionRef(u.Version) {
			return nil, fmt.Errorf("%w: invalid action update", domain.ErrInvalid)
		}
		p, err := domain.ParseTreePath(u.Directory)
		if err != nil {
			return nil, err
		}
		original, ok := out[p]
		if !ok {
			return nil, fmt.Errorf("%w: missing action file %s", domain.ErrInvalid, u.Directory)
		}
		lines := bytes.Split(original, []byte("\n"))
		matched := false
		for i, line := range lines {
			m := usesLine.FindSubmatchIndex(line)
			if m == nil {
				continue
			}
			name := string(line[m[2]:m[3]]) + "/" + string(line[m[4]:m[5]])
			if name != u.Name || string(line[m[8]:m[9]]) != u.Current {
				continue
			}
			changed := append([]byte(nil), line[:m[8]]...)
			changed = append(changed, u.Version...)
			changed = append(changed, line[m[9]:]...)
			lines[i] = changed
			matched = true
		}
		if !matched {
			return nil, fmt.Errorf("%w: action %s@%s not found in %s", domain.ErrInvalid, u.Name, u.Current, u.Directory)
		}
		out[p] = bytes.Join(lines, []byte("\n"))
	}
	return out, nil
}

func validActionRef(s string) bool { return s != "" && !strings.ContainsAny(s, " \t\r\n\"'#@") }

// Validate accepts only the exact requested substitutions, with no other byte changes.
func (a Actions) Validate(before, after map[domain.TreePath][]byte, ups []domain.DepUpdate) error {
	expected, err := a.Apply(before, ups)
	if err != nil {
		return err
	}
	for p, b := range after {
		want, ok := expected[p]
		if !ok || !bytes.Equal(b, want) {
			return fmt.Errorf("%w: unexpected action changes in %s", domain.ErrInvalid, p.String())
		}
	}
	for p, b := range expected {
		if !bytes.Equal(b, before[p]) {
			if actual, ok := after[p]; !ok || !bytes.Equal(actual, b) {
				return fmt.Errorf("%w: missing action update in %s", domain.ErrInvalid, p.String())
			}
		}
	}
	return nil
}

func filePaths(files map[domain.TreePath][]byte) []domain.TreePath {
	out := make([]domain.TreePath, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
