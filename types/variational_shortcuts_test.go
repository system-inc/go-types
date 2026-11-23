package types_test

import (
	"strings"
	"testing"
)

// abandonedErrorFiles report an error in a body before a read that splits it: the run for every build is abandoned
// at the read, and only the runs for each group of builds that answer alike may report what they find. The array's
// length is negative on darwin and linux alone.
var abandonedErrorFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Report() {
	_ = missing
	_ = N
	var _ [N - 2]int
}
`},
	{"unix.go", buildDarwin | buildLinux, "package fork\n\nconst N = 1\n"},
	{"windows.go", buildWindows, "package fork\n\nconst N = 2\n"},
}

// TestVariationalPublishesOnlyTheErrorsOfRunsThatSurvive is the projection test over abandonedErrorFiles, which
// requires each error once for each run that survives: undefined missing for darwin and linux and for windows, and
// the negative length for darwin and linux, and nothing from the run abandoned at the split.
func TestVariationalPublishesOnlyTheErrorsOfRunsThatSurvive(t *testing.T) {
	t.Parallel()
	_, published := projectAndCompareWithDiagnostics(t, abandonedErrorFiles, true)
	counts := map[string]int{}
	for _, diagnostic := range published {
		switch {
		case strings.Contains(diagnostic, "undefined: missing"):
			counts["undefined"]++
		case strings.Contains(diagnostic, "invalid array length"):
			counts["negative"]++
		}
	}
	if len(published) != 3 || counts["undefined"] != 2 || counts["negative"] != 1 {
		t.Errorf("published %d diagnostics, want undefined missing twice and the negative length once: %s",
			len(published), strings.Join(published, "; "))
	}
}

// aliasTargetFiles declare an alias whose target differs by build, and methods declared on it in a file every build
// compiles: each build's method is its own target's, as each build's plain check has it. Handle is the driver's case,
// a defined type on unix and an alias of the platform's own type on windows, returned by a shared function. The
// generic function's constraint follows the alias, as the fuzzer's seeds have it.
var aliasTargetFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type First int

type Second int

func (Value) Measure() int { return 1 }

func Measured(value Value) int { return value.Measure() }

func (*Value) Reset() {}

func ResetPointer(value *Value) { value.Reset() }

func Invalid() Handle { return Handle(invalidHandle) }

func Direct() Handle { return invalidHandle }

func Zero() Handle {
	var handle Handle
	return handle
}

var Converted Alias = Alias(1)

var Identified = Identity(Alias(2))
`},
	{"platform_unix.go", buildDarwin | buildLinux, `package fork

type Value = First

type Handle int

const invalidHandle Handle = -1

type Alias = int16

func Identity[T ~int16](value T) T { return value }
`},
	{"platform_windows.go", buildWindows, `package fork

type Value = Second

type SystemHandle uintptr

type Handle = SystemHandle

const invalidHandle = ^SystemHandle(0)

type Alias = int8

func Identity[T ~int8](value T) T { return value }
`},
}

// TestVariationalProjectsAnAliasWhoseTargetDiffersByBuild is the projection test over aliasTargetFiles.
func TestVariationalProjectsAnAliasWhoseTargetDiffersByBuild(t *testing.T) {
	t.Parallel()
	projectAndCompare(t, aliasTargetFiles)
}

// genericDefinitionFiles declare a generic type again in each build's file, its type parameters' constraints and its
// fields differing, and one generic type in a shared file whose fields differ by build: each build's instances
// substitute that build's own type parameters. Holder is the fuzzer's class, its constraint following an alias whose
// target differs by build, instantiated with the alias and given a generic function's result.
var genericDefinitionFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func BoxValue(box Box[int]) int { return box.Value }

var Held = Holder[Alias]{Value: Identity(Alias(2))}

var Unheld Alias = Held.Get()

type Sized[T any] struct{ values [Length]T }

func First(sized Sized[string]) string { return sized.values[0] }
`},
	{"platform_unix.go", buildDarwin | buildLinux, `package fork

type Box[T any] struct{ Value T }

type Alias = int16

type Holder[T ~int16] struct{ Value T }

func (holder Holder[T]) Get() T { return holder.Value }

func Identity[T ~int16](value T) T { return value }

const Length = 2
`},
	{"platform_windows.go", buildWindows, `package fork

type Box[T ~int] struct {
	Extra string
	Value T
}

type Alias = int8

type Holder[T ~int8] struct{ Value T }

func (holder Holder[T]) Get() T { return holder.Value }

func Identity[T ~int8](value T) T { return value }

const Length = 3
`},
}

// TestVariationalProjectsAGenericTypeDeclaredByBuild is the projection test over genericDefinitionFiles.
func TestVariationalProjectsAGenericTypeDeclaredByBuild(t *testing.T) {
	t.Parallel()
	projectAndCompare(t, genericDefinitionFiles)
}
