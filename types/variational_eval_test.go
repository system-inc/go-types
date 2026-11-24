package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"testing"

	"github.com/system-inc/go-types/types"
)

// evalFiles declare a function with a local in a file windows alone compiles, beside a file every build compiles.
var evalFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Shared() int { return 1 }
`},
	{"only_windows.go", buildWindows, `package fork

func OnlyWindows() int {
	local := 2
	return local
}
`},
}

// TestVariationalEvalAnswersAsEachBuildsPlainCheck checks evalFiles once for every build and evaluates an expression
// at a position in the file windows alone compiles, read under InBuild for each build and with no state as the host:
// Eval answers what it answers of the build's plain check. A build that doesn't compile the file has no scope there,
// so the position is none of its package's, whatever the expression names, and windows finds the function's scope.
func TestVariationalEvalAnswersAsEachBuildsPlainCheck(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, evalFiles)
	variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	checked, checkError := (&types.Config{Variation: variation, Importer: fixtureImporter{}}).Check("fork", fileSet,
		parsed.files, fullInfo())
	if checkError != nil {
		test.Fatal(checkError)
	}
	// The position of the return in OnlyWindows, after its local is declared.
	var position token.Pos
	for _, file := range parsed.files {
		ast.Inspect(file, func(node ast.Node) bool {
			if returned, isReturn := node.(*ast.ReturnStmt); isReturn && parsed.masks[file] == buildWindows {
				position = returned.Pos()
			}
			return true
		})
	}
	if !position.IsValid() {
		test.Fatal("the fixture has no return in the file windows alone compiles")
	}
	spell := func(checked *types.Package, expression string) string {
		answer, evalError := types.Eval(fileSet, checked, position, expression)
		if evalError != nil {
			return "error: " + evalError.Error()
		}
		return answer.Type.String()
	}
	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		plain, plainError := (&types.Config{Importer: fixtureImporter{build}}).Check("fork", fileSet, parsed.filesIn(build),
			fullInfo())
		if plainError != nil {
			test.Fatal(plainError)
		}
		views := map[string]func(work func()){
			fmt.Sprintf("under InBuild %03b", build): func(work func()) { types.InBuild(build, work) },
		}
		if build == buildDarwin {
			views["with no state, as the host"] = func(work func()) { work() }
		}
		for _, view := range slices.Sorted(maps.Keys(views)) {
			for _, expression := range []string{"local", "Shared"} {
				var got string
				views[view](func() { got = spell(checked, expression) })
				want := spell(plain, expression)
				if got != want {
					test.Errorf("%s: Eval of %s at %s answers %q, and the build's plain check answers %q", view, expression,
						fileSet.Position(position), got, want)
				}
				// The plain check's answer is held to what the fixture means, since Eval of both goes one way.
				meant := "error: no position only_windows.go:5:2 found in package fork"
				if build == buildWindows {
					meant = map[string]string{"local": "int", "Shared": "func() int"}[expression]
				}
				if want != meant {
					test.Errorf("%s: Eval of %s in the build's plain check answers %q, and the fixture means %q", view,
						expression, want, meant)
				}
			}
		}
	}
}
