package types_test

import (
	"go/token"
	"testing"

	"github.com/system-inc/go-types/types"
)

// sizeFiles declare a shared struct whose field's named type has a different underlying type by build, of another
// size and alignment, and shared constants and an array length computed from the struct's size and alignment, as
// runtime's runtime2.go needs: reading the field's underlying type inside the size computation splits the unit.
var sizeFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

import "unsafe"

type Holder struct {
	flag byte
	word Word
}

const HolderSize = unsafe.Sizeof(Holder{})

const HolderAlign = unsafe.Alignof(Holder{})

var Padding [HolderAlign]byte
`},
	{"word_unix.go", buildDarwin | buildLinux, `package fork

type Word struct{ value uint64 }
`},
	{"word_windows.go", buildWindows, `package fork

type Word struct{ value uint16 }
`},
}

// TestVariationalSizesSplitInsideTheSizeComputation checks sizeFiles once for every build with the gc sizes and with
// StdSizes, whose Alignof each guards its result in a deferred assertion, and requires each build's HolderSize and
// HolderAlign, read under InBuild, to be the build's plain check's with the same sizes.
func TestVariationalSizesSplitInsideTheSizeComputation(test *testing.T) {
	test.Parallel()
	for name, sizes := range map[string]types.Sizes{"gc": nil, "std": &types.StdSizes{WordSize: 8, MaxAlign: 8}} {
		fileSet := token.NewFileSet()
		parsed := parseFixture(test, fileSet, sizeFiles)
		checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
			Importer: fixtureImporter{}, Sizes: sizes}).Check("fork", fileSet, parsed.files, &types.Info{})
		if checkError != nil {
			test.Fatalf("%s sizes, variational check: %v", name, checkError)
		}
		for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
			plain, plainError := (&types.Config{Importer: fixtureImporter{}, Sizes: sizes}).Check("fork", fileSet,
				parsed.filesIn(build), &types.Info{})
			if plainError != nil {
				test.Fatalf("%s sizes, plain check of build %03b: %v", name, build, plainError)
			}
			for _, constant := range []string{"HolderSize", "HolderAlign"} {
				want := plain.Scope().Lookup(constant).(*types.Const).Val().ExactString()
				var got string
				types.InBuild(build, func() {
					got = checked.Scope().Lookup(constant).(*types.Const).Val().ExactString()
				})
				if got != want {
					test.Errorf("%s sizes, build %03b, %s: %s, plain %s", name, build, constant, got, want)
				}
			}
		}
	}
}
