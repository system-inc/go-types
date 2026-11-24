package types_test

import (
	"fmt"
	"go/constant"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// agreeingFiles declare, beside names that are one in every build, one name for each property that can differ by
// build, each alike on darwin and linux and otherwise on windows: a constant's value, a variable's type, a named
// type's underlying type, a name only darwin and linux declare, a method only they declare, methods windows orders
// otherwise, an interface's type set, a declaration in each build's own file, and each build's imports. Partial and
// PartialType are darwin's and linux's alone, each its own, so windows has no answer of theirs.
var agreeingFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

import _ "example/one"

const Steady = 7

var SteadyVariable int

type SteadyType struct{ value int }

func (SteadyType) Method() int { return 1 }

type SteadyInterface interface{ Steady() int }

type Status uint32

type Ordered uint32

type Reader interface {
	Closer
	Read() int
}
`},
	{"unix.go", buildDarwin | buildLinux, `package fork

import _ "example/unix"

const N = 1

var V int64

type T struct{ Value uint32 }

var OnlyUnix = 1

func (Status) Exited() bool { return false }

func (Status) Signaled() bool { return true }

func (Ordered) First() int { return 1 }

func (Ordered) Second() int { return 2 }

type Closer interface{ Close() error }
`},
	{"windows.go", buildWindows, `package fork

import _ "example/windows"

const N = 2

var V int32

type T struct{ Value uint16 }

func (Status) Signaled() bool { return true }

func (Ordered) Second() int { return 2 }

func (Ordered) First() int { return 1 }

type Closer interface {
	Close() error
	Handle() uintptr
}
`},
	{"partial_darwin.go", buildDarwin, "package fork\n\nconst Partial = 1\n\ntype PartialType struct{ A int }\n"},
	{"partial_linux.go", buildLinux, "package fork\n\nconst Partial = 2\n\ntype PartialType struct{ B int }\n"},
}

// checkAgreeingFixture checks fixtures once for every build.
func checkAgreeingFixture(test *testing.T, fileSet *token.FileSet, fixtures []fixtureFile) *types.Package {
	test.Helper()
	parsed := parseFixture(test, fileSet, fixtures)
	checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
		Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
	if checkError != nil {
		test.Fatal(checkError)
	}
	return checked
}

// lookupInEveryBuild is name's first object in scope, whichever builds declare it, as a read outside every unit
// finds it.
func lookupInEveryBuild(scope *types.Scope, name string) types.Object {
	var found types.Object
	types.Outside(func() { found = scope.Lookup(name) })
	return found
}

// mergedPackage is a package made as the merging importer makes one, its answers alike on darwin and linux and
// otherwise on windows: Limit's value, Stat's underlying type, Status's methods, with Exited on darwin and linux alone
// and each build's own order given by index, and Handle, a type on darwin and linux and a variable on windows, kept
// beside it. Plain and Fixed are one in every build.
func mergedPackage() *types.Package {
	merged := types.NewPackage("example.com/merged", "merged")
	unix := buildDarwin | buildLinux
	limit := types.NewConst(token.NoPos, merged, "Limit", types.Typ[types.Int], constant.MakeInt64(1))
	types.SetVariants(limit, []types.Variant{
		{Builds: unix, Type: types.Typ[types.Int], Value: constant.MakeInt64(1)},
		{Builds: buildWindows, Type: types.Typ[types.Int], Value: constant.MakeInt64(2)},
	})
	merged.Scope().Insert(limit)
	merged.Scope().Insert(types.NewConst(token.NoPos, merged, "Fixed", types.Typ[types.Int], constant.MakeInt64(5)))

	structOf := func(mode types.Type) *types.Struct {
		return types.NewStruct([]*types.Var{types.NewField(token.NoPos, merged, "Mode", mode, false)}, nil)
	}
	stat := types.NewNamed(types.NewTypeName(token.NoPos, merged, "Stat", nil), structOf(types.Typ[types.Uint32]), nil)
	types.SetUnderlyingVariants(stat, []types.UnderlyingVariant{
		{Builds: unix, Underlying: structOf(types.Typ[types.Uint32])},
		{Builds: buildWindows, Underlying: structOf(types.Typ[types.Uint16])},
	})
	merged.Scope().Insert(stat.Obj())

	status := types.NewNamed(types.NewTypeName(token.NoPos, merged, "Status", nil), types.Typ[types.Uint32], nil)
	method := func(named *types.Named, name string) *types.Func {
		receiver := types.NewParam(token.NoPos, merged, "", named)
		result := types.NewTuple(types.NewParam(token.NoPos, merged, "", types.Typ[types.Bool]))
		made := types.NewFunc(token.NoPos, merged, name, types.NewSignatureType(receiver, nil, nil, nil, result, false))
		named.AddMethod(made)
		return made
	}
	exited, signaled, stopped := method(status, "Exited"), method(status, "Signaled"), method(status, "Stopped")
	types.SetPresence(exited, unix)
	types.SetMethodIndex(exited, unix, 0)
	types.SetMethodIndex(signaled, unix, 1)
	types.SetMethodIndex(stopped, unix, 2)
	types.SetMethodIndex(stopped, buildWindows, 0)
	types.SetMethodIndex(signaled, buildWindows, 1)
	merged.Scope().Insert(status.Obj())

	plain := types.NewNamed(types.NewTypeName(token.NoPos, merged, "Plain", nil), types.Typ[types.Int], nil)
	method(plain, "Method")
	merged.Scope().Insert(plain.Obj())

	handle := types.NewNamed(types.NewTypeName(token.NoPos, merged, "Handle", nil), types.Typ[types.Uintptr], nil)
	types.SetPresence(handle.Obj(), unix)
	variable := types.NewVar(token.NoPos, merged, "Handle", types.Typ[types.Uintptr])
	types.SetPresence(variable, buildWindows)
	types.SetAlternates(handle.Obj(), []types.Object{variable})
	merged.Scope().Insert(handle.Obj())
	merged.MarkComplete()
	return merged
}

// agreeingRead is one read of a property, named for the test's report.
type agreeingRead struct {
	name string
	read func()
}

// TestVariationalAgreeingNarrowsAtEachReadThatDiffers reads, one at a time, each property of agreeingFiles and of a
// merged package that darwin and linux answer alike and windows otherwise: gathered as darwin, the agreement is darwin
// and linux, and gathered as windows it is windows alone.
func TestVariationalAgreeingNarrowsAtEachReadThatDiffers(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, agreeingFiles)
	scope := checked.Scope()
	n := lookupInEveryBuild(scope, "N").(*types.Const)
	v := lookupInEveryBuild(scope, "V")
	t := lookupInEveryBuild(scope, "T").Type().(*types.Named)
	status := lookupInEveryBuild(scope, "Status").Type().(*types.Named)
	ordered := lookupInEveryBuild(scope, "Ordered").Type().(*types.Named)
	reader := lookupInEveryBuild(scope, "Reader").Type().(*types.Named)
	merged := mergedPackage()
	limit := lookupInEveryBuild(merged.Scope(), "Limit").(*types.Const)
	stat := lookupInEveryBuild(merged.Scope(), "Stat").Type().(*types.Named)
	mergedStatus := lookupInEveryBuild(merged.Scope(), "Status").Type().(*types.Named)
	methodNames := func(named *types.Named) string {
		var names []string
		for index := range named.NumMethods() {
			names = append(names, named.Method(index).Name())
		}
		return strings.Join(names, " ")
	}
	// The interface's type set is read twice, since the first read of a set may compute it and the second finds it kept.
	reads := []agreeingRead{
		{"a constant's value", func() { _ = n.Val() }},
		{"a constant's type and value by its variants", func() { _ = limit.Val() }},
		{"a variable's type", func() { _ = v.Type() }},
		{"a named type's underlying type", func() { _ = t.Underlying() }},
		{"an imported named type's underlying type", func() { _ = stat.Underlying() }},
		{"a name only some builds declare", func() { _ = scope.Lookup("OnlyUnix") }},
		{"a name of another kind in other builds", func() { _ = merged.Scope().Lookup("Handle") }},
		{"a method only some builds declare", func() {
			_, _, _ = types.LookupFieldOrMethod(status, false, checked, "Exited")
		}},
		{"an imported method only some builds declare", func() {
			_, _, _ = types.LookupFieldOrMethod(mergedStatus, false, merged, "Exited")
		}},
		{"how many methods a type has", func() { _ = status.NumMethods() }},
		{"a method set", func() { _ = types.NewMethodSet(status) }},
		{"a method order that differs", func() { _ = ordered.Method(0) }},
		{"a method's index where the order differs", func() {
			_, _, _ = types.LookupFieldOrMethod(ordered, false, checked, "First")
		}},
		{"an imported type's methods in their order", func() { _ = methodNames(mergedStatus) }},
		{"an interface's type set", func() { _ = reader.Underlying().(*types.Interface).NumMethods() }},
		{"an interface's type set, read again", func() { _ = reader.Underlying().(*types.Interface).NumMethods() }},
		{"an object's position", func() { _ = n.Pos() }},
		{"a package's imports", func() { _ = checked.Imports() }},
		{"a scope's Names", func() { _ = scope.Names() }},
		{"a scope's Len", func() { _ = scope.Len() }},
		{"an imported scope's Names", func() { _ = merged.Scope().Names() }},
	}
	for _, each := range reads {
		if agreeing := types.Agreeing(buildDarwin, everyBuild, each.read); agreeing != buildDarwin|buildLinux {
			test.Errorf("%s, read as darwin: agreeing %03b, and darwin and linux alone answer alike, 011", each.name,
				agreeing)
		}
		if agreeing := types.Agreeing(buildWindows, everyBuild, each.read); agreeing != buildWindows {
			test.Errorf("%s, read as windows: agreeing %03b, and windows alone answers so, 100", each.name, agreeing)
		}
		if agreeing := types.Agreeing(buildLinux, buildLinux|buildWindows, each.read); agreeing != buildLinux {
			test.Errorf("%s, read as linux among linux and windows: agreeing %03b, and linux alone is left, 010",
				each.name, agreeing)
		}
		if agreeing := types.Agreeing(buildDarwin, buildLinux|buildWindows, each.read); agreeing != buildLinux {
			test.Errorf("%s, read as darwin among linux and windows: agreeing %03b, and of them linux agrees, 010",
				each.name, agreeing)
		}
	}

	// An object held from a build that declares it, read as builds that don't: no answer of its own holds there, so
	// the first stands in, and the builds with no answer agree with each other and with none that has one. Two more
	// builds than the fixture's, which no answer holds in, are asked about.
	const buildFourth, buildFifth types.Mask = 1 << 3, 1 << 4
	const five = everyBuild | buildFourth | buildFifth
	partial := lookupInEveryBuild(scope, "Partial").(*types.Const)
	partialType := lookupInEveryBuild(scope, "PartialType").Type().(*types.Named)
	for _, each := range []agreeingRead{
		{"a constant's value where no answer holds", func() { _ = partial.Val() }},
		{"a named type's underlying type where none holds", func() { _ = partialType.Underlying() }},
	} {
		for build, want := range map[types.Mask]types.Mask{
			buildDarwin:  buildDarwin,
			buildLinux:   buildLinux,
			buildWindows: buildWindows | buildFourth | buildFifth,
			buildFourth:  buildWindows | buildFourth | buildFifth,
		} {
			if agreeing := types.Agreeing(build, five, each.read); agreeing != want {
				test.Errorf("%s, read as %05b: agreeing %05b, want %05b", each.name, build, agreeing, want)
			}
		}
	}
}

// TestVariationalAgreeingKeepsEveryBuildWhereNothingReadDiffers reads what agreeingFiles and the merged package hold
// alike in every build, from each build: the agreement is every build asked about, whatever else of the package
// differs.
func TestVariationalAgreeingKeepsEveryBuildWhereNothingReadDiffers(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, agreeingFiles)
	scope := checked.Scope()
	merged := mergedPackage()
	steadyType := func() *types.Named { return scope.Lookup("SteadyType").Type().(*types.Named) }
	reads := []agreeingRead{
		{"nothing", func() {}},
		{"a constant's value", func() { _ = scope.Lookup("Steady").(*types.Const).Val() }},
		{"an imported constant's value", func() { _ = merged.Scope().Lookup("Fixed").(*types.Const).Val() }},
		{"a variable's type", func() { _ = scope.Lookup("SteadyVariable").Type() }},
		{"a named type's underlying type", func() { _ = steadyType().Underlying() }},
		{"a named type's methods", func() {
			for index := range steadyType().NumMethods() {
				_ = steadyType().Method(index).Type()
			}
			_ = types.NewMethodSet(steadyType())
			_, _, _ = types.LookupFieldOrMethod(steadyType(), false, checked, "Method")
			_, _, _ = types.LookupFieldOrMethod(steadyType(), false, checked, "Missing")
		}},
		{"an imported named type's methods", func() {
			plain := merged.Scope().Lookup("Plain").Type().(*types.Named)
			_ = plain.Underlying()
			_ = plain.Method(plain.NumMethods() - 1)
		}},
		{"an interface's type set", func() {
			_ = scope.Lookup("SteadyInterface").Type().Underlying().(*types.Interface).NumMethods()
		}},
		{"an object's position", func() { _ = scope.Lookup("Steady").Pos() }},
		{"a name no build declares", func() { _ = scope.Lookup("Missing") }},
		{"a type with methods that differ, never asked its methods", func() {
			_ = scope.Lookup("Status").Type().Underlying()
		}},
	}
	for _, each := range reads {
		for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
			if agreeing := types.Agreeing(build, everyBuild, each.read); agreeing != everyBuild {
				test.Errorf("%s, read as %03b: agreeing %03b, and nothing read differs by build, 111", each.name, build,
					agreeing)
			}
			if agreeing := types.Agreeing(build, buildLinux|buildWindows, each.read); agreeing != buildLinux|buildWindows {
				test.Errorf("%s, read as %03b among linux and windows: agreeing %03b, and both agree, 110", each.name,
					build, agreeing)
			}
		}
	}
	// A package no variational check made, and the universe, hold nothing by build. The package is reached before the
	// gathering, since the imports it is read from are each build's own.
	one := checked.Imports()[0]
	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		agreeing := types.Agreeing(build, everyBuild, func() {
			_ = one.Imports()
			_ = one.Scope().Names()
			_ = one.Scope().Len()
			_ = types.Universe.Names()
			_ = types.Universe.Len()
			_ = types.Universe.Lookup("int").Type().Underlying()
		})
		if agreeing != everyBuild {
			test.Errorf("a package and a universe that hold nothing by build, read as %03b: agreeing %03b, 111", build,
				agreeing)
		}
	}
}

// TestVariationalAgreeingNests holds Agreeing to each rule of what runs inside it and around it: InBuild of its build
// and of another, Agreeing of its build and of another, Agreeing under a plain InBuild, Carry, Outside and
// NoteAgreement.
func TestVariationalAgreeingNests(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, agreeingFiles)
	n := lookupInEveryBuild(checked.Scope(), "N").(*types.Const)
	// readDiffering reads what darwin and linux answer alike and windows otherwise, and readNothing nothing by build.
	readDiffering := func() { _ = n.Val() }
	readNothing := func() {}
	value := func() string { return n.Val().ExactString() }
	unix := buildDarwin | buildLinux
	asDarwin := func(work func()) types.Mask { return types.Agreeing(buildDarwin, everyBuild, work) }
	require := func(what string, got types.Mask, want types.Mask) {
		test.Helper()
		if got != want {
			test.Errorf("%s: agreeing %03b, want %03b", what, got, want)
		}
	}

	require("InBuild of the same build, whose reads the agreement gathers",
		asDarwin(func() { types.InBuild(buildDarwin, readDiffering) }), unix)
	require("InBuild of the same build reading nothing by build",
		asDarwin(func() { types.InBuild(buildDarwin, readNothing) }), everyBuild)
	require("InBuild of another build, which nothing relates to the agreement's",
		asDarwin(func() { types.InBuild(buildLinux, readNothing) }), buildDarwin)
	require("InBuild of the same build inside InBuild of another, which gathers nothing more",
		asDarwin(func() { types.InBuild(buildWindows, func() { types.InBuild(buildDarwin, readDiffering) }) }),
		buildDarwin)

	var inner types.Mask
	outer := asDarwin(func() { inner = types.Agreeing(buildDarwin, everyBuild, readDiffering) })
	require("Agreeing of the same build inside Agreeing, the inner", inner, unix)
	require("Agreeing of the same build inside Agreeing, the outer, narrowed by the inner", outer, unix)
	outer = asDarwin(func() { inner = types.Agreeing(buildDarwin, buildDarwin|buildWindows, readNothing) })
	require("Agreeing among fewer builds inside Agreeing, the inner", inner, buildDarwin|buildWindows)
	require("Agreeing among fewer builds inside Agreeing, the outer, narrowed by what the inner returned", outer,
		buildDarwin|buildWindows)
	outer = asDarwin(func() { inner = types.Agreeing(buildWindows, everyBuild, readDiffering) })
	require("Agreeing of another build inside Agreeing, the inner", inner, buildWindows)
	require("Agreeing of another build inside Agreeing, the outer, left its build alone", outer, buildDarwin)
	outer = asDarwin(func() {
		inner = types.Agreeing(buildDarwin, everyBuild, readNothing)
		readDiffering()
	})
	require("an inner Agreeing that reads nothing by build", inner, everyBuild)
	require("a read after an inner Agreeing ends, which the outer gathers again", outer, unix)
	// An inner Agreeing that panics gave no answer, so one that recovers it reads on with its build alone.
	outer = asDarwin(func() {
		defer func() { _ = recover() }()
		types.Agreeing(buildDarwin, everyBuild, func() { panic("abandoned") })
	})
	require("an inner Agreeing whose work panicked", outer, buildDarwin)

	var windowsBefore, windowsAfter string
	types.InBuild(buildWindows, func() {
		windowsBefore = value()
		inner = types.Agreeing(buildWindows, everyBuild, readDiffering)
		outer = types.Agreeing(buildDarwin, everyBuild, readDiffering)
		windowsAfter = value()
	})
	require("Agreeing as windows under a plain InBuild for windows", inner, buildWindows)
	require("Agreeing as darwin under a plain InBuild for windows", outer, unix)
	if windowsBefore != "2" || windowsAfter != "2" {
		test.Errorf("N under InBuild for windows is %s before an Agreeing inside it and %s after, want windows's 2 twice",
			windowsBefore, windowsAfter)
	}

	var carried string
	agreeing := types.Agreeing(buildWindows, everyBuild, func() {
		done := make(chan struct{})
		go types.Carry(func() {
			defer close(done)
			carried = value()
		})()
		<-done
	})
	require("Carry inside Agreeing as windows, whose goroutine's reads aren't gathered", agreeing, buildWindows)
	if carried != "2" {
		test.Errorf("N read on a goroutine started with Carry under Agreeing as windows: %s, want windows's 2", carried)
	}
	require("Carry inside Agreeing as darwin, never run", asDarwin(func() { _ = types.Carry(readNothing) }), buildDarwin)

	var outside string
	agreeing = types.Agreeing(buildWindows, everyBuild, func() { types.Outside(func() { outside = value() }) })
	require("Outside inside Agreeing, whose reads answer by no build", agreeing, everyBuild)
	for _, build := range []types.Mask{buildDarwin, buildLinux} {
		var again string
		types.InBuild(build, func() { types.Outside(func() { again = value() }) })
		if again != outside {
			test.Errorf("N inside Outside under InBuild %03b is %s, and inside Outside under Agreeing as windows %s", build,
				again, outside)
		}
	}

	require("NoteAgreement of darwin and windows",
		asDarwin(func() { types.NoteAgreement(buildDarwin | buildWindows) }), buildDarwin|buildWindows)
	require("NoteAgreement of darwin and windows, then a read darwin and linux share", asDarwin(func() {
		types.NoteAgreement(buildDarwin | buildWindows)
		readDiffering()
	}), buildDarwin)
	require("NoteAgreement of windows alone, read as darwin, which agrees with itself",
		asDarwin(func() { types.NoteAgreement(buildWindows) }), buildDarwin|buildWindows)
	require("NoteAgreement under InBuild of the same build inside Agreeing",
		asDarwin(func() { types.InBuild(buildDarwin, func() { types.NoteAgreement(unix) }) }), unix)
	require("NoteAgreement inside Outside inside Agreeing",
		asDarwin(func() { types.Outside(func() { types.NoteAgreement(unix) }) }), unix)
	// With no Agreeing running it does nothing, under InBuild and with no state.
	types.NoteAgreement(buildWindows)
	types.InBuild(buildWindows, func() {
		types.NoteAgreement(buildDarwin)
		if got := value(); got != "2" {
			test.Errorf("N under InBuild for windows after NoteAgreement: %s, want windows's 2", got)
		}
	})
	if got := value(); got != "1" {
		test.Errorf("N with no state after NoteAgreement: %s, want the host's 1", got)
	}

	// Agreeing reads as its build, and puts back what it found when work ends.
	var asWindows string
	types.Agreeing(buildWindows, everyBuild, func() { asWindows = value() })
	if after := value(); asWindows != "2" || after != "1" {
		test.Errorf("N under Agreeing as windows is %s and after it %s, want windows's 2 and the host's 1", asWindows,
			after)
	}
	func() {
		defer func() {
			if recover() == nil {
				test.Error("Agreeing of two builds at once didn't panic, as InBuild does")
			}
		}()
		types.Agreeing(unix, everyBuild, readNothing)
	}()
}

// reachSpelling spells everything a reader can read of an object and of what it reaches: its kind, type, value and
// place, a named type's type parameters, underlying type, methods by index and method sets, every selector of it any
// build has, the interfaces of its package it implements, a generic type's instance, made anew and found again in a
// context every spelling shares, a struct's fields, an interface's methods and embedded types, and a signature's
// parameters, each named type and type literal once.
type reachSpelling struct {
	fileSet *token.FileSet
	checked *types.Package
	context *types.Context
	// names are the package-level names of every build, and selectors every method and field name the spelling met in
	// any build, which the next spelling looks up in each type.
	names     []string
	selectors map[string]bool
	builder   strings.Builder
	seen      map[types.Type]bool
	// disagreements are the accessors met that answered otherwise than the type of their object in the build read as
	// (function, methodsInSet): an accessor that reads one build's answer for every build agrees with itself across
	// builds, so comparing builds alone never finds it.
	disagreements map[string]bool
}

// disagree notes an accessor that answered otherwise than its object's type in the build read as.
func (spelling *reachSpelling) disagree(format string, arguments ...any) {
	spelling.disagreements[fmt.Sprintf(format, arguments...)] = true
}

// function spells what a function's own accessors answer, its signature, its scope and its full name, and holds
// each to the function's type in the build read as: Signature is Type, the scope declares that signature's own named
// parameters and results, and the full name is written with that signature's receiver.
func (spelling *reachSpelling) function(function *types.Func) {
	typed, isSignature := function.Type().(*types.Signature)
	if !isSignature {
		return
	}
	signature := function.Signature()
	scope := function.Scope()
	fmt.Fprintf(&spelling.builder, "\n function %s, signature %s, scope %s", function.FullName(),
		spellSignature(signature), spellScope(spelling.fileSet, scope))
	if signature != typed {
		spelling.disagree("%s: Signature is %s, and Type %s", function.Name(), spellSignature(signature),
			spellSignature(typed))
	}
	wantName := function.Name()
	switch receiver := typed.Recv(); {
	case receiver == nil && function.Pkg() != nil:
		wantName = function.Pkg().Path() + "." + wantName
	case receiver == nil:
	case types.IsInterface(receiver.Type()) && types.Unalias(receiver.Type()) == receiver.Type().Underlying():
		wantName = "(interface)." + wantName
	default:
		wantName = "(" + types.TypeString(receiver.Type(), nil) + ")." + wantName
	}
	if fullName := function.FullName(); fullName != wantName {
		spelling.disagree("%s: FullName is %s, and its type's receiver gives %s", function.Name(), fullName, wantName)
	}
	if scope == nil {
		return
	}
	for _, tuple := range []*types.Tuple{typed.Params(), typed.Results()} {
		for index := range tuple.Len() {
			variable := tuple.At(index)
			if variable.Name() != "" && variable.Name() != "_" && scope.Lookup(variable.Name()) != types.Object(variable) {
				spelling.disagree("%s: Scope, in %s, doesn't declare %s of its type %s", function.Name(),
					spellScope(spelling.fileSet, scope), variable.Name(), spellSignature(typed))
			}
		}
	}
}

// methodsInSet holds a named type's method set to its methods' receivers in the build read as: a method is in the
// type's own method set exactly when its receiver is no pointer. A method a field of the type's own struct clashes
// with, which a build's check reports, is in neither set and is left out.
func (spelling *reachSpelling) methodsInSet(named *types.Named) {
	methodSet := types.NewMethodSet(named)
	fields := map[string]bool{}
	if structure, isStruct := named.Underlying().(*types.Struct); isStruct {
		for index := range structure.NumFields() {
			fields[structure.Field(index).Name()] = true
		}
	}
	for index := range named.NumMethods() {
		method := named.Method(index)
		signature, isSignature := method.Type().(*types.Signature)
		if !isSignature || signature.Recv() == nil || method.Name() == "_" || fields[method.Name()] {
			continue
		}
		_, byPointer := types.Unalias(signature.Recv().Type()).(*types.Pointer)
		if inSet := methodSet.Lookup(method.Pkg(), method.Name()) != nil; inSet == byPointer {
			spelling.disagree("%s.%s: in its type's method set %t, and its receiver is a pointer %t", named.Obj().Name(),
				method.Name(), inSet, byPointer)
		}
	}
}

func (spelling *reachSpelling) position(pos token.Pos) string {
	if !pos.IsValid() {
		return "-"
	}
	return spelling.fileSet.Position(pos).String()
}

// member spells the package-level name.
func (spelling *reachSpelling) member(name string) string {
	spelling.builder.Reset()
	spelling.seen = map[types.Type]bool{}
	found := spelling.checked.Scope().Lookup(name)
	spelling.builder.WriteString(spellMember(found))
	if found == nil {
		return spelling.builder.String()
	}
	spelling.object(found)
	if named, isNamed := types.Unalias(found.Type()).(*types.Named); isNamed && named.TypeParams().Len() > 0 {
		arguments := make([]types.Type, named.TypeParams().Len())
		for index := range arguments {
			arguments[index] = types.Typ[types.Int]
		}
		for _, context := range []*types.Context{nil, spelling.context} {
			instance, instanceError := types.Instantiate(context, named, arguments, false)
			fmt.Fprintf(&spelling.builder, "\n instance, in a shared context %t, error %v", context != nil, instanceError)
			if instanceError == nil {
				spelling.typed(instance)
			}
		}
	}
	if named, isNamed := types.Unalias(found.Type()).(*types.Named); isNamed && named.TypeParams().Len() == 0 {
		// Every interface of the package the type implements, itself or through a pointer.
		for _, other := range spelling.names {
			candidate, isTypeName := spelling.checked.Scope().Lookup(other).(*types.TypeName)
			if !isTypeName {
				continue
			}
			candidateNamed, candidateIsNamed := types.Unalias(candidate.Type()).(*types.Named)
			if !candidateIsNamed || candidateNamed.TypeParams().Len() > 0 {
				continue
			}
			if iface, isInterface := candidate.Type().Underlying().(*types.Interface); isInterface {
				fmt.Fprintf(&spelling.builder, "\n implements %s: %t, by pointer %t", other,
					types.Implements(named, iface), types.Implements(types.NewPointer(named), iface))
			}
		}
	}
	return spelling.builder.String()
}

// held spells an object the reader holds already, whether or not the build reading declares it.
func (spelling *reachSpelling) held(object types.Object) string {
	spelling.builder.Reset()
	spelling.seen = map[types.Type]bool{}
	if constantObject, isConstant := object.(*types.Const); isConstant {
		spelling.builder.WriteString(constantObject.Val().ExactString())
	}
	spelling.object(object)
	return spelling.builder.String()
}

// heldAspects are the reads of one property each of an object the reader holds already: its type, a constant's
// value, a function's signature, full name and scope, and a type name's underlying type, its methods' names by index,
// and its interface's methods' names.
func heldAspects(object types.Object) map[string]func() string {
	aspects := map[string]func() string{"its type": func() string { return canonical.Spell(object.Type()) }}
	if function, isFunction := object.(*types.Func); isFunction && function.Type() != nil {
		aspects["its signature"] = func() string { return spellSignature(function.Signature()) }
		aspects["its full name"] = function.FullName
		aspects["its scope"] = func() string {
			scope := function.Scope()
			if scope == nil {
				return "no scope"
			}
			return fmt.Sprintf("%d %s", scope.Pos(), strings.Join(scope.Names(), " "))
		}
	}
	if constantObject, isConstant := object.(*types.Const); isConstant {
		aspects["its value"] = func() string { return constantObject.Val().ExactString() }
	}
	if _, isTypeName := object.(*types.TypeName); !isTypeName {
		return aspects
	}
	aspects["its underlying type"] = func() string { return canonical.Spell(object.Type().Underlying()) }
	aspects["its methods by index"] = func() string {
		named, isNamed := types.Unalias(object.Type()).(*types.Named)
		if !isNamed {
			return "not a named type"
		}
		var names []string
		for index := range named.NumMethods() {
			names = append(names, named.Method(index).Name())
		}
		return strings.Join(names, " ")
	}
	aspects["its type set's methods"] = func() string {
		iface, isInterface := object.Type().Underlying().(*types.Interface)
		if !isInterface {
			return "not an interface"
		}
		var names []string
		for index := range iface.NumMethods() {
			names = append(names, iface.Method(index).Name())
		}
		return fmt.Sprintf("%s, comparable %t", strings.Join(names, " "), iface.IsComparable())
	}
	return aspects
}

func (spelling *reachSpelling) object(object types.Object) {
	fmt.Fprintf(&spelling.builder, "\n object %T %s at %s: %s", object, object.Name(), spelling.position(object.Pos()),
		types.ObjectString(object, nil))
	if typeName, isTypeName := object.(*types.TypeName); isTypeName {
		fmt.Fprintf(&spelling.builder, " alias %t", typeName.IsAlias())
	}
	if function, isFunction := object.(*types.Func); isFunction {
		spelling.function(function)
	}
	spelling.typed(object.Type())
}

func (spelling *reachSpelling) method(method *types.Func) {
	spelling.selectors[method.Name()] = true
	fmt.Fprintf(&spelling.builder, "\n method %s at %s: %s", method.FullName(), spelling.position(method.Pos()),
		canonical.Spell(method.Type()))
	spelling.function(method)
	spelling.typed(method.Type())
}

func (spelling *reachSpelling) tuple(label string, tuple *types.Tuple) {
	for index := range tuple.Len() {
		variable := tuple.At(index)
		fmt.Fprintf(&spelling.builder, "\n %s %d %s at %s", label, index, variable.Name(), spelling.position(variable.Pos()))
		spelling.typed(variable.Type())
	}
}

func (spelling *reachSpelling) typed(typed types.Type) {
	if typed == nil {
		spelling.builder.WriteString("\n type <nil>")
		return
	}
	fmt.Fprintf(&spelling.builder, "\n type %T %s", typed, canonical.Spell(typed))
	switch typed.(type) {
	case *types.Named, *types.Struct, *types.Interface, *types.Signature, *types.TypeParam:
		if spelling.seen[typed] {
			return
		}
		spelling.seen[typed] = true
	}
	switch typed := typed.(type) {
	case *types.Alias:
		spelling.typed(types.Unalias(typed))
	case *types.Named:
		parameters := typed.TypeParams()
		for index := range parameters.Len() {
			fmt.Fprintf(&spelling.builder, "\n type parameter %s", parameters.At(index).Obj().Name())
			spelling.typed(parameters.At(index).Constraint())
		}
		spelling.builder.WriteString("\n underlying")
		spelling.typed(typed.Underlying())
		fmt.Fprintf(&spelling.builder, "\n %d methods", typed.NumMethods())
		for index := range typed.NumMethods() {
			spelling.method(typed.Method(index))
		}
		fmt.Fprintf(&spelling.builder, "\n method set %s, pointer's %s", spellMethodSet(typed),
			spellMethodSet(types.NewPointer(typed)))
		spelling.methodsInSet(typed)
		selectors := make([]string, 0, len(spelling.selectors))
		for selector := range spelling.selectors {
			selectors = append(selectors, selector)
		}
		slices.Sort(selectors)
		for _, selector := range selectors {
			selected, index, indirect := types.LookupFieldOrMethod(typed, true, spelling.checked, selector)
			if selected == nil {
				fmt.Fprintf(&spelling.builder, "\n selector %s: none", selector)
				continue
			}
			fmt.Fprintf(&spelling.builder, "\n selector %s: %T %s at %s, %v %t", selector, selected,
				canonical.Spell(selected.Type()), spelling.position(selected.Pos()), index, indirect)
		}
	case *types.Pointer:
		spelling.typed(typed.Elem())
	case *types.Slice:
		spelling.typed(typed.Elem())
	case *types.Array:
		spelling.typed(typed.Elem())
	case *types.Chan:
		spelling.typed(typed.Elem())
	case *types.Map:
		spelling.typed(typed.Key())
		spelling.typed(typed.Elem())
	case *types.Struct:
		for index := range typed.NumFields() {
			field := typed.Field(index)
			spelling.selectors[field.Name()] = true
			fmt.Fprintf(&spelling.builder, "\n field %d %s at %s embedded %t tag %q", index, field.Name(),
				spelling.position(field.Pos()), field.Embedded(), typed.Tag(index))
			spelling.typed(field.Type())
		}
	case *types.Signature:
		if receiver := typed.Recv(); receiver != nil {
			fmt.Fprintf(&spelling.builder, "\n receiver %s %s at %s", receiver.Name(), canonical.Spell(receiver.Type()),
				spelling.position(receiver.Pos()))
		}
		parameters := typed.TypeParams()
		for index := range parameters.Len() {
			fmt.Fprintf(&spelling.builder, "\n type parameter %s", parameters.At(index).Obj().Name())
			spelling.typed(parameters.At(index).Constraint())
		}
		spelling.tuple("parameter", typed.Params())
		spelling.tuple("result", typed.Results())
	case *types.Interface:
		fmt.Fprintf(&spelling.builder, "\n interface comparable %t, method set %t, empty %t", typed.IsComparable(),
			typed.IsMethodSet(), typed.Empty())
		fmt.Fprintf(&spelling.builder, ", %d explicit methods, %d embedded, %d methods", typed.NumExplicitMethods(),
			typed.NumEmbeddeds(), typed.NumMethods())
		for index := range typed.NumExplicitMethods() {
			spelling.method(typed.ExplicitMethod(index))
		}
		for index := range typed.NumEmbeddeds() {
			spelling.typed(typed.EmbeddedType(index))
		}
		for index := range typed.NumMethods() {
			spelling.method(typed.Method(index))
		}
	case *types.Union:
		for index := range typed.Len() {
			fmt.Fprintf(&spelling.builder, "\n term %d tilde %t", index, typed.Term(index).Tilde())
			spelling.typed(typed.Term(index).Type())
		}
	case *types.TypeParam:
		fmt.Fprintf(&spelling.builder, "\n type parameter %s index %d", typed.Obj().Name(), typed.Index())
		spelling.typed(typed.Constraint())
		spelling.typed(typed.Underlying())
	case *types.Tuple:
		spelling.tuple("element", typed)
	}
}

// agreementTally counts what the property test met, so it can tell it was no empty proof.
type agreementTally struct {
	reads    int // reads gathered
	every    int // of them, those every build agreed on
	narrowed int // those some build was dropped from
	compared int // builds read again and compared
	// lost counts the builds an agreement dropped that in fact spelled alike, which narrowing too much allows.
	lost int
}

// requireAgreement is the property Agreeing promises, over everything a reader can read of checked: gathered as each
// build, every build in the agreement, read again under InBuild, spells exactly what the gathering read spelled. Each
// package-level name of any build is gathered alone, and the scope's names, its length and the imports each alone. It
// runs twice, so the second pass reads what the first left kept (a type set, a resolved underlying type, a type's
// methods by build).
func requireAgreement(test *testing.T, label string, fileSet *token.FileSet, checked *types.Package,
	tally *agreementTally,
) {
	test.Helper()
	spelling := &reachSpelling{fileSet: fileSet, checked: checked, context: types.NewContext(),
		selectors: map[string]bool{}, disagreements: map[string]bool{}}
	types.Outside(func() { spelling.names = checked.Scope().Names() })
	builds := []types.Mask{buildDarwin, buildLinux, buildWindows}
	// Every build's selectors are met before any spelling is compared, so each spelling looks up the same ones.
	for _, build := range builds {
		types.InBuild(build, func() {
			for _, name := range spelling.names {
				spelling.member(name)
			}
		})
	}
	reads := []struct {
		name string
		read func() string
	}{
		{"scope names", func() string { return strings.Join(checked.Scope().Names(), " ") }},
		{"scope length", func() string { return fmt.Sprint(checked.Scope().Len()) }},
		{"imports", func() string {
			var paths []string
			for _, imported := range checked.Imports() {
				paths = append(paths, imported.Path())
				for _, indirect := range imported.Imports() {
					paths = append(paths, imported.Path()+" imports "+indirect.Path())
				}
			}
			return strings.Join(paths, " ")
		}},
	}
	for _, name := range spelling.names {
		reads = append(reads, struct {
			name string
			read func() string
		}{"member " + name, func() string { return spelling.member(name) }})
		// The name's first object and the objects kept beside it, held from before and read whether or not the build
		// reading declares them, as a reader holds an object another build's read gave it.
		first := lookupInEveryBuild(checked.Scope(), name)
		for index, held := range append([]types.Object{first}, types.AlternatesOf(first)...) {
			reads = append(reads, struct {
				name string
				read func() string
			}{fmt.Sprintf("held %s %d", name, index), func() string { return spelling.held(held) }})
			// Each property alone as well, so a build one read should drop is dropped by no other read beside it.
			for aspect, read := range heldAspects(held) {
				reads = append(reads, struct {
					name string
					read func() string
				}{fmt.Sprintf("held %s %d, %s alone", name, index, aspect), read})
			}
		}
	}
	for pass := range 2 {
		for _, build := range builds {
			for _, each := range reads {
				var spelled string
				agreeing := types.Agreeing(build, everyBuild, func() { spelled = each.read() })
				tally.reads++
				if agreeing == everyBuild {
					tally.every++
				} else {
					tally.narrowed++
				}
				if agreeing&build == 0 || agreeing&^everyBuild != 0 {
					test.Errorf("%s, pass %d, %s read as %03b: agreeing %03b, which must hold the build and no build not asked about",
						label, pass, each.name, build, agreeing)
				}
				for _, other := range builds {
					var again string
					types.InBuild(other, func() { again = each.read() })
					if agreeing&other == 0 {
						if again == spelled {
							tally.lost++
						}
						continue
					}
					tally.compared++
					if again != spelled {
						test.Errorf("%s, pass %d, %s read as %03b: Agreeing kept %03b (agreeing %03b), which reads otherwise."+
							"\nas %03b:\n%s\nas %03b:\n%s", label, pass, each.name, build, other, agreeing, build, spelled,
							other, again)
					}
				}
			}
		}
	}
	disagreements := slices.Sorted(maps.Keys(spelling.disagreements))
	for _, disagreement := range disagreements {
		test.Errorf("%s: %s", label, disagreement)
	}
}

// TestVariationalAgreeingBuildsReadAlike is Agreeing's contract, over every projection fixture, agreeingFiles, the
// concurrency fixture's dependency and a package importing it, and a package made as the merging importer makes one:
// every build Agreeing keeps reads everything exactly as the build it gathered under read it.
func TestVariationalAgreeingBuildsReadAlike(test *testing.T) {
	test.Parallel()
	tally := &agreementTally{}
	fixtures := map[string][]fixtureFile{"agreeing": agreeingFiles}
	for name, files := range projectionFixtures {
		fixtures[name] = files
	}
	var names []string
	for name := range fixtures {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fileSet := token.NewFileSet()
		parsed := parseFixture(test, fileSet, fixtures[name])
		// A fixture may report errors, as the abandoned errors fixture does; what was checked is read all the same.
		checked, _ := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
			Importer: fixtureImporter{}, Error: func(error) {}}).Check("fork", fileSet, parsed.files, &types.Info{})
		before := *tally
		requireAgreement(test, name, fileSet, checked, tally)
		if name == "hard" || name == "agreeing" {
			if tally.every == before.every || tally.narrowed == before.narrowed {
				test.Errorf("%s: %d reads agreed in every build and %d in fewer, and the fixture holds both kinds", name,
					tally.every-before.every, tally.narrowed-before.narrowed)
			}
		}
	}

	fileSet := token.NewFileSet()
	dependency := parseFixture(test, fileSet, concurrencyDependency)
	dependencyPackage, dependencyError := (&types.Config{Variation: &types.Variation{Builds: everyBuild,
		Files: dependency.masks}, Importer: fixtureImporter{}}).Check("dep", fileSet, dependency.files, &types.Info{})
	if dependencyError != nil {
		test.Fatalf("variational check of dep: %v", dependencyError)
	}
	user := parseFixture(test, fileSet, concurrencyUser(0))
	userPackage, userError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: user.masks},
		Importer: packageImporter{"dep": dependencyPackage}}).Check("user0", fileSet, user.files, &types.Info{})
	if userError != nil {
		test.Fatalf("variational check of user0: %v", userError)
	}
	before := *tally
	requireAgreement(test, "concurrency dependency", fileSet, dependencyPackage, tally)
	requireAgreement(test, "concurrency user", fileSet, userPackage, tally)
	if tally.every == before.every || tally.narrowed == before.narrowed {
		test.Errorf("concurrency fixture: %d reads agreed in every build and %d in fewer, and it holds both kinds",
			tally.every-before.every, tally.narrowed-before.narrowed)
	}
	requireAgreement(test, "merged", token.NewFileSet(), mergedPackage(), tally)
	test.Logf("%d reads gathered: %d agreed in every build, %d in fewer; %d builds read again and compared; "+
		"%d builds dropped that spelled alike", tally.reads, tally.every, tally.narrowed, tally.compared, tally.lost)
}
