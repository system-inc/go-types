package types_test

import (
	"fmt"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// TestVariationalInBuildReadsAsEachBuild checks each projection fixture once for every build, then reads the package
// under InBuild for each build: every package-level name of the build's plain check is found by Lookup with the type,
// the constant value, the underlying type and the method sets of the plain check's, spelled as the census spells
// them, no other name is found, and Names is the plain scope's Names.
func TestVariationalInBuildReadsAsEachBuild(test *testing.T) {
	test.Parallel()
	for name, fixtures := range projectionFixtures {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, fixtures)
			variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
			// A fixture may report errors, as the abandoned errors fixture does; the projection test compares them.
			checked, _ := (&types.Config{Variation: variation, Importer: fixtureImporter{}, Error: func(error) {}}).
				Check("fork", fileSet, parsed.files, &types.Info{})
			everyName := checked.Scope().Names()
			for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				plain, _ := (&types.Config{Importer: fixtureImporter{}, Error: func(error) {}}).Check("fork", fileSet,
					parsed.filesIn(build), &types.Info{})
				plainNames := plain.Scope().Names()
				want := map[string]string{}
				for _, name := range plainNames {
					want[name] = spellMember(plain.Scope().Lookup(name))
				}
				got := map[string]string{}
				var gotNames []string
				gotLength := 0
				types.InBuild(build, func() {
					gotNames = checked.Scope().Names()
					gotLength = checked.Scope().Len()
					for _, name := range everyName {
						if found := checked.Scope().Lookup(name); found != nil || slices.Contains(plainNames, name) {
							got[name] = spellMember(found)
						}
					}
				})
				label := fmt.Sprintf("build %03b", build)
				compareSpelled(test, label+" package scope", got, want)
				if !slices.Equal(gotNames, plainNames) || gotLength != len(plainNames) {
					test.Errorf("%s: Names %v and Len %d, plain Names %v", label, gotNames, gotLength, plainNames)
				}
			}
		})
	}
}

// TestVariationalInBuildAnswersTheBuildNotTheHost reads what differs by build in the hard and method order fixtures
// without InBuild, which answers the host's (darwin's), and under InBuild for windows, which answers windows's: N's
// value, and WaitStatus's methods, which windows declares two of in its own order.
func TestVariationalInBuildAnswersTheBuildNotTheHost(test *testing.T) {
	test.Parallel()
	for _, read := range []struct {
		fixtures []fixtureFile
		name     string
		spell    func(types.Object) string
		host     string
		windows  string
	}{
		{hardFiles, "N", func(object types.Object) string { return object.(*types.Const).Val().ExactString() }, "4", "2"},
		{methodOrderFiles, "WaitStatus", func(object types.Object) string {
			named := object.Type().(*types.Named)
			var methods []string
			for index := range named.NumMethods() {
				methods = append(methods, named.Method(index).Name())
			}
			return strings.Join(methods, " ")
		}, "Exited Signaled Stopped", "Stopped Signaled"},
	} {
		fileSet := token.NewFileSet()
		parsed := parseFixture(test, fileSet, read.fixtures)
		checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
			Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
		if checkError != nil {
			test.Fatal(checkError)
		}
		object := checked.Scope().Lookup(read.name)
		if host := read.spell(object); host != read.host {
			test.Errorf("%s read without InBuild: %q, the host's is %q", read.name, host, read.host)
		}
		var windows string
		types.InBuild(buildWindows, func() { windows = read.spell(object) })
		if windows != read.windows {
			test.Errorf("%s read in windows: %q, windows's is %q", read.name, windows, read.windows)
		}
	}
}

// spellMember spells a package-level object by its kind and type, a constant's value, and a type name's underlying
// type and the method sets of its named type and a pointer to it, each method by name, type, index and indirection.
func spellMember(object types.Object) string {
	if object == nil {
		return "absent"
	}
	spelled := fmt.Sprintf("%T %s", object, canonical.Spell(object.Type()))
	if constantObject, isConstant := object.(*types.Const); isConstant {
		spelled += " = " + constantObject.Val().ExactString()
	}
	if _, isTypeName := object.(*types.TypeName); isTypeName {
		spelled += ", underlying " + canonical.Spell(object.Type().Underlying())
		if named, isNamed := types.Unalias(object.Type()).(*types.Named); isNamed {
			spelled += ", methods " + spellMethodSet(named) + ", pointer methods " +
				spellMethodSet(types.NewPointer(named))
		}
	}
	return spelled
}

func spellMethodSet(typed types.Type) string {
	methodSet := types.NewMethodSet(typed)
	var methods []string
	for index := range methodSet.Len() {
		selection := methodSet.At(index)
		methods = append(methods, fmt.Sprintf("%s %s %v %t", selection.Obj().Name(),
			canonical.Spell(selection.Type()), selection.Index(), selection.Indirect()))
	}
	return "[" + strings.Join(methods, "; ") + "]"
}
