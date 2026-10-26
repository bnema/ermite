// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONFieldsHelp(t *testing.T) {
	type leaf struct {
		A string `json:"a" help:"first"`
		B int    `json:"b"`
	}
	type base struct {
		ID string `json:"id"`
	}
	type shape struct {
		base
		Skipped string      `json:"-"`
		Name    string      `json:"name,omitempty" help:"say it"`
		At      jsonInstant `json:"at"`
		One     *leaf       `json:"one"`
		Many    []leaf      `json:"many" help:"all of them"`
		Tags    []string    `json:"tags"`
		hidden  string
	}
	_ = shape{}.hidden
	assert.Equal(t,
		"JSON fields of a shape (--json): id, name (say it), at, one (a (first), b), many (all of them; a (first), b), tags."+
			" Times are RFC 3339 UTC or null.",
		jsonFieldsHelp("a shape", shape{}))
	assert.Equal(t, jsonFieldsHelp("a shape", shape{}), jsonFieldsHelp("a shape", &shape{}), "a pointer reads as its struct")
	assert.Equal(t, "JSON fields of a leaf (--json): a (first), b.", jsonFieldsHelp("a leaf", []leaf{}), "a slice reads as its element")

	type embedding struct {
		base `help:"the base" see:"ermite base"`
		Own  string `json:"own"`
	}
	assert.Equal(t, "JSON fields of e (--json): the base (see `ermite base --help`), own.", jsonFieldsHelp("e", embedding{}),
		"an embedded struct with a see tag stands for its fields")
}

// parseSources parses the non-test files of the package.
func parseSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files[name] = f
	}
	return fset, files
}

// TestJSONStructsHaveTags checks that each exported field of a jsonX struct has a
// json tag: the field list of the help is built from it, and a field without a
// tag would show under its Go name while the output uses another.
func TestJSONStructsHaveTags(t *testing.T) {
	_, files := parseSources(t)
	found := 0
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !strings.HasPrefix(spec.Name.Name, "json") {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			found++
			for _, field := range st.Fields.List {
				tag := ""
				if field.Tag != nil {
					tag, _ = strconv.Unquote(field.Tag.Value)
				}
				st := reflect.StructTag(tag)
				label := name + ": " + spec.Name.Name
				if len(field.Names) == 0 { // embedded: flattened, or stands for its fields with a see tag
					if st.Get("json") == "" {
						assert.NotEmpty(t, st.Get("help"), "%s: an embedded struct needs a json name or a help", label)
					}
					continue
				}
				for _, id := range field.Names {
					if id.IsExported() {
						assert.NotEmpty(t, st.Get("json"), "%s.%s has no json tag", label, id.Name)
					}
				}
				assert.True(t, isASCII(tag), "%s: the tags of %v are ASCII", label, field.Names)
			}
			return true
		})
	}
	assert.Greater(t, found, 40, "the json structs are found")
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// TestNoHandWrittenFieldList checks that no source text but the generator writes
// a list of JSON fields: those come from the structs, so that the help follows
// the output.
func TestNoHandWrittenFieldList(t *testing.T) {
	_, files := parseSources(t)
	for name, f := range files {
		if name == "cli_jsonhelp.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				assert.NotContains(t, lit.Value, "JSON fields", "%s: build field lists with jsonFieldsHelp", name)
				assert.NotContains(t, lit.Value, "JSON (--json)", "%s: build field lists with jsonFieldsHelp", name)
			}
			return true
		})
	}
}

// TestHelpDocumentsJSON checks that every command with a --json flag says what it
// prints: the generated field list, or a pointer to the help that holds it.
func TestHelpDocumentsJSON(t *testing.T) {
	walkCommands(newRootCmd(), func(cmd *cobra.Command) {
		if cmd.Flags().Lookup("json") == nil {
			return
		}
		documented := strings.Contains(cmd.Long, "(--json): ") || jsonPointer.MatchString(cmd.Long)
		assert.True(t, documented, "%s has --json and no JSON help", cmd.CommandPath())
	})
}
