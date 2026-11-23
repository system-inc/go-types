package types_test

import (
	"go/token"
	"sync"
	"testing"

	"github.com/system-inc/go-types/types"
)

// interfaceFiles declare an interface whose methods differ by build, embedded in a shared interface, in an interface
// literal and in a generic interface, so the shared interfaces' type sets differ by build too, though one declaration
// declares each.
var interfaceFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

type Reader interface {
	Closer
	Read() int
}

type Wrapped interface {
	interface{ Closer }
	Size() int
}

func Use(reader Reader) int { return reader.Read() }

type Getter[T any] interface {
	Closer
	Get() T
}
`},
	{"closer_unix.go", buildDarwin | buildLinux, `package fork

type Closer interface{ Close() error }
`},
	{"closer_windows.go", buildWindows, `package fork

type Closer interface {
	Close() error
	Handle() uintptr
}
`},
}

// TestVariationalTypeSetsReadAtOnceByBuild reads the type sets of interfaceFiles' interfaces, whose methods differ by
// build, from many goroutines at once: under InBuild for each build, and with no state, as the host. One of them is
// an instance of the generic interface made after the check, whose sets the readers compute at once. Each answer
// must be its build's plain check's, and under the race detector the reads must share no unguarded write.
func TestVariationalTypeSetsReadAtOnceByBuild(test *testing.T) {
	test.Parallel()
	const readers = 8
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, interfaceFiles)
	checked, checkError := (&types.Config{Variation: &types.Variation{Builds: everyBuild, Files: parsed.masks},
		Importer: fixtureImporter{}}).Check("fork", fileSet, parsed.files, &types.Info{})
	if checkError != nil {
		test.Fatal(checkError)
	}
	names := []string{"Reader", "Wrapped", "Closer", "Getter[int]"}
	// lookup is a name's type in checked, the generic interface instantiated with int.
	lookup := func(checked *types.Package, name string) types.Type {
		if name != "Getter[int]" {
			return checked.Scope().Lookup(name).Type()
		}
		instance, instanceError := types.Instantiate(nil, checked.Scope().Lookup("Getter").Type(),
			[]types.Type{types.Typ[types.Int]}, true)
		if instanceError != nil {
			test.Fatal(instanceError)
		}
		return instance
	}
	shared := map[string]types.Type{}
	for _, name := range names {
		shared[name] = lookup(checked, name)
	}
	want := map[types.Mask]map[string]string{}
	for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		plain, plainError := (&types.Config{Importer: fixtureImporter{}}).Check("fork", fileSet,
			parsed.filesIn(build), &types.Info{})
		if plainError != nil {
			test.Fatal(plainError)
		}
		want[build] = map[string]string{}
		for _, name := range names {
			want[build][name] = spellTypeSet(lookup(plain, name))
		}
	}
	type answer struct {
		build types.Mask
		name  string
		got   string
	}
	answers := make(chan answer, readers*4*len(names))
	var group sync.WaitGroup
	start := make(chan struct{})
	for range readers {
		for _, build := range []types.Mask{buildDarwin, buildLinux, buildWindows, 0} {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				read := func() {
					for _, name := range names {
						answers <- answer{build, name, spellTypeSet(shared[name])}
					}
				}
				if build == 0 {
					read()
					return
				}
				types.InBuild(build, read)
			}()
		}
	}
	close(start)
	group.Wait()
	close(answers)
	for each := range answers {
		build, label := each.build, "InBuild"
		if build == 0 {
			build, label = buildDarwin, "no state"
		}
		if each.got != want[build][each.name] {
			test.Errorf("%s %03b, %s: %s, plain %s", label, each.build, each.name, each.got, want[build][each.name])
		}
	}
}

// spellTypeSet spells a named interface's methods, as its type set has them, and its method set.
func spellTypeSet(typed types.Type) string {
	iface := typed.Underlying().(*types.Interface)
	spelled := ""
	for index := range iface.NumMethods() {
		spelled += iface.Method(index).Name() + " "
	}
	return spelled + spellMethodSet(typed)
}
