// This file is cohere's, not upstream's (#yraxcp9): the reader reads each package-level object the first time it is
// looked up, as go1.27's types2 importer does, rather than every object of a package when the package is imported.
// The fork's InsertLazy holds each name until then. A named type's lookup gives only its type name, and its
// declaration (type parameters, underlying type and methods) is read by its loader the first time any of it is
// needed, so a lookup reads the declarations the reader actually reaches and no more.
//
// A universe is read from one goroutine at a time: its lazy entries and loaders share its state and the package
// readers' caches. A caller that reads one from many goroutines holds a lock of its own around every read.

package gcimporter

import (
	"time"

	"github.com/system-inc/go-types/types"
)

// A Universe is one build's imported packages, by path, and the state of the objects being read on demand in them.
type Universe struct {
	Packages map[string]*types.Package
	// Elapsed is the time spent reading objects and named types' declarations on demand, after their packages were
	// imported.
	Elapsed time.Duration

	depth int // how many reads are running, one inside another: only the outermost finishes them

	// aliasing are the aliases being read. An alias reached inside its own declaration is read again, as the eager
	// reader did, rather than waited on.
	aliasing map[objectKey]bool

	// later and ifaces are a read's work left for the end of the outermost read, when every named type it reached
	// has its underlying type.
	later  []func()
	ifaces []*types.Interface
}

// objectKey names a package-level object of a universe.
type objectKey struct {
	path string
	name string
}

// NewUniverse is an empty universe.
func NewUniverse() *Universe {
	return &Universe{
		Packages: map[string]*types.Package{},
		aliasing: map[objectKey]bool{},
	}
}

// resolve runs read, one object's lazy read, and finishes the work it left once the outermost read is done.
func (universe *Universe) resolve(read func() types.Object) types.Object {
	if universe.depth > 0 {
		return read()
	}
	universe.depth++
	started := time.Now()
	defer universe.ended(started)
	object := read()
	universe.finish()
	return object
}

// load runs read, a named type's loader reading its declaration. The work the read left is the loader's delayed
// work when it is the outermost read, run once the named type has its underlying type, since finishing it (a type
// parameter's constraint, an interface's completion) may read the named type itself.
func (universe *Universe) load(read func() ([]*types.TypeParam, types.Type, []*types.Func)) ([]*types.TypeParam, types.Type, []*types.Func, []func()) {
	if universe.depth > 0 {
		tparams, underlying, methods := read()
		return tparams, underlying, methods, nil
	}
	tparams, underlying, methods := universe.readOutermost(read)
	return tparams, underlying, methods, []func(){func() {
		universe.resolve(func() types.Object { return nil })
	}}
}

// readOutermost runs read as the outermost read, leaving its work for the caller to finish.
func (universe *Universe) readOutermost(read func() ([]*types.TypeParam, types.Type, []*types.Func)) ([]*types.TypeParam, types.Type, []*types.Func) {
	universe.depth++
	defer universe.ended(time.Now())
	return read()
}

// ended closes the outermost read begun at started. A read that panicked leaves its unfinished work behind, which
// refers to what it never finished making, so that work is dropped rather than left for the next read to run.
func (universe *Universe) ended(started time.Time) {
	universe.depth--
	universe.Elapsed += time.Since(started)
	if recovered := recover(); recovered != nil {
		universe.later = nil
		universe.ifaces = nil
		panic(recovered)
	}
}

// finish runs the work reads left: type parameters get their constraints, then interfaces are completed. Either can
// look up objects not read yet, whose reads leave work of their own, so both run again until none is left.
func (universe *Universe) finish() {
	for len(universe.later) > 0 || len(universe.ifaces) > 0 {
		for len(universe.later) > 0 {
			later := universe.later
			universe.later = nil
			for _, run := range later {
				run()
			}
		}
		ifaces := universe.ifaces
		universe.ifaces = nil
		for _, iface := range ifaces {
			iface.Complete()
		}
	}
}
