package gcimporter_test

import (
	"bytes"
	"encoding/json"
	"go/token"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/system-inc/go-types/gcimporter"
	"github.com/system-inc/go-types/types"
)

// exportFiles is the export data file of path and of every package it depends on, as this toolchain builds them.
func exportFiles(test *testing.T, path string) map[string]string {
	test.Helper()
	command := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export", path)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = os.Stderr
	if runError := command.Run(); runError != nil {
		test.Fatalf("go list -export %s: %v", path, runError)
	}
	files := map[string]string{}
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var listed struct{ ImportPath, Export string }
		if decodeError := decoder.Decode(&listed); decodeError != nil {
			test.Fatal(decodeError)
		}
		if listed.Export != "" {
			files[listed.ImportPath] = listed.Export
		}
	}
	return files
}

// readAtOnce imports path into universe, then reads every object of it from many goroutines at once: each object's
// type, and a named type's underlying type and methods, the reads a check makes of an imported package.
func readAtOnce(test *testing.T, universe *gcimporter.Universe, path string) {
	test.Helper()
	files := exportFiles(test, path)
	lookup := func(imported string) (io.ReadCloser, error) { return os.Open(files[imported]) }
	imported, importError := gcimporter.Import(token.NewFileSet(), universe, path, "", lookup)
	if importError != nil {
		test.Fatal(importError)
	}
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			scope := imported.Scope()
			for _, name := range scope.Names() {
				object := scope.Lookup(name)
				if named, isNamed := types.Unalias(object.Type()).(*types.Named); isNamed {
					_ = named.Underlying()
					for index := range named.NumMethods() {
						_ = named.Method(index).Type()
					}
				}
			}
		}()
	}
	group.Wait()
}

// TestEagerUniverseIsReadAtOnce reads net/http, whole at its import, from sixteen goroutines at once. Under the race
// detector it is green only because the eager universe read it whole: a lazy universe's reads share its state, and
// with Eager left false the same reads race (5 races and a panic, Nov 23).
func TestEagerUniverseIsReadAtOnce(test *testing.T) {
	test.Parallel()
	universe := gcimporter.NewUniverse()
	universe.Eager = true
	readAtOnce(test, universe, "net/http")
}
