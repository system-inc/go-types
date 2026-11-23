package types_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"

	"github.com/system-inc/go-types/types"
)

// The concurrency fixture (#dfnzqjb): a dependency whose constant, struct and methods differ by build, checked once
// for every build, and many packages importing it, checked at once, each by its own checker on its own goroutine. They
// share the dependency's objects, as packages checked along the import graph share what they import, and each reads
// them under its own units' builds.
var concurrencyDependency = []fixtureFile{
	{"shared.go", everyBuild, `package dep

type File struct{ *file }

func (File) Shared() int { return 1 }

type Wrapper struct{ Stat }
`},
	{"dep_linux.go", buildLinux, `package dep

const N = 1

type Stat struct{ Mode uint32 }

type file struct{ fd int }

func (File) Fd() int { return 0 }
`},
	{"dep_windows.go", buildWindows, `package dep

const N = 2

type Stat struct{ Mode uint16 }

type file struct {
	handle uintptr
	name   string
}

var OnlyWindows = 7
`},
	{"dep_other.go", buildDarwin, `package dep

const N = 3

type Stat struct{ Mode uint32 }

type file struct{ fd int }

func (File) Fd() int { return 0 }
`},
}

// concurrencyUser is the importing package numbered index: what it reads of dep differs by build in every way the
// fixture can, a constant's value, a struct's field, a size, a promoted field, a method present in some builds and an
// object present in one.
func concurrencyUser(index int) []fixtureFile {
	return []fixtureFile{
		{fmt.Sprintf("user%d.go", index), everyBuild, fmt.Sprintf(`package user%d

import (
	"dep"
	"unsafe"
)

func Value() int { return dep.N * %d }

func Mode(stat dep.Stat) uint64 { return uint64(stat.Mode) }

func Size() uintptr { return unsafe.Sizeof(dep.Stat{}) }

func Inner(wrapper dep.Wrapper) any { return wrapper.Mode }

func Shared(file dep.File) int { return file.Shared() }
`, index, index+1)},
		{fmt.Sprintf("user%d_unix.go", index), buildLinux | buildDarwin, fmt.Sprintf(`package user%d

import "dep"

func Descriptor(file dep.File) int { return file.Fd() }
`, index)},
		{fmt.Sprintf("user%d_windows.go", index), buildWindows, fmt.Sprintf(`package user%d

import "dep"

func Only() int { return dep.OnlyWindows }
`, index)},
	}
}

// packageImporter imports unsafe and the packages it holds by path.
type packageImporter map[string]*types.Package

func (imported packageImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if found := imported[path]; found != nil {
		return found, nil
	}
	return nil, fmt.Errorf("no package %q", path)
}

// parsedFixture is a fixture parsed, with the builds each file compiles in.
type parsedFixture struct {
	files []*ast.File
	masks map[*ast.File]types.Mask
}

func parseFixture(t testing.TB, fileSet *token.FileSet, fixtures []fixtureFile) parsedFixture {
	t.Helper()
	parsed := parsedFixture{masks: map[*ast.File]types.Mask{}}
	for _, fixture := range fixtures {
		file, parseError := parser.ParseFile(fileSet, fixture.name, fixture.source, parser.ParseComments)
		if parseError != nil {
			t.Fatal(parseError)
		}
		parsed.files = append(parsed.files, file)
		parsed.masks[file] = fixture.builds
	}
	return parsed
}

// filesIn is the fixture's files that build compiles.
func (parsed parsedFixture) filesIn(build types.Mask) []*ast.File {
	var built []*ast.File
	for _, file := range parsed.files {
		if parsed.masks[file]&build != 0 {
			built = append(built, file)
		}
	}
	return built
}

// TestVariationalChecksRunAtOnce checks the dependency once for every build, then checks many packages importing it
// at once, several rounds of each, and requires each package's answers projected to each build to be that build's
// plain answers. Under the race detector it also requires the checks to share no unguarded write.
func TestVariationalChecksRunAtOnce(t *testing.T) {
	t.Parallel()
	const users = 16
	const rounds = 4
	fileSet := token.NewFileSet()
	dependency := parseFixture(t, fileSet, concurrencyDependency)
	userFixtures := make([]parsedFixture, users)
	for index := range users {
		userFixtures[index] = parseFixture(t, fileSet, concurrencyUser(index))
	}

	dependencyVariation := &types.Variation{Builds: everyBuild, Files: dependency.masks}
	dependencyPackage, dependencyError := (&types.Config{Variation: dependencyVariation, Importer: fixtureImporter{}}).
		Check("dep", fileSet, dependency.files, &types.Info{})
	if dependencyError != nil {
		t.Fatalf("variational check of dep: %v", dependencyError)
	}
	importer := packageImporter{"dep": dependencyPackage}

	// Every user, every round, at once: rounds times users checks share dep's objects.
	variations := make([][]*types.Variation, rounds)
	checkErrors := make([][]error, rounds)
	var group sync.WaitGroup
	start := make(chan struct{})
	for round := range rounds {
		variations[round] = make([]*types.Variation, users)
		checkErrors[round] = make([]error, users)
		for index := range users {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				variation := &types.Variation{Builds: everyBuild, Files: userFixtures[index].masks}
				configuration := &types.Config{Variation: variation, Importer: importer}
				_, checkErrors[round][index] = configuration.Check(fmt.Sprintf("user%d", index), fileSet,
					userFixtures[index].files, &types.Info{})
				variations[round][index] = variation
			}()
		}
	}
	close(start)
	group.Wait()
	for round := range rounds {
		for index := range users {
			forked := 0
			for _, answers := range variations[round][index].Types {
				if answers.Len() > 1 {
					forked++
				}
			}
			// Value, Mode, Size and Inner each answer by build, so a check that read dep as one build did would
			// fork nothing.
			if forked < 4 {
				t.Errorf("round %d, user%d: %d expressions answer by build, want at least 4", round, index, forked)
			}
		}
	}

	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		plainDependency, plainError := (&types.Config{Importer: fixtureImporter{}}).Check("dep", fileSet,
			dependency.filesIn(build), &types.Info{})
		if plainError != nil {
			t.Fatalf("plain check of dep, build %03b: %v", build, plainError)
		}
		plainImporter := packageImporter{"dep": plainDependency}
		for index := range users {
			plain := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
			if _, checkError := (&types.Config{Importer: plainImporter}).Check(fmt.Sprintf("user%d", index), fileSet,
				userFixtures[index].filesIn(build), plain); checkError != nil {
				t.Fatalf("plain check of user%d, build %03b: %v", index, build, checkError)
			}
			for round := range rounds {
				if checkErrors[round][index] != nil {
					t.Fatalf("round %d, variational check of user%d: %v", round, index, checkErrors[round][index])
				}
				compareProjection(t, fileSet, fmt.Sprintf("round %d, user%d, build %03b", round, index, build),
					variations[round][index], plain, build)
			}
		}
	}
}

// compareProjection requires variation's answers for build to be exactly plain's, expression for expression, each
// keyed by its place and text.
func compareProjection(t *testing.T, fileSet *token.FileSet, label string, variation *types.Variation,
	plain *types.Info, build types.Mask,
) {
	t.Helper()
	projected := map[string]string{}
	for expression, answers := range variation.Types {
		for candidate := range answers.All() {
			if candidate.Builds&build != 0 {
				projected[positionKey(fileSet, expression)] = answer(candidate.TypeAndValue)
			}
		}
	}
	if len(plain.Types) == 0 {
		t.Errorf("%s: the plain check recorded nothing", label)
	}
	for expression, want := range plain.Types {
		key := positionKey(fileSet, expression)
		got, held := projected[key]
		if !held {
			t.Errorf("%s, %s: no projected answer, plain %q", label, key, answer(want))
			continue
		}
		if got != answer(want) {
			t.Errorf("%s, %s: projected %q, plain %q", label, key, got, answer(want))
		}
		delete(projected, key)
	}
	for key, got := range projected {
		t.Errorf("%s, %s: projected %q, and the plain check has no answer", label, key, got)
	}
}
