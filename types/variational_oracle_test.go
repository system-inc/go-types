package types_test

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/system-inc/go-types/types"
)

// The accessor oracle. The projection tests compare what the Info maps of a variational check hold with each build's
// plain check, entry for entry. This compares what the objects and types those entries hand over then answer: every
// exported accessor of every object and type the maps reach, and of everything those accessors return in turn, read
// as the build, against the same accessor of the plain check's own object. An accessor is found by reflection, so
// one added later is compared without anyone listing it; the few that take an argument are listed (unreflected,
// asked).
//
// A scope's children are the one answer compared otherwise (children). The package scope's, the file scopes, are
// compared place by place like any list: they are in the order the checker was given the files, in both checks, and
// a reader binds to it. Below the package's scope the children are compared as a set, each matched to the plain
// check's by the node its Scopes names it for, since the two checks make their scopes in different orders: a plain
// check in the order it happens to check its declarations in, which is held to nothing, and a variational check in
// the order of one run over every build. The variational check's are then held to the order they are written in, the
// order of their nodes' positions, which is what the fork gives a reader. Compared place by place they differed in
// two constructs, both read as windows: a method declared again in each build's own file (methods), whose scopes are
// made in the order of the first file that declares it, and a method on an alias whose target is another type on
// windows (alias targets), whose windows scope is made after the scopes of the declarations written after it.

// oracleDenied are the accessors reflection finds and the oracle doesn't call, each with why.
var oracleDenied = map[string]string{
	"*types.Scope.String":       "it writes the scope's address",
	"*types.Interface.Complete": "it completes the interface, which writes it",
}

// oracleFailure is one kind of disagreement: how many places it was found at, and the first.
type oracleFailure struct {
	count int
	first string
}

// accessorOracle walks a variational check's answers for one build and that build's plain check's side by side.
type accessorOracle struct {
	fileSet *token.FileSet
	// pairedGot and pairedWant hold, for each object, named type, type parameter, alias, package and scope met, the
	// one of the other side it was first met beside: the two checks have different objects, and whatever is one object
	// on one side is one on the other.
	pairedGot  map[any]oraclePair
	pairedWant map[any]oraclePair
	// walked are the pairs of values whose accessors are already compared.
	walked   map[[2]any]bool
	failures map[string]*oracleFailure
	compared int
	// asking, while an accessor that takes an argument is compared (asked), is its name: whatever disagrees in what
	// it answers, or in anything reached from that, is that one accessor's disagreement.
	asking string
	// gotNodes and wantNodes are the node each side's Scopes names each of its scopes for, which a scope's children
	// are matched by (children).
	gotNodes  map[*types.Scope]ast.Node
	wantNodes map[*types.Scope]ast.Node
	// files are every file of the fixture, the ones the build doesn't compile among them, which Eval is asked at
	// (asked).
	files []*ast.File
}

// oraclePair is the value of the other side a value was first met beside, and where.
type oraclePair struct {
	other any
	path  string
}

func newAccessorOracle(fileSet *token.FileSet) *accessorOracle {
	return &accessorOracle{fileSet: fileSet, pairedGot: map[any]oraclePair{}, pairedWant: map[any]oraclePair{},
		walked: map[[2]any]bool{}, failures: map[string]*oracleFailure{}}
}

// fail notes a disagreement of kind at path.
func (oracle *accessorOracle) fail(kind string, path string, got any, want any) {
	if oracle.asking != "" {
		kind = oracle.asking
	}
	failure := oracle.failures[kind]
	if failure == nil {
		failure = &oracleFailure{first: fmt.Sprintf("%s: read as the build %v, and the plain check answers %v", path, got,
			want)}
		oracle.failures[kind] = failure
	}
	failure.count++
}

var (
	oracleObjectType   = reflect.TypeFor[types.Object]()
	oracleConstantType = reflect.TypeFor[constant.Value]()
	oraclePosType      = reflect.TypeFor[token.Pos]()
	oracleTypesPath    = reflect.TypeFor[types.Package]().PkgPath()
)

// bearsIdentity reports whether a value of typed is one thing a reader tells from another of its kind by which one
// it is: an object, a named type, a type parameter, an alias, a package or a scope. Any other type is what it says.
func bearsIdentity(typed reflect.Type) bool {
	if typed.Implements(oracleObjectType) {
		return true
	}
	switch typed {
	case reflect.TypeFor[*types.Named](), reflect.TypeFor[*types.TypeParam](), reflect.TypeFor[*types.Alias](),
		reflect.TypeFor[*types.Package](), reflect.TypeFor[*types.Scope]():
		return true
	}
	return false
}

// place spells a position by its file and offset.
func (oracle *accessorOracle) place(pos token.Pos) string {
	if !pos.IsValid() {
		return "no position"
	}
	position := oracle.fileSet.Position(pos)
	return fmt.Sprintf("%s+%d", filepath.Base(position.Filename), position.Offset)
}

// compare requires got, the variational check's answer at path, to be want, the plain check's, and everything their
// accessors answer in turn. leaf names the accessor that returned them, for the kind of a disagreement.
func (oracle *accessorOracle) compare(path string, leaf string, got reflect.Value, want reflect.Value) {
	oracle.compared++
	if got.IsValid() && got.Kind() == reflect.Interface {
		got = got.Elem()
	}
	if want.IsValid() && want.Kind() == reflect.Interface {
		want = want.Elem()
	}
	isNil := func(value reflect.Value) bool {
		if !value.IsValid() {
			return true
		}
		switch value.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Func, reflect.Map:
			return value.IsNil()
		}
		return false
	}
	if isNil(got) || isNil(want) {
		// An empty list is no list.
		empty := func(value reflect.Value) bool {
			return isNil(value) || value.Kind() == reflect.Slice && value.Len() == 0
		}
		if empty(got) != empty(want) {
			oracle.fail(leaf+": one is nil", path, spellPresence(got), spellPresence(want))
		}
		return
	}
	if got.Type() != want.Type() {
		oracle.fail(leaf+": of another type", path, got.Type(), want.Type())
		return
	}
	if got.Type() == oraclePosType {
		gotPlace, wantPlace := oracle.place(token.Pos(got.Int())), oracle.place(token.Pos(want.Int()))
		if gotPlace != wantPlace {
			oracle.fail(leaf, path, gotPlace, wantPlace)
		}
		return
	}
	if got.Type().Implements(oracleConstantType) {
		gotValue, wantValue := got.Interface().(constant.Value), want.Interface().(constant.Value)
		gotSpelled := fmt.Sprintf("%v %s", gotValue.Kind(), gotValue.ExactString())
		if wantSpelled := fmt.Sprintf("%v %s", wantValue.Kind(), wantValue.ExactString()); gotSpelled != wantSpelled {
			oracle.fail(leaf, path, gotSpelled, wantSpelled)
		}
		return
	}
	switch got.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.String:
		format := "%v"
		if got.Kind() == reflect.String {
			format = "%q"
		}
		gotSpelled, wantSpelled := fmt.Sprintf(format, got.Interface()), fmt.Sprintf(format, want.Interface())
		if gotSpelled != wantSpelled {
			oracle.fail(leaf, path, gotSpelled, wantSpelled)
		}
	case reflect.Slice, reflect.Array:
		if got.Len() != want.Len() {
			oracle.fail(leaf+": of another length", path, got.Len(), want.Len())
			return
		}
		for index := range got.Len() {
			oracle.compare(fmt.Sprintf("%s[%d]", path, index), leaf, got.Index(index), want.Index(index))
		}
	case reflect.Func:
		// An iterator's elements, as a list.
		if isSequence(got.Type()) {
			oracle.compare(path, leaf, collectSequence(got), collectSequence(want))
		}
	case reflect.Struct:
		oracle.fields(path, got, want)
		oracle.accessors(path, got, want)
	case reflect.Pointer:
		if got.Type().Elem().PkgPath() != oracleTypesPath {
			return
		}
		gotKey, wantKey := got.Interface(), want.Interface()
		// One value on both sides, as the universe's are, answers one way.
		if gotKey == wantKey {
			return
		}
		if bearsIdentity(got.Type()) && !oracle.pair(path, leaf, got.Type(), gotKey, wantKey) {
			return
		}
		if oracle.walked[[2]any{gotKey, wantKey}] {
			return
		}
		oracle.walked[[2]any{gotKey, wantKey}] = true
		if got.Type().Elem().Kind() == reflect.Struct {
			oracle.fields(path, got.Elem(), want.Elem())
		}
		oracle.accessors(path, got, want)
		oracle.unreflected(path, gotKey, wantKey)
	}
}

// pair holds got and want, each one thing a reader tells from others of its kind, to being met beside each other
// wherever either is met, and reports whether they are.
func (oracle *accessorOracle) pair(path string, leaf string, typed reflect.Type, got any, want any) bool {
	gotPaired, gotMet := oracle.pairedGot[got]
	wantPaired, wantMet := oracle.pairedWant[want]
	switch {
	case gotMet && gotPaired.other == want:
		return true
	case gotMet && wantMet:
		oracle.fail(leaf+": another "+typed.String()+" than the one met before", path,
			"the one first met at "+gotPaired.path, "the one first met at "+wantPaired.path)
	case gotMet:
		oracle.fail(leaf+": another "+typed.String()+" than the one met before", path,
			"the one first met at "+gotPaired.path, "one not met before")
	case wantMet:
		oracle.fail(leaf+": another "+typed.String()+" than the one met before", path, "one not met before",
			"the one first met at "+wantPaired.path)
	default:
		oracle.pairedGot[got] = oraclePair{want, path}
		oracle.pairedWant[want] = oraclePair{got, path}
		return true
	}
	return false
}

// seedRoots pairs each of got's answers with want's of the same key before anything is walked, for the maps that say
// which node declares what: a scope and a declared object are then the build's own wherever an accessor answers
// them, and an accessor that answers another is the one reported.
func seedRoots[K comparable, V any](oracle *accessorOracle, name string, got map[K]V, want map[K]V,
	key func(K) string,
) {
	gotByKey := make(map[string]V, len(got))
	for node, answer := range got {
		gotByKey[key(node)] = answer
	}
	nodes := slices.SortedFunc(maps.Keys(want), func(left K, right K) int {
		return strings.Compare(key(left), key(right))
	})
	for _, node := range nodes {
		gotAnswer, held := gotByKey[key(node)]
		gotValue, wantValue := reflect.ValueOf(gotAnswer), reflect.ValueOf(want[node])
		if !held || !gotValue.IsValid() || !wantValue.IsValid() || gotValue.Type() != wantValue.Type() ||
			gotValue.Kind() != reflect.Pointer || gotValue.IsNil() || wantValue.IsNil() ||
			gotValue.Interface() == wantValue.Interface() {
			continue
		}
		oracle.pair(name+"["+key(node)+"]", "Info."+name, gotValue.Type(), gotValue.Interface(), wantValue.Interface())
	}
}

// spellPresence spells whether a value is there.
func spellPresence(value reflect.Value) string {
	if !value.IsValid() {
		return "nil"
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Func, reflect.Map:
		if value.IsNil() {
			return "nil"
		}
	}
	if value.Kind() == reflect.Slice {
		return fmt.Sprintf("a list of %d", value.Len())
	}
	return "a " + value.Type().String()
}

// isSequence reports whether typed is an iterator of single values.
func isSequence(typed reflect.Type) bool {
	if typed.NumIn() != 1 || typed.NumOut() != 0 || typed.In(0).Kind() != reflect.Func {
		return false
	}
	yield := typed.In(0)
	return yield.NumIn() == 1 && yield.NumOut() == 1 && yield.Out(0).Kind() == reflect.Bool
}

// collectSequence is the elements of sequence, an iterator, as a list.
func collectSequence(sequence reflect.Value) reflect.Value {
	collected := reflect.MakeSlice(reflect.SliceOf(sequence.Type().In(0).In(0)), 0, 0)
	for element := range sequence.Seq() {
		collected = reflect.Append(collected, element)
	}
	return collected
}

// fields compares the exported fields of two structs.
func (oracle *accessorOracle) fields(path string, got reflect.Value, want reflect.Value) {
	for index := range got.NumField() {
		if field := got.Type().Field(index); field.IsExported() {
			oracle.compare(path+"."+field.Name, got.Type().String()+"."+field.Name, got.Field(index), want.Field(index))
		}
	}
}

// accessors compares what each exported accessor of got and want answers: every method that takes nothing and
// returns something, but those oracleDenied names.
func (oracle *accessorOracle) accessors(path string, got reflect.Value, want reflect.Value) {
	typed := got.Type()
	for index := range typed.NumMethod() {
		method := typed.Method(index)
		leaf := typed.String() + "." + method.Name
		if method.Type.NumIn() != 1 || method.Type.NumOut() == 0 || oracleDenied[leaf] != "" {
			continue
		}
		// The children of a scope below the package's are compared as a set, and held to their order apart.
		if leaf == "*types.Scope.Children" {
			gotScope, wantScope := got.Interface().(*types.Scope), want.Interface().(*types.Scope)
			if gotScope.Parent() != types.Universe {
				oracle.children(path+".Children()", gotScope, wantScope)
				continue
			}
		}
		gotResults, gotPanic := callAccessor(got.Method(index))
		wantResults, wantPanic := callAccessor(want.Method(index))
		if gotPanic != nil || wantPanic != nil {
			if (gotPanic == nil) != (wantPanic == nil) {
				oracle.fail(leaf+": one panics", path+"."+method.Name+"()", fmt.Sprint(gotPanic), fmt.Sprint(wantPanic))
			}
			continue
		}
		for result := range gotResults {
			oracle.compare(path+"."+method.Name+"()", leaf, gotResults[result], wantResults[result])
		}
	}
}

// children compares the children of got and want, a scope below the package's in each check, as a set: each child
// is matched to the other side's by the node its own side's Scopes names it for, never by its place among them, and
// the two are then compared like any answer. Apart from that it holds got's, the variational check's, to the order of
// their nodes' positions. A plain check's own order is held to nothing.
func (oracle *accessorOracle) children(path string, got *types.Scope, want *types.Scope) {
	const leaf = "*types.Scope.Children"
	// byNode is scope's children by their nodes. spell puts what is wrong with one of them on its own side of a
	// disagreement, and the other side's children are spelled as sound.
	byNode := func(scope *types.Scope, nodes map[*types.Scope]ast.Node, isGot bool) map[string]*types.Scope {
		spell := func(wrong string) (string, string) {
			if isGot {
				return wrong, "children its Scopes names, one of a node"
			}
			return "children its Scopes names, one of a node", wrong
		}
		children := map[string]*types.Scope{}
		var before ast.Node
		for child := range scope.Children() {
			node := nodes[child]
			if node == nil {
				gotSpelled, wantSpelled := spell("a child at " + oracle.place(child.Pos()) + " its Scopes doesn't name")
				oracle.fail(leaf+": a child the check's Scopes doesn't name", path, gotSpelled, wantSpelled)
				continue
			}
			key := nodeKey(oracle.fileSet, node)
			if children[key] != nil {
				gotSpelled, wantSpelled := spell("two children of the node at " + key)
				oracle.fail(leaf+": two children of one node", path, gotSpelled, wantSpelled)
			}
			children[key] = child
			if isGot && before != nil && node.Pos() < before.Pos() {
				oracle.fail(leaf+": out of position order", path,
					"the child of "+key+" after the child of "+nodeKey(oracle.fileSet, before),
					"in an order of its own, which is held to nothing")
			}
			before = node
		}
		return children
	}
	gotChildren, wantChildren := byNode(got, oracle.gotNodes, true), byNode(want, oracle.wantNodes, false)
	keys := slices.Concat(slices.Collect(maps.Keys(gotChildren)), slices.Collect(maps.Keys(wantChildren)))
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		oracle.compare(path+"["+key+"]", leaf, reflect.ValueOf(gotChildren[key]), reflect.ValueOf(wantChildren[key]))
	}
}

// callAccessor calls accessor, and answers what it panicked with when it did.
func callAccessor(accessor reflect.Value) (results []reflect.Value, panicked any) {
	defer func() { panicked = recover() }()
	return accessor.Call(nil), nil
}

// unreflected compares what reflection can't call of got and want, since each takes an argument: a struct's tags, a
// scope's object of each name either side has, and a named type's method set and its pointer's.
func (oracle *accessorOracle) unreflected(path string, got any, want any) {
	switch got := got.(type) {
	case *types.Struct:
		want := want.(*types.Struct)
		for index := range min(got.NumFields(), want.NumFields()) {
			oracle.compare(fmt.Sprintf("%s.Tag(%d)", path, index), "*types.Struct.Tag", reflect.ValueOf(got.Tag(index)),
				reflect.ValueOf(want.Tag(index)))
		}
	case *types.Scope:
		want := want.(*types.Scope)
		names := slices.Concat(got.Names(), want.Names())
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			oracle.compare(fmt.Sprintf("%s.Lookup(%q)", path, name), "*types.Scope.Lookup",
				reflect.ValueOf(&[]types.Object{got.Lookup(name)}[0]).Elem(),
				reflect.ValueOf(&[]types.Object{want.Lookup(name)}[0]).Elem())
		}
	case *types.Named:
		want := want.(*types.Named)
		oracle.methodSets("NewMethodSet("+path+")", "types.NewMethodSet", got, want)
		oracle.methodSets("NewMethodSet(*"+path+")", "types.NewMethodSet of a pointer", types.NewPointer(got),
			types.NewPointer(want))
	}
}

// methodSets compares the method sets of got and want, or whether making each panics.
func (oracle *accessorOracle) methodSets(path string, leaf string, got types.Type, want types.Type) {
	methodSet := func(typed types.Type) (methodSet *types.MethodSet, panicked any) {
		defer func() { panicked = recover() }()
		return types.NewMethodSet(typed), nil
	}
	gotSet, gotPanic := methodSet(got)
	wantSet, wantPanic := methodSet(want)
	if gotPanic != nil || wantPanic != nil {
		if (gotPanic == nil) != (wantPanic == nil) {
			oracle.fail(leaf+": one panics", path, fmt.Sprint(gotPanic), fmt.Sprint(wantPanic))
		}
		return
	}
	oracle.compare(path, leaf, reflect.ValueOf(gotSet), reflect.ValueOf(wantSet))
}

// roots compares one Info map's answers, each named by key, entry for entry in the keys' order.
func compareRoots[K comparable, V any](oracle *accessorOracle, name string, got map[K]V, want map[K]V,
	key func(K) string,
) {
	keyed := func(answers map[K]V) map[string]V {
		byKey := make(map[string]V, len(answers))
		for node, answer := range answers {
			byKey[key(node)] = answer
		}
		return byKey
	}
	gotByKey, wantByKey := keyed(got), keyed(want)
	keys := slices.Collect(maps.Keys(wantByKey))
	slices.Sort(keys)
	for _, each := range keys {
		gotAnswer, held := gotByKey[each]
		if !held {
			oracle.fail("Info."+name+": no entry", name+"["+each+"]", "no entry", "an entry")
			continue
		}
		wantAnswer := wantByKey[each]
		oracle.compare(name+"["+each+"]", "Info."+name, reflect.ValueOf(&gotAnswer).Elem(),
			reflect.ValueOf(&wantAnswer).Elem())
	}
}

// oracleSide is one check's answers for one build: its package and every Info map. held is the Info as the check's
// caller holds it, whose own methods a reader calls: the plain check's own, and for a variational check the one it
// was given, which holds the host's answers whatever build reads it.
type oracleSide struct {
	checked   *types.Package
	info      *types.Info
	initOrder []*types.Initializer
	held      *types.Info
}

// compareSides compares everything got, a variational check's answers projected to one build, holds with want, that
// build's plain check's: each Info map's entries, the initialization order, and the package.
func (oracle *accessorOracle) compareSides(got oracleSide, want oracleSide) {
	nodeKeyOf := func(node ast.Node) string { return nodeKey(oracle.fileSet, node) }
	identKeyOf := func(ident *ast.Ident) string { return nodeKeyOf(ident) }
	oracle.gotNodes = make(map[*types.Scope]ast.Node, len(got.info.Scopes))
	for node, scope := range got.info.Scopes {
		oracle.gotNodes[scope] = node
	}
	oracle.wantNodes = make(map[*types.Scope]ast.Node, len(want.info.Scopes))
	for node, scope := range want.info.Scopes {
		oracle.wantNodes[scope] = node
	}
	seedRoots(oracle, "Scopes", got.info.Scopes, want.info.Scopes, nodeKeyOf)
	seedRoots(oracle, "Defs", got.info.Defs, want.info.Defs, identKeyOf)
	seedRoots(oracle, "Implicits", got.info.Implicits, want.info.Implicits, nodeKeyOf)
	compareRoots(oracle, "Scopes", got.info.Scopes, want.info.Scopes, nodeKeyOf)
	compareRoots(oracle, "Defs", got.info.Defs, want.info.Defs, identKeyOf)
	compareRoots(oracle, "Uses", got.info.Uses, want.info.Uses, identKeyOf)
	compareRoots(oracle, "Implicits", got.info.Implicits, want.info.Implicits, nodeKeyOf)
	compareRoots(oracle, "Selections", got.info.Selections, want.info.Selections,
		func(selector *ast.SelectorExpr) string { return nodeKeyOf(selector) })
	// An expression is keyed by its place and text, never by its node, which a checker may make afresh.
	compareRoots(oracle, "Types", got.info.Types, want.info.Types,
		func(expression ast.Expr) string { return positionKey(oracle.fileSet, expression) })
	compareRoots(oracle, "Instances", got.info.Instances, want.info.Instances, identKeyOf)
	oracle.compare("InitOrder", "Info.InitOrder", reflect.ValueOf(got.initOrder), reflect.ValueOf(want.initOrder))
	oracle.compare("Package", "Package", reflect.ValueOf(got.checked), reflect.ValueOf(want.checked))
	oracle.asked(got, want)
}

// asked compares what the accessors that take a node or a position answer, which reflection can't call, each asked
// of both sides with the plain check's own nodes: Info's TypeOf of each expression, ObjectOf of each identifier and
// PkgNameOf of each import, and the package scope's Innermost at each identifier. They come last, so what they
// answer is held to the objects and scopes the build's own maps name. Eval is asked too, of a literal at the package
// clause of every file of the fixture: in a file the build doesn't compile the position is none of its package's,
// and Eval says so of both.
func (oracle *accessorOracle) asked(got oracleSide, want oracleSide) {
	object := func(object types.Object) reflect.Value { return reflect.ValueOf(&object).Elem() }
	defer func() { oracle.asking = "" }()
	oracle.asking = "*types.Info.TypeOf"
	expressions := slices.SortedFunc(maps.Keys(want.held.Types), func(left ast.Expr, right ast.Expr) int {
		return strings.Compare(positionKey(oracle.fileSet, left), positionKey(oracle.fileSet, right))
	})
	// An expression is asked of each side by that side's own node at its place: a checker makes some nodes afresh.
	ownNodes := make(map[string]ast.Expr, len(got.info.Types))
	for expression := range got.info.Types {
		ownNodes[positionKey(oracle.fileSet, expression)] = expression
	}
	for _, expression := range expressions {
		var gotType types.Type
		if own := ownNodes[positionKey(oracle.fileSet, expression)]; own != nil {
			gotType = got.held.TypeOf(own)
		}
		wantType := want.held.TypeOf(expression)
		oracle.compare("Info.TypeOf("+positionKey(oracle.fileSet, expression)+")", "*types.Info.TypeOf",
			reflect.ValueOf(&gotType).Elem(), reflect.ValueOf(&wantType).Elem())
	}
	idents := slices.Concat(slices.Collect(maps.Keys(want.held.Defs)), slices.Collect(maps.Keys(want.held.Uses)))
	slices.SortFunc(idents, func(left *ast.Ident, right *ast.Ident) int { return int(left.Pos() - right.Pos()) })
	for _, ident := range idents {
		key := nodeKey(oracle.fileSet, ident)
		oracle.asking = "*types.Info.ObjectOf"
		oracle.compare("Info.ObjectOf("+key+")", "*types.Info.ObjectOf", object(got.held.ObjectOf(ident)),
			object(want.held.ObjectOf(ident)))
		oracle.asking = "*types.Scope.Innermost"
		oracle.compare("Package.Scope().Innermost("+key+")", "*types.Scope.Innermost",
			reflect.ValueOf(got.checked.Scope().Innermost(ident.Pos())),
			reflect.ValueOf(want.checked.Scope().Innermost(ident.Pos())))
	}
	oracle.asking = "*types.Info.PkgNameOf"
	specs := slices.SortedFunc(maps.Keys(want.held.Implicits), func(left ast.Node, right ast.Node) int {
		return int(left.Pos() - right.Pos())
	})
	for _, node := range specs {
		if spec, isImport := node.(*ast.ImportSpec); isImport {
			oracle.compare("Info.PkgNameOf("+nodeKey(oracle.fileSet, spec)+")", "*types.Info.PkgNameOf",
				object(got.held.PkgNameOf(spec)), object(want.held.PkgNameOf(spec)))
		}
	}
	oracle.asking = "types.Eval"
	evaluated := func(checked *types.Package, position token.Pos) reflect.Value {
		answer, evalError := types.Eval(oracle.fileSet, checked, position, "0")
		if evalError != nil {
			return reflect.ValueOf("error: " + evalError.Error())
		}
		return reflect.ValueOf(answer.Type.String())
	}
	for _, file := range oracle.files {
		position := file.Name.Pos()
		oracle.compare("Eval(0 at "+oracle.place(position)+")", "types.Eval", evaluated(got.checked, position),
			evaluated(want.checked, position))
	}
}

// oracleTotals are the oracle's findings over every fixture, by kind.
type oracleTotals struct {
	guard    sync.Mutex
	compared int
	kinds    map[string]*oracleFailure
	fixtures map[string][]string
}

// TestVariationalAccessorsAnswerAsEachBuildsPlainCheck is the accessor oracle over every projection fixture: each
// fixture is checked once for every build with every Info map, and each build alone the plain way, and then every
// exported accessor of everything the build's projection reaches, read under InBuild for the build, must answer what
// the same accessor of the plain check's answers, and the host's read with no state as well. A disagreement is
// reported by its kind, the accessor it was found at, once for each fixture with how many places hold it and the
// first of them.
func TestVariationalAccessorsAnswerAsEachBuildsPlainCheck(test *testing.T) {
	test.Parallel()
	totals := &oracleTotals{kinds: map[string]*oracleFailure{}, fixtures: map[string][]string{}}
	test.Run("fixtures", func(test *testing.T) {
		for name, fixtures := range projectionFixtures {
			test.Run(name, func(test *testing.T) {
				test.Parallel()
				runAccessorOracle(test, name, fixtures, totals)
			})
		}
	})
	kinds := slices.Sorted(maps.Keys(totals.kinds))
	test.Logf("%d answers compared; %d kinds of disagreement", totals.compared, len(kinds))
	for _, kind := range kinds {
		slices.Sort(totals.fixtures[kind])
		test.Logf("%s: %d places, in %s; first, %s", kind, totals.kinds[kind].count,
			strings.Join(totals.fixtures[kind], ", "), totals.kinds[kind].first)
	}
}

// runAccessorOracle runs the oracle over one fixture, every build, and adds what it finds to totals.
func runAccessorOracle(test *testing.T, name string, fixtures []fixtureFile, totals *oracleTotals) {
	fileSet := token.NewFileSet()
	parsed := parseFixture(test, fileSet, fixtures)
	variation := &types.Variation{Builds: everyBuild, Files: parsed.masks}
	info := fullInfo()
	// A fixture may report errors, as the abandoned errors fixture does; what was checked is compared all the same.
	checked, _ := (&types.Config{Variation: variation, Importer: fixtureImporter{}, Error: func(error) {}}).Check("fork",
		fileSet, parsed.files, info)
	failures := map[string]*oracleFailure{}
	compared := 0
	for index, build := range []types.Mask{buildDarwin, buildLinux, buildWindows} {
		plainInfo := fullInfo()
		plainPackage, _ := (&types.Config{Importer: fixtureImporter{build}, Error: func(error) {}}).Check("fork", fileSet,
			parsed.filesIn(build), plainInfo)
		overlays := types.BuildOverlays{}
		if index > 0 {
			overlays = variation.Overlays[index]
		}
		projected := oracleSide{checked: checked, initOrder: info.InitOrder, held: info, info: &types.Info{
			Types:      project(info.Types, overlays.Types),
			Defs:       project(info.Defs, overlays.Defs),
			Uses:       project(info.Uses, overlays.Uses),
			Implicits:  project(info.Implicits, overlays.Implicits),
			Selections: project(info.Selections, overlays.Selections),
			Instances:  project(info.Instances, overlays.Instances),
			Scopes:     project(info.Scopes, overlays.Scopes),
		}}
		if overlays.InitOrder != nil {
			projected.initOrder = overlays.InitOrder
		}
		plain := oracleSide{checked: plainPackage, info: plainInfo, initOrder: plainInfo.InitOrder, held: plainInfo}
		views := map[string]func(work func()){
			fmt.Sprintf("under InBuild %03b", build): func(work func()) { types.InBuild(build, work) },
		}
		if build == buildDarwin {
			views["with no state, as the host"] = func(work func()) { work() }
		}
		for _, view := range slices.Sorted(maps.Keys(views)) {
			oracle := newAccessorOracle(fileSet)
			oracle.files = parsed.files
			views[view](func() { oracle.compareSides(projected, plain) })
			if oracle.compared < 100 {
				test.Errorf("%s: the oracle compared %d answers, and the smallest fixture holds more", view, oracle.compared)
			}
			compared += oracle.compared
			for kind, failure := range oracle.failures {
				if failures[kind] == nil {
					failures[kind] = &oracleFailure{first: view + ", " + failure.first}
				}
				failures[kind].count += failure.count
			}
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(failures)) {
		test.Errorf("%s: %d places; first, %s", kind, failures[kind].count, failures[kind].first)
	}
	totals.guard.Lock()
	defer totals.guard.Unlock()
	totals.compared += compared
	for kind, failure := range failures {
		if totals.kinds[kind] == nil {
			totals.kinds[kind] = &oracleFailure{first: name + ", " + failure.first}
		}
		totals.kinds[kind].count += failure.count
		totals.fixtures[kind] = append(totals.fixtures[kind], name)
	}
}
