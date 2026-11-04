package deps

import (
	"errors"
	"reflect"
	"testing"

	"bnema.dev/ermite/internal/domain"
)

func goUp(dir, name, version, newName string) domain.DepUpdate {
	return domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: domain.EcosystemGoMod, Directory: dir, Name: name, Version: version}, NewName: newName}
}

func TestGoModBump(t *testing.T) {
	const base = `module example.com/app

go 1.24

toolchain go1.24.2

require (
	example.com/a v1.0.0
	example.com/b v1.1.0 // indirect
	example.com/lib v1.5.0
)

require example.com/single v0.1.0

replace example.com/a => ../a

exclude example.com/b v1.0.5
`
	const sub = `module example.com/app/tools

go 1.24

require example.com/a v1.0.0
`
	tests := []struct {
		name  string
		files map[string]string
		ups   []domain.DepUpdate
		want  map[string]string
		err   error
	}{
		{
			name:  "single bump",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("", "example.com/a", "v1.2.0", "")},
			want:  map[string]string{"go.mod": replaceOnce(base, "example.com/a v1.0.0", "example.com/a v1.2.0")},
		},
		{
			name:  "indirect kept",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("", "example.com/b", "v1.3.0", "")},
			want:  map[string]string{"go.mod": replaceOnce(base, "example.com/b v1.1.0 // indirect", "example.com/b v1.3.0 // indirect")},
		},
		{
			name:  "multiple updates in one file",
			files: map[string]string{"go.mod": base},
			ups: []domain.DepUpdate{
				goUp("", "example.com/a", "v1.2.0", ""),
				goUp("", "example.com/single", "v0.2.0", ""),
			},
			want: map[string]string{"go.mod": replaceOnce(replaceOnce(base, "example.com/a v1.0.0", "example.com/a v1.2.0"), "require example.com/single v0.1.0", "require example.com/single v0.2.0")},
		},
		{
			name:  "two directories",
			files: map[string]string{"go.mod": base, "tools/go.mod": sub},
			ups: []domain.DepUpdate{
				goUp("", "example.com/a", "v1.2.0", ""),
				goUp("tools", "example.com/a", "v1.3.0", ""),
			},
			want: map[string]string{
				"go.mod":       replaceOnce(base, "example.com/a v1.0.0", "example.com/a v1.2.0"),
				"tools/go.mod": replaceOnce(sub, "example.com/a v1.0.0", "example.com/a v1.3.0"),
			},
		},
		{
			name:  "unchanged file omitted",
			files: map[string]string{"go.mod": base, "tools/go.mod": sub},
			ups:   []domain.DepUpdate{goUp("tools", "example.com/a", "v1.3.0", "")},
			want:  map[string]string{"tools/go.mod": replaceOnce(sub, "example.com/a v1.0.0", "example.com/a v1.3.0")},
		},
		{
			name:  "path-major rename",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("", "example.com/lib", "v2.0.0", "example.com/lib/v2")},
			want:  map[string]string{"go.mod": replaceOnce(base, "example.com/lib v1.5.0", "example.com/lib/v2 v2.0.0")},
		},
		{
			name:  "path-major rename keeps indirect",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("", "example.com/b", "v2.0.0", "example.com/b/v2")},
			want:  map[string]string{"go.mod": replaceOnce(base, "example.com/b v1.1.0 // indirect", "example.com/b/v2 v2.0.0 // indirect")},
		},
		{
			name:  "missing require",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("", "example.com/unknown", "v1.0.0", "")},
			err:   domain.ErrPreconditionFailed,
		},
		{
			name:  "missing go.mod",
			files: map[string]string{"go.mod": base},
			ups:   []domain.DepUpdate{goUp("tools", "example.com/a", "v1.0.0", "")},
			err:   domain.ErrPreconditionFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := map[domain.TreePath][]byte{}
			snapshot := map[domain.TreePath]string{}
			for p, c := range tt.files {
				tp, err := domain.ParseTreePath(p)
				if err != nil {
					t.Fatal(err)
				}
				in[tp] = []byte(c)
				snapshot[tp] = c
			}
			got, err := GoMod{}.Bump(in, tt.ups)
			for p, c := range in {
				if string(c) != snapshot[p] {
					t.Fatalf("input %s mutated:\n%s", p, c)
				}
			}
			if len(in) != len(snapshot) {
				t.Fatalf("input map mutated: %v", in)
			}
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gotS := map[string]string{}
			for p, c := range got {
				gotS[p.String()] = string(c)
			}
			if !reflect.DeepEqual(gotS, tt.want) {
				t.Fatalf("got:\n%v\nwant:\n%v", gotS, tt.want)
			}
		})
	}
}

func replaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	panic("replaceOnce: " + old + " not found")
}
