// SPDX-License-Identifier: AGPL-3.0-or-later

package deps

import (
	"errors"
	"os"
	"path"
	"strings"
	"testing"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func tp(t *testing.T, s string) domain.TreePath {
	t.Helper()
	p, e := domain.ParseTreePath(s)
	require.NoError(t, e)
	return p
}
func update(eco domain.DepsEcosystem, dir, name, current, version string) domain.DepUpdate {
	return domain.DepUpdate{DepRef: domain.DepRef{Ecosystem: eco, Directory: dir, Name: name, Version: version}, Current: current}
}

func TestManifests(t *testing.T) {
	paths := domain.PathList{Paths: []string{"go.mod", "sub/go.mod", "vendor/a/go.mod", "testdata/go.mod", "sub/testdata/go.mod", "vendored/go.mod", "vendored/vendor/modules.txt", "action.yml", "nested/action.yaml", ".github/workflows/a.yml", ".github/workflows/b.yaml", ".github/workflows/sub/c.yml", "other.yml"}}
	assert.Equal(t, []domain.TreePath{tp(t, "go.mod"), tp(t, "sub/go.mod")}, (GoMod{}).Manifests(paths))
	assert.Equal(t, []domain.TreePath{tp(t, "action.yml"), tp(t, "nested/action.yaml"), tp(t, ".github/workflows/a.yml"), tp(t, ".github/workflows/b.yaml")}, (Actions{}).Manifests(paths))
}

func TestGoModDetect(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().GoVersions(mock.Anything, "example.org/a", "v1.0.0").Return([]domain.DepVersion{{Version: "v1.1.0"}}, nil).Once()
	reg.EXPECT().GoVersions(mock.Anything, "example.org/b/v2", "v2.0.0").Return([]domain.DepVersion{{Version: "v2.1.0"}}, nil).Once()
	reg.EXPECT().GoVersions(mock.Anything, "private.org/m", "v1.0.0").Return(nil, domain.ErrNotFound).Once()
	files := map[domain.TreePath][]byte{
		tp(t, "go.mod"):     []byte("module example.org/root\ngo 1.27\nrequire (\nexample.org/a v1.0.0 // indirect\nexample.org/replaced v1.0.0\nprivate.org/m v1.0.0\n)\nreplace example.org/replaced => ../local\n"),
		tp(t, "sub/go.mod"): []byte("module example.org/sub\ngo 1.25.3\nrequire example.org/b/v2 v2.0.0\n"),
	}
	got, err := (GoMod{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "", got[0].Directory)
	assert.Equal(t, "sub", got[1].Directory)
	assert.Equal(t, "example.org/b/v2", got[1].Name)
	assert.Equal(t, "1.27", got[0].GoDirective)
	assert.Equal(t, "1.25.3", got[1].GoDirective, "each candidate carries the go directive of its own module")
	assert.False(t, (GoMod{}).ApplyInServer())
	_, err = (GoMod{}).Apply(files, nil)
	require.Error(t, err)
}

func TestActionsDetect(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().ActionTags(mock.Anything, "actions", "checkout").Return([]domain.DepTag{{Name: "v5", SHA: "peeled"}}, nil).Twice()
	reg.EXPECT().ActionTags(mock.Anything, "gone", "action").Return(nil, domain.ErrNotFound).Once()
	files := map[domain.TreePath][]byte{
		tp(t, ".github/workflows/ci.yml"): []byte("steps:\n - uses: 'actions/checkout@v4' # comment\n - uses: actions/checkout/sub@v4\n - uses: ./local\n - uses: docker://image:tag\n - uses: actions/checkout@" + strings.Repeat("a", 40) + "\n - uses: gone/action@v1\n"),
		tp(t, "action.yml"):               []byte("runs:\n using: composite\n steps:\n  - uses: \"actions/checkout@v3\"\n"),
	}
	got, err := (Actions{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, ".github/workflows/ci.yml", got[0].Directory)
	assert.Equal(t, "v4", got[0].Current)
	assert.Equal(t, []domain.DepVersion{{Version: "v5", SHA: "peeled"}}, got[0].Versions)
	assert.Equal(t, "v3", got[1].Current)
	assert.True(t, (Actions{}).ApplyInServer())
}

func TestRegistryErrors(t *testing.T) {
	boom := errors.New("registry unavailable")
	reg := mocks.NewDepsRegistry(t)
	reg.EXPECT().ActionTags(mock.Anything, "a", "b").Return(nil, boom)
	_, err := (Actions{}).Detect(t.Context(), map[domain.TreePath][]byte{tp(t, "action.yml"): []byte("uses: a/b@v1")}, reg)
	require.ErrorIs(t, err, boom)
	reg.EXPECT().GoVersions(mock.Anything, "example.org/a", "v1.0.0").Return(nil, boom)
	_, err = (GoMod{}).Detect(t.Context(), map[domain.TreePath][]byte{tp(t, "go.mod"): []byte("module example.org/root\nrequire example.org/a v1.0.0\n")}, reg)
	require.ErrorIs(t, err, boom)
}

func TestActionsApplyGoldenAndValidate(t *testing.T) {
	before, err := os.ReadFile("testdata/actions.before.yml")
	require.NoError(t, err)
	want, err := os.ReadFile("testdata/actions.after.yml")
	require.NoError(t, err)
	p := tp(t, ".github/workflows/ci.yml")
	files := map[domain.TreePath][]byte{p: before}
	ups := []domain.DepUpdate{update(domain.EcosystemActions, p.String(), "actions/checkout", "v4", "v5")}
	got, err := (Actions{}).Apply(files, ups)
	require.NoError(t, err)
	assert.Equal(t, want, got[p])
	assert.Equal(t, before, files[p])
	{
		_, validationErr := (Actions{}).Validate(files, got, ups)
		require.NoError(t, validationErr)
	}
	for _, bad := range [][]byte{before, []byte(strings.ReplaceAll(string(want), "keep me", "changed")), append(append([]byte(nil), want...), '\n')} {
		{
			_, validationErr := (Actions{}).Validate(files, map[domain.TreePath][]byte{p: bad}, ups)
			require.Error(t, validationErr)
		}
	}
	{
		_, validationErr := (Actions{}).Validate(files, map[domain.TreePath][]byte{tp(t, "extra.yml"): want}, ups)
		require.Error(t, validationErr)
	}
	{
		_, validationErr := (Actions{}).Validate(files, nil, ups)
		require.Error(t, validationErr)
	}
	ups[0].Version = "v5\nrun: evil"
	_, err = (Actions{}).Apply(files, ups)
	require.Error(t, err)
}

func TestGoModValidateGoDirectiveNormalisation(t *testing.T) {
	p := tp(t, "go.mod")
	tmpl := "module example.org/root\ngo GO\nrequire example.org/a v1.VER.0\n"
	mk := func(goV, ver string) []byte {
		return []byte(strings.NewReplacer("GO", goV, "VER", ver).Replace(tmpl))
	}
	ups := []domain.DepUpdate{update(domain.EcosystemGoMod, "", "example.org/a", "v1.0.0", "v1.1.0")}
	for _, tc := range []struct {
		name, from, to string
		ok             bool
	}{
		{"1.24 to 1.24.0 accepted (go get normalisation)", "1.24", "1.24.0", true},
		{"1.24.0 to 1.24 accepted (same normalisation, either direction)", "1.24.0", "1.24", true},
		{"1.24 to 1.24.1 refused", "1.24", "1.24.1", false},
		{"1.24 to 1.25.0 refused", "1.24", "1.25.0", false},
		{"1.24 to 1.25 refused", "1.24", "1.25", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (GoMod{}).Validate(map[domain.TreePath][]byte{p: mk(tc.from, "0")}, map[domain.TreePath][]byte{p: mk(tc.to, "1")}, ups)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, domain.ErrInvalid)
			}
		})
	}
}

func TestGoModValidate(t *testing.T) {
	p := tp(t, "sub/go.mod")
	old := "module example.org/root\ngo 1.27\ntoolchain go1.27.0\nrequire (\nexample.org/a v1.0.0\nexample.org/b v1.0.0\nexample.org/indirect v1.0.0 // indirect\n)\n"
	good := strings.Replace(old, "example.org/a v1.0.0", "example.org/a v1.1.0", 1)
	ups := []domain.DepUpdate{update(domain.EcosystemGoMod, "sub", "example.org/a", "v1.0.0", "v1.1.0")}
	before := map[domain.TreePath][]byte{p: []byte(old)}
	for _, tc := range []struct {
		name, content string
		ok            bool
	}{
		{"requested", good, true},
		{"indirect changed", strings.Replace(good, "example.org/indirect v1.0.0", "example.org/indirect v1.9.0", 1), true},
		{"indirect removed", strings.Replace(good, "example.org/indirect v1.0.0 // indirect\n", "", 1), true},
		{"indirect added", good + "require example.org/new v1.0.0 // indirect\n", true},
		{"not applied", old, false},
		{"higher requested version", strings.Replace(good, "v1.1.0", "v1.2.0", 1), true},
		{"unrequested direct raised", strings.Replace(good, "example.org/b v1.0.0", "example.org/b v1.1.0", 1), true},
		{"unrequested direct downgrade", strings.Replace(good, "example.org/b v1.0.0", "example.org/b v0.9.0", 1), false},
		{"requested direct removed", strings.Replace(good, "example.org/a v1.1.0\n", "", 1), false},
		{"direct removed", strings.Replace(good, "example.org/b v1.0.0\n", "", 1), false},
		{"direct added", good + "require example.org/new v1.0.0\n", false},
		{"direct becomes indirect", strings.Replace(good, "example.org/b v1.0.0", "example.org/b v1.0.0 // indirect", 1), false},
		{"replace", good + "replace example.org/a => ../evil\n", false},
		{"exclude", good + "exclude example.org/b v1.2.0\n", false},
		{"retract", good + "retract v1.0.0\n", false},
		{"godebug", good + "godebug default=go1.20\n", false},
		{"tool", good + "tool example.org/a/cmd\n", false},
		{"ignore", good + "ignore ./vendorish\n", false},
		{"go", strings.Replace(good, "go 1.27", "go 1.26", 1), false},
		{"toolchain", strings.Replace(good, "go1.27.0", "go1.28.0", 1), false},
		{"module", strings.Replace(good, "module example.org/root", "module example.org/evil", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(tc.content)}, ups)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	sum := "example.org/a v1.1.0/go.mod h1:" + strings.Repeat("A", 43) + "=\n"
	{
		_, validationErr := (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(good), tp(t, "sub/go.sum"): []byte(sum)}, ups)
		require.NoError(t, validationErr)
	}
	for _, bad := range []string{"bad", "example.org/a v1.0.0 h1:bad", "example.org/a invalid h1:" + strings.Repeat("A", 43) + "="} {
		{
			_, validationErr := (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(good), tp(t, "sub/go.sum"): []byte(bad)}, ups)
			require.Error(t, validationErr)
		}
	}
	{
		_, validationErr := (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(good), tp(t, "extra.txt"): nil}, ups)
		require.Error(t, validationErr)
	}
	{
		_, validationErr := (GoMod{}).Validate(before, map[domain.TreePath][]byte{tp(t, "go.mod"): []byte(good)}, ups)
		require.Error(t, validationErr)
	}
}

func TestGoModGroupedReplacements(t *testing.T) {
	reg := mocks.NewDepsRegistry(t)
	files := map[domain.TreePath][]byte{tp(t, "go.mod"): []byte("module example.org/root\nrequire (\nexample.org/a v1.0.0\nexample.org/b v1.0.0\n)\nreplace (\nexample.org/a v1.0.0 => ../a\n\"example.org/b\" => ../b\n)\n")}
	got, err := (GoMod{}).Detect(t.Context(), files, reg)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestActionsApplyPreservesCRLFAndNoFinalNewline(t *testing.T) {
	p := tp(t, "action.yaml")
	before := map[domain.TreePath][]byte{p: []byte("steps:\r\n - uses: owner/repo@v1 # comment\r\n - run: true")}
	ups := []domain.DepUpdate{update(domain.EcosystemActions, p.String(), "owner/repo", "v1", "v2")}
	got, err := (Actions{}).Apply(before, ups)
	require.NoError(t, err)
	assert.Equal(t, "steps:\r\n - uses: owner/repo@v2 # comment\r\n - run: true", string(got[p]))
	{
		_, validationErr := (Actions{}).Validate(before, got, ups)
		require.NoError(t, validationErr)
	}
}

func TestGoModRequestedIndirectTidyAndSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, after string
		ok          bool
		to          string
	}{
		{"tidy removed", "", true, ""},
		{"higher", "require example.org/a v1.3.0 // indirect\n", true, "v1.3.0"},
		{"exact", "require example.org/a v1.2.0 // indirect\n", true, ""},
		{"below", "require example.org/a v1.1.0 // indirect\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tp(t, "sub/go.mod")
			header := "module example.org/root\ngo 1.24\n"
			effects, err := (GoMod{}).Validate(map[domain.TreePath][]byte{p: []byte(header + "require example.org/a v1.0.0 // indirect\n")}, map[domain.TreePath][]byte{p: []byte(header + tc.after)}, []domain.DepUpdate{update(domain.EcosystemGoMod, "sub", "example.org/a", "v1.0.0", "v1.2.0")})
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.name == "exact" {
				require.Empty(t, effects)
				return
			}
			from := "v1.2.0"
			if tc.after == "" {
				from = "v1.0.0"
			}
			require.Equal(t, []domain.DepSideEffect{{Ecosystem: domain.EcosystemGoMod, Directory: "sub", Name: "example.org/a", From: from, To: tc.to}}, effects)
		})
	}
}

func TestGoModSideEffectsAreSorted(t *testing.T) {
	before, after := map[domain.TreePath][]byte{}, map[domain.TreePath][]byte{}
	var ups []domain.DepUpdate
	var want []domain.DepSideEffect
	for _, dir := range []string{"", "sub"} {
		p := tp(t, path.Join(dir, "go.mod"))
		before[p] = []byte("module example.org/root\ngo 1.24\nrequire (\nexample.org/z v1.0.0 // indirect\nexample.org/b v1.0.0\nexample.org/a v1.0.0\n)\n")
		after[p] = []byte("module example.org/root\ngo 1.24\nrequire (\nexample.org/b v1.2.0\nexample.org/a v1.3.0\n)\n")
		ups = append(ups, update(domain.EcosystemGoMod, dir, "example.org/z", "v1.0.0", "v1.1.0"))
		want = append(want, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: dir, Name: "example.org/a", From: "v1.0.0", To: "v1.3.0"}, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: dir, Name: "example.org/b", From: "v1.0.0", To: "v1.2.0"}, domain.DepSideEffect{Ecosystem: domain.EcosystemGoMod, Directory: dir, Name: "example.org/z", From: "v1.0.0"})
	}
	effects, err := (GoMod{}).Validate(before, after, ups)
	require.NoError(t, err)
	require.Equal(t, want, effects)
}

func TestGoModValidateRefusesDuplicateRequirements(t *testing.T) {
	p := tp(t, "go.mod")
	header := "module example.org/root\ngo 1.24\n"
	clean := header + "require (\nexample.org/a v1.0.0\nexample.org/d v1.0.0\n)\n"
	ups := []domain.DepUpdate{update(domain.EcosystemGoMod, "", "example.org/a", "v1.0.0", "v1.1.0")}
	for _, tc := range []struct{ name, before, after string }{
		{"incompatible twin after", clean, header + "require (\nexample.org/a v1.1.0\nexample.org/d v7.0.0+incompatible\nexample.org/d v1.0.0\n)\n"},
		{"new module direct and indirect", clean, header + "require (\nexample.org/a v1.1.0\nexample.org/d v1.0.0\nexample.org/n v1.0.0\nexample.org/n v1.0.0 // indirect\n)\n"},
		{"requested twice", clean, header + "require (\nexample.org/a v1.1.0\nexample.org/a v1.0.0\nexample.org/d v1.0.0\n)\n"},
		{"duplicate before", header + "require (\nexample.org/a v1.0.0\nexample.org/d v1.0.0\nexample.org/d v1.0.0 // indirect\n)\n", header + "require (\nexample.org/a v1.1.0\nexample.org/d v1.0.0\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (GoMod{}).Validate(map[domain.TreePath][]byte{p: []byte(tc.before)}, map[domain.TreePath][]byte{p: []byte(tc.after)}, ups)
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.Contains(t, err.Error(), "duplicate requirement")
		})
	}
}

func TestGoModValidateRequestedModuleDirectness(t *testing.T) {
	p := tp(t, "go.mod")
	header := "module example.org/root\ngo 1.24\n"
	ups := []domain.DepUpdate{update(domain.EcosystemGoMod, "", "example.org/a", "v1.0.0", "v1.1.0")}
	for _, tc := range []struct{ name, before, after, want string }{
		{"direct demoted", "require example.org/a v1.0.0\n", "require example.org/a v1.1.0 // indirect\n", "became indirect"},
		{"indirect promoted", "require example.org/a v1.0.0 // indirect\n", "require example.org/a v1.1.0\n", "new direct requirement"},
		{"direct stays direct", "require example.org/a v1.0.0\n", "require example.org/a v1.1.0\n", ""},
		{"indirect stays indirect", "require example.org/a v1.0.0 // indirect\n", "require example.org/a v1.1.0 // indirect\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (GoMod{}).Validate(map[domain.TreePath][]byte{p: []byte(header + tc.before)}, map[domain.TreePath][]byte{p: []byte(header + tc.after)}, ups)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, domain.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestGoModValidateRefusesSameVersionRespelled(t *testing.T) {
	p := tp(t, "go.mod")
	header := "module example.org/root\ngo 1.24\n"
	ups := []domain.DepUpdate{update(domain.EcosystemGoMod, "", "example.org/a", "v1.0.0", "v1.1.0")}
	before := map[domain.TreePath][]byte{p: []byte(header + "require (\nexample.org/a v1.0.0\nexample.org/b v1.0.0\n)\n")}
	_, err := (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(header + "require (\nexample.org/a v1.1.0\nexample.org/b v1.0.0+incompatible\n)\n")}, ups)
	require.ErrorIs(t, err, domain.ErrInvalid)
	assert.Contains(t, err.Error(), `"example.org/b"`)
	_, err = (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(header + "require (\nexample.org/a v1.1.0+incompatible\nexample.org/b v1.0.0\n)\n")}, ups)
	require.ErrorIs(t, err, domain.ErrInvalid)
	_, err = (GoMod{}).Validate(before, map[domain.TreePath][]byte{p: []byte(header + "require (\nexample.org/a v1.1.0\nexample.org/b v1.0.0\n)\n")}, ups)
	require.NoError(t, err)
}
