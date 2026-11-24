package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/system-inc/go-types/types"
)

// declaredFiles declare a function and a method in each build's own file with types identical in every build and
// each file's own names for the receiver, the parameter and the result, as os declares Remove in file_unix.go and in
// file_windows.go. A shared file calls each, and nothing it reads differs by build. Reset's receiver is a value on
// darwin and linux and a pointer on windows, which the signatures' types don't tell apart. Only is darwin's and
// linux's alone, each its own, so windows declares it nowhere.
var declaredFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type W struct{}

func Caller() error { return Remove("x") }

func MethodCaller(w W) int { return w.Size("x") }
`},
	{"declared_unix.go", buildDarwin | buildLinux, `package fork

func Remove(name string) (failure error) {
	_ = name
	return failure
}

func (w W) Size(unix string) (size int) { return len(unix) }

func (w W) Reset() {}
`},
	{"declared_windows.go", buildWindows, `package fork

func Remove(path string) (problem error) {
	_ = path
	return problem
}

func (receiver W) Size(windows string) (length int) { return len(windows) }

func (receiver *W) Reset() {}
`},
	{"only_darwin.go", buildDarwin, "package fork\n\nfunc Only(first int) int { return first }\n"},
	{"only_linux.go", buildLinux, "package fork\n\nfunc Only(second int) int { return second }\n"},
}

// declaredGenericFiles declare a generic type's method in each build's own file, alike but for each file's own names,
// and a shared file calls it through an instance. Each declaration binds its own receiver type parameter, so Get's
// two signatures, which name it, are never one type, and Get answers by build as any method that differs does. Len's
// name no type parameter and are one type.
var declaredGenericFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type Box[T any] struct{ value T }

func BoxCaller(box Box[int]) int { return box.Get(1) }

func LenCaller(box Box[int]) int { return box.Len(1) }
`},
	{"generic_unix.go", buildDarwin | buildLinux, `package fork

func (box Box[T]) Get(unix int) (held T) {
	_ = unix
	return box.value
}

func (box Box[T]) Len(unix int) (count int) { return unix }
`},
	{"generic_windows.go", buildWindows, `package fork

func (b Box[T]) Get(windows int) (kept T) {
	_ = windows
	return b.value
}

func (b Box[T]) Len(windows int) (total int) { return windows }
`},
}

// declaredUserFiles are a package that calls declaredFiles' function and method from another package.
var declaredUserFiles = []fixtureFile{
	{"user.go", everyBuild, `package user

import "fork"

func Use() error { return fork.Remove("x") }

func UseMethod(w fork.W) int { return w.Size("x") }

var Held = fork.Remove
`},
}

// spellDeclared spells a signature's receiver, parameters and results, each by its name, its type and the file and
// offset it is declared at.
func spellDeclared(fileSet *token.FileSet, signature *types.Signature) string {
	spelled := ""
	variable := func(label string, variable *types.Var) {
		position := fileSet.Position(variable.Pos())
		spelled += fmt.Sprintf("%s %s %s at %s+%d; ", label, variable.Name(), variable.Type(),
			filepath.Base(position.Filename), position.Offset)
	}
	if signature.Recv() != nil {
		variable("receiver", signature.Recv())
	}
	for index := range signature.Params().Len() {
		variable("parameter", signature.Params().At(index))
	}
	for index := range signature.Results().Len() {
		variable("result", signature.Results().At(index))
	}
	return spelled
}

// TestVariationalDeclaredSignaturesReadAsEachBuildDeclares checks declaredFiles and declaredGenericFiles once for
// every build, then reads each function and method under InBuild for each build and with no state, as the host: Type
// and Signature are one signature, whose receiver, parameters and results are the build's own declaration's, by name
// and by place, as the build's plain check has them, and they are the very objects the build's Defs declare at those
// names and its Uses name in the body. A generic type's instance has its method's signature of the build too, with
// the type argument in the type parameter's place, and a type's method set holds the methods whose receiver, as the
// build declares it, is no pointer. Under Agreeing the builds left are those the one declaration is compiled in.
func TestVariationalDeclaredSignaturesReadAsEachBuildDeclares(test *testing.T) {
	test.Parallel()
	method := func(typeName string, name string) func(checked *types.Package) *types.Func {
		return func(checked *types.Package) *types.Func { return methodOf(checked, typeName, name) }
	}
	// The method of an instance, whose signature is its origin's declaration's in the build, instantiated.
	instanceMethod := func(name string) func(checked *types.Package) *types.Func {
		return func(checked *types.Package) *types.Func {
			instance, instanceError := types.Instantiate(nil, checked.Scope().Lookup("Box").Type(),
				[]types.Type{types.Typ[types.Int]}, true)
			if instanceError != nil {
				test.Fatal(instanceError)
			}
			found, _, _ := types.LookupFieldOrMethod(instance, false, checked, name)
			return found.(*types.Func)
		}
	}
	for _, each := range []struct {
		name      string
		fixtures  []fixtureFile
		functions map[string]func(checked *types.Package) *types.Func
		instances map[string]func(checked *types.Package) *types.Func
	}{
		{"declared again", declaredFiles, map[string]func(checked *types.Package) *types.Func{
			"Remove": func(checked *types.Package) *types.Func { return checked.Scope().Lookup("Remove").(*types.Func) },
			"W.Size": method("W", "Size"),
		}, nil},
		{"a generic type's method", declaredGenericFiles, map[string]func(checked *types.Package) *types.Func{
			"Box.Get": method("Box", "Get"),
			"Box.Len": method("Box", "Len"),
		}, map[string]func(checked *types.Package) *types.Func{
			"Box[int].Get": instanceMethod("Get"),
			"Box[int].Len": instanceMethod("Len"),
		}},
	} {
		test.Run(each.name, func(test *testing.T) {
			test.Parallel()
			requireDeclaredSignatures(test, each.fixtures, each.functions, each.instances)
		})
	}
}

// requireDeclaredSignatures is TestVariationalDeclaredSignaturesReadAsEachBuildDeclares over one fixture: functions
// are held to their build's Defs and Uses as well, and instances, whose objects no Defs entry declares, to the plain
// check's spelling alone.
func requireDeclaredSignatures(test *testing.T, fixtures []fixtureFile,
	functions map[string]func(checked *types.Package) *types.Func,
	instances map[string]func(checked *types.Package) *types.Func,
) {
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, fixtures)
	variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	info := fullInfo()
	checked, checkError := (&types.Config{Variation: variation, Importer: fixtureImporter{}}).Check("fork", fileSet,
		parsed.files, info)
	if checkError != nil {
		test.Fatal(checkError)
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
		defs, uses := project(info.Defs, overlays.Defs), project(info.Uses, overlays.Uses)
		views := map[string]func(work func()){
			fmt.Sprintf("under InBuild %03b", build): func(work func()) { types.InBuild(build, work) },
		}
		if build == buildDarwin {
			views["with no state, as the host"] = func(work func()) { work() }
		}
		for view, read := range views {
			for name, function := range functions {
				want := spellDeclared(fileSet, function(plain).Signature())
				read(func() {
					signature := function(checked).Signature()
					if function(checked).Type() != types.Type(signature) {
						test.Errorf("%s %s: Signature is not Type", name, view)
					}
					if got := spellDeclared(fileSet, signature); got != want {
						test.Errorf("%s %s: its signature declares\n\t%s\nand the build's plain check's\n\t%s", name, view, got,
							want)
					}
					requireDeclaredBy(test, fmt.Sprintf("%s %s", name, view), fileSet, signature, defs, uses)
				})
			}
			// The method sets, which tell a value receiver from a pointer where the signatures' types don't.
			for _, name := range checked.Scope().Names() {
				spell := func(checked *types.Package) string {
					typeName, isTypeName := checked.Scope().Lookup(name).(*types.TypeName)
					if !isTypeName {
						return ""
					}
					named, isNamed := typeName.Type().(*types.Named)
					if !isNamed {
						return ""
					}
					spelled := spellMethodSet(named) + ", pointer's " + spellMethodSet(types.NewPointer(named))
					for index := range named.NumMethods() {
						spelled += ", " + named.Method(index).FullName()
					}
					return spelled
				}
				want := spell(plain)
				read(func() {
					if got := spell(checked); got != want {
						test.Errorf("%s %s: its method sets and methods are\n\t%s\nand the build's plain check's\n\t%s", name, view,
							got, want)
					}
				})
			}
			for name, function := range instances {
				want := spellDeclared(fileSet, function(plain).Signature())
				read(func() {
					if got := spellDeclared(fileSet, function(checked).Signature()); got != want {
						test.Errorf("%s %s: its signature declares\n\t%s\nand the build's plain check's\n\t%s", name, view, got,
							want)
					}
				})
			}
		}
	}
	unix := buildDarwin | buildLinux
	for name, function := range functions {
		read := func() { _ = function(checked).Signature().Params().At(0).Name() }
		for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
			want := unix
			if build == buildWindows {
				want = buildWindows
			}
			if agreeing := types.Agreeing(build, everyBuild, read); agreeing != want {
				test.Errorf("%s's signature read under Agreeing as %03b: agreeing %03b, and its declaration is %03b's", name,
					build, agreeing, want)
			}
		}
	}
}

// requireDeclaredBy requires each named receiver, parameter and result of signature to be the object defs declares
// at its place, and each use of an object at such a place to name it, and at least one to: the body's objects are
// the signature's.
func requireDeclaredBy(test *testing.T, label string, fileSet *token.FileSet, signature *types.Signature,
	defs map[*ast.Ident]types.Object, uses map[*ast.Ident]types.Object,
) {
	test.Helper()
	variables := []*types.Var{}
	if signature.Recv() != nil {
		variables = append(variables, signature.Recv())
	}
	for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
		for index := range tuple.Len() {
			variables = append(variables, tuple.At(index))
		}
	}
	used := 0
	for _, variable := range variables {
		declared := false
		for ident, object := range defs {
			if ident.Pos() == variable.Pos() {
				declared = true
				if object != types.Object(variable) {
					test.Errorf("%s: %s, at %s, isn't the object the build's Defs declare there", label, variable.Name(),
						fileSet.Position(variable.Pos()))
				}
			}
		}
		if !declared {
			test.Errorf("%s: %s, at %s, is declared by no Defs entry of the build", label, variable.Name(),
				fileSet.Position(variable.Pos()))
		}
		for ident, object := range uses {
			if object.Pos() == variable.Pos() {
				used++
				if object != types.Object(variable) {
					test.Errorf("%s: the use of %s at %s names another object than the signature's", label, variable.Name(),
						fileSet.Position(ident.Pos()))
				}
			}
		}
	}
	if used == 0 {
		test.Errorf("%s: the build's Uses name none of its parameters, and its body uses one", label)
	}
}

// TestVariationalDeclaredSignatureWhereNoBuildDeclaresIt holds Only, which darwin and linux each declare with a
// parameter of their own name and windows doesn't declare, and reads its signature as each build: darwin's and
// linux's are their own, alone in their agreement, and read as a build that declares it nowhere it is the one type
// every such build reads, which the agreement keeps together.
func TestVariationalDeclaredSignatureWhereNoBuildDeclaresIt(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, declaredFiles)
	only := lookupInEveryBuild(checked.Scope(), "Only").(*types.Func)
	const buildFourth types.Mask = 1 << 3
	for _, each := range []struct {
		build     types.Mask
		parameter string
		agreeing  types.Mask
	}{
		{buildDarwin, "first", buildDarwin},
		{buildLinux, "second", buildLinux},
		{buildWindows, "first", buildWindows | buildFourth},
		{buildFourth, "first", buildWindows | buildFourth},
	} {
		var parameter string
		agreeing := types.Agreeing(each.build, everyBuild|buildFourth, func() {
			parameter = only.Signature().Params().At(0).Name()
		})
		if parameter != each.parameter || agreeing != each.agreeing {
			test.Errorf("Only's parameter read as %04b is %s, agreeing %04b; want %s and %04b", each.build, parameter,
				agreeing, each.parameter, each.agreeing)
		}
	}
}

// declaredByHand is a package made as the merging importer makes one, with a function and a method whose signatures
// are identical in every build and hold each build's own parameter: Remove's is name on darwin and linux and path on
// windows, and W.Size's unix and windows.
func declaredByHand() (merged *types.Package, removeByBuild map[types.Mask]*types.Signature) {
	merged = types.NewPackage("fork", "fork")
	unix := buildDarwin | buildLinux
	named := types.NewNamed(types.NewTypeName(token.NoPos, merged, "W", nil), types.NewStruct(nil, nil), nil)
	merged.Scope().Insert(named.Obj())
	signature := func(receiver bool, parameter string, result types.Type) *types.Signature {
		var receiverVariable *types.Var
		if receiver {
			receiverVariable = types.NewParam(token.NoPos, merged, "", named)
		}
		return types.NewSignatureType(receiverVariable, nil, nil,
			types.NewTuple(types.NewParam(token.NoPos, merged, parameter, types.Typ[types.String])),
			types.NewTuple(types.NewParam(token.NoPos, merged, "", result)), false)
	}
	failure := types.Universe.Lookup("error").Type()
	removeByBuild = map[types.Mask]*types.Signature{
		unix:         signature(false, "name", failure),
		buildWindows: signature(false, "path", failure),
	}
	remove := types.NewFunc(token.NoPos, merged, "Remove", removeByBuild[unix])
	types.SetVariants(remove, []types.Variant{
		{Builds: unix, Type: removeByBuild[unix]},
		{Builds: buildWindows, Type: removeByBuild[buildWindows]},
	})
	merged.Scope().Insert(remove)
	sizeUnix := signature(true, "unix", types.Typ[types.Int])
	sizeWindows := signature(true, "windows", types.Typ[types.Int])
	size := types.NewFunc(token.NoPos, merged, "Size", sizeUnix)
	named.AddMethod(size)
	types.SetVariants(size, []types.Variant{{Builds: unix, Type: sizeUnix}, {Builds: buildWindows, Type: sizeWindows}})
	merged.MarkComplete()
	return merged, removeByBuild
}

// TestVariationalSetVariantsKeepsEachBuildsOwnSignature gives a function, as the merging importer does, a signature
// for each build that is identical in type and holds that build's own parameter: read under InBuild for a build, and
// with no state as the host, its Type and Signature are the very signature given for the build, and under Agreeing
// the builds left are those given that signature. VariantsOf still answers one type for every build.
func TestVariationalSetVariantsKeepsEachBuildsOwnSignature(test *testing.T) {
	test.Parallel()
	merged, removeByBuild := declaredByHand()
	unix := buildDarwin | buildLinux
	remove := merged.Scope().Lookup("Remove").(*types.Func)
	size := methodOf(merged, "W", "Size")
	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		want := unix
		if build == buildWindows {
			want = buildWindows
		}
		var signature *types.Signature
		var sizeParameter string
		agreeing := types.Agreeing(build, everyBuild, func() {
			signature = remove.Signature()
			sizeParameter = size.Signature().Params().At(0).Name()
		})
		if signature != removeByBuild[want] {
			test.Errorf("Remove's signature read as %03b names its parameter %s, and isn't the one given for %03b", build,
				signature.Params().At(0).Name(), want)
		}
		if host := remove.Signature(); host != removeByBuild[unix] {
			test.Errorf("Remove's signature read with no state names its parameter %s, and isn't the one given for the host",
				host.Params().At(0).Name())
		}
		types.InBuild(build, func() {
			if remove.Type() != types.Type(removeByBuild[want]) {
				test.Errorf("Remove's Type under InBuild %03b isn't the signature given for %03b", build, want)
			}
		})
		wantParameter := map[types.Mask]string{unix: "unix", buildWindows: "windows"}[want]
		if sizeParameter != wantParameter {
			test.Errorf("W.Size's parameter read as %03b is %s, want %s", build, sizeParameter, wantParameter)
		}
		if agreeing != want {
			test.Errorf("the signatures read under Agreeing as %03b: agreeing %03b, want %03b", build, agreeing, want)
		}
	}
	if variants := types.VariantsOf(remove); len(variants) != 0 {
		test.Errorf("VariantsOf(Remove) holds %d answers, and its type is one in every build", len(variants))
	}
}

// TestVariationalAUnitCallingADeclaredAgainFunctionRunsOnce checks, variationally, packages that call a function and
// a method declared in each build's own file with identical types, from the package itself and from another, with
// the callee checked from source and made as the merging importer makes it: no unit restarts, since the callee's type
// is one in every build, and the callers answer one way for every build.
func TestVariationalAUnitCallingADeclaredAgainFunctionRunsOnce(test *testing.T) {
	test.Parallel()
	forked := projectAndCompare(test, declaredFiles)
	for _, caller := range []string{"Caller", "MethodCaller"} {
		if forked[caller] {
			test.Errorf("%s forked, though what it calls has one type in every build", caller)
		}
	}
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, declaredFiles)
	fromSource := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	checked, checkError := (&types.Config{Variation: fromSource, Importer: fixtureImporter{}}).Check("fork", fileSet,
		parsed.files, &types.Info{})
	if checkError != nil {
		test.Fatal(checkError)
	}
	if fromSource.Restarts != 0 {
		test.Errorf("the check of the declaring package restarted %d units of %d, want none", fromSource.Restarts,
			fromSource.Units)
	}
	test.Logf("the declaring package: units %d, restarts %d", fromSource.Units, fromSource.Restarts)
	// A generic type's method that names its type parameter answers by build, and a unit that calls it splits.
	generic := projectAndCompare(test, declaredGenericFiles)
	if generic["LenCaller"] {
		test.Error("LenCaller forked, though Len has one type in every build")
	}
	byHand, _ := declaredByHand()
	for label, imported := range map[string]*types.Package{"checked from source": checked, "made by hand": byHand} {
		user := parseFixture(test, fileSet, declaredUserFiles)
		variation := &types.Variation{Builds: everyBuild, Files: user.masks}
		if _, userError := (&types.Config{Variation: variation, Importer: packageImporter{"fork": imported}}).Check("user",
			fileSet, user.files, &types.Info{}); userError != nil {
			test.Fatalf("the user of the package %s: %v", label, userError)
		}
		for expression, answers := range variation.Types {
			if answers.Len() > 1 {
				test.Errorf("the user of the package %s: %s answers %d ways, want one", label, types.ExprString(expression),
					answers.Len())
			}
		}
		if variation.Restarts != 0 || variation.Units == 0 {
			test.Errorf("the user of the package %s: units %d, restarts %d, want every unit run once", label,
				variation.Units, variation.Restarts)
		}
		test.Logf("the user of the package %s: units %d, restarts %d", label, variation.Units, variation.Restarts)
	}
}

// declaredTypesUserFiles are a package that names declaredFiles' function and methods from another package every
// way an expression's type can hold their signatures: the function itself, a method value, a method expression, and
// a call.
var declaredTypesUserFiles = []fixtureFile{
	{"user.go", everyBuild, `package user

import "fork"

var Held = fork.Remove

func Value(w fork.W) func(string) int { return w.Size }

var Expression = fork.W.Size

func Call() error { return fork.Remove("x") }
`},
}

// TestVariationalACallersTypesHoldEachBuildsOwnSignature checks a package that names, from units that never split, a
// function and a method declared again by build with one type, and requires each build's projection of its Types to
// hold that build's own declaration's objects where the plain check's does: the function's very signature for the
// name of the function, and for a method value and a method expression the method's own parameter and result. The
// plain map, the host's, holds the host's.
func TestVariationalACallersTypesHoldEachBuildsOwnSignature(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, declaredFiles)
	user := parseFixture(test, fileSet, declaredTypesUserFiles)
	variation := &types.Variation{Builds: everyBuild, Files: user.masks}
	info := fullInfo()
	if _, userError := (&types.Config{Variation: variation, Importer: packageImporter{"fork": checked}}).Check("user",
		fileSet, user.files, info); userError != nil {
		test.Fatal(userError)
	}
	if variation.Restarts != 0 {
		test.Errorf("the user's check restarted %d units of %d, want none", variation.Restarts, variation.Units)
	}
	remove := checked.Scope().Lookup("Remove").(*types.Func)
	size := methodOf(checked, "W", "Size")
	for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		projected := info.Types
		if index > 0 {
			projected = project(info.Types, variation.Overlays[index].Types)
		}
		var removeSignature, sizeSignature *types.Signature
		types.InBuild(build, func() { removeSignature, sizeSignature = remove.Signature(), size.Signature() })
		met := map[string]bool{}
		for expression, answer := range projected {
			spelled := types.ExprString(expression)
			met[spelled] = true
			signature, _ := answer.Type.(*types.Signature)
			switch spelled {
			case "fork.Remove":
				if signature != removeSignature {
					test.Errorf("build %03b: the type of fork.Remove names its parameter %s, and the build's own signature %s",
						build, signature.Params().At(0).Name(), removeSignature.Params().At(0).Name())
				}
			case "w.Size":
				if signature.Params().At(0) != sizeSignature.Params().At(0) ||
					signature.Results().At(0) != sizeSignature.Results().At(0) {
					test.Errorf("build %03b: the type of w.Size is %s, and the build's own method's is %s", build, signature,
						sizeSignature)
				}
			case "fork.W.Size":
				if signature.Params().Len() != 2 || signature.Params().At(1) != sizeSignature.Params().At(0) ||
					signature.Results().At(0) != sizeSignature.Results().At(0) ||
					signature.Params().At(0).Name() != sizeSignature.Recv().Name() ||
					signature.Params().At(0).Pos() != sizeSignature.Recv().Pos() {
					test.Errorf("build %03b: the type of fork.W.Size is %s, and the build's own method's is %s of %s", build,
						signature, sizeSignature, sizeSignature.Recv().Name())
				}
			}
		}
		for _, spelled := range []string{"fork.Remove", "w.Size", "fork.W.Size"} {
			if !met[spelled] {
				test.Errorf("build %03b: no Types entry for %s", build, spelled)
			}
		}
	}
}

// receiverFiles declare a method with one type in every build and a value receiver on darwin and linux, a pointer
// receiver on windows. A shared file needs the method in the type's own method set twice, assigning a value to an
// interface and calling the method on a value that has no address: both are right on darwin and linux, and on
// windows each is an error its plain check reports.
var receiverFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type W struct{}

type Resetter interface{ Reset() }

var Held Resetter = W{}

func Call() { W{}.Reset() }
`},
	{"receiver_unix.go", buildDarwin | buildLinux, "package fork\n\nfunc (w W) Reset() {}\n"},
	{"receiver_windows.go", buildWindows, "package fork\n\nfunc (receiver *W) Reset() {}\n"},
}

// TestVariationalAUnitReadsEachBuildsOwnReceiver is the projection test over receiverFiles with diagnostics: a unit
// that needs a method in a type's method set answers for each build by that build's own receiver, so windows's two
// errors are reported, for windows alone.
func TestVariationalAUnitReadsEachBuildsOwnReceiver(test *testing.T) {
	test.Parallel()
	_, published := projectAndCompareWithDiagnostics(test, receiverFiles, true)
	if len(published) != 2 {
		test.Errorf("the variational check reported %d errors, %v, and windows's plain check reports two", len(published),
			published)
	}
}
