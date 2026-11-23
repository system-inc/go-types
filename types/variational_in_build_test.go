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
			for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				compareReads(test, fixtures, build, func(work func()) { types.InBuild(build, work) })
			}
		})
	}
}

// TestVariationalReadsWithNoStateAsTheHost is TestVariationalInBuildReadsAsEachBuild for a read from a goroutine
// running no check and no InBuild, as a host lint reads: it answers as the host's (darwin's) plain check.
func TestVariationalReadsWithNoStateAsTheHost(test *testing.T) {
	test.Parallel()
	for name, fixtures := range projectionFixtures {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			compareReads(test, fixtures, buildDarwin, func(work func()) { work() })
		})
	}
}

// compareReads checks fixtures once for every build and requires the package's reads inside read to be build's plain
// check's: every package-level name of the plain check is found by Lookup with the type, the constant value, the
// underlying type and the method sets of the plain check's, spelled as the census spells them, no other name is found,
// and Names and Len are the plain scope's.
func compareReads(test *testing.T, fixtures []fixtureFile, build types.Mask, read func(work func())) {
	test.Helper()
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, fixtures)
	variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	// A fixture may report errors, as the abandoned errors fixture does; the projection test compares them.
	checked, _ := (&types.Config{Variation: variation, Importer: fixtureImporter{}, Error: func(error) {}}).
		Check("fork", fileSet, parsed.files, &types.Info{})
	var everyName []string
	types.InBuild(buildDarwin, func() { everyName = append(everyName, checked.Scope().Names()...) })
	types.InBuild(buildLinux, func() { everyName = append(everyName, checked.Scope().Names()...) })
	types.InBuild(buildWindows, func() { everyName = append(everyName, checked.Scope().Names()...) })
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
	read(func() {
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

// TestVariationalInBuildAnswersTheBuildNotTheHost reads what differs by build in the spike, hard and method order
// fixtures without InBuild, which answers the host's (darwin's), and under InBuild for windows, which answers
// windows's: N's value, and WaitStatus's methods, which windows declares two of in its own order. The spike's N is
// declared first in linux's file, so its first answer is linux's and not the host's.
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
		{fixtureFiles, "N", func(object types.Object) string { return object.(*types.Const).Val().ExactString() }, "3",
			"2"},
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

// TestVariationalOutsideReadsEveryBuild checks a package whose OnlyWindows darwin, the host, doesn't declare, then reads
// it with no state, as the host, and inside Outside, as the merging importer reads its own universe: there every
// build's objects are found and no name is left out, so no lazy entry is resolved only to learn whether the host has
// it. A scope of another universe holds a lazy entry that counts its resolutions.
func TestVariationalOutsideReadsEveryBuild(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, []fixtureFile{
		{"shared.go", everyBuild, "package fork\n\nvar Shared = 1\n"},
		{"only_windows.go", buildWindows, "package fork\n\nvar OnlyWindows = 2\n"},
	})
	checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
		Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
	if checkError != nil {
		test.Fatal(checkError)
	}
	if found := checked.Scope().Lookup("OnlyWindows"); found != nil {
		test.Errorf("OnlyWindows read with no state is %v, and the host doesn't declare it", found)
	}
	resolutions := 0
	other := types.NewScope(nil, token.NoPos, token.NoPos, "another universe's package")
	other.InsertLazy("Lazy", func() types.Object {
		resolutions++
		return types.NewVar(token.NoPos, nil, "Lazy", types.Typ[types.Int])
	})
	types.Outside(func() {
		if found := checked.Scope().Lookup("OnlyWindows"); found == nil {
			test.Error("OnlyWindows read inside Outside is absent, and windows declares it")
		}
		if names := checked.Scope().Names(); !slices.Contains(names, "OnlyWindows") {
			test.Errorf("Names inside Outside are %v, without windows's OnlyWindows", names)
		}
		if names := other.Names(); !slices.Equal(names, []string{"Lazy"}) || resolutions != 0 {
			test.Errorf("Names inside Outside are %v, and resolved the lazy entry %d times", names, resolutions)
		}
	})
}

// TestVariationalCarryTakesInBuildToAnotherGoroutine checks the hard fixture, then, under InBuild for windows, reads
// N on a goroutine of its own twice: started with Carry, it reads windows's N, and started plainly, with no state, it
// reads the host's, darwin's, as every goroutine InBuild didn't start on does. Outside InBuild, Carry's goroutine reads
// the host's too.
func TestVariationalCarryTakesInBuildToAnotherGoroutine(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, hardFiles)
	checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
		Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
	if checkError != nil {
		test.Fatal(checkError)
	}
	n := checked.Scope().Lookup("N").(*types.Const)
	readOnAnotherGoroutine := func(start func(read func())) string {
		var value string
		done := make(chan struct{})
		start(func() {
			defer close(done)
			value = n.Val().ExactString()
		})
		<-done
		return value
	}
	var carried, plain string
	types.InBuild(buildWindows, func() {
		carried = readOnAnotherGoroutine(func(read func()) { go types.Carry(read)() })
		plain = readOnAnotherGoroutine(func(read func()) { go read() })
	})
	if carried != "2" {
		test.Errorf("N read on a goroutine started with Carry under InBuild for windows: %q, windows's is \"2\"", carried)
	}
	if plain != "4" {
		test.Errorf("N read on a goroutine started plainly under InBuild for windows: %q, the host's is \"4\"", plain)
	}
	if outside := readOnAnotherGoroutine(func(read func()) { go types.Carry(read)() }); outside != "4" {
		test.Errorf("N read on a goroutine started with Carry outside InBuild: %q, the host's is \"4\"", outside)
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
