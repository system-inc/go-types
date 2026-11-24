package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/system-inc/go-types/types"
)

// alikeFiles declare a struct type and an interface type in each build's own file, alike in every build, as syscall
// declares Linger in syscall_windows.go and in each ztypes file. A shared file selects the field and calls the
// method.
var alikeFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Use(t T) int { return t.A }

func Call(i I) int { return i.M() }
`},
	{"alike_unix.go", buildDarwin | buildLinux, `package fork

type T struct{ A int }

type I interface{ M() int }
`},
	{"alike_windows.go", buildWindows, `package fork

// Windows's declarations are alike and sit elsewhere in their file.
type T struct{ A int }

type I interface{ M() int }
`},
}

// TestVariationalATypeDeclaredAlikeReadsItsFieldsAtEachBuildsFile checks alikeFiles once for every build, then reads
// the struct's field and the interface's method under InBuild for each build and with no state, as the host: each is
// at the build's own declaration, in the build's own file, and is the very object the build's Defs declare there and
// the shared file's selection of it names.
func TestVariationalATypeDeclaredAlikeReadsItsFieldsAtEachBuildsFile(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, alikeFiles)
	variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	info := fullInfo()
	checked, checkError := (&types.Config{Variation: variation, Importer: fixtureImporter{}}).Check("fork", fileSet,
		parsed.files, info)
	if checkError != nil {
		test.Fatal(checkError)
	}
	if variation.Restarts != 0 {
		test.Errorf("the check restarted %d units of %d, and nothing differs by build", variation.Restarts,
			variation.Units)
	}
	members := map[string]func(checked *types.Package) types.Object{
		"T's field A": func(checked *types.Package) types.Object {
			return checked.Scope().Lookup("T").Type().Underlying().(*types.Struct).Field(0)
		},
		"I's method M": func(checked *types.Package) types.Object {
			return checked.Scope().Lookup("I").Type().Underlying().(*types.Interface).ExplicitMethod(0)
		},
	}
	for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		plain, plainError := (&types.Config{Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.filesIn(build),
			&types.Info{})
		if plainError != nil {
			test.Fatal(plainError)
		}
		overlays := types.BuildOverlays{}
		if index > 0 {
			overlays = variation.Overlays[index]
		}
		defs := project(info.Defs, overlays.Defs)
		selections := project(info.Selections, overlays.Selections)
		views := map[string]func(work func()){
			fmt.Sprintf("under InBuild %03b", build): func(work func()) { types.InBuild(build, work) },
		}
		if build == buildDarwin {
			views["with no state, as the host"] = func(work func()) { work() }
		}
		for view, read := range views {
			for name, member := range members {
				want := fileSet.Position(member(plain).Pos())
				read(func() {
					object := member(checked)
					got := fileSet.Position(object.Pos())
					if got != want {
						test.Errorf("%s %s is at %s:%d, and the build declares it at %s:%d", name, view,
							filepath.Base(got.Filename), got.Line, filepath.Base(want.Filename), want.Line)
					}
					declared, selected := false, false
					for ident, defined := range defs {
						if fileSet.Position(ident.Pos()) == want {
							declared = defined == object
						}
					}
					for _, selection := range selections {
						if selection.Obj().Name() == object.Name() {
							selected = selection.Obj() == object
						}
					}
					if !declared || !selected {
						test.Errorf("%s %s: the build's Defs declare it at its place %t, and the shared file's selection names it %t",
							name, view, declared, selected)
					}
				})
			}
		}
	}
	// The struct type expression of each build's file has the type the build reads.
	for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		typesOf := info.Types
		if index > 0 {
			typesOf = project(info.Types, variation.Overlays[index].Types)
		}
		compiled := map[string]bool{}
		for _, file := range parsed.filesIn(build) {
			compiled[fileSet.Position(file.Pos()).Filename] = true
		}
		types.InBuild(build, func() {
			underlying := checked.Scope().Lookup("T").Type().Underlying()
			for expression, answer := range typesOf {
				_, isStruct := expression.(*ast.StructType)
				if isStruct && compiled[fileSet.Position(expression.Pos()).Filename] && answer.Type != underlying {
					test.Errorf("build %03b: the struct type at %s isn't T's underlying type in the build", build,
						fileSet.Position(expression.Pos()))
				}
			}
		})
	}
}

// TestVariationalSetPositionGivesAnObjectItsPlaceByBuild makes a struct's field as the merging importer does, one
// field for every build, and gives it windows's place: read under InBuild for windows it is there, and under InBuild
// for darwin or linux, or with no state, at its own. Under Agreeing its position leaves the builds that share it.
func TestVariationalSetPositionGivesAnObjectItsPlaceByBuild(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	own := fileSet.AddFile("ztypes_darwin.go", -1, 100).Pos(10)
	windows := fileSet.AddFile("syscall_windows.go", -1, 100).Pos(20)
	merged := types.NewPackage("example.com/merged", "merged")
	field := types.NewField(own, merged, "Onoff", types.Typ[types.Int32], false)
	types.SetPosition(field, buildWindows, windows)
	unix := buildDarwin | buildLinux
	for _, each := range []struct {
		build    types.Mask
		want     token.Pos
		agreeing types.Mask
	}{{buildDarwin, own, unix}, {buildLinux, own, unix}, {buildWindows, windows, buildWindows}} {
		var got token.Pos
		agreeing := types.Agreeing(each.build, everyBuild, func() { got = field.Pos() })
		if got != each.want || agreeing != each.agreeing {
			test.Errorf("the field read as %03b is at %s, agreeing %03b; want %s and %03b", each.build,
				fileSet.Position(got), agreeing, fileSet.Position(each.want), each.agreeing)
		}
	}
	if got := field.Pos(); got != own {
		test.Errorf("the field read with no state is at %s, want its own place, %s", fileSet.Position(got),
			fileSet.Position(own))
	}
}
