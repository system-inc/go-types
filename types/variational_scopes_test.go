package types_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/go-types/types"
)

// reachableScopes are the scopes reached from scope through Children, scope itself among them.
func reachableScopes(scope *types.Scope, reached map[*types.Scope]bool) map[*types.Scope]bool {
	if reached[scope] {
		return reached
	}
	reached[scope] = true
	for child := range scope.Children() {
		reachableScopes(child, reached)
	}
	return reached
}

// spellScopePlace spells a scope by where it is and what it declares.
func spellScopePlace(fileSet *token.FileSet, scope *types.Scope) string {
	return fmt.Sprintf("%s-%s {%s}", fileSet.Position(scope.Pos()), fileSet.Position(scope.End()),
		strings.Join(scope.Names(), " "))
}

// panickingAccessors are the exported accessors of value, each a method that takes nothing, that panic.
func panickingAccessors(value any) []string {
	var panicking []string
	reflected := reflect.ValueOf(value)
	for index := range reflected.Type().NumMethod() {
		method := reflected.Type().Method(index)
		leaf := reflected.Type().String() + "." + method.Name
		if method.Type.NumIn() != 1 || method.Type.NumOut() == 0 || oracleDenied[leaf] != "" {
			continue
		}
		if _, panicked := callAccessor(reflected.Method(index)); panicked != nil {
			panicking = append(panicking, fmt.Sprintf("%s: %v", leaf, panicked))
		}
	}
	return panicking
}

// TestVariationalScopeTreeIsEachBuildsOwn checks every projection fixture once for every build and holds its scope
// tree, read under InBuild for each build and with no state as the host, to the build's own scopes: the scopes
// reached from the package's through Children are exactly the scopes the build's Scopes names, a function's Scope is
// the scope the build's Scopes names for its type, and every object's Parent is among them. Read inside Outside,
// the scopes reached are those some build's Scopes names, and no other: no scope of a run abandoned for a split is
// reachable under any view, and no object of a scope reached has an accessor that panics.
func TestVariationalScopeTreeIsEachBuildsOwn(test *testing.T) {
	test.Parallel()
	for name, fixtures := range projectionFixtures {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			fileSet := token.NewFileSet()
			parsed := parseFixture(test, fileSet, fixtures)
			variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
			info := fullInfo()
			// A fixture may report errors, as the abandoned errors fixture does; its scopes are held all the same.
			checked, _ := (&types.Config{Variation: variation, Importer: fixtureImporter{}, Error: func(error) {}}).Check(
				"fork", fileSet, parsed.files, info)
			surviving := map[*types.Scope]bool{}
			requireSound := func(view string, reached map[*types.Scope]bool) {
				for scope := range reached {
					for _, name := range scope.Names() {
						object := scope.Lookup(name)
						values := []any{object}
						if typed := object.Type(); typed != nil {
							values = append(values, typed)
						}
						for _, value := range values {
							for _, panicking := range panickingAccessors(value) {
								test.Errorf("%s: %s, in the scope at %s, panics in %s", view, name,
									spellScopePlace(fileSet, scope), panicking)
							}
						}
					}
				}
			}
			for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
				overlays := types.BuildOverlays{}
				if index > 0 {
					overlays = variation.Overlays[index]
				}
				scopes := project(info.Scopes, overlays.Scopes)
				defs := project(info.Defs, overlays.Defs)
				own := map[*types.Scope]bool{checked.Scope(): true}
				for _, scope := range scopes {
					own[scope] = true
					surviving[scope] = true
				}
				views := map[string]func(work func()){
					fmt.Sprintf("under InBuild %03b", build): func(work func()) { types.InBuild(build, work) },
				}
				if build == buildDarwin {
					views["with no state, as the host"] = func(work func()) { work() }
				}
				for _, view := range slices.Sorted(maps.Keys(views)) {
					views[view](func() {
						reached := reachableScopes(checked.Scope(), map[*types.Scope]bool{})
						for scope := range reached {
							if !own[scope] {
								test.Errorf("%s: the scope at %s is reached from the package's, and the build's Scopes doesn't name it",
									view, spellScopePlace(fileSet, scope))
							}
						}
						for scope := range own {
							if !reached[scope] {
								test.Errorf("%s: the scope at %s, which the build's Scopes names, isn't reached from the package's",
									view, spellScopePlace(fileSet, scope))
							}
						}
						for node, scope := range scopes {
							if _, isFunction := node.(*ast.FuncType); isFunction {
								if inner := checked.Scope().Innermost(node.Pos()); inner != scope {
									test.Errorf("%s: Innermost at the function type at %s is the scope at %s, and the build's Scopes names %s",
										view, fileSet.Position(node.Pos()), spellScopePlace(fileSet, inner),
										spellScopePlace(fileSet, scope))
								}
							}
						}
						for ident, object := range defs {
							if object == nil {
								continue
							}
							if parent := object.Parent(); parent != nil && !own[parent] && parent != types.Universe {
								test.Errorf("%s: the parent of %s, declared at %s, is the scope at %s, which the build's Scopes doesn't name",
									view, object.Name(), fileSet.Position(ident.Pos()), spellScopePlace(fileSet, parent))
							}
							function, isFunction := object.(*types.Func)
							if !isFunction || function.Type() == nil {
								continue
							}
							if scope := function.Scope(); scope != nil && !own[scope] {
								test.Errorf("%s: the scope of %s is the one at %s, which the build's Scopes doesn't name", view,
									function.Name(), spellScopePlace(fileSet, scope))
							}
						}
						requireSound(view, reached)
					})
				}
			}
			surviving[checked.Scope()] = true
			types.Outside(func() {
				reached := reachableScopes(checked.Scope(), map[*types.Scope]bool{})
				abandoned := 0
				for scope := range reached {
					// A function whose body ran again for each group of builds keeps its first scope as their place among
					// its siblings, holding what its signature declared and nothing of the run that was abandoned.
					if !surviving[scope] {
						abandoned++
						if scope.NumChildren() != 0 {
							test.Errorf("inside Outside: the scope at %s, which no build's Scopes names, has %d children",
								spellScopePlace(fileSet, scope), scope.NumChildren())
						}
						// It stands alone at its place: a declaration's run that was abandoned would sit beside the scope
						// of the run that replaced it.
						for sibling := range scope.Parent().Children() {
							if sibling != scope && sibling.Pos() == scope.Pos() && sibling.End() == scope.End() {
								test.Errorf("inside Outside: the scope at %s, which no build's Scopes names, has a sibling at its place",
									spellScopePlace(fileSet, scope))
							}
						}
					}
				}
				requireSound("inside Outside", reached)
				test.Logf("inside Outside: %d scopes reached, %d of them no build's own", len(reached), abandoned)
			})
		})
	}
}

// TestVariationalAgreeingNarrowsAtEachScopeReadThatDiffers reads the scope tree of the spike fixture, whose F's body
// runs once for each build, since N is each build's own, and whose files are each build's own: the package scope's
// children, F's scope and the scope Innermost finds in F leave the builds that share the answer, and a function whose
// body ran once keeps every build.
func TestVariationalAgreeingNarrowsAtEachScopeReadThatDiffers(test *testing.T) {
	test.Parallel()
	fileSet := token.NewFileSet()
	checked := checkAgreeingFixture(test, fileSet, fixtureFiles)
	forked := checked.Scope().Lookup("F").(*types.Func)
	steady := checked.Scope().Lookup("Untouched").(*types.Func)
	for _, each := range []agreeingRead{
		{"the package scope's children", func() { _ = checked.Scope().NumChildren() }},
		{"a forked function's scope", func() { _ = forked.Scope() }},
		{"the scope Innermost finds in a forked function", func() { _ = checked.Scope().Innermost(forked.Pos()) }},
	} {
		for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
			if agreeing := types.Agreeing(build, everyBuild, each.read); agreeing != build {
				test.Errorf("%s, read as %03b: agreeing %03b, and each build has its own, %03b", each.name, build, agreeing,
					build)
			}
		}
	}
	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		agreeing := types.Agreeing(build, everyBuild, func() {
			scope := steady.Scope()
			_ = scope.NumChildren()
			_ = scope.Names()
		})
		if agreeing != everyBuild {
			test.Errorf("the scope of a function whose body ran once, read as %03b: agreeing %03b, want 111", build, agreeing)
		}
	}
}
