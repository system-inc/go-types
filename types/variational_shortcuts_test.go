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

// kindFiles declare names of a different kind in each build: Value is a constant on darwin and linux and a defined
// type with a method on windows, Measure a defined type with a method on windows and a function elsewhere, declared
// in the other order, and Choice a constant on darwin and linux and a variable on windows, read by a shared
// declaration. Each build's method is found on its own build's type.
var kindFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

var ChoiceValue = Choice + 1
`},
	{"kind_unix.go", buildDarwin | buildLinux, `package fork

const Value = 1

func Convert() int { return int(Value) }

func Measure() int { return 3 }

const Choice = 1
`},
	{"kind_windows.go", buildWindows, `package fork

type Value int

func (Value) Measure() int { return 2 }

func Measured(value Value) int { return value.Measure() }

type Measure struct{}

func (*Measure) Size() int { return 4 }

func Sized(measure *Measure) int { return measure.Size() }

var Choice = 2
`},
}

// TestVariationalProjectsANameOfADifferentKindByBuild is the projection test over kindFiles.
func TestVariationalProjectsANameOfADifferentKindByBuild(t *testing.T) {
	t.Parallel()
	projectAndCompare(t, kindFiles)
}

// TestVariationalReportsANameRedeclaredInABuildItsMergedDeclarationsCover declares Value in each build's own file,
// which together cover every build, and again on linux as a variable: linux's plain check reports it redeclared, and
// so must the variational check.
func TestVariationalReportsANameRedeclaredInABuildItsMergedDeclarationsCover(t *testing.T) {
	t.Parallel()
	_, published := projectAndCompareWithDiagnostics(t, []fixtureFile{
		{"value_unix.go", buildDarwin | buildLinux, "package fork\n\nconst Value = 1\n"},
		{"value_windows.go", buildWindows, "package fork\n\nconst Value = 2\n"},
		{"value_linux.go", buildLinux, "package fork\n\nvar Value = 3\n"},
	}, true)
	if len(published) == 0 {
		t.Error("the variational check reported nothing, and linux's plain check reports Value redeclared")
	}
}

// methodSignatureFiles declare a method on a shared type in each build's own file, its result a slice on darwin and
// linux and a string on windows, on the value and on the pointer receiver. A shared file calls each, takes each as a
// method value and as a method expression, and assigns the type to an interface declared by build, and each build's
// plain check reads its own build's signature.
var methodSignatureFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type W struct{}

func Called(w W) int { return len(w.M()) }

func PointerCalled(w *W) int { return len(w.P()) }

func Valued(w W) int {
	method := w.M
	return len(method())
}

func Expressed() int { return len(W.M(W{})) }

func PointerExpressed(w *W) int { return len((*W).P(w)) }

var Value = W{}.M

var Expression = W.M

var PointerValue = (&W{}).P

var PointerExpression = (*W).P

var Held I = W{}

func Satisfied(w *W) I { return w }
`},
	{"method_unix.go", buildDarwin | buildLinux, `package fork

func (W) M() []int { return nil }

func (*W) P() []int { return nil }

type I interface{ M() []int }
`},
	{"method_windows.go", buildWindows, `package fork

func (W) M() string { return "" }

func (*W) P() string { return "" }

type I interface{ M() string }
`},
}

// TestVariationalProjectsAMethodWhoseSignatureDiffersByBuild is the projection test over methodSignatureFiles.
func TestVariationalProjectsAMethodWhoseSignatureDiffersByBuild(t *testing.T) {
	t.Parallel()
	projectAndCompare(t, methodSignatureFiles)
}
