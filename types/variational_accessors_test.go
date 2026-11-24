package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/go-types/canonical"
	"github.com/system-inc/go-types/types"
)

// accessorFiles declare a method and a function in each build's own file, alike on darwin and linux and otherwise on
// windows: M's result and its receiver, a value on darwin and linux and a pointer on windows, and Free's parameter and
// result.
var accessorFiles = []fixtureFile{
	{"shared.go", everyBuild, "package fork\n\ntype W struct{}\n"},
	{"accessors_unix.go", buildDarwin | buildLinux, `package fork

func (W) M() []int { return nil }

func Free(value int64) int64 { return value }
`},
	{"accessors_windows.go", buildWindows, `package fork

func (*W) M() string { return "" }

func Free(value int32) int32 { return value }
`},
}

// accessorRead is what one exported accessor answers of a package, spelled.
type accessorRead struct {
	name  string
	spell func(fileSet *token.FileSet, checked *types.Package) string
}

// methodOf is the method name of the package's type typeName, found through a pointer, which has every method.
func methodOf(checked *types.Package, typeName string, name string) *types.Func {
	found, _, _ := types.LookupFieldOrMethod(types.NewPointer(checked.Scope().Lookup(typeName).Type()), true, checked,
		name)
	method, _ := found.(*types.Func)
	return method
}

// spellScope spells a function's scope: the file it is in, and each name it declares with its type.
func spellScope(fileSet *token.FileSet, scope *types.Scope) string {
	if scope == nil {
		return "no scope"
	}
	spelled := filepath.Base(fileSet.Position(scope.Pos()).Filename)
	for _, name := range scope.Names() {
		spelled += fmt.Sprintf(" %s %s", name, canonical.Spell(scope.Lookup(name).Type()))
	}
	return spelled
}

// spellSignature spells a signature as the census does, with each parameter's and result's name.
func spellSignature(signature *types.Signature) string {
	var names []string
	for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
		for index := range tuple.Len() {
			names = append(names, tuple.At(index).Name())
		}
	}
	receiver := "no receiver"
	if signature.Recv() != nil {
		receiver = "receiver " + canonical.Spell(signature.Recv().Type())
	}
	return fmt.Sprintf("%s, %s, names %s", canonical.Spell(signature), receiver, strings.Join(names, " "))
}

// functionAccessorReads are the accessors of the function a package's object gives, each answered by build.
func functionAccessorReads(label string, function func(checked *types.Package) *types.Func) []accessorRead {
	return []accessorRead{
		{label + ": Func.Signature", func(_ *token.FileSet, checked *types.Package) string {
			return spellSignature(function(checked).Signature())
		}},
		{label + ": Func.Signature is Func.Type", func(_ *token.FileSet, checked *types.Package) string {
			return fmt.Sprint(types.Type(function(checked).Signature()) == function(checked).Type())
		}},
		{label + ": Func.Scope", func(fileSet *token.FileSet, checked *types.Package) string {
			return spellScope(fileSet, function(checked).Scope())
		}},
		{label + ": Func.FullName", func(_ *token.FileSet, checked *types.Package) string {
			return function(checked).FullName()
		}},
		{label + ": Func.String", func(_ *token.FileSet, checked *types.Package) string {
			return function(checked).String()
		}},
		{label + ": ObjectString", func(_ *token.FileSet, checked *types.Package) string {
			return types.ObjectString(function(checked), types.RelativeTo(checked))
		}},
	}
}

// TestVariationalAccessorsAnswerByBuild reads, through each exported accessor of a function, a method and a function
// that each build declares in its own file with its own signature: under InBuild for a build, each accessor answers
// as that build's plain check does, and under Agreeing it leaves the builds that answer alike, darwin and linux read
// as darwin and windows alone read as windows.
func TestVariationalAccessorsAnswerByBuild(test *testing.T) {
	test.Parallel()
	for _, each := range []struct {
		name     string
		fixtures []fixtureFile
		reads    []accessorRead
	}{
		{"method signatures", methodSignatureFiles, slices.Concat(
			functionAccessorReads("W.M", func(checked *types.Package) *types.Func { return methodOf(checked, "W", "M") }),
			functionAccessorReads("W.P", func(checked *types.Package) *types.Func { return methodOf(checked, "W", "P") }),
		)},
		{"accessors", accessorFiles, slices.Concat(
			functionAccessorReads("W.M", func(checked *types.Package) *types.Func { return methodOf(checked, "W", "M") }),
			functionAccessorReads("Free", func(checked *types.Package) *types.Func {
				return checked.Scope().Lookup("Free").(*types.Func)
			}),
			[]accessorRead{
				{"W: NewMethodSet", func(_ *token.FileSet, checked *types.Package) string {
					named := checked.Scope().Lookup("W").Type()
					return spellMethodSet(named) + ", pointer's " + spellMethodSet(types.NewPointer(named))
				}},
				{"W: LookupFieldOrMethod of a value", func(_ *token.FileSet, checked *types.Package) string {
					found, index, indirect := types.LookupFieldOrMethod(checked.Scope().Lookup("W").Type(), false, checked,
						"M")
					return fmt.Sprintf("found %t, %v, indirect %t", found != nil, index, indirect)
				}},
				{"W: MissingMethod of an interface with M", func(_ *token.FileSet, checked *types.Package) string {
					method := methodOf(checked, "W", "M")
					wanted := types.NewInterfaceType([]*types.Func{types.NewFunc(token.NoPos, checked, "M",
						types.NewSignatureType(nil, nil, nil, method.Signature().Params(), method.Signature().Results(),
							false))}, nil).Complete()
					missing, wrongType := types.MissingMethod(checked.Scope().Lookup("W").Type(), wanted, true)
					return fmt.Sprintf("missing %t, wrong type %t", missing != nil, wrongType)
				}},
			},
		)},
	} {
		test.Run(each.name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, each.fixtures)
			checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
				Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
			if checkError != nil {
				test.Fatal(checkError)
			}
			plain := map[types.Mask]*types.Package{}
			for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				plainPackage, plainError := (&types.Config{Importer: fixtureImporter{}}).Check("fork", fileSet,
					parsed.filesIn(build), &types.Info{})
				if plainError != nil {
					test.Fatal(plainError)
				}
				plain[build] = plainPackage
			}
			for _, read := range each.reads {
				for build, agree := range map[types.Mask]types.Mask{
					buildDarwin:  buildDarwin | buildLinux,
					buildLinux:   buildDarwin | buildLinux,
					buildWindows: buildWindows,
				} {
					want := read.spell(fileSet, plain[build])
					var got string
					types.InBuild(build, func() { got = read.spell(fileSet, checked) })
					if got != want {
						test.Errorf("%s under InBuild %03b: %s, and the build's plain check answers %s", read.name, build, got,
							want)
					}
					agreeing := types.Agreeing(build, everyBuild, func() { got = read.spell(fileSet, checked) })
					if got != want {
						test.Errorf("%s under Agreeing as %03b: %s, and the build's plain check answers %s", read.name, build,
							got, want)
					}
					// Only an accessor whose answer differs on windows has builds to leave out.
					if differs := read.spell(fileSet, plain[buildDarwin]) != read.spell(fileSet, plain[buildWindows]); !differs {
						continue
					}
					if agreeing != agree {
						test.Errorf("%s under Agreeing as %03b: agreeing %03b, want %03b", read.name, build, agreeing, agree)
					}
				}
			}
		})
	}
}

// TestVariationalInfoAccessorsAnswerByBuild checks the kinds fixture, whose Choice is a constant on darwin and linux
// and a variable on windows, and the imports fixture, whose files import by build, with every Info map, and asks the
// Info itself, the one the check was given, under InBuild for each build: TypeOf of every expression, ObjectOf of
// every identifier and PkgNameOf of every import answer what the build's projection of the maps holds, where the
// maps themselves hold the host's. Under Agreeing each leaves the builds that answer alike.
func TestVariationalInfoAccessorsAnswerByBuild(test *testing.T) {
	test.Parallel()
	for name, fixtures := range map[string][]fixtureFile{"kinds": kindFiles, "imports": importFiles} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, fixtures)
			variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
			info := fullInfo()
			if _, checkError := (&types.Config{Variation: variation, Importer: fixtureImporter{}}).Check("fork", fileSet,
				parsed.files, info); checkError != nil {
				test.Fatal(checkError)
			}
			asked := 0
			for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				overlays := types.BuildOverlays{}
				if index > 0 {
					overlays = variation.Overlays[index]
				}
				typesOf := project(info.Types, overlays.Types)
				defs, uses := project(info.Defs, overlays.Defs), project(info.Uses, overlays.Uses)
				implicits := project(info.Implicits, overlays.Implicits)
				types.InBuild(build, func() {
					// Every expression and identifier any build answers, so one the build lacks is asked too.
					for _, every := range variation.Overlays[:3] {
						for expression := range project(info.Types, every.Types) {
							want := typesOf[expression].Type
							// With no entry, TypeOf answers an identifier with its object's type.
							if _, isIdent := expression.(*ast.Ident); isIdent && want == nil {
								continue
							}
							if got := info.TypeOf(expression); got != want {
								test.Errorf("build %03b: TypeOf(%s at %s) is %v, and the build's Types holds %v", build,
									types.ExprString(expression), fileSet.Position(expression.Pos()), got, want)
							}
							asked++
						}
						for _, idents := range []map[*ast.Ident]types.Object{project(info.Defs, every.Defs),
							project(info.Uses, every.Uses)} {
							for ident := range idents {
								want := defs[ident]
								if want == nil {
									want = uses[ident]
								}
								if got := info.ObjectOf(ident); got != want {
									test.Errorf("build %03b: ObjectOf(%s at %s) is %v, and the build's Defs and Uses hold %v", build,
										ident.Name, fileSet.Position(ident.Pos()), got, want)
								}
								asked++
							}
						}
					}
					for _, file := range parsed.files {
						for _, spec := range file.Imports {
							var want types.Object = implicits[spec]
							if spec.Name != nil {
								want = defs[spec.Name]
							}
							wantName, _ := want.(*types.PkgName)
							if got := info.PkgNameOf(spec); got != wantName {
								test.Errorf("build %03b: PkgNameOf(%s at %s) is %v, and the build's maps hold %v", build,
									spec.Path.Value, fileSet.Position(spec.Pos()), got, wantName)
							}
							asked++
						}
					}
				})
			}
			if asked == 0 {
				test.Error("nothing was asked of the Info")
			}
			if name != "kinds" {
				return
			}
			// In the shared file, the use of Choice names a constant on darwin and linux and a variable on windows, and
			// ChoiceValue is one variable of one type in every build.
			unix := buildDarwin | buildLinux
			shared := map[*ast.Ident]bool{}
			for _, idents := range []map[*ast.Ident]types.Object{info.Defs, info.Uses} {
				for ident := range idents {
					shared[ident] = fileSet.Position(ident.Pos()).Filename == "shared.go"
				}
			}
			agreed := 0
			for ident, inShared := range shared {
				want := map[types.Mask]types.Mask{buildDarwin: everyBuild, buildLinux: everyBuild, buildWindows: everyBuild}
				if ident.Name == "Choice" {
					want = map[types.Mask]types.Mask{buildDarwin: unix, buildLinux: unix, buildWindows: buildWindows}
				} else if ident.Name != "ChoiceValue" {
					continue
				}
				if !inShared {
					continue
				}
				agreed++
				for build, agree := range want {
					if agreeing := types.Agreeing(build, everyBuild, func() { _ = info.ObjectOf(ident) }); agreeing != agree {
						test.Errorf("ObjectOf(%s) under Agreeing as %03b: agreeing %03b, want %03b", ident.Name, build, agreeing,
							agree)
					}
					if agreeing := types.Agreeing(build, everyBuild, func() { _ = info.TypeOf(ident) }); agreeing != agree {
						test.Errorf("TypeOf(%s) under Agreeing as %03b: agreeing %03b, want %03b", ident.Name, build, agreeing,
							agree)
					}
				}
			}
			if agreed != 2 {
				test.Errorf("%d identifiers of the shared file were read under Agreeing, want Choice and ChoiceValue", agreed)
			}
		})
	}
}
