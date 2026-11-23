// This file is cohere's, not upstream's: a universe that reads each package it imports whole, for callers that check
// many packages at once over one universe. The lazy reader (lazy.go) shares its state and its package readers' caches
// with no lock of its own, so checks that run at once must not read through it. A universe made with Eager set reads,
// before Import returns a package, every object of it and every type those reach: each named type's declaration, an
// instance's underlying type and methods, an interface's type set. After that its checks read only decoded types,
// which are safe to read at once as go/types' own are.

package gcimporter

import (
	"github.com/system-inc/go-types/types"
)

// readWholeIfEager reads imported whole when the universe is eager and hasn't read it whole yet.
func (universe *Universe) readWholeIfEager(imported *types.Package) {
	if !universe.Eager || imported == nil || universe.wholePackages[imported] {
		return
	}
	universe.wholePackages[imported] = true
	scope := imported.Scope()
	for _, name := range scope.Names() {
		universe.walk(scope.Lookup(name).Type())
	}
}

// walk reads reachedType and every type it reaches, each once.
func (universe *Universe) walk(reachedType types.Type) {
	if reachedType == nil || universe.walked[reachedType] {
		return
	}
	universe.walked[reachedType] = true
	switch concreteType := reachedType.(type) {
	case *types.Basic:
		// A basic type reaches nothing.
	case *types.Pointer:
		universe.walk(concreteType.Elem())
	case *types.Slice:
		universe.walk(concreteType.Elem())
	case *types.Array:
		universe.walk(concreteType.Elem())
	case *types.Map:
		universe.walk(concreteType.Key())
		universe.walk(concreteType.Elem())
	case *types.Chan:
		universe.walk(concreteType.Elem())
	case *types.Struct:
		for index := range concreteType.NumFields() {
			universe.walk(concreteType.Field(index).Type())
		}
	case *types.Tuple:
		// Len and At take a nil tuple, a signature's empty parameters or results.
		for index := range concreteType.Len() {
			universe.walk(concreteType.At(index).Type())
		}
	case *types.Signature:
		if receiver := concreteType.Recv(); receiver != nil {
			universe.walk(receiver.Type())
		}
		universe.walkTypeParameters(concreteType.TypeParams())
		universe.walkTypeParameters(concreteType.RecvTypeParams())
		universe.walk(concreteType.Params())
		universe.walk(concreteType.Results())
	case *types.Interface:
		// NumMethods computes the type set now if the reader left it to its first use.
		for index := range concreteType.NumMethods() {
			universe.walk(concreteType.Method(index).Type())
		}
		for index := range concreteType.NumEmbeddeds() {
			universe.walk(concreteType.EmbeddedType(index))
		}
	case *types.Union:
		for index := range concreteType.Len() {
			universe.walk(concreteType.Term(index).Type())
		}
	case *types.TypeParam:
		universe.walk(concreteType.Constraint())
	case *types.Alias:
		universe.walkTypeParameters(concreteType.TypeParams())
		universe.walkTypeList(concreteType.TypeArgs())
		universe.walk(concreteType.Origin())
		universe.walk(concreteType.Rhs())
	case *types.Named:
		// Underlying runs the named type's loader, which reads its declaration; an instance's underlying type and
		// methods are expanded.
		universe.walkTypeParameters(concreteType.TypeParams())
		universe.walkTypeList(concreteType.TypeArgs())
		universe.walk(concreteType.Origin())
		universe.walk(concreteType.Underlying())
		for index := range concreteType.NumMethods() {
			universe.walk(concreteType.Method(index).Type())
		}
	}
}

// walkTypeParameters walks each of parameters' constraints.
func (universe *Universe) walkTypeParameters(parameters *types.TypeParamList) {
	for index := range parameters.Len() {
		universe.walk(parameters.At(index))
	}
}

// walkTypeList walks each of list's types.
func (universe *Universe) walkTypeList(list *types.TypeList) {
	for index := range list.Len() {
		universe.walk(list.At(index))
	}
}
