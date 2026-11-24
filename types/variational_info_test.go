package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// projectionFixtures are the fixtures whose projection to each build is compared with that build's plain check.
var projectionFixtures = map[string][]fixtureFile{
	"spike":              fixtureFiles,
	"hard":               hardFiles,
	"usage":              usageFiles,
	"methods":            methodOrderFiles,
	"abandoned errors":   abandonedErrorFiles,
	"alias targets":      aliasTargetFiles,
	"generic types":      genericDefinitionFiles,
	"kinds":              kindFiles,
	"scopes":             scopeFiles,
	"init order":         initOrderFiles,
	"imports":            importFiles,
	"interfaces":         interfaceFiles,
	"sizes":              sizeFiles,
	"method signatures":  methodSignatureFiles,
	"pairs":              pairFiles,
	"defined over":       definedOverFiles,
	"field methods":      fieldMethodFiles,
	"closures":           closureFiles,
	"literal parameters": literalParameterFiles,
	"imported goos":      importedGOOSFiles,
	"accessors":          accessorFiles,
	"declared again":     declaredFiles,
	"receivers":          receiverFiles,
	"alike":              alikeFiles,
}

// TestVariationalInfoProjectsToEachBuild checks each fixture once for every build with every Info map, then requires
// each build's projection of them (the plain maps, which hold the host's answers, with that build's overlay laid over
// them) to be that build's plain check's maps, entry for entry. Objects compare by kind, name, their type in the build
// as the census spells it (canonical: a signature by its parameters' types, not their names)
// and where they're declared. A declaration in a file every build compiles compares by its exact position; one in a
// file only some builds compile compares only as being in such a file, since a name declared in each build's own file
// is one object for all of them, at its first declaration, and declarations alike across builds fold into one, as the
// model has it (variational.go). Selections compare by kind, index, indirection and object, and instances by their
// type arguments and type, expressions' types and values as the census spells them, scopes by the names they declare
// and the object each name is, the initialization order initializer for initializer, each by its variables and its
// expression's place, and the package's imports by path, in order. The variational check's diagnostics whose builds
// hold each build are that build's plain diagnostics. And each build's projection names only objects it declares
// (requireDeclaredObjects), which spellings can't tell: a use of another run's object spells as the build's own.
func TestVariationalInfoProjectsToEachBuild(test *testing.T) {
	test.Parallel()
	for name, fixtures := range projectionFixtures {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, fixtures)
			variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
			info := fullInfo()
			// A fixture may report errors, as the abandoned errors fixture does: they're compared with the plain
			// checks' rather than refused.
			var published []types.Error
			plainDiagnostics := map[types.Mask][]string{}
			checked, checkError := (&types.Config{Variation: variation, Importer: fixtureImporter{},
				Error: func(diagnostic error) { published = append(published, diagnostic.(types.Error)) }}).Check(
				"fork", fileSet, parsed.files, info)
			if checkError != nil && len(published) == 0 {
				test.Fatalf("variational check: %v", checkError)
			}
			overlaid := 0
			spelling := objectSpelling{fileSet: fileSet, shared: map[string]bool{}}
			for file, builds := range parsed.masks {
				spelling.shared[fileSet.Position(file.Pos()).Filename] = builds == everyBuild
			}
			for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				spelling.build = build
				plain := fullInfo()
				plainConfiguration := &types.Config{Importer: fixtureImporter{build}, Error: func(diagnostic error) {
					plainDiagnostics[build] = append(plainDiagnostics[build], diagnostic.Error())
				}}
				plainPackage, checkError := plainConfiguration.Check("fork", fileSet, parsed.filesIn(build), plain)
				if checkError != nil && len(plainDiagnostics[build]) == 0 {
					test.Fatalf("plain check of build %03b: %v", build, checkError)
				}
				if len(plain.Defs) == 0 || len(plain.Uses) == 0 {
					test.Fatalf("build %03b: the plain check recorded no Defs or no Uses", build)
				}
				overlays := types.BuildOverlays{}
				if index > 0 {
					overlays = variation.Overlays[index]
					overlaid += overlays.Len()
				}
				label := fmt.Sprintf("build %03b", build)
				requireDeclaredObjects(test, label+" plain", fileSet, plain.Types, plain.Defs, plain.Uses,
					plain.Implicits, plain.Selections, plain.Scopes)
				requireDeclaredObjects(test, label, fileSet, project(info.Types, overlays.Types),
					project(info.Defs, overlays.Defs), project(info.Uses, overlays.Uses),
					project(info.Implicits, overlays.Implicits), project(info.Selections, overlays.Selections),
					project(info.Scopes, overlays.Scopes))
				requireUsedParameters(test, label, fileSet, project(info.Defs, overlays.Defs),
					project(info.Uses, overlays.Uses), plain.Defs, plain.Uses)
				// What the projection answers is read as the build has it: one interface's type set differs by build.
				projectedDefs, projectedUses := project(info.Defs, overlays.Defs), project(info.Uses, overlays.Uses)
				if build == buildDarwin {
					// The host's projection read with no state, as a host lint reads it.
					requirePositionsInBuild(test, label+" with no state", fileSet, parsed, build, projectedDefs,
						projectedUses)
				}
				types.InBuild(build, func() {
					requirePositionsInBuild(test, label, fileSet, parsed, build, projectedDefs, projectedUses)
					compareSpelled(test, label+" Defs", spellObjects(spelling, project(info.Defs, overlays.Defs)),
						spellObjects(spelling, plain.Defs))
					compareSpelled(test, label+" Uses", spellObjects(spelling, project(info.Uses, overlays.Uses)),
						spellObjects(spelling, plain.Uses))
					compareSpelled(test, label+" Implicits",
						spellObjects(spelling, project(info.Implicits, overlays.Implicits)),
						spellObjects(spelling, plain.Implicits))
					compareSpelled(test, label+" Selections",
						spelling.selections(project(info.Selections, overlays.Selections)),
						spelling.selections(plain.Selections))
					compareSpelled(test, label+" Instances",
						spellInstances(fileSet, project(info.Instances, overlays.Instances)),
						spellInstances(fileSet, plain.Instances))
					compareTypes(test, fileSet, label+" Types", project(info.Types, overlays.Types), plain.Types)
					compareSpelled(test, label+" Scopes", spelling.scopes(project(info.Scopes, overlays.Scopes)),
						spelling.scopes(plain.Scopes))
					initOrder := info.InitOrder
					if overlays.InitOrder != nil {
						initOrder = overlays.InitOrder
					}
					if got, want := spelling.initOrder(initOrder), spelling.initOrder(plain.InitOrder); got != want {
						test.Errorf("%s InitOrder: projected\n\t%s\nplain\n\t%s", label, got, want)
					}
					imports := checked.Imports()
					if overlays.Imports != nil {
						imports = overlays.Imports
					}
					if got, want := importPaths(imports), importPaths(plainPackage.Imports()); got != want {
						test.Errorf("%s Imports: projected %s, plain %s", label, got, want)
					}
					// Read as the build, the package's own Imports are the build's, with no overlay.
					if got, want := importPaths(checked.Imports()), importPaths(plainPackage.Imports()); got != want {
						test.Errorf("%s Imports read in the build: %s, plain %s", label, got, want)
					}
				})
			}
			compareDiagnostics(test, published, plainDiagnostics)
			// Every fixture's builds declare things in files of their own, so some answers differ from the host's.
			if overlaid == 0 {
				test.Error("no build's overlay holds anything")
			}
			test.Logf("overlay entries: linux %d, windows %d", variation.Overlays[1].Len(), variation.Overlays[2].Len())
		})
	}
}

// methodOrderFiles declare a type's methods in each build's own file, in a different order on windows, with one of
// them on linux and darwin alone: a selection's method index is the method's place among its type's methods as each
// build declares them, which the merged type's one list of methods doesn't give.
var methodOrderFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type WaitStatus uint32

func Status(status WaitStatus) bool { return status.Signaled() && status.Stopped() }

func Pointer(status *WaitStatus) bool { return status.Stopped() }
`},
	{"status_unix.go", buildLinux | buildDarwin, `package fork

func (WaitStatus) Exited() bool { return false }

func (WaitStatus) Signaled() bool { return true }

func (WaitStatus) Stopped() bool { return false }
`},
	{"status_windows.go", buildWindows, `package fork

func (WaitStatus) Stopped() bool { return false }

func (WaitStatus) Signaled() bool { return true }
`},
}

// scopeFiles split a body that declares a local in the function's own scope and another in a block, each of a type
// that differs by build: each build's scopes hold its own locals, as its plain check's do, and the file scope of a
// file only some builds compile is in those builds alone.
var scopeFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Scoped() int {
	width := Word(1)
	if width > 0 {
		inner := Word(2)
		return int(inner)
	}
	return int(width)
}
`},
	{"word_unix.go", buildDarwin | buildLinux, `package fork

import "unsafe"

type Word = uint64

var _ = unsafe.Sizeof(0)
`},
	{"word_windows.go", buildWindows, "package fork\n\ntype Word = uint32\n"},
}

// initOrderFiles declare variables whose initializers depend on each other differently by build: A reads B on darwin
// and linux and nothing on windows, Width reads C or B, Picked reads a function whose body reads a constant on darwin
// and linux and a variable that reads D on windows, and OnlyWindows exists on windows alone. Routed reads a function
// whose body calls the method of each build's own Slot: First on darwin and linux, Second on windows, though every
// build declares both, so the edge to First holds on darwin and linux alone. Each build's order is its plain check's.
var initOrderFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

var D = A + E

var B = C

var C = 2

var Picked = pick()

func pick() int { return Value }

var Sized = Width + 1

var Routed = route()

var First = Second + 1

var Second = 2

type slotUnix struct{}

func (slotUnix) value() int { return First }

type slotWindows struct{}

func (slotWindows) value() int { return Second }

func route() int {
	var slot Slot
	return slot.value()
}
`},
	{"order_unix.go", buildDarwin | buildLinux, `package fork

var E = 1

var A = B + 1

const Value = 1

var Width = C

type Slot = slotUnix
`},
	{"order_windows.go", buildWindows, `package fork

var A = 1

var OnlyWindows = D

var Value = D + 1

var E = Width

var Width = B

type Slot = slotWindows
`},
}

// importFiles import a package in a file every build compiles and others in each build's own file, in another order
// on windows, which alone imports delta: each build's imports are its plain check's, in the order its files meet them.
var importFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

import _ "example/one"

var Value = 1

var Other = Value
`},
	{"imports_unix.go", buildDarwin | buildLinux, `package fork

import (
	_ "example/beta"
	_ "example/gamma"
)
`},
	{"imports_windows.go", buildWindows, `package fork

import (
	_ "example/gamma"
	_ "example/delta"
	_ "example/beta"
)
`},
}

// closureFiles declare a function whose body splits by build, at a constant whose value differs by build, declaring
// a closure that reads the constant, and using it before and after the split point and inside nested func literals.
var closureFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Runner() int {
	run := func() int { return N }
	first := run()
	inner := func() int {
		nested := func() int { return run() + N }
		return nested() + run()
	}
	if N > 1 {
		first += run()
	}
	return first + inner() + run()
}

func apply(work func(int) int, value int) int { return work(value) }

func Cases() (total int) {
	for _, value := range []int{1, 2} {
		run := func(each int) int { return each * N }
		total += apply(run, value)
		defer func() { total += run(value) }()
	}
	check := func() bool { return total > N }
	if !check() {
		total++
	}
	return total
}
`},
	{"n_darwin.go", buildDarwin, "package fork\n\nconst N = 1\n"},
	{"n_linux.go", buildLinux, "package fork\n\nconst N = 2\n"},
	{"n_windows.go", buildWindows, "package fork\n\nconst N = 3\n"},
}

// literalParameterFiles are cohere-dev's main_test.go's shape: a body that opens reading a string constant which
// differs in every build, so it splits into a run for each, and then declares func literals whose parameters only
// their own bodies use, a nested literal inside one, and a deferred literal.
var literalParameterFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Body() (string, error) {
	if GOOS == "windows" {
		return "", nil
	}
	run := func(arguments ...string) (string, error) {
		joined := ""
		for _, argument := range arguments {
			joined += argument
		}
		check := func(value string) bool { return value != "" }
		if !check(joined) {
			return "", nil
		}
		return joined, nil
	}
	defer func(reason string) { _ = reason }("done")
	waitFor := func(what string, done func() bool) bool { return what != "" && done() }
	first, _ := run("a")
	if !waitFor(first, func() bool { return true }) {
		return "", nil
	}
	return run(first, "b")
}
`},
	{"goos_darwin.go", buildDarwin, "package fork\n\nconst GOOS = \"darwin\"\n"},
	{"goos_linux.go", buildLinux, "package fork\n\nconst GOOS = \"linux\"\n"},
	{"goos_windows.go", buildWindows, "package fork\n\nconst GOOS = \"windows\"\n"},
}

// importedGOOSFiles are literalParameterFiles with the split at an imported constant, runtime.GOOS, whose value the
// merging importer gives by build, as cohere-dev's main_test.go has it.
var importedGOOSFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

import "example/runtime"

func Body() (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	run := func(arguments ...string) (string, error) {
		joined := ""
		for _, argument := range arguments {
			joined += argument
		}
		check := func(value string) bool { return value != "" }
		if !check(joined) {
			return "", nil
		}
		return joined, nil
	}
	defer func(reason string) { _ = reason }("done")
	waitFor := func(what string, done func() bool) bool { return what != "" && done() }
	first, _ := run("a")
	if !waitFor(first, func() bool { return true }) {
		return "", nil
	}
	return run(first, "b")
}
`},
	{"build_darwin.go", buildDarwin, "package fork\n"},
	{"build_linux.go", buildLinux, "package fork\n"},
	{"build_windows.go", buildWindows, "package fork\n"},
}

// requirePositionsInBuild requires each object of the package that the build's projected Defs and Uses name to be at
// a position in a file the build compiles, read as the reader reads it (under InBuild, or with no state for the host):
// a name declared in each build's own file is one object, and a build's reader finds the file of its position.
func requirePositionsInBuild(test *testing.T, label string, fileSet *token.FileSet, parsed parsedFixture,
	build types.Mask, defs map[*ast.Ident]types.Object, uses map[*ast.Ident]types.Object,
) {
	test.Helper()
	compiled := map[string]bool{}
	for _, file := range parsed.filesIn(build) {
		compiled[fileSet.Position(file.Pos()).Filename] = true
	}
	reported := map[types.Object]bool{}
	for _, objects := range []map[*ast.Ident]types.Object{defs, uses} {
		for ident, object := range objects {
			if object == nil || object.Pkg() == nil || object.Pkg().Path() != "fork" || !object.Pos().IsValid() ||
				reported[object] {
				continue
			}
			if position := fileSet.Position(object.Pos()); !compiled[position.Filename] {
				reported[object] = true
				test.Errorf("%s: %T %s, named at %s, is at %s, in a file the build doesn't compile", label, object,
					object.Name(), fileSet.Position(ident.Pos()), position)
			}
		}
	}
}

// requireUsedParameters requires each parameter the build's projected Defs declare, when the build's plain check
// uses the parameter declared at its place, to be used in the projection too, as an unused-parameter report reads it.
func requireUsedParameters(test *testing.T, label string, fileSet *token.FileSet, defs map[*ast.Ident]types.Object,
	uses map[*ast.Ident]types.Object, plainDefs map[*ast.Ident]types.Object, plainUses map[*ast.Ident]types.Object,
) {
	test.Helper()
	plainUsed := map[string]bool{}
	for _, object := range plainUses {
		plainUsed[fileSet.Position(object.Pos()).String()] = true
	}
	used := map[types.Object]bool{}
	for _, object := range uses {
		used[object] = true
	}
	for ident, object := range defs {
		variable, isVariable := object.(*types.Var)
		if !isVariable || variable.Kind() != types.ParamVar || used[object] {
			continue
		}
		if place := fileSet.Position(ident.Pos()).String(); plainUsed[place] {
			test.Errorf("%s: parameter %s declared at %s has no use, and the plain check uses it", label,
				variable.Name(), place)
		}
	}
}

// requireDeclaredObjects requires each object that uses, implicits, selections and scopes name, and each parameter
// and result of a func literal's signature in typesOf, when it is declared in the package's own files, to be one that
// defs or implicits declare: a build's answers name its own objects, never another run's. A field or method of an
// instance is its origin's. An unused-parameter report reads a literal's parameters from its type.
func requireDeclaredObjects(test *testing.T, label string, fileSet *token.FileSet,
	typesOf map[ast.Expr]types.TypeAndValue, defs map[*ast.Ident]types.Object, uses map[*ast.Ident]types.Object,
	implicits map[ast.Node]types.Object, selections map[*ast.SelectorExpr]*types.Selection,
	scopes map[ast.Node]*types.Scope,
) {
	test.Helper()
	declared := map[types.Object]bool{}
	for _, object := range defs {
		declared[object] = true
	}
	for _, object := range implicits {
		declared[object] = true
	}
	require := func(where string, object types.Object) {
		switch origin := object.(type) {
		case *types.Var:
			object = origin.Origin()
		case *types.Func:
			object = origin.Origin()
		}
		if object == nil || object.Pkg() == nil || object.Pkg().Path() != "fork" || !object.Pos().IsValid() ||
			declared[object] {
			return
		}
		test.Errorf("%s: %s names %s %s declared at %s, which no Defs or Implicits entry declares", label, where,
			fmt.Sprintf("%T", object), object.Name(), fileSet.Position(object.Pos()))
	}
	for ident, object := range uses {
		require("the use at "+fileSet.Position(ident.Pos()).String(), object)
	}
	for node, object := range implicits {
		require("the implicit at "+fileSet.Position(node.Pos()).String(), object)
	}
	for selector, selection := range selections {
		require("the selection at "+fileSet.Position(selector.Pos()).String(), selection.Obj())
	}
	for node, scope := range scopes {
		for _, name := range scope.Names() {
			require("the scope at "+fileSet.Position(node.Pos()).String(), scope.Lookup(name))
		}
	}
	for expression, answer := range typesOf {
		if _, isLiteral := expression.(*ast.FuncLit); !isLiteral {
			continue
		}
		signature, isSignature := answer.Type.(*types.Signature)
		if !isSignature {
			continue
		}
		for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
			for index := range tuple.Len() {
				require("the type of the func literal at "+fileSet.Position(expression.Pos()).String(),
					tuple.At(index))
			}
		}
	}
}

// fullInfo is an Info with every map that varies by build.
func fullInfo() *types.Info {
	return &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
		Scopes:     map[ast.Node]*types.Scope{},
	}
}

// project is plain with overlay laid over it: a build's whole answers.
func project[K comparable, V any](plain map[K]V, overlay types.Overlay[K, V]) map[K]V {
	projected := make(map[K]V, len(plain)+len(overlay.Set))
	for key, value := range plain {
		if !overlay.Removed[key] {
			projected[key] = value
		}
	}
	for key, value := range overlay.Set {
		projected[key] = value
	}
	return projected
}

// nodeKey names a node by where it is and what it is.
func nodeKey(fileSet *token.FileSet, node ast.Node) string {
	return fmt.Sprintf("%s-%s %T", fileSet.Position(node.Pos()), fileSet.Position(node.End()), node)
}

// objectSpelling spells objects as one build's plain check and its projection can be compared.
type objectSpelling struct {
	fileSet *token.FileSet
	shared  map[string]bool // whether every build compiles the file of each name
	build   types.Mask
}

// object spells an object by its kind, name, type in the build and where it's declared (see the test's comment). A
// nil object (a package clause's name) spells as nil.
func (spelling objectSpelling) object(object types.Object) string {
	if object == nil {
		return "nil"
	}
	position := spelling.fileSet.Position(object.Pos())
	where := "in a file of some builds"
	if spelling.shared[position.Filename] || !position.IsValid() {
		where = position.String()
	}
	return fmt.Sprintf("%T %s %s %s", object, object.Name(), canonical.Spell(typeIn(object, spelling.build)),
		where)
}

// typeIn is object's type in build: its answer for build when it has answers by build.
func typeIn(object types.Object, build types.Mask) types.Type {
	for _, variant := range types.VariantsOf(object) {
		if variant.Builds&build != 0 {
			return variant.Type
		}
	}
	return object.Type()
}

// spellObjects spells each of objects by its node.
func spellObjects[K interface {
	comparable
	ast.Node
}](spelling objectSpelling, objects map[K]types.Object) map[string]string {
	spelled := make(map[string]string, len(objects))
	for node, object := range objects {
		spelled[nodeKey(spelling.fileSet, node)] = spelling.object(object)
	}
	return spelled
}

func (spelling objectSpelling) selections(selections map[*ast.SelectorExpr]*types.Selection) map[string]string {
	spelled := make(map[string]string, len(selections))
	for node, selection := range selections {
		spelled[nodeKey(spelling.fileSet, node)] = fmt.Sprintf("%d %v %t %s", selection.Kind(), selection.Index(),
			selection.Indirect(), spelling.object(selection.Obj()))
	}
	return spelled
}

// scopes spells each scope by the names it declares and the object each name is.
func (spelling objectSpelling) scopes(scopes map[ast.Node]*types.Scope) map[string]string {
	spelled := make(map[string]string, len(scopes))
	for node, scope := range scopes {
		var names []string
		for _, name := range scope.Names() {
			names = append(names, name+": "+spelling.object(scope.Lookup(name)))
		}
		spelled[nodeKey(spelling.fileSet, node)] = strings.Join(names, "; ")
	}
	return spelled
}

// initOrder spells an initialization order, each initializer by its variables and its expression's place.
func (spelling objectSpelling) initOrder(order []*types.Initializer) string {
	var initializers []string
	for _, initializer := range order {
		var variables []string
		for _, variable := range initializer.Lhs {
			variables = append(variables, spelling.object(variable))
		}
		initializers = append(initializers, fmt.Sprintf("%s = %s", strings.Join(variables, ", "),
			nodeKey(spelling.fileSet, initializer.Rhs)))
	}
	return strings.Join(initializers, "\n\t")
}

// importPaths spells a package's imports by their paths, in order.
func importPaths(imports []*types.Package) string {
	var paths []string
	for _, imported := range imports {
		paths = append(paths, imported.Path())
	}
	return "[" + strings.Join(paths, " ") + "]"
}

func spellInstances(fileSet *token.FileSet, instances map[*ast.Ident]types.Instance) map[string]string {
	spelled := make(map[string]string, len(instances))
	for node, instance := range instances {
		var arguments []string
		for index := range instance.TypeArgs.Len() {
			arguments = append(arguments, canonical.Spell(instance.TypeArgs.At(index)))
		}
		spelled[nodeKey(fileSet, node)] = fmt.Sprintf("%v %s", arguments, canonical.Spell(instance.Type))
	}
	return spelled
}

// compareSpelled requires got to be want, key for key.
func compareSpelled(test *testing.T, label string, got map[string]string, want map[string]string) {
	test.Helper()
	var keys []string
	for key := range want {
		keys = append(keys, key)
	}
	for key := range got {
		if _, held := want[key]; !held {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		gotValue, gotHeld := got[key]
		wantValue, wantHeld := want[key]
		switch {
		case !gotHeld:
			test.Errorf("%s, %s: no projected answer, plain %q", label, key, wantValue)
		case !wantHeld:
			test.Errorf("%s, %s: projected %q, and the plain check has no answer", label, key, gotValue)
		case gotValue != wantValue:
			test.Errorf("%s, %s: projected %q, plain %q", label, key, gotValue, wantValue)
		}
	}
}
