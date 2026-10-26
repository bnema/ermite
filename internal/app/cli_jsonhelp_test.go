// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
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

// timeField matches the json names of the fields that hold an instant.
var timeField = regexp.MustCompile(`^(at|time)$|_(at|until)$`)

// TestJSONStructsHaveTags checks that each exported field of a jsonX struct has a
// json tag: the field list of the help is built from it, and a field without a
// tag would show under its Go name while the output uses another. A time field
// must be a jsonInstant, which is how the help finds the times.
func TestJSONStructsHaveTags(t *testing.T) {
	_, files := parseSources(t)
	found := 0
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || !strings.HasPrefix(spec.Name.Name, "json") {
				return true
			}
			struc, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			found++
			for _, field := range struc.Fields.List {
				raw := ""
				if field.Tag != nil {
					raw, _ = strconv.Unquote(field.Tag.Value)
				}
				tag := reflect.StructTag(raw)
				label := name + ": " + spec.Name.Name
				if len(field.Names) == 0 { // embedded: flattened, or stands for its fields with a see tag
					if tag.Get("see") != "" {
						assert.NotEmpty(t, tag.Get("help"), "%s: an embedded struct with a see tag needs a help", label)
					}
					continue
				}
				jsonName, _, _ := strings.Cut(tag.Get("json"), ",")
				for _, id := range field.Names {
					if id.IsExported() {
						assert.NotEmpty(t, jsonName, "%s.%s has no json tag", label, id.Name)
					}
				}
				if timeField.MatchString(jsonName) {
					typ, _ := field.Type.(*ast.Ident)
					assert.True(t, typ != nil && typ.Name == "jsonInstant", "%s.%s is a time: make it a jsonInstant", label, jsonName)
				}
			}
			return true
		})
	}
	assert.NotZero(t, found, "the json structs are found")
}

// TestWatchTypesDocumented checks that the help of `ermite watch` names every
// event type it can print.
func TestWatchTypesDocumented(t *testing.T) {
	f, ok := reflect.TypeOf(jsonEvent{}).FieldByName("Type")
	require.True(t, ok)
	help := f.Tag.Get("help")
	_, files := parseSources(t)
	found := 0
	ast.Inspect(files["cli_watch.go"], func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || !strings.HasPrefix(spec.Names[0].Name, "event") || len(spec.Values) != 1 {
			return true
		}
		if lit, ok := spec.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, _ := strconv.Unquote(lit.Value)
			found++
			assert.Regexp(t, `\b`+regexp.QuoteMeta(value)+`\b`, help, "the watch help names %s", value)
		}
		return true
	})
	assert.NotZero(t, found, "the event types are found")
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
				assert.NotContains(t, strings.ToLower(lit.Value), "json field", "%s: build field lists with jsonFieldsHelp", name)
				assert.NotContains(t, lit.Value, "(--json)", "%s: build field lists with jsonFieldsHelp", name)
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
