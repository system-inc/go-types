package types_test

import (
	"go/token"
	"testing"

	"github.com/system-inc/go-types/types"
)

// BenchmarkVariationalReadOutsideCheck reads every package-level object of the hard fixture from a goroutine running
// no check, as a host lint does: Lookup, each object's type, a constant's value, a type's underlying type and its
// methods. The variational package is checked once for every build, and the plain one for darwin alone, so the plain
// reads are what a process with no variational check pays.
func BenchmarkVariationalReadOutsideCheck(benchmark *testing.B) {
	fileSet := token.NewFileSet()
	parsed := parseFixture(benchmark, fileSet, hardFiles)
	variational, variationalError := (&types.Config{Variation: &types.Variation{Builds: everyBuild,
		Files: parsed.masks}, Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
	if variationalError != nil {
		benchmark.Fatal(variationalError)
	}
	plain, plainError := (&types.Config{Importer: fixtureImporter{}}).Check("fork", fileSet,
		parsed.filesIn(buildDarwin), &types.Info{})
	if plainError != nil {
		benchmark.Fatal(plainError)
	}
	for _, each := range []struct {
		name    string
		checked *types.Package
	}{{"variational", variational}, {"plain", plain}} {
		names := each.checked.Scope().Names()
		benchmark.Run(each.name, func(benchmark *testing.B) {
			for benchmark.Loop() {
				for _, name := range names {
					readObject(each.checked.Scope().Lookup(name))
				}
			}
		})
	}
}

// readObject reads what a lint reads of a package-level object.
func readObject(object types.Object) {
	if object == nil {
		return
	}
	_ = object.Type()
	if constantObject, isConstant := object.(*types.Const); isConstant {
		_ = constantObject.Val()
	}
	if named, isNamed := types.Unalias(object.Type()).(*types.Named); isNamed {
		_ = named.Underlying()
		for index := range named.NumMethods() {
			_ = named.Method(index).Type()
		}
	}
}
