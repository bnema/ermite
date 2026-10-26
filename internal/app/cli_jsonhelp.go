// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"reflect"
	"strings"
)

// jsonInstant is an instant in a JSON output: an RFC 3339 UTC string, or null
// when unset (see jsonTime). jsonFieldsHelp finds the times by this type.
type jsonInstant *string

var jsonInstantType = reflect.TypeOf(jsonInstant(nil))

// jsonFieldsHelp lists the JSON fields of v, a struct the command encodes (or a
// pointer to one), for the help of that command. It walks the struct in field
// order, so that the help follows the real output. Each field takes its name
// from the json tag. These struct tags refine it:
//
//	help:"text"  said in parentheses after the name, when the meaning is not obvious
//	see:"ermite mr"  the fields are not repeated: see `ermite mr --help`
//
// Nested structs, slices of structs and pointers to structs are listed as
// "name (help; field, field)". An embedded struct is flattened, as encoding/json
// does, unless it has a see tag: its help then stands for it. Maps, MarshalJSON
// types and recursive types are not handled: the json structs have none (a
// recursive one would overflow the stack in every test that builds the CLI).
func jsonFieldsHelp(title string, v any) string {
	times := false
	list := jsonFieldList(indirectType(reflect.TypeOf(v)), &times)
	s := "JSON fields of " + title + " (--json): " + strings.Join(list, ", ") + "."
	if times {
		s += " Times are RFC 3339 UTC or null."
	}
	return s
}

// indirectType strips pointers and slices.
func indirectType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t
}

func jsonFieldList(t reflect.Type, times *bool) []string {
	var out []string
	for _, f := range reflect.VisibleFields(t) {
		if len(f.Index) > 1 {
			continue // promoted from an embedded struct, listed at the embedding
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		help, see := f.Tag.Get("help"), f.Tag.Get("see")
		sub := indirectType(f.Type)
		if f.Anonymous && name == "" && sub.Kind() == reflect.Struct {
			if see != "" {
				out = append(out, jsonFieldItem(help, "", see, nil))
			} else {
				out = append(out, jsonFieldList(sub, times)...)
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name // as encoding/json does
		}
		if f.Type == jsonInstantType {
			*times = true
		}
		var nested []string
		if sub.Kind() == reflect.Struct && see == "" {
			nested = jsonFieldList(sub, times)
		}
		out = append(out, jsonFieldItem(name, help, see, nested))
	}
	return out
}

// jsonFieldItem is "name (help; see `ermite X --help`; field, field)".
func jsonFieldItem(name, help, see string, nested []string) string {
	var parts []string
	if help != "" {
		parts = append(parts, help)
	}
	if see != "" {
		parts = append(parts, "see `"+see+" --help`")
	}
	if len(nested) > 0 {
		parts = append(parts, strings.Join(nested, ", "))
	}
	if len(parts) == 0 {
		return name
	}
	return name + " (" + strings.Join(parts, "; ") + ")"
}
