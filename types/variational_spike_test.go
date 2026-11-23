package types_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// The builds of the fixture: bit 0 is darwin, bit 1 linux, bit 2 windows.
const (
	buildDarwin  types.Mask = 1 << 0
	buildLinux   types.Mask = 1 << 1
	buildWindows types.Mask = 1 << 2
	everyBuild              = buildDarwin | buildLinux | buildWindows
)

// fixtureFiles is the census's fork fixture with the Square's contagion case beside it: a struct whose unexported
// field differs by build, reached through a pointer by a body that only passes the pointer.
type fixtureFile struct {
	name   string
	builds types.Mask
	source string
}

var fixtureFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

import "unsafe"

func F() int { return N * 3 }

func Untouched() int { return 7 }

func WordSize() uintptr { return unsafe.Sizeof(uintptr(0)) }

const IFMT = 0xf000

func IsPipe(stat Stat) bool { return stat.Mode&IFMT != 0 }

func Signal() error { return Kill(1, 9) }

type File struct{ *file }

func Pass(f *File) *File { return f }

func Size() uintptr { return unsafe.Sizeof(File{}) }

func InnerSize() uintptr { return unsafe.Sizeof(file{}) }

func Same(left File, right File) bool { return left == right }

const separator = string(Separator)

func Join(left string, right string) string { return left + separator + right }
`},
	{"n_linux.go", buildLinux, `package fork

const N = 1

var Kill func(pid int, signum int) error

type Stat struct{ Mode uint32 }

type file struct{ fd int }

const Separator = '/'
`},
	{"n_windows.go", buildWindows, `package fork

const N = 2

var Kill func(pid int, sig int) error

type Stat struct{ Mode uint16 }

type file struct {
	handle uintptr
	name   string
}

const Separator = '\\'
`},
	{"n_other.go", buildDarwin, `package fork

const N = 3

var Kill func(pid int, sig int) error

type Stat struct{ Mode uint32 }

type file struct{ fd int }

const Separator = '/'
`},
}

// unsafeImporter imports unsafe, the fixture's one import.
type unsafeImporter struct{}

func (unsafeImporter) Import(path string) (*types.Package, error) { return types.Unsafe, nil }

// answer spells a recorded type and value as the census compares them.
func answer(tv types.TypeAndValue) string { return canonical.Answer(tv) }

// TestVariationalCheckProjectsToEachBuild checks the fixture once for every build, then checks each build alone the
// plain way, and requires every expression's projected answer to be the plain one, in every build.
func TestVariationalCheckProjectsToEachBuild(t *testing.T) {
	t.Parallel()
	forked := projectAndCompare(t, fixtureFiles)
	for _, mustNotFork := range []string{"Untouched", "Signal", "Pass", "Size", "WordSize"} {
		if forked[mustNotFork] {
			t.Errorf("%s forked, though nothing it reads differs by build", mustNotFork)
		}
	}
	for _, mustFork := range []string{"F", "IsPipe", "InnerSize", "Join"} {
		if !forked[mustFork] {
			t.Errorf("%s didn't fork, though it reads what differs by build", mustFork)
		}
	}
}

// projectAndCompare checks fixtures once for every build, compares the projection to each build's plain check, and
// returns the functions with an expression answered differently by build. Every check must be clean.
func projectAndCompare(t *testing.T, fixtures []fixtureFile) map[string]bool {
	t.Helper()
	forked, _ := projectAndCompareWithDiagnostics(t, fixtures, false)
	return forked
}

// projectAndCompareWithDiagnostics is projectAndCompare, and when diagnosing it lets the checks report errors: the
// variational check's diagnostics, each spelled as Error spells it, must be every build's plain diagnostics together,
// and are returned in the order it published them.
func projectAndCompareWithDiagnostics(t *testing.T, fixtures []fixtureFile, diagnosing bool) (map[string]bool,
	[]string,
) {
	t.Helper()
	fileSet := token.NewFileSet()
	var files []*ast.File
	masks := map[*ast.File]types.Mask{}
	for _, fixture := range fixtures {
		parsed, parseError := parser.ParseFile(fileSet, fixture.name, fixture.source, parser.ParseComments)
		if parseError != nil {
			t.Fatal(parseError)
		}
		files = append(files, parsed)
		masks[parsed] = fixture.builds
	}
	variation := &types.Variation{Builds: everyBuild, Files: masks}
	configuration := &types.Config{Variation: variation, Importer: unsafeImporter{}}
	var published []string
	if diagnosing {
		configuration.Error = func(diagnostic error) { published = append(published, diagnostic.Error()) }
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, checkError := configuration.Check("fork", fileSet, files, info); checkError != nil && !diagnosing {
		t.Fatalf("variational check: %v", checkError)
	}
	plainDiagnostics := map[string]bool{}
	if len(info.Types) == 0 {
		t.Fatal("the variational check recorded no answers")
	}
	builds := []types.Mask{buildDarwin, buildLinux, buildWindows}
	// Each build's answers are the host's, in Info.Types, with the build's overlay laid over them.
	projections := make([]map[ast.Expr]types.TypeAndValue, len(builds))
	answered := map[ast.Expr]map[string]bool{}
	for index := range builds {
		overlay := types.Overlay[ast.Expr, types.TypeAndValue]{}
		if index > 0 {
			overlay = variation.Overlays[index].Types
		}
		projections[index] = project(info.Types, overlay)
		for expression, tv := range projections[index] {
			if answered[expression] == nil {
				answered[expression] = map[string]bool{}
			}
			answered[expression][answer(tv)] = true
		}
	}
	forked := map[string]bool{}
	for expression, distinct := range answered {
		if len(distinct) > 1 {
			forked[enclosingFunction(files, fileSet, expression)] = true
		}
	}
	for index, build := range builds {
		var built []*ast.File
		for _, file := range files {
			if masks[file]&build != 0 {
				built = append(built, file)
			}
		}
		plain := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		plainConfiguration := &types.Config{Importer: unsafeImporter{}}
		if diagnosing {
			plainConfiguration.Error = func(diagnostic error) { plainDiagnostics[diagnostic.Error()] = true }
		}
		if _, checkError := plainConfiguration.Check("fork", fileSet, built, plain); checkError != nil && !diagnosing {
			t.Fatalf("plain check of build %03b: %v", build, checkError)
		}
		compareTypes(t, fileSet, fmt.Sprintf("build %03b", build), projections[index], plain.Types)
	}
	compareDiagnostics(t, published, plainDiagnostics)
	var forkedNames []string
	for name := range forked {
		forkedNames = append(forkedNames, name)
	}
	sort.Strings(forkedNames)
	t.Logf("functions that forked: %s; units %d, restarts %d", strings.Join(forkedNames, ", "), variation.Units,
		variation.Restarts)
	return forked, published
}

// compareDiagnostics requires the diagnostics a variational check published to be, once each told apart, the plain
// checks' diagnostics together. A diagnostic reported in more than one build may be published once for each run that
// reports it, so their counts aren't compared here.
func compareDiagnostics(t *testing.T, published []string, plain map[string]bool) {
	t.Helper()
	publishedOnce := map[string]bool{}
	for _, diagnostic := range published {
		publishedOnce[diagnostic] = true
		if !plain[diagnostic] {
			t.Errorf("the variational check reported %q, and no build's plain check does", diagnostic)
		}
	}
	for diagnostic := range plain {
		if !publishedOnce[diagnostic] {
			t.Errorf("a plain check reported %q, and the variational check doesn't", diagnostic)
		}
	}
}

// compareTypes requires projected, one build's projection of a variational check's Types, to be plain, that build's
// plain check's, expression for expression. Keyed by position and text, never by node: a checker makes some nodes
// afresh each time it checks them (the 1 of an IncDec statement).
func compareTypes(t *testing.T, fileSet *token.FileSet, label string, projected map[ast.Expr]types.TypeAndValue,
	plain map[ast.Expr]types.TypeAndValue,
) {
	t.Helper()
	spelled := map[string]string{}
	for expression, tv := range projected {
		key := positionKey(fileSet, expression)
		if earlier, held := spelled[key]; held {
			// Two nodes at one place answer for this build: a run abandoned for a split left its own node (one the
			// checker makes afresh, as an IncDec's 1) behind.
			t.Errorf("%s, %s: two answers at one place, %q and %q", label, key, earlier, answer(tv))
		}
		spelled[key] = answer(tv)
	}
	matched := 0
	for expression, want := range plain {
		key := positionKey(fileSet, expression)
		got, held := spelled[key]
		delete(spelled, key)
		if !held || got != answer(want) {
			t.Errorf("%s, %s: projected %q, plain %q", label, key, got, answer(want))
			continue
		}
		matched++
	}
	if matched == 0 {
		t.Errorf("%s: no expression compared", label)
	}
	for key, got := range spelled {
		t.Errorf("%s, %s: projected %q, and the plain check has no answer", label, key, got)
	}
}

// positionKey names expression by where it is and what it says.
func positionKey(fileSet *token.FileSet, expression ast.Expr) string {
	return fileSet.Position(expression.Pos()).String() + "-" + fileSet.Position(expression.End()).String() + " " +
		types.ExprString(expression)
}

// enclosingFunction is the name of the function declaration holding expression, or the empty string.
func enclosingFunction(files []*ast.File, fileSet *token.FileSet, expression ast.Expr) string {
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if isFunction && function.Pos() <= expression.Pos() && expression.End() <= function.End() {
				return function.Name.Name
			}
		}
	}
	return ""
}
