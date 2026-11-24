package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// importedInterfaceSources are a package's files by build: I embeds J, whose method M returns an int on darwin and
// linux and a string on windows, so I's type set differs by build through J's underlying type alone.
var importedInterfaceSources = map[Mask]string{
	1 << 0: "package q\n\ntype J interface{ M() int }\n\ntype I interface{ J }\n",
	1 << 1: "package q\n\ntype J interface{ M() int }\n\ntype I interface{ J }\n",
	1 << 2: "package q\n\ntype J interface{ M() string }\n\ntype I interface{ J }\n",
}

// TestVariationalAnImportedUnderlyingTypeAloneReadsTypeSetsByBuild makes I and J as the merging importer does, J one
// named type whose underlying type is set by build (SetUnderlyingVariants) and nothing else differing by build, then
// reads I's type set with no state, as the host, and under InBuild for each build, each against that build's plain
// check. variationsExist is process-wide, and any variational check sets it, so the test runs as a fresh process
// would: not in parallel, so no other test runs beside it, with the flag cleared and put back when it ends.
func TestVariationalAnImportedUnderlyingTypeAloneReadsTypeSetsByBuild(test *testing.T) {
	saved := variationsExist.Load()
	variationsExist.Store(false)
	test.Cleanup(func() { variationsExist.Store(saved) })

	importedPackage := NewPackage("example.com/q", "q")
	methodReturning := func(result Type) *Interface {
		signature := NewSignatureType(nil, nil, nil, nil, NewTuple(NewParam(token.NoPos, importedPackage, "", result)),
			false)
		return NewInterfaceType([]*Func{NewFunc(token.NoPos, importedPackage, "M", signature)}, nil).Complete()
	}
	unixInterface := methodReturning(Typ[Int])
	embedded := NewNamed(NewTypeName(token.NoPos, importedPackage, "J", nil), nil, nil)
	embedded.SetUnderlying(unixInterface)
	SetUnderlyingVariants(embedded, []UnderlyingVariant{
		{Builds: 1<<0 | 1<<1, Underlying: unixInterface},
		{Builds: 1 << 2, Underlying: methodReturning(Typ[String])},
	})
	embedding := NewInterfaceType(nil, []Type{embedded})
	NewNamed(NewTypeName(token.NoPos, importedPackage, "I", nil), embedding, nil)

	spell := func() string { return TypeString(embedding.Method(0).Type(), nil) }
	if host, plain := spell(), plainMethod(test, 1<<0); host != plain {
		test.Errorf("I's M read with no state: %s, the host's plain check has %s", host, plain)
	}
	for _, build := range []Mask{1 << 0, 1 << 1, 1 << 2} {
		var read string
		InBuild(build, func() { read = spell() })
		if plain := plainMethod(test, build); read != plain {
			test.Errorf("I's M read in build %03b: %s, its plain check has %s", build, read, plain)
		}
	}
}

// plainMethod is I's method M as build's plain check of importedInterfaceSources spells it.
func plainMethod(test *testing.T, build Mask) string {
	test.Helper()
	fileSet := token.NewFileSet()
	file, parseError := parser.ParseFile(fileSet, "q.go", importedInterfaceSources[build], 0)
	if parseError != nil {
		test.Fatal(parseError)
	}
	checked, checkError := (&Config{}).Check("example.com/q", fileSet, []*ast.File{file}, nil)
	if checkError != nil {
		test.Fatal(checkError)
	}
	embedding := checked.Scope().Lookup("I").Type().Underlying().(*Interface)
	return TypeString(embedding.Method(0).Type(), nil)
}

// importedGenericInterfaceSource is a package whose interface Compact embeds an instance of the generic interface
// Graph, so Compact's method Out is the instance's.
const importedGenericInterfaceSource = "package q\n\ntype Graph[Node comparable] interface{ Out(node Node) Node }\n\n" +
	"type Compact interface{ Graph[int] }\n"

// TestVariationalAnImportedGenericInterfacesInstanceKeepsItsMethodsOnTheInstance makes Graph and Compact as an export
// data reader does, with no checker: Graph a named type whose interface's method has Graph itself for its receiver,
// and Compact an interface embedding Graph[int], instantiated with no checker. The substitution completes the
// instance's interface before the expansion gives its methods the instance for their receiver (golang/go#61561), and
// the type set it kept by build then outlived the methods it was computed from, so in a process where anything
// differs by build Compact's Out had the generic Graph for its receiver, and a plain check's has Graph[int]. The flag
// is process-wide, so the test runs as TestVariationalAnImportedUnderlyingTypeAloneReadsTypeSetsByBuild does, with it
// cleared and then set, and put back when it ends.
func TestVariationalAnImportedGenericInterfacesInstanceKeepsItsMethodsOnTheInstance(test *testing.T) {
	saved := variationsExist.Load()
	test.Cleanup(func() { variationsExist.Store(saved) })

	fileSet := token.NewFileSet()
	file, parseError := parser.ParseFile(fileSet, "q.go", importedGenericInterfaceSource, 0)
	if parseError != nil {
		test.Fatal(parseError)
	}
	checked, checkError := (&Config{}).Check("example.com/q", fileSet, []*ast.File{file}, nil)
	if checkError != nil {
		test.Fatal(checkError)
	}
	spell := func(embedding *Interface) string {
		method := embedding.Method(0)
		return method.Name() + " of " + TypeString(method.Signature().Recv().Type(), nil)
	}
	plain := spell(checked.Scope().Lookup("Compact").Type().Underlying().(*Interface))

	for _, exist := range []bool{false, true} {
		variationsExist.Store(exist)
		importedPackage := NewPackage("example.com/q", "q")
		generic := NewNamed(NewTypeName(token.NoPos, importedPackage, "Graph", nil), nil, nil)
		node := NewTypeParam(NewTypeName(token.NoPos, importedPackage, "Node", nil), Universe.Lookup("comparable").Type())
		generic.SetTypeParams([]*TypeParam{node})
		signature := NewSignatureType(NewVar(token.NoPos, importedPackage, "", generic), nil, nil,
			NewTuple(NewParam(token.NoPos, importedPackage, "node", node)),
			NewTuple(NewParam(token.NoPos, importedPackage, "", node)), false)
		generic.SetUnderlying(NewInterfaceType([]*Func{NewFunc(token.NoPos, importedPackage, "Out", signature)}, nil).
			Complete())
		instance, instantiateError := Instantiate(nil, generic, []Type{Typ[Int]}, false)
		if instantiateError != nil {
			test.Fatal(instantiateError)
		}
		embedding := NewInterfaceType(nil, []Type{instance}).Complete()
		NewNamed(NewTypeName(token.NoPos, importedPackage, "Compact", nil), embedding, nil)

		if read := spell(embedding); read != plain {
			test.Errorf("Compact's method read with variationsExist %t: %s, a plain check has %s", exist, read, plain)
		}
	}
}
