package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// TestVariationalInfoProjectsToEachBuild checks each fixture once for every build with every Info map, then requires
// each build's projection of them (the plain maps, which hold the host's answers, with that build's overlay laid over
// them) to be that build's plain check's maps, entry for entry. Objects compare by kind, name, their type in the build
// as the census spells it (canonical: a signature by its parameters' types, not their names)
// and where they're declared. A declaration in a file every build compiles compares by its exact position; one in a
// file only some builds compile compares only as being in such a file, since a name declared in each build's own file
// is one object for all of them, at its first declaration, and declarations alike across builds fold into one, as the
// model has it (variational.go). Selections compare by kind, index, indirection and object, and instances by their
// type arguments and type. The variational check's diagnostics, told apart, are every build's plain diagnostics.
func TestVariationalInfoProjectsToEachBuild(test *testing.T) {
	test.Parallel()
	for name, fixtures := range map[string][]fixtureFile{
		"spike":            fixtureFiles,
		"hard":             hardFiles,
		"usage":            usageFiles,
		"methods":          methodOrderFiles,
		"abandoned errors": abandonedErrorFiles,
		"alias targets":    aliasTargetFiles,
		"generic types":    genericDefinitionFiles,
	} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, fixtures)
			variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
			info := fullInfo()
			// A fixture may report errors, as the abandoned errors fixture does: they're compared with the plain
			// checks' rather than refused.
			var published []string
			plainDiagnostics := map[string]bool{}
			if _, checkError := (&types.Config{Variation: variation, Importer: unsafeImporter{},
				Error: func(diagnostic error) { published = append(published, diagnostic.Error()) }}).Check("fork",
				fileSet, parsed.files, info); checkError != nil && len(published) == 0 {
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
				if _, checkError := (&types.Config{Importer: unsafeImporter{},
					Error: func(diagnostic error) { plainDiagnostics[diagnostic.Error()] = true }}).Check("fork", fileSet,
					parsed.filesIn(build), plain); checkError != nil && !plainDiagnostics[checkError.Error()] {
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

// fullInfo is an Info with every map that varies by build.
func fullInfo() *types.Info {
	return &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Implicits:  map[ast.Node]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
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
