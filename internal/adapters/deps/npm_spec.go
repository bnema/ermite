// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"golang.org/x/mod/semver"
)

var npmSpecRE = regexp.MustCompile(`^(\^|~|>=|=)?v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)

func npmSpec(s string) (operator, version string, ok bool) {
	m := npmSpecRE.FindStringSubmatch(s)
	if m == nil || !semver.IsValid("v"+m[2]) {
		return "", "", false
	}
	return m[1], "v" + m[2], true
}

type npmJSONNode struct {
	start, end int
	value      string
	object     map[string]*npmJSONNode
}

// npmJSON records original byte spans rather than reserializing the manifest.
func npmJSON(content []byte) (*npmJSONNode, error) {
	if !json.Valid(content) {
		return nil, fmt.Errorf("%w: invalid package.json", domain.ErrInvalid)
	}
	d := json.NewDecoder(bytes.NewReader(content))
	d.UseNumber() // numbers stay text: 1e400 is valid JSON
	var parse func() (*npmJSONNode, error)
	parse = func() (*npmJSONNode, error) {
		start := int(d.InputOffset())
		for start < len(content) && (content[start] == ' ' || content[start] == '\n' || content[start] == '\r' || content[start] == '\t' || content[start] == ':' || content[start] == ',') {
			start++
		}
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		n := &npmJSONNode{start: start}
		switch v := tok.(type) {
		case string:
			n.value = v
		case json.Delim:
			if v == '{' {
				n.object = map[string]*npmJSONNode{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return nil, err
					}
					k := key.(string)
					if _, dup := n.object[k]; dup {
						return nil, fmt.Errorf("%w: duplicate JSON key %q", domain.ErrInvalid, k)
					}
					child, err := parse()
					if err != nil {
						return nil, err
					}
					n.object[k] = child
				}
			} else if v == '[' {
				for d.More() {
					if _, err := parse(); err != nil {
						return nil, err
					}
				}
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
		}
		n.end = int(d.InputOffset())
		return n, nil
	}
	return parse()
}

// npmEditSpec replaces only matching dependency string values, preserving all
// other bytes, including formatting, scripts and unrelated nested objects.
//
// newSpec is written as raw bytes, like the runner's edit script: it must
// match the runner's range rule, which needs no JSON escaping.
func npmEditSpec(content []byte, name, oldSpec, newSpec string) ([]byte, error) {
	if !npmRangeRE.MatchString(newSpec) {
		return nil, fmt.Errorf("%w: unsupported npm range %q", domain.ErrInvalid, newSpec)
	}
	root, err := npmJSON(content)
	if err != nil {
		return nil, err
	}
	var edits []*npmJSONNode
	for _, section := range []string{"dependencies", "devDependencies"} {
		object := root.object[section]
		if object == nil {
			continue
		}
		n := object.object[name]
		if n == nil {
			continue
		}
		if n.value != oldSpec || n.start >= len(content) || content[n.start] != '"' {
			return nil, fmt.Errorf("%w: dependency spec mismatch for %q", domain.ErrInvalid, name)
		}
		edits = append(edits, n)
	}
	if len(edits) == 0 {
		return nil, fmt.Errorf("%w: dependency %q missing", domain.ErrInvalid, name)
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	replacement := []byte(`"` + newSpec + `"`)
	out := bytes.Clone(content)
	for _, n := range edits {
		out = append(append(append([]byte{}, out[:n.start]...), replacement...), out[n.end:]...)
	}
	return out, nil
}

func npmRange(spec, version string) (string, error) {
	op, _, ok := npmSpec(spec)
	if !ok {
		return "", fmt.Errorf("%w: unsupported npm spec", domain.ErrInvalid)
	}
	return op + strings.TrimPrefix(version, "v"), nil
}
