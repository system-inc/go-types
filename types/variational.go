// This file is the spike's variability (#yraxcp9): one check of a package for every build at once. It is cohere's,
// not upstream's, and every other file of the fork reaches into it at a few marked sites.
//
// The model. A build set is a Mask, one bit per build. A package-level object keeps one identity across every build
// that declares it, as Go's own rule for a declared type has it (one declaration, one type), and only its properties
// may differ: a constant's value, a variable's or function's type, a named type's underlying type, and in which builds
// it exists at all. The checker runs each unit of work (a package-level declaration, a delayed action such as a
// function body) under the builds it holds in. When a read inside a unit would answer differently across those builds,
// the read panics with a split, and the unit starts again once for each group of builds that answer alike. A unit
// that never reads a differing property runs once for all of its builds, which is the shared case and nearly every
// case.
//
// Many packages are checked at once, each by its own Checker on its own goroutine, so each check holds its own state
// (variationalState): the checker reaches it directly, and the reads that never see a Checker (Named.Underlying,
// Object.Type, Const.Val, Scope.Lookup, a method's presence, a type set's memo) find it by the goroutine they run on.
// A goroutine that runs no variational check and no InBuild finds none and reads every property as the host (build 0)
// has it (hostRead). go/types
// starts no goroutine inside a check, and the merging importer resolves an object on the goroutine whose check reads
// it, so a check's reads are all on its goroutine. Threading the state through every such read instead would change
// 234 declarations and 875 calls of the fork, and every caller of go/types' API beside it (#dfnzqjb, from the
// concurrency census of #y5mh5he); this finds it on the few reads that meet a property that differs by build.

package types

import (
	"go/ast"
	"go/constant"
	"go/token"
	"iter"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
)

// Mask is a set of builds, bit b for build b. The spike holds at most eight.
type Mask uint8

// Variation is what a variational check is given and what it answers. Builds is every build the check covers, and
// Files says which of them compile each file, a file it doesn't name compiling in every one. Types is the answer when
// the caller's Info has no Types map: every expression's type and value, with the builds each holds in. With one, it
// stays nil, and Info.Types holds the host's answers and Overlays each other build's, as every other Info map.
type Variation struct {
	Builds Mask
	Files  map[*ast.File]Mask
	Types  map[ast.Expr]Answers
	// Overlays are, for each build b other than the host (build 0), the answers of the Info maps the check was given
	// in which b differs from the host. The maps themselves hold the host's answers (variational_info.go).
	Overlays []BuildOverlays

	// Counts of the check's work, for the spike's report.
	Units    int // units run to their end
	Restarts int // unit runs abandoned for a split
}

// BuildTypeAndValue is an expression's answer in Builds.
type BuildTypeAndValue struct {
	Builds Mask
	TypeAndValue
}

// Answers are an expression's answers by build. Nearly every expression has one, kept inline so the map needs no
// slice for it; the rest are in more.
type Answers struct {
	first      BuildTypeAndValue
	firstOrder int64
	more       []orderedAnswer
}

// orderedAnswer is an answer with the order it was written in (see vrecord's order).
type orderedAnswer struct {
	BuildTypeAndValue
	order int64
}

// Len is how many answers there are.
func (answers Answers) Len() int {
	if answers.first.Builds == 0 {
		return 0
	}
	return 1 + len(answers.more)
}

// All yields each answer.
func (answers Answers) All() iter.Seq[BuildTypeAndValue] {
	return func(yield func(BuildTypeAndValue) bool) {
		if answers.first.Builds == 0 || !yield(answers.first) {
			return
		}
		for _, answer := range answers.more {
			if !yield(answer.BuildTypeAndValue) {
				return
			}
		}
	}
}

// with is answers after an answer tv written for builds with order: it takes those builds from each earlier answer
// written at an order no higher, as a plain check's later write replaces its earlier one, and an earlier answer at a
// higher order keeps its builds. answers itself is never changed, so an undo can put it back.
func (answers Answers) with(builds Mask, tv TypeAndValue, order int64) Answers {
	// The common cases: the expression's first answer, or a write again for the builds of its only answer.
	if answers.first.Builds == 0 {
		return Answers{first: BuildTypeAndValue{Builds: builds, TypeAndValue: tv}, firstOrder: order}
	}
	if len(answers.more) == 0 && answers.first.Builds&^builds == 0 && answers.firstOrder <= order {
		return Answers{first: BuildTypeAndValue{Builds: builds, TypeAndValue: tv}, firstOrder: order}
	}
	all := make([]orderedAnswer, 0, 2+len(answers.more))
	all = append(all, orderedAnswer{answers.first, answers.firstOrder})
	all = append(all, answers.more...)
	kept := all[:0:0]
	for _, earlier := range all {
		if earlier.Builds&builds != 0 {
			if earlier.order > order {
				builds &^= earlier.Builds
			} else {
				earlier.Builds &^= builds
			}
		}
		if earlier.Builds != 0 {
			kept = append(kept, earlier)
		}
	}
	if builds != 0 {
		folded := false
		for index := range kept {
			if kept[index].TypeAndValue == tv {
				kept[index].Builds |= builds
				kept[index].order = max(kept[index].order, order)
				folded = true
				break
			}
		}
		if !folded {
			kept = append(kept, orderedAnswer{BuildTypeAndValue{Builds: builds, TypeAndValue: tv}, order})
		}
	}
	if len(kept) == 0 {
		return Answers{}
	}
	result := Answers{first: kept[0].BuildTypeAndValue, firstOrder: kept[0].order}
	if len(kept) > 1 {
		result.more = kept[1:]
	}
	return result
}

// variationalState is the state of one variational check in progress, held by its Checker, or of a read as one
// build after a check has ended (InBuild).
type variationalState struct {
	all   Mask
	mask  Mask // the builds the current unit runs under
	depth int  // how many units are running; a read outside every unit never splits
	// narrowed is how many reads have answered for the current unit's builds alone. It is atomic because hostRead is
	// one state for every goroutine that reads with none of its own.
	narrowed  atomic.Uint64
	variation *Variation
	info      infoLogs // the Info maps' answers by build until the check ends (variational_info.go)
	// outer is the check this goroutine was running when this one began, put back when this one ends: an importer
	// may check a package from source in the middle of another check.
	outer *variationalState
	// inBuild is set for a read as one build (InBuild and hostRead), never for a check: a scope's names and a named
	// type's methods then leave out what the build doesn't have.
	inBuild bool
	// deps are what each dependency's builds were before a write inside a unit that could restart (addVariationalDep).
	deps []depUndo
	// merges counts the declarations mergeVariant folded into an earlier object of their name, so each has its own
	// rank in source order.
	merges uint64
	// errorBuilds, when set, are the builds an error reported outside every unit holds in: initOrderByBuild's, which
	// reports an initialization cycle for each build it orders.
	errorBuilds Mask
	// imports are each build's imports, in the order its files meet them (noteImport).
	imports [8][]*Package
	// building are the type sets this goroutine is computing for one build each (typeSetIn), so an interface that
	// reaches itself finds its set in the making, as tset is for a check.
	building map[buildInterface]*_TypeSet
}

// buildInterface is an interface's type set in one build.
type buildInterface struct {
	iface *Interface
	build int
}

// buildTypeSets are an interface's type set in each build, each written once, for an interface whose checker is gone.
// alike is set once a set was computed reading nothing that differs by build, and then every build has it.
type buildTypeSets struct {
	sets  [8]atomic.Pointer[_TypeSet]
	alike atomic.Bool
}

// typeSetState is the state a type set read on check's behalf answers under, by build: check's own when it runs a
// variational check, else the goroutine's, or hostRead for a goroutine with none.
func typeSetState(check *Checker) *variationalState {
	if check != nil && check.variational != nil {
		return check.variational
	}
	if state := runningState(); state != nil {
		return state
	}
	return hostRead
}

// typeSetByBuild is ityp's type set in state's builds, splitting a unit whose builds have different sets. Read outside
// every unit, it is the first build's. A set that differs by build is a narrowed read, as an object's answer is.
func (ityp *Interface) typeSetByBuild(pos token.Pos, state *variationalState) *_TypeSet {
	sets := ityp.byBuild.Load()
	if sets == nil {
		sets = &buildTypeSets{}
		if !ityp.byBuild.CompareAndSwap(nil, sets) {
			sets = ityp.byBuild.Load()
		}
	}
	if sets.alike.Load() {
		return sets.sets[0].Load()
	}
	mask := state.mask
	if state.depth == 0 {
		mask &= -mask
	}
	if state == hostRead {
		// hostRead is every goroutine's, so a set not yet computed is computed under a state of this goroutine's own.
		if set := sets.sets[0].Load(); set != nil {
			if !sets.alike.Load() {
				state.narrowed.Add(1)
			}
			return set
		}
		var set *_TypeSet
		InBuild(1, func() { set = ityp.typeSetByBuild(pos, runningState()) })
		if !sets.alike.Load() {
			hostRead.narrowed.Add(1)
		}
		return set
	}
	var parts []Mask
	var partSets []*_TypeSet
	for build := range 8 {
		if mask&(1<<build) == 0 {
			continue
		}
		set := ityp.typeSetIn(pos, build, sets, state)
		index := slices.IndexFunc(partSets, func(other *_TypeSet) bool { return sameTypeSet(other, set) })
		if index < 0 {
			parts = append(parts, 0)
			partSets = append(partSets, set)
			index = len(parts) - 1
		}
		parts[index] |= 1 << build
	}
	if !sets.alike.Load() {
		state.narrowed.Add(1)
	}
	if len(parts) > 1 {
		splitAt(state, parts)
	}
	return partSets[0]
}

// typeSetIn is ityp's type set in build, computed under state narrowed to build when no goroutine has kept it yet. A
// set computed reading nothing that differs by build is every build's.
func (ityp *Interface) typeSetIn(pos token.Pos, build int, sets *buildTypeSets, state *variationalState) *_TypeSet {
	if set := sets.sets[build].Load(); set != nil {
		return set
	}
	key := buildInterface{ityp, build}
	if set := state.building[key]; set != nil {
		return set
	}
	savedMask, savedDepth := state.mask, state.depth
	state.mask, state.depth = 1<<build, max(savedDepth, 1)
	set := &_TypeSet{terms: allTermlist}
	if state.building == nil {
		state.building = map[buildInterface]*_TypeSet{}
	}
	state.building[key] = set
	defer func() {
		delete(state.building, key)
		state.mask, state.depth = savedMask, savedDepth
	}()
	before := state.narrowed.Load()
	fillInterfaceTypeSet(nil, pos, ityp, set)
	if state.narrowed.Load() == before {
		for each := range sets.sets {
			sets.sets[each].CompareAndSwap(nil, set)
		}
		sets.alike.Store(true)
		return sets.sets[build].Load()
	}
	if !sets.sets[build].CompareAndSwap(nil, set) {
		set = sets.sets[build].Load()
	}
	return set
}

// settleTypeSets gives iface, an interface of check's leaving it, its type set in each of check's builds, and keeps it
// in tset as well when every build has one.
func (check *Checker) settleTypeSets(iface *Interface) {
	if iface.tset != nil || !iface.complete {
		return
	}
	state := check.variational
	sets := &buildTypeSets{}
	iface.byBuild.Store(sets)
	for build := range 8 {
		if state.all&(1<<build) != 0 {
			iface.typeSetIn(nopos, build, sets, state)
		}
	}
	if sets.alike.Load() {
		iface.tset = sets.sets[0].Load()
	}
}

// sameTypeSet reports whether two type sets are one: the same methods, comparability and terms.
func sameTypeSet(left *_TypeSet, right *_TypeSet) bool {
	return left == right || slices.Equal(left.methods, right.methods) && left.comparable == right.comparable &&
		left.terms.equal(right.terms)
}

// noteImport adds imported to the imports of each of builds that hasn't met it yet.
func (state *variationalState) noteImport(imported *Package, builds Mask) {
	for build := range 8 {
		if builds&(1<<build) != 0 && !slices.Contains(state.imports[build], imported) {
			state.imports[build] = append(state.imports[build], imported)
		}
	}
}

// endImports gives the checked package the host's (build 0's) imports, and each other build whose imports differ its
// own in its overlay, as each build's plain check lists them. The package keeps every build's, which Imports answers
// under InBuild.
func (check *Checker) endImports() {
	state := check.variational
	if state.all&1 == 0 {
		return
	}
	host := state.imports[0]
	check.pkg.imports = host
	var byBuild [8][]*Package
	for build := range 8 {
		if state.all&(1<<build) == 0 {
			continue
		}
		// A list is nil only for a build the check doesn't cover, so an empty one is kept empty.
		byBuild[build] = append([]*Package{}, state.imports[build]...)
		if build > 0 && !slices.Equal(state.imports[build], host) {
			state.variation.Overlays[build].Imports = byBuild[build]
		}
	}
	check.pkg.importsByBuild = &byBuild
}

// depUndo is what one dependency write replaced, so a unit that restarts can put it back.
type depUndo struct {
	declaration *declInfo
	dependency  Object
	had         bool
	previous    Mask
}

// addVariationalDep notes that from depends on to in the builds the current unit runs under, or outside every unit
// in the builds from's file compiles in.
func (check *Checker) addVariationalDep(from *declInfo, to Object) {
	state := check.variational
	builds := state.all
	if state.depth > 0 {
		builds = state.mask
	} else if from.mask != 0 {
		builds = from.mask
	}
	previous, had := from.depBuilds[to]
	if previous&builds == builds {
		return
	}
	if state.depth > 0 {
		state.deps = append(state.deps, depUndo{from, to, had, previous})
	}
	if from.depBuilds == nil {
		from.depBuilds = map[Object]Mask{}
	}
	from.depBuilds[to] = previous | builds
}

// initOrderByBuild gives Info.InitOrder the host's (build 0's) initialization order, and each other build whose order
// differs its own, each computed as that build's plain check computes it: over the package-level objects the build
// declares, each by the declaration the build compiles, with the dependencies that build's runs found, in the build's
// own source order. An initialization cycle is reported once for each build it holds in.
func (check *Checker) initOrderByBuild() {
	state := check.variational
	var host []*Initializer
	for build := range 8 {
		mask := Mask(1) << build
		if state.all&mask == 0 {
			continue
		}
		objMap, ranks := check.objMapIn(mask)
		state.errorBuilds = mask
		order := check.initOrderOf(objMap, nil, func(obj dependency) uint64 { return ranks[obj] })
		state.errorBuilds = 0
		if build == 0 {
			host = order
			check.Info.InitOrder = append(check.Info.InitOrder, order...)
			continue
		}
		if !sameInitOrder(order, host) {
			// An overlay's order is nil only when it is the host's, so an empty one is kept empty.
			state.info.initOrders[build] = append([]*Initializer{}, order...)
		}
	}
}

// objMapIn is the package-level objects build declares, each with the declaration build compiles holding only the
// dependencies build's runs found on objects it declares, and each object's rank in build's source order.
func (check *Checker) objMapIn(build Mask) (map[Object]*declInfo, map[Object]uint64) {
	declared := map[Object]*declInfo{}
	ranks := map[Object]uint64{}
	for obj, d := range check.objMap {
		if declaration, rank := declarationIn(obj, d, build); declaration != nil {
			declared[obj] = declaration
			ranks[obj] = rank
		}
	}
	// An n:1 declaration is one declInfo for each variable on its left, and stays one.
	copies := map[*declInfo]*declInfo{}
	objMap := make(map[Object]*declInfo, len(declared))
	for obj, declaration := range declared {
		inBuild := copies[declaration]
		if inBuild == nil {
			inBuild = &declInfo{lhs: check.mergedLeftSide(declaration.lhs), init: declaration.init,
				fdecl: declaration.fdecl, mask: build}
			for dependency, builds := range declaration.depBuilds {
				if _, isDeclared := declared[dependency]; isDeclared && builds&build != 0 {
					inBuild.addDep(dependency)
				}
			}
			copies[declaration] = inBuild
		}
		objMap[obj] = inBuild
	}
	return objMap, ranks
}

// declarationIn is the declaration of obj, declared by d, that build compiles, and obj's rank in build's source
// order, or nil when build doesn't declare obj. An object's rank is its order, and a declaration folded into it from
// another build's file ranks after the objects declared before it and before the next (mergeVariant).
func declarationIn(obj Object, d *declInfo, build Mask) (*declInfo, uint64) {
	variation := variationOf(obj)
	if variation != nil && len(variation.decls) > 0 {
		for _, each := range variation.decls {
			if each.mask&build != 0 {
				return each.decl, each.rank
			}
		}
		return nil, 0
	}
	if d.mask != 0 && d.mask&build == 0 ||
		variation != nil && variation.presence != 0 && variation.presence&build == 0 {
		return nil, 0
	}
	return d, uint64(obj.order()) << 32
}

// sameInitOrder reports whether two initialization orders are one: the same variables and expressions in one order.
func sameInitOrder(left []*Initializer, right []*Initializer) bool {
	return slices.EqualFunc(left, right, func(left *Initializer, right *Initializer) bool {
		return left.Rhs == right.Rhs && slices.Equal(left.Lhs, right.Lhs)
	})
}

// register makes state the one reads on this goroutine answer under, keeping the one it replaces to put back.
func (state *variationalState) register() {
	goroutine := goroutineIdentity()
	if outer, running := checksByGoroutine.Load(goroutine); running {
		state.outer = outer.(*variationalState)
	}
	checksByGoroutine.Store(goroutine, state)
	runningChecks.Add(1)
}

// unregister puts back the state register replaced.
func (state *variationalState) unregister() {
	goroutine := goroutineIdentity()
	if state.outer != nil {
		checksByGoroutine.Store(goroutine, state.outer)
	} else {
		checksByGoroutine.Delete(goroutine)
	}
	runningChecks.Add(-1)
}

// InBuild runs work on the calling goroutine reading every property that differs by build as build, a single build,
// has it: an object's type and value, a named type's underlying type, a method's presence and its index among its
// type's methods, a name's presence in a scope (Lookup, Names and Len), and an interface's type set. It is for after a
// check has ended, so that what reads one build's view (a lint rule of that build) can read the check of every build.
// A read under it never splits, since a single build can't, and it keeps no memo that holds in only some builds.
func InBuild(build Mask, work func()) {
	if buildCount(build) != 1 {
		panic("types.InBuild: build must be a single build")
	}
	state := &variationalState{all: build, mask: build, depth: 1, inBuild: true}
	state.register()
	defer state.unregister()
	work()
}

// Carry takes the build the calling goroutine reads as under InBuild to work, which another goroutine runs: it returns
// a func that runs work under InBuild of that build on whatever goroutine calls it, or work as it is when the caller
// runs under no InBuild. A goroutine starts with no state and reads as the host, so analysis that starts goroutines of
// its own under InBuild (an IR builder building functions in parallel, a dispatcher running shards) starts each with
// Carry to keep reading as the build. It captures InBuild's build alone, never a check's state, since a check starts no
// goroutines, nor Outside's. With no check or InBuild running anywhere, the capture is one atomic load.
func Carry(work func()) func() {
	state := runningState()
	if state == nil || !state.inBuild {
		return work
	}
	build := state.mask
	return func() { InBuild(build, work) }
}

// hostRead is the state of every read on a goroutine running no variational check and no InBuild: it answers as the
// host, build 0, has it, as the host's plain check would, and never splits. It is one state for every such goroutine,
// so it is read-only but for narrowed, which another goroutine's reads may raise: a memo kept only when nothing
// narrowed is then kept less often, never wrongly.
var hostRead = &variationalState{all: 1, mask: 1, depth: 1, inBuild: true}

// viewState is the state of a read as one build, which leaves out what the build doesn't have: the InBuild running on
// this goroutine, or with no check and no InBuild on it, hostRead. It is nil in a check, which leaves nothing out, and
// in a process where nothing differs by build.
func viewState() *variationalState {
	if !variationsExist.Load() {
		return nil
	}
	state := runningState()
	if state == nil {
		return hostRead
	}
	if state.inBuild {
		return state
	}
	return nil
}

// buildMethodOrders are a named type's methods each build has, by their index among its methods, in each build's own
// order, or none when every build has every method in one order.
type buildMethodOrders struct {
	differs bool
	orders  [8][]int
}

// sameMethodOrders is the buildMethodOrders of every type whose methods are every build's in one order.
var sameMethodOrders = &buildMethodOrders{}

// methodsInBuild is, read as one build (viewState), the indices among n's origin's methods of the methods the build
// has, in the build's own order, and whether it answers: in a check, and for a type whose methods are every build's
// in one order, the origin's own list answers. The orders are kept on the origin once read, since a read as one build
// comes after its check.
func (n *Named) methodsInBuild() ([]int, bool) {
	// A type found once to have every build's methods in one order answers with its own list everywhere, so it needn't
	// find the goroutine's state.
	origin := n.Origin()
	orders := origin.buildMethods.Load()
	if orders == sameMethodOrders {
		return nil, false
	}
	state := viewState()
	if state == nil {
		return nil, false
	}
	if orders == nil {
		orders = origin.methodOrdersByBuild()
		origin.buildMethods.Store(orders)
	}
	if !orders.differs {
		return nil, false
	}
	return orders.orders[bits.TrailingZeros8(uint8(state.mask))], true
}

// methodOrdersByBuild is n's methods in each build, as methodsInBuild reads them.
func (n *Named) methodOrdersByBuild() *buildMethodOrders {
	if !methodsDifferByBuild(n) {
		return sameMethodOrders
	}
	orders := &buildMethodOrders{differs: true}
	methods := n.unpack().methods
	for build := range 8 {
		mask := Mask(1) << build
		type placed struct{ index, origin int }
		var present []placed
		for index, method := range methods {
			// A method absent from the build has no place among its methods.
			if variation := method.variation; variation != nil && variation.presence != 0 &&
				variation.presence&mask == 0 {
				continue
			}
			inBuild, _ := methodIndexIn(method, n, mask)
			present = append(present, placed{inBuild, index})
		}
		slices.SortStableFunc(present, func(left placed, right placed) int { return left.index - right.index })
		orders.orders[build] = make([]int, len(present))
		for index, each := range present {
			orders.orders[build][index] = each.origin
		}
	}
	return orders
}

// runningChecks counts the variational checks and InBuild reads in progress anywhere, so a read in a process running
// none pays one atomic load and nothing more. checksByGoroutine holds each one's state by the goroutine it runs on.
// variationsExist is set once any object differs by build, so a read that answers otherwise only for such an object (a
// scope's names, a named type's methods) pays one atomic load in a process where none does.
var (
	runningChecks     atomic.Int32
	checksByGoroutine sync.Map // goroutineIdentity() to *variationalState
	variationsExist   atomic.Bool
)

// runningState is the state of the variational check running on this goroutine, or nil.
func runningState() *variationalState {
	if runningChecks.Load() == 0 {
		return nil
	}
	state, _ := checksByGoroutine.Load(goroutineIdentity())
	running, _ := state.(*variationalState)
	return running
}

// readState is the state a read on check's behalf answers under: check's own when check is given, and when it isn't
// (a type set computed for an interface whose checker is gone) the goroutine's, or hostRead for a goroutine with none.
func (check *Checker) readState() *variationalState {
	if check != nil {
		return check.variational
	}
	if state := runningState(); state != nil {
		return state
	}
	if variationsExist.Load() {
		return hostRead
	}
	return nil
}

// recordTypes reports whether check records expressions' types: when its Info has a Types map, and always in a
// variational check, which without one records its answers in Variation.Types. go/types skips recording builtins and
// comma-ok types without it.
func (check *Checker) recordTypes() bool {
	return check.Types != nil || check.variational != nil
}

// splitRequest is the panic a read raises when its answer differs across the current unit's builds: parts are the
// groups of builds that answer alike, and together they are the unit's builds.
type splitRequest struct{ parts []Mask }

// objectVariation is what a package-level object holds when it doesn't hold the same in every build.
type objectVariation struct {
	presence Mask            // the builds it exists in; zero for every build
	variants []objectVariant // its answers by build once declared; nil while one answer holds everywhere
	decls    []declVariant   // a source object declared in more than one file: each declaration and its builds
	named    *Named          // a type name's one named type, kept across its declarations
	// methodIndices are a method's index among its named type's methods in each build, as each build's own declaration
	// orders them, when the merging importer gives them (SetMethodIndex).
	methodIndices []maskedIndex
	// alternates are the objects of a package-level object's name, of other kinds, in builds it doesn't exist in, when
	// the merging importer read them (SetAlternates). They are kept on the object rather than in its scope's
	// alternates, since a merged package's scope is read by many checks at once while its other names are merged.
	alternates []Object
	// answered are a variable's answers in the builds where another variable's n:1 declaration typed it beside that
	// one. The variable stays untyped until they cover every build that declares it, and when it has declarations of
	// its own still to run (a name declared two at a time on unix and alone on windows), they run in the rest alone.
	answered []objectVariant
}

type objectVariant struct {
	mask Mask
	typ  Type
	val  constant.Value
	// tparams are a generic type's type parameters, which its right-hand side typ is written in.
	tparams *TypeParamList
}

type declVariant struct {
	mask Mask
	decl *declInfo
	iota constant.Value // a constant's iota in this declaration
	rank uint64         // the declaration's place in source order (declarationIn)
}

// namedVariation is a named type's underlying type by build, when it differs.
type namedVariation struct {
	variants []namedVariant
}

type namedVariant struct {
	mask Mask
	rhs  Type
	// tparams are a generic type's type parameters in these builds, which rhs is written in: each build's declaration
	// declares its own.
	tparams *TypeParamList
	// underlying is rhs's underlying type once resolved. Checks of many packages read one named type at once, so it is
	// kept atomically.
	underlying atomic.Pointer[resolvedUnderlying]
}

// resolvedUnderlying holds a namedVariant's resolved underlying type.
type resolvedUnderlying struct{ underlying Type }

// variationOf is obj's variation, or nil.
func variationOf(obj Object) *objectVariation {
	// The package-level kinds first, by their concrete types, which costs less than asserting an interface on every
	// Lookup.
	switch obj := obj.(type) {
	case *Var:
		return obj.variation
	case *Const:
		return obj.variation
	case *TypeName:
		return obj.variation
	case *Func:
		return obj.variation
	}
	if base, isBase := obj.(interface{ base() *object }); isBase {
		return base.base().variation
	}
	return nil
}

func (obj *object) base() *object { return obj }

// ensureVariation is obj's variation, made when it has none.
func (obj *object) ensureVariation() *objectVariation {
	if obj.variation == nil {
		obj.variation = &objectVariation{}
		noteVariationsExist()
	}
	return obj.variation
}

// noteVariationsExist sets variationsExist, for each place that makes a variation: an object's (ensureVariation) and
// a named type's underlying types by build the merging importer gives (SetUnderlyingVariants), which may be all that
// differs by build in a process.
func noteVariationsExist() {
	if !variationsExist.Load() {
		variationsExist.Store(true)
	}
}

// Variant is one answer of an imported object, for the importer that merges each build's export data.
type Variant struct {
	Builds Mask
	Type   Type
	Value  constant.Value
}

// SetVariants gives obj its answers by build, which the merging importer read from each build's export data. Answers
// alike are kept as one. obj's own type and value are set to the first answer's.
func SetVariants(obj Object, variants []Variant) {
	base := obj.(interface{ base() *object }).base()
	collapsed := make([]objectVariant, 0, len(variants))
	for _, variant := range variants {
		collapsed = appendVariant(collapsed, objectVariant{mask: variant.Builds, typ: variant.Type, val: variant.Value})
	}
	if len(collapsed) == 0 {
		return
	}
	base.typ = collapsed[0].typ
	if constantObject, isConstant := obj.(*Const); isConstant {
		constantObject.val = collapsed[0].val
	}
	if len(collapsed) > 1 {
		base.ensureVariation().variants = collapsed
	}
}

// maskedIndex is an index and the builds it holds in.
type maskedIndex struct {
	builds Mask
	index  int
}

// SetMethodIndex says method is at index among its named type's methods in builds, as those builds' own export data
// orders them: for the merging importer, whose merged named type holds every build's methods in one list.
func SetMethodIndex(method *Func, builds Mask, index int) {
	variation := method.ensureVariation()
	variation.methodIndices = append(variation.methodIndices, maskedIndex{builds, index})
}

// methodDeclarations are each base type's method declarations in source order, with the builds each holds in, for a
// variational check: a merged method's declarations are one method, so each build's index among its type's methods is
// counted from them, as each build's plain check counts.
type methodDeclarations struct {
	bases  []*TypeName
	byBase map[*TypeName][]declaredMethod
}

type declaredMethod struct {
	method *Func
	builds Mask
}

// note adds method, declared for base, before it may be merged into another of its name.
func (declared *methodDeclarations) note(check *Checker, base *TypeName, method *Func) {
	if check.variational == nil {
		return
	}
	if declared.byBase == nil {
		declared.byBase = map[*TypeName][]declaredMethod{}
	}
	if _, seen := declared.byBase[base]; !seen {
		declared.bases = append(declared.bases, base)
	}
	builds := check.variational.all
	if d := check.objMap[method]; d != nil && d.mask != 0 {
		builds = d.mask
	}
	declared.byBase[base] = append(declared.byBase[base], declaredMethod{method, builds})
}

// setIndices gives each method of a base type whose methods differ by build its index in each build, counted over
// the declarations that build compiles. A type whose every method holds in every build needs none: its methods' order
// is every build's.
func (declared *methodDeclarations) setIndices(check *Checker) {
	for _, base := range declared.bases {
		methods := declared.byBase[base]
		differs := false
		for _, each := range methods {
			differs = differs || each.builds != check.variational.all
		}
		if !differs {
			continue
		}
		var counts [8]int
		for _, each := range methods {
			// A declaration merged into an earlier one of its name answers as that one.
			target := each.method
			for _, kept := range check.methods[base] {
				if kept.name == each.method.name {
					target = kept
					break
				}
			}
			for build := range 8 {
				if each.builds&(1<<build) != 0 {
					SetMethodIndex(target, 1<<build, counts[build])
					counts[build]++
				}
			}
		}
	}
}

// methodsDifferByBuild reports whether named's methods can sit elsewhere in some build than in its one list: a method
// is absent from some build, or the merging importer gave its indices by build.
func methodsDifferByBuild(named *Named) bool {
	for _, method := range named.methods {
		if variation := method.variation; variation != nil &&
			(variation.presence != 0 || len(variation.methodIndices) > 0) {
			return true
		}
	}
	return false
}

// methodIndexIn is method's index among named's methods in build: the index the merging importer gave for build, or
// else its place among the methods present in build, since a package's methods are declared in its files' order in
// every build.
func methodIndexIn(method *Func, named *Named, build Mask) (int, bool) {
	if variation := method.variation; variation != nil {
		for _, given := range variation.methodIndices {
			if given.builds&build != 0 {
				return given.index, true
			}
		}
	}
	index := 0
	for _, candidate := range named.Origin().methods {
		if candidate == method {
			return index, true
		}
		if variation := candidate.variation; variation == nil || variation.presence == 0 || variation.presence&build != 0 {
			index++
		}
	}
	return 0, false
}

// SetPresence says obj exists only in builds.
func SetPresence(obj Object, builds Mask) {
	base := obj.(interface{ base() *object }).base()
	base.ensureVariation().presence = builds
}

// SetAlternates gives obj, a package-level object the merging importer read, the objects of its name declared as other
// kinds in the builds obj doesn't exist in, each present in its own builds (SetPresence). A lookup of the name in
// their builds finds them. It is the importer's twin of the checker's Scope.alternates, kept on the object because a
// merged package's scope is read by many checks at once while its other names are merged.
func SetAlternates(obj Object, alternates []Object) {
	base := obj.(interface{ base() *object }).base()
	base.ensureVariation().alternates = alternates
}

// AlternatesOf is the objects of obj's name declared as other kinds in the builds obj doesn't exist in, which the
// merging importer gave it, or nil.
func AlternatesOf(obj Object) []Object {
	if variation := variationOf(obj); variation != nil {
		return variation.alternates
	}
	return nil
}

// UnderlyingVariant is one of a named type's underlying types, for the merging importer. A generic type's variant
// gives the type parameters its underlying type is written in: the named type's own (TypeParams), or type parameters
// its builds' declarations bind alone, when their constraints differ from the named type's.
type UnderlyingVariant struct {
	Builds     Mask
	Underlying Type
	TypeParams []*TypeParam
}

// SetUnderlyingVariants gives n its underlying types by build. Alike ones, identical and written in the same type
// parameters, are kept as one, and when one is left n is left as it is. Type parameters other than n's own are bound
// here, once for each variant that gives them.
func SetUnderlyingVariants(n *Named, variants []UnderlyingVariant) {
	var alike []UnderlyingVariant
	for _, variant := range variants {
		merged := false
		for index := range alike {
			if slices.Equal(alike[index].TypeParams, variant.TypeParams) &&
				Identical(alike[index].Underlying, variant.Underlying) {
				alike[index].Builds |= variant.Builds
				merged = true
				break
			}
		}
		if !merged {
			alike = append(alike, variant)
		}
	}
	if len(alike) > 1 {
		own := n.unpack().tparams
		// Type parameters given by more than one variant, whose underlying types differ, are bound once.
		bound := map[*TypeParam]*TypeParamList{}
		kept := make([]namedVariant, len(alike))
		for index, variant := range alike {
			kept[index].mask = variant.Builds
			kept[index].rhs = variant.Underlying
			kept[index].underlying.Store(&resolvedUnderlying{variant.Underlying})
			switch {
			case len(variant.TypeParams) == 0:
				// Not generic: written in no type parameters.
			case slices.Equal(variant.TypeParams, own.list()):
				kept[index].tparams = own
			case bound[variant.TypeParams[0]] != nil:
				kept[index].tparams = bound[variant.TypeParams[0]]
			default:
				kept[index].tparams = bindTParams(variant.TypeParams)
				bound[variant.TypeParams[0]] = kept[index].tparams
			}
		}
		n.variation = &namedVariation{variants: kept}
		noteVariationsExist()
	}
}

// VariantsOf is obj's answers by build, or nil when one answer holds in every build.
func VariantsOf(obj Object) []Variant {
	variation := variationOf(obj)
	if variation == nil || len(variation.variants) == 0 {
		return nil
	}
	variants := make([]Variant, len(variation.variants))
	for index, variant := range variation.variants {
		variants[index] = Variant{Builds: variant.mask, Type: variant.typ, Value: variant.val}
	}
	return variants
}

// PresenceOf is the builds obj exists in, or zero when it exists in every build.
func PresenceOf(obj Object) Mask {
	variation := variationOf(obj)
	if variation == nil {
		return 0
	}
	return variation.presence
}

// UnderlyingVariantsOf is n's underlying types by build, or nil when one holds in every build.
func UnderlyingVariantsOf(n *Named) []UnderlyingVariant {
	if n.variation == nil {
		return nil
	}
	variants := make([]UnderlyingVariant, len(n.variation.variants))
	for index := range n.variation.variants {
		variant := &n.variation.variants[index]
		variants[index] = UnderlyingVariant{Builds: variant.mask, Underlying: variant.rhs,
			TypeParams: variant.tparams.list()}
	}
	return variants
}

// appendVariant adds variant to variants, folding it into an answer alike.
func appendVariant(variants []objectVariant, variant objectVariant) []objectVariant {
	for index := range variants {
		if sameAnswer(variants[index], variant) {
			variants[index].mask |= variant.mask
			return variants
		}
	}
	return append(variants, variant)
}

// sameAnswer reports whether two answers are alike: identical types, equal values when they are constants, and one
// list of type parameters when they are a generic type's, since a right-hand side that never names its type
// parameters is identical across declarations whose constraints differ.
func sameAnswer(left objectVariant, right objectVariant) bool {
	if left.tparams != right.tparams {
		return false
	}
	if (left.typ == nil) != (right.typ == nil) {
		return false
	}
	if left.typ != nil && !Identical(left.typ, right.typ) {
		return false
	}
	if (left.val == nil) != (right.val == nil) {
		return false
	}
	return left.val == nil || left.val.Kind() == right.val.Kind() && left.val.ExactString() == right.val.ExactString()
}

// declaredHere is where method is declared in the current unit's builds: a method declared again in other builds'
// files is one method, at its first declaration, and each build's plain check has it at that build's own. A unit
// whose builds compile more than one of its declarations splits, one group for each.
func (check *Checker) declaredHere(method *Func) positioner {
	if check.variational == nil || method.variation == nil || len(method.variation.decls) == 0 {
		return method
	}
	state := check.variational
	var parts []Mask
	var declared positioner = method
	for _, each := range method.variation.decls {
		if builds := each.mask & state.mask; builds != 0 && each.decl.fdecl != nil {
			parts = append(parts, builds)
			declared = atPos(each.decl.fdecl.Name.Pos())
		}
	}
	if len(parts) > 1 && state.depth > 0 {
		splitAt(state, parts)
	}
	return declared
}

// passSplit raises recovered again when it is a split, for a deferred guard that recovers a panic before asserting
// what upstream's code computed (Alignof's result): a split isn't a failure, and its unit runs again.
func passSplit(recovered any) {
	if request, isSplit := recovered.(splitRequest); isSplit {
		panic(request)
	}
}

// splitAt raises a split of state's current unit's builds into parts, plus whatever of them no part holds.
func splitAt(state *variationalState, parts []Mask) {
	covered := Mask(0)
	for _, part := range parts {
		covered |= part
	}
	if rest := state.mask &^ covered; rest != 0 {
		parts = append(parts, rest)
	}
	panic(splitRequest{parts: parts})
}

// Outside runs work as if no unit were running, so no read inside it splits: for the merging importer, which resolves
// an imported object the first time a unit reads it, and would be left half done by a split's panic. A unit that reads
// the object afterwards answers for its own builds as always. It writes only the state of the check running on its
// own goroutine, and nothing when none is in a unit, so goroutines that resolve imported objects share no write.
//
// On a goroutine running no check, or reading as one build (InBuild), work reads as a check outside every unit does,
// not as one build's view (hostRead's or InBuild's): every build's objects are found and no name is left out, so the
// importer's own reads of each build's universe and of the merged one resolve no lazy entry only to learn whether
// one build has it. That goroutine's state for work is its own.
func Outside(work func()) {
	state := runningState()
	if state == nil || state.inBuild {
		outside := &variationalState{all: 1, mask: 1}
		outside.register()
		defer outside.unregister()
		work()
		return
	}
	if state.depth == 0 {
		work()
		return
	}
	savedDepth := state.depth
	state.depth = 0
	defer func() { state.depth = savedDepth }()
	work()
}

// CompleteWithMethods completes iface, read from one build's export data, with methods as its type set: for the
// merging importer, whose interface embeds merged interfaces whose methods differ by build, so computing its type set
// now, outside every unit, would read them as another build declares them. methods are every method of iface's type
// set in the build it was read from, its explicit ones among them, and the type set holds every type, as a method
// set's does. An interface whose type set is computed already is left as it is.
func CompleteWithMethods(iface *Interface, methods []*Func) {
	if iface.tset != nil {
		return
	}
	sorted := slices.Clone(methods)
	sortMethods(sorted)
	iface.complete = true
	iface.tset = &_TypeSet{methods: sorted, terms: allTermlist}
}

// readingState is the state a read must answer by build under: the variational check's running on this goroutine
// when it is in a unit, an InBuild's, or hostRead on a goroutine running neither. It is nil for a check outside every
// unit (types.Outside, the merging importer's resolution), whose reads answer as the first build.
func readingState() *variationalState {
	state := runningState()
	if state == nil {
		return hostRead
	}
	if state.depth == 0 {
		return nil
	}
	return state
}

// pickObjectVariant is the answer of obj's variation for the current unit's builds, splitting the unit when they
// differ.
func pickObjectVariant(variation *objectVariation) (objectVariant, bool) {
	if variation == nil || len(variation.variants) == 0 {
		return objectVariant{}, false
	}
	state := readingState()
	if state == nil {
		return objectVariant{}, false
	}
	state.narrowed.Add(1)
	mask := state.mask
	first := -1
	for index, variant := range variation.variants {
		if variant.mask&mask == 0 {
			continue
		}
		if first >= 0 {
			splitAt(state, answeringParts(variation.variants, mask))
		}
		first = index
	}
	if first < 0 {
		return variation.variants[0], true
	}
	if variation.variants[first].mask&mask != mask {
		// The answer holds in some of the unit's builds and the object is absent in the rest.
		splitAt(state, answeringParts(variation.variants, mask))
	}
	return variation.variants[first], true
}

// answeringParts are the groups of mask's builds each of variants answers for, the parts of a split: built only when
// a read splits, so a read that doesn't allocates nothing.
func answeringParts(variants []objectVariant, mask Mask) []Mask {
	var parts []Mask
	for _, variant := range variants {
		if builds := variant.mask & mask; builds != 0 {
			parts = append(parts, builds)
		}
	}
	return parts
}

// presentHere reports whether an object with variation exists in the current unit's builds: true in all of them,
// false in none, and a split when only some.
func presentHere(variation *objectVariation) bool {
	if variation == nil || variation.presence == 0 {
		return true
	}
	state := readingState()
	if state == nil {
		return true
	}
	mask := state.mask
	switch {
	case mask&variation.presence == mask:
		return true
	case mask&variation.presence == 0:
		return false
	default:
		splitAt(state, []Mask{mask & variation.presence, mask &^ variation.presence})
		return false
	}
}

// pickUnderlying is n's underlying type in the current unit's builds, splitting the unit when they differ.
func (n *Named) pickUnderlying() Type {
	variant, state := n.pickVariant()
	return variant.resolved(state)
}

// pickVariant is n's variant in the current unit's builds, splitting the unit when they differ, and the check reading
// in a unit, or nil.
func (n *Named) pickVariant() (*namedVariant, *variationalState) {
	variation := n.variation
	state := readingState()
	if state == nil {
		return &variation.variants[0], state
	}
	state.narrowed.Add(1)
	mask := state.mask
	first := -1
	for index := range variation.variants {
		if variation.variants[index].mask&mask == 0 {
			continue
		}
		if first >= 0 {
			// More than one answer holds in the builds: split, building the parts only now.
			var parts []Mask
			for each := range variation.variants {
				if builds := variation.variants[each].mask & mask; builds != 0 {
					parts = append(parts, builds)
				}
			}
			splitAt(state, parts)
		}
		first = index
	}
	if first < 0 {
		first = 0
	}
	return &variation.variants[first], state
}

// variesTypeParameters reports whether n is a generic type declared again by build, with each build's own type
// parameters (or generic in only some builds).
func (n *Named) variesTypeParameters() bool {
	if n.variation == nil {
		return false
	}
	for index := range n.variation.variants {
		if n.variation.variants[index].tparams != nil {
			return true
		}
	}
	return false
}

// expandVariants gives instance n, whose origin is a generic type declared again by build, its right-hand side in
// each of those builds: the origin's right-hand side there, with that build's own type parameters replaced by n's type
// arguments. Its underlying type is then read by build, as its origin's is. The expansion is of every build at once,
// whichever unit reads n first, so it runs outside every unit: a split inside it would leave n half unpacked.
func (n *Named) expandVariants() {
	origin := n.inst.orig
	if !origin.variesTypeParameters() {
		return
	}
	variants := make([]namedVariant, len(origin.variation.variants))
	Outside(func() {
		for index := range origin.variation.variants {
			declared := &origin.variation.variants[index]
			variants[index].mask = declared.mask
			variants[index].tparams = declared.tparams
			variants[index].rhs = n.expandRightSide(declared.rhs, declared.tparams)
		}
	})
	n.fromRHS = variants[0].rhs
	n.variation = &namedVariation{variants: variants}
}

// resolved is variant's underlying type, following its right-hand side through aliases and named types, read under
// state, the check reading in a unit, or nil. It is kept once a read in a unit finds it with nothing inside narrowed,
// so an answer that holds in only some builds is never kept for all of them.
func (variant *namedVariant) resolved(state *variationalState) Type {
	if kept := variant.underlying.Load(); kept != nil {
		return kept.underlying
	}
	var before uint64
	if state != nil {
		before = state.narrowed.Load()
	}
	var underlying Type
	switch rhs := Unalias(variant.rhs).(type) {
	case *Named:
		underlying = rhs.Underlying()
	case nil:
		underlying = Typ[Invalid]
	default:
		underlying = rhs.Underlying()
	}
	if state != nil && state.narrowed.Load() == before {
		variant.underlying.Store(&resolvedUnderlying{underlying})
	}
	return underlying
}

// expandMethodVariants gives expanded, orig instantiated for instance t, orig's answers by build instantiated for t
// as well. cloneFunc copied orig's variation, whose answers are orig's own signatures, written in its receiver type
// parameters; expanded keeps the builds orig exists in, and answers with each build's signature instantiated. Every
// build's is instantiated at once, whichever unit reads the method first, so outside every unit, as expandVariants.
func (t *Named) expandMethodVariants(orig *Func, expanded *Func) {
	if orig.variation == nil {
		return
	}
	variation := *orig.variation
	variation.variants = make([]objectVariant, len(orig.variation.variants))
	Outside(func() {
		for index, variant := range orig.variation.variants {
			variation.variants[index] = variant
			if signature, isSignature := variant.typ.(*Signature); isSignature {
				variation.variants[index].typ = t.instantiateMethodSignature(orig, signature)
			}
		}
	})
	expanded.variation = &variation
}

// fileMask is the builds file compiles in.
func (check *Checker) fileMask(file *ast.File) Mask {
	variation := check.conf.Variation
	if variation == nil {
		return 0
	}
	if mask, named := variation.Files[file]; named {
		return mask
	}
	return variation.Builds
}

// vundo is what an answer write replaced, so a unit that restarts can put it back.
type vundo struct {
	expr     ast.Expr
	had      bool
	previous Answers
}

// runUnit runs run once for builds, or once for each group of them that answers alike when a read inside splits it;
// run reads the group it runs for from check.variational.mask. A declaration unit's work outlives any unit around it that
// restarts, since a declared object is never declared again; an action unit's is redone with the unit around it.
func (check *Checker) runUnit(builds Mask, run func(), declaration bool) {
	split := check.tryUnit(builds, run, declaration)
	if split == nil {
		return
	}
	pending := split
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if split := check.tryUnit(part, run, declaration); split != nil {
			pending = append(split, pending...)
		}
	}
}

// tryUnit runs run under builds and returns nil, or the split a read raised, after putting back what the abandoned
// run changed: its answers, its delayed actions, its untyped expressions, its object path and its environment. What a
// finished declaration inside it did is kept.
func (check *Checker) tryUnit(builds Mask, run func(), declaration bool) (split []Mask) {
	savedMask := check.variational.mask
	savedUndo := len(check.vundo)
	savedInfo := check.variational.info.mark()
	savedDelayed := len(check.delayed)
	savedUntyped := len(check.untypedLog)
	savedPositions := len(check.posStack)
	savedPath := len(check.objPath)
	savedEnvironment := check.environment
	savedErrors := len(check.pendingErrors)
	savedDeps := len(check.variational.deps)
	check.variational.mask = builds
	check.variational.depth++
	defer func() {
		check.variational.depth--
		check.variational.mask = savedMask
		recovered := recover()
		if recovered == nil {
			check.settleErrors(savedErrors, declaration)
			return
		}
		request, isSplit := recovered.(splitRequest)
		if !isSplit {
			panic(recovered)
		}
		types := check.variational.variation.Types
		for index := len(check.vundo) - 1; index >= savedUndo; index-- {
			undo := check.vundo[index]
			if undo.had {
				types[undo.expr] = undo.previous
			} else {
				delete(types, undo.expr)
			}
		}
		check.vundo = check.vundo[:savedUndo]
		check.variational.info.rollback(savedInfo)
		for index := len(check.variational.deps) - 1; index >= savedDeps; index-- {
			undo := check.variational.deps[index]
			if undo.had {
				undo.declaration.depBuilds[undo.dependency] = undo.previous
			} else {
				delete(undo.declaration.depBuilds, undo.dependency)
			}
		}
		check.variational.deps = check.variational.deps[:savedDeps]
		for index := savedDelayed; index < len(check.delayed); index++ {
			if !check.delayed[index].persistent {
				check.delayed[index].f = func() {}
			}
		}
		for _, expr := range check.untypedLog[savedUntyped:] {
			delete(check.untyped, expr)
		}
		check.untypedLog = check.untypedLog[:savedUntyped]
		check.posStack = check.posStack[:savedPositions]
		for len(check.objPath) > savedPath {
			check.pop()
		}
		check.environment = savedEnvironment
		check.dropAbandonedErrors(savedErrors)
		check.variational.variation.Restarts++
		split = request.parts
	}()
	run()
	check.flushUntyped(savedUntyped)
	// A finished declaration's answers stay whatever happens around it, and nothing undoes a unit no other holds.
	if declaration || check.variational.depth == 1 {
		check.vundo = check.vundo[:savedUndo]
		check.variational.info.keep(savedInfo)
		check.variational.deps = check.variational.deps[:savedDeps]
	}
	if declaration {
		for index := savedDelayed; index < len(check.delayed); index++ {
			check.delayed[index].persistent = true
		}
	}
	check.variational.variation.Units++
	return nil
}

// pendingError is an error reported inside a unit and not yet published. It is persistent once a finished
// declaration reported it, since a declaration is never checked again, so a unit around it that restarts keeps it.
type pendingError struct {
	err        Error
	persistent bool
}

// settleErrors settles the errors a run that finished reported after the first from of them: a declaration's are
// kept whatever happens around it, and once no unit is running every held error is published, in the order reported.
func (check *Checker) settleErrors(from int, declaration bool) {
	if declaration {
		for index := from; index < len(check.pendingErrors); index++ {
			check.pendingErrors[index].persistent = true
		}
	}
	if check.variational.depth > 0 {
		return
	}
	pending := check.pendingErrors
	check.pendingErrors = nil
	for _, held := range pending {
		check.publishError(held.err)
	}
}

// dropAbandonedErrors drops the errors a run abandoned for a split reported after the first from of them, all but
// those of a declaration that finished inside it.
func (check *Checker) dropAbandonedErrors(from int) {
	kept := check.pendingErrors[:from]
	for _, held := range check.pendingErrors[from:] {
		if held.persistent {
			kept = append(kept, held)
		}
	}
	clear(check.pendingErrors[len(kept):])
	check.pendingErrors = kept
}

// flushUntyped records, under the current unit's builds, the untyped expressions the unit remembered and never
// typed, since nothing after the unit can type them.
func (check *Checker) flushUntyped(from int) {
	for _, expr := range check.untypedLog[from:] {
		info, remembered := check.untyped[expr]
		if !remembered {
			continue
		}
		delete(check.untyped, expr)
		check.recordTypeAndValue(expr, info.mode, info.typ, info.val)
	}
	check.untypedLog = check.untypedLog[:from]
}

// recordVariational writes x's answer for the current unit's builds, keeping what it replaced while a unit runs that
// could restart: in the caller's Types map as every Info map is answered (variational_info.go), and in Variation.Types
// when the caller gives none.
func (check *Checker) recordVariational(x ast.Expr, tv TypeAndValue) {
	if check.Types != nil {
		builds, undoable := check.infoBuilds(x.Pos())
		check.variational.info.types.recordAt(x, tv, builds, check.variational.all, check.recordOrder, undoable)
		return
	}
	mask := check.variational.all
	if check.variational.depth > 0 {
		mask = check.variational.mask
	}
	types := check.variational.variation.Types
	previous, had := types[x]
	if check.variational.depth > 0 {
		check.vundo = append(check.vundo, vundo{expr: x, had: had, previous: previous})
	}
	types[x] = previous.with(mask, tv, check.recordOrder)
}

// startVariational begins a variational check of check's package, if its configuration asks for one.
func (check *Checker) startVariational() bool {
	variation := check.conf.Variation
	if variation == nil {
		return false
	}
	if variation.Types == nil && check.Types == nil {
		variation.Types = map[ast.Expr]Answers{}
	}
	check.variational = &variationalState{all: variation.Builds, mask: variation.Builds, variation: variation}
	check.variational.register()
	check.startInfo()
	// A package checked before, from other files, keeps the imports they met, in every build.
	for build := range 8 {
		check.variational.imports[build] = slices.Clone(check.pkg.imports)
	}
	check.recordOrder = unordered
	return true
}

// endVariational ends the check. Its answers are already in the variation, written as they came.
func (check *Checker) endVariational() {
	check.vundo = nil
	check.pendingErrors = nil
	check.endInfo()
	check.endImports()
	check.variational.unregister()
	check.variational = nil
}

// buildCount is how many builds mask holds.
func buildCount(mask Mask) int { return bits.OnesCount8(uint8(mask)) }

// How mergeVariant declared a package-level object.
const (
	variantNew       = iota // the name's first object: declare it as always
	variantMerged           // folded into the name's first object as another build's declaration
	variantAlternate        // another kind in builds the first doesn't hold: kept beside it, never declared
)

// mergeVariant folds obj, declared by d, into the package-level object of its name when they are declared in disjoint
// builds and are of one kind, so the name is one object with a declaration for each. A name's first object only notes
// the builds it exists in. Objects of one name in overlapping builds are left to be redeclarations, as always.
func (check *Checker) mergeVariant(ident *ast.Ident, obj Object, d *declInfo) int {
	if check.variational == nil || obj.Name() == "_" {
		return variantNew
	}
	base := obj.(interface{ base() *object }).base()
	first := check.pkg.scope.elems[obj.Name()]
	if first == nil {
		if d.mask != check.variational.all {
			base.ensureVariation().presence = d.mask
		}
		return variantNew
	}
	firstDecl := check.objMap[first]
	if firstDecl == nil {
		return variantNew
	}
	firstBase := first.(interface{ base() *object }).base()
	firstVariation := firstBase.ensureVariation()
	if len(firstVariation.decls) == 0 {
		firstVariation.decls = []declVariant{{mask: firstDecl.mask, decl: firstDecl, iota: iotaOf(first),
			rank: uint64(first.order()) << 32}}
		firstVariation.presence = firstDecl.mask
	}
	held := firstVariation.presence
	// A presence of zero is every build: the name's declarations merged so far cover them all, and another
	// declaration of it in any build redeclares it.
	if held == 0 {
		held = check.variational.all
	}
	for _, alternate := range check.pkg.scope.alternates[obj.Name()] {
		held |= variationOf(alternate).presence
	}
	if held&d.mask != 0 {
		return variantNew
	}
	if variantKind(first, firstDecl) != variantKind(obj, d) {
		base.ensureVariation().presence = d.mask
		if check.pkg.scope.alternates == nil {
			check.pkg.scope.alternates = map[string][]Object{}
		}
		check.pkg.scope.alternates[obj.Name()] = append(check.pkg.scope.alternates[obj.Name()], obj)
		obj.setParent(check.pkg.scope)
		return variantAlternate
	}
	// Its rank is after the last object declared, whose order is how many there are, and after any declaration folded
	// in since.
	check.variational.merges++
	firstVariation.decls = append(firstVariation.decls, declVariant{mask: d.mask, decl: d, iota: iotaOf(obj),
		rank: uint64(len(check.objMap))<<32 | check.variational.merges})
	firstVariation.presence |= d.mask
	if firstVariation.presence == check.variational.all {
		firstVariation.presence = 0
	}
	check.recordDef(ident, first)
	return variantMerged
}

// mergeMethodVariant folds method into the method of its name among methods when they are declared in disjoint
// builds, and reports whether it did.
func (check *Checker) mergeMethodVariant(methods []*Func, method *Func) bool {
	if check.variational == nil {
		return false
	}
	d := check.objMap[method]
	if d == nil {
		return false
	}
	for _, first := range methods {
		if first.name != method.name {
			continue
		}
		firstDecl := check.objMap[first]
		variation := first.ensureVariation()
		if len(variation.decls) == 0 {
			variation.decls = []declVariant{{mask: firstDecl.mask, decl: firstDecl}}
			variation.presence = firstDecl.mask
		}
		if variation.presence&d.mask != 0 {
			return false
		}
		variation.decls = append(variation.decls, declVariant{mask: d.mask, decl: d})
		variation.presence |= d.mask
		if variation.presence == check.variational.all {
			variation.presence = 0
		}
		delete(check.objMap, method)
		check.recordDef(d.fdecl.Name, first)
		return true
	}
	if d.mask != check.variational.all {
		method.ensureVariation().presence = d.mask
	}
	return false
}

// methodReceiver is a method and the base type its receiver names, through a pointer when ptr.
type methodReceiver struct {
	method *Func
	ptr    bool
	base   *TypeName
}

// methodReceivers resolves method's receiver, named by recv, to its base type. A receiver named through an alias
// whose target differs by build names a different base type in different builds, and a method of each base type is a
// method of its own, as each build's plain check and each build's export data have it, so each base after the first
// gets a method object of its own, declared in the builds that name that base. nextOrder is the last order given to
// an object, and each new method is given the next.
func (check *Checker) methodReceivers(method *Func, ptr bool, recv *ast.Ident, nextOrder *uint32) []methodReceiver {
	if check.variational == nil {
		resolvedPointer, base := check.resolveBaseTypeName(ptr, recv, 0)
		return []methodReceiver{{method, resolvedPointer, base}}
	}
	declaration := check.objMap[method]
	var receivers []methodReceiver
	var builds []Mask
	for build := Mask(1); build != 0; build <<= 1 {
		if build&declaration.mask == 0 {
			continue
		}
		resolvedPointer, base := check.resolveBaseTypeName(ptr, recv, build)
		index := slices.IndexFunc(receivers, func(receiver methodReceiver) bool {
			return receiver.ptr == resolvedPointer && receiver.base == base
		})
		if index < 0 {
			receivers = append(receivers, methodReceiver{method, resolvedPointer, base})
			builds = append(builds, 0)
			index = len(receivers) - 1
		}
		builds[index] |= build
	}
	if len(receivers) <= 1 {
		return receivers
	}
	for index := range receivers {
		narrowed := *declaration
		narrowed.mask = builds[index]
		if index > 0 {
			receivers[index].method = NewFunc(method.pos, method.pkg, method.name, nil)
			*nextOrder++
			receivers[index].method.setOrder(*nextOrder)
			// The declaring name is this method's in the builds that declare it.
			if check.Defs != nil {
				check.variational.info.defs.record(declaration.fdecl.Name, receivers[index].method, builds[index], check.variational.all, false)
			}
		}
		check.objMap[receivers[index].method] = &narrowed
	}
	return receivers
}

// lastOrder is the highest order given to an object of the package so far.
func (check *Checker) lastOrder() uint32 {
	last := uint32(0)
	for obj := range check.objMap {
		last = max(last, obj.order())
	}
	return last
}

// lookupInBuild is the package-level object of name in build, when build isn't zero: the name's first object when
// build declares it, or else the object of another kind kept beside it for build, or nil. Outside every unit, as
// collectObjects resolves methods' receivers, Lookup answers the name's first object for every build.
func (check *Checker) lookupInBuild(name string, build Mask) Object {
	first := check.pkg.scope.Lookup(name)
	if build == 0 || first == nil {
		return first
	}
	if variation := variationOf(first); variation == nil || variation.presence == 0 || variation.presence&build != 0 {
		return first
	}
	for _, alternate := range check.pkg.scope.alternates[name] {
		if variationOf(alternate).presence&build != 0 {
			return alternate
		}
	}
	return nil
}

// typeSpecIn is the declaration of tname that build compiles, when build isn't zero and tname is declared again in
// another build's file, and else declared, its first.
func (check *Checker) typeSpecIn(tname *TypeName, declared *ast.TypeSpec, build Mask) *ast.TypeSpec {
	if build == 0 || tname.variation == nil {
		return declared
	}
	for _, each := range tname.variation.decls {
		if each.mask&build != 0 {
			return each.decl.tdecl
		}
	}
	return declared
}

// iotaOf is a constant's iota as collectObjects left it in the constant's value, or nil.
func iotaOf(obj Object) constant.Value {
	if constantObject, isConstant := obj.(*Const); isConstant {
		return constantObject.val
	}
	return nil
}

// variantKind tells package-level objects' kinds apart, declared by d. A defined type and an alias are of different
// kinds: a defined type is a type of its own, and an alias another's, so a name that is one in some builds and the
// other in the rest can't be one object (the driver's Handle, a defined type on unix and syscall.Handle on windows).
func variantKind(obj Object, d *declInfo) int {
	switch obj.(type) {
	case *Const:
		return 1
	case *Var:
		return 2
	case *TypeName:
		if d.tdecl != nil && d.tdecl.Assign.IsValid() {
			return 5
		}
		return 3
	case *Func:
		return 4
	}
	return 0
}

// declareVariational checks each of obj's declarations as a unit under the builds its file compiles in, splitting a
// unit whose reads differ across its builds, and keeps obj's answers by build. A named type keeps one identity across
// every run, and its underlying type is what varies.
func (check *Checker) declareVariational(obj Object, d *declInfo) {
	base := obj.(interface{ base() *object }).base()
	variation := variationOf(obj)
	decls := check.declarationsOf(obj, d)
	typeName, isTypeName := obj.(*TypeName)
	isDefinedType := isTypeName && !decls[0].decl.tdecl.Assign.IsValid()
	if isDefinedType || len(decls) > 1 {
		variation = base.ensureVariation()
	}
	var answers []objectVariant
	// An n:1 variable declaration types every variable on its left at once, in each build's file its own left side, so
	// each run starts them all untyped, and hands each sibling its answer and leaves it untyped again: another build's
	// run must never read it as this one typed it.
	var siblings []*Var
	every := Mask(0)
	for _, declared := range decls {
		every |= declared.mask
		// A variable another's n:1 declaration typed in these builds already holds its answers for them.
		if taken, covered := takeAnswered(base.variation, declared.mask); covered {
			answers = append(answers, taken...)
			continue
		}
		var declaredSiblings []*Var
		if variable, isVariable := obj.(*Var); isVariable && len(declared.decl.lhs) > 1 {
			for _, sibling := range check.mergedLeftSide(declared.decl.lhs) {
				if sibling == variable {
					continue
				}
				declaredSiblings = append(declaredSiblings, sibling)
				if !slices.Contains(siblings, sibling) {
					siblings = append(siblings, sibling)
				}
			}
		}
		check.runUnit(declared.mask, func() {
			builds := check.variational.mask
			base.typ = nil
			for _, sibling := range declaredSiblings {
				sibling.typ = nil
			}
			if constantObject, isConstant := obj.(*Const); isConstant {
				constantObject.val = declared.iota
			}
			check.environment = environment{scope: declared.decl.file, version: declared.decl.version}
			switch obj := obj.(type) {
			case *Const:
				check.decl = declared.decl
				// Only a constant declared in more than one build's file runs out of its file's order; any other
				// constant's records keep the plain check's order, the order its declarations ran in.
				savedOrder := check.recordOrder
				if iota, isInt := constant.Int64Val(declared.iota); isInt && len(decls) > 1 {
					check.recordOrder = iota
				}
				check.constDecl(obj, declared.decl.vtyp, declared.decl.init, declared.decl.inherited)
				check.recordOrder = savedOrder
				answers = append(answers, objectVariant{mask: builds, typ: obj.typ, val: obj.val})
			case *Var:
				check.decl = declared.decl
				check.varDecl(obj, check.mergedLeftSide(declared.decl.lhs), declared.decl.vtyp, declared.decl.init)
				answers = append(answers, objectVariant{mask: builds, typ: obj.typ})
				for _, sibling := range declaredSiblings {
					siblingVariation := sibling.ensureVariation()
					siblingVariation.answered = append(siblingVariation.answered, objectVariant{mask: builds, typ: sibling.typ})
					sibling.typ = nil
				}
			case *TypeName:
				check.typeDecl(obj, declared.decl.tdecl)
				if named := asNamed(obj.typ); isDefinedType && named != nil {
					answers = append(answers, objectVariant{mask: builds, typ: named.fromRHS, tparams: named.tparams})
				} else {
					answers = append(answers, objectVariant{mask: builds, typ: obj.typ})
				}
			case *Func:
				check.funcDecl(obj, declared.decl)
				answers = append(answers, objectVariant{mask: builds, typ: obj.typ})
			default:
				panic("unreachable")
			}
		}, true)
	}
	for _, sibling := range siblings {
		check.settleSibling(sibling)
	}
	var collapsed []objectVariant
	for _, answer := range answers {
		collapsed = appendVariant(collapsed, answer)
	}
	if isDefinedType {
		named := variation.named
		if named != nil && len(collapsed) > 1 {
			variants := make([]namedVariant, len(collapsed))
			for index, answer := range collapsed {
				variants[index] = namedVariant{mask: answer.mask, rhs: answer.typ, tparams: answer.tparams}
			}
			named.variation = &namedVariation{variants: variants}
			// Read outside every unit, the type is its first build's: its right-hand side and the type parameters it
			// is written in, which the last run replaced.
			named.fromRHS = collapsed[0].typ
			named.tparams = collapsed[0].tparams
		}
		if named != nil {
			base.typ = named
		}
		// Methods are collected once, for every build the type is declared in.
		saved := check.variational.mask
		check.variational.mask = every
		check.collectMethods(typeName)
		check.variational.mask = saved
		return
	}
	if len(collapsed) == 0 {
		return
	}
	base.typ = collapsed[0].typ
	if constantObject, isConstant := obj.(*Const); isConstant {
		constantObject.val = collapsed[0].val
	}
	if len(collapsed) > 1 {
		variation = base.ensureVariation()
		variation.variants = collapsed
	}
}

// declarationsOf are obj's declarations, each with the builds it holds in: every build's when it was declared in more
// than one file, and else d alone.
func (check *Checker) declarationsOf(obj Object, d *declInfo) []declVariant {
	if variation := variationOf(obj); variation != nil && len(variation.decls) > 0 {
		return variation.decls
	}
	mask := d.mask
	if mask == 0 {
		mask = check.variational.all
	}
	return []declVariant{{mask: mask, decl: d, iota: iotaOf(obj)}}
}

// takeAnswered takes from variation the answers another variable's n:1 declarations gave in builds, and reports
// whether they cover builds. They are left where they are when they don't.
func takeAnswered(variation *objectVariation, builds Mask) ([]objectVariant, bool) {
	if variation == nil {
		return nil, false
	}
	answered := Mask(0)
	for _, answer := range variation.answered {
		if answer.mask&builds != 0 {
			answered |= answer.mask
		}
	}
	if builds&^answered != 0 {
		return nil, false
	}
	var taken []objectVariant
	variation.answered = slices.DeleteFunc(variation.answered, func(answer objectVariant) bool {
		if answer.mask&builds == 0 {
			return false
		}
		taken = append(taken, answer)
		return true
	})
	return taken, true
}

// settleSibling types sibling with the answers other variables' n:1 declarations gave it, once they cover every build
// that declares it. While its own declarations in other builds are still to run, it stays untyped with them, and the
// first read of it declares it in the rest. A sibling typed already was declared on its own, from the answers.
func (check *Checker) settleSibling(sibling *Var) {
	if sibling.typ != nil || sibling.variation == nil {
		return
	}
	answered := Mask(0)
	for _, answer := range sibling.variation.answered {
		answered |= answer.mask
	}
	declared := Mask(0)
	if d := check.objMap[sibling]; d != nil {
		for _, each := range check.declarationsOf(sibling, d) {
			declared |= each.mask
		}
	}
	if declared&^answered != 0 {
		return
	}
	var collapsed []objectVariant
	for _, answer := range sibling.variation.answered {
		collapsed = appendVariant(collapsed, answer)
	}
	sibling.variation.answered = nil
	if len(collapsed) == 0 {
		return
	}
	sibling.typ = collapsed[0].typ
	if len(collapsed) > 1 {
		sibling.variation.variants = collapsed
	}
}

// resetForVariant readies n to be declared again, for another build's declaration or another group of builds after a
// split: its right-hand side, underlying type and type parameters are taken again.
func (n *Named) resetForVariant() {
	n.fromRHS = nil
	n.underlying = nil
	n.tparams = nil
	n.variation = nil
	n.state_ = 0
}

// bodyScope is the scope sig's body runs in: sig's own scope the first time, and when the body runs again, for each
// group of builds after a read split it, a scope of its own holding what the signature declared (the receiver,
// parameters, results and type parameters) and no child scope another run opened. Each run's locals are then its own
// run's, as each build's plain check has them, and the run's scope is recorded for the function's node in its builds.
func (check *Checker) bodyScope(sig *Signature) *Scope {
	scope := sig.scope
	if !scope.bodyRan {
		scope.bodyRan = true
		scope.bodyChildren = len(scope.children)
		return scope
	}
	own := map[Object]bool{}
	if sig.recv != nil {
		own[sig.recv] = true
	}
	for _, tuple := range []*Tuple{sig.params, sig.results} {
		if tuple != nil {
			for _, variable := range tuple.vars {
				own[variable] = true
			}
		}
	}
	for _, list := range []*TypeParamList{sig.tparams, sig.rparams} {
		for _, parameter := range list.list() {
			own[parameter.obj] = true
		}
	}
	run := &Scope{parent: scope.parent, children: slices.Clone(scope.children[:scope.bodyChildren]),
		number: scope.number, pos: scope.pos, end: scope.end, comment: scope.comment, isFunc: true, bodyRan: true,
		bodyChildren: scope.bodyChildren}
	for name, obj := range scope.elems {
		if own[resolve(name, obj)] {
			run.insert(name, obj)
		}
	}
	if node, recorded := check.variational.info.functionNodes[scope]; recorded {
		check.recordScope(node, run)
	}
	return run
}

// rewriteCommaOk is recordCommaOkTypes for a variational check: x's answer for the current unit's builds, and each
// parenthesized expression inside it, becomes the comma-ok tuple.
func (check *Checker) rewriteCommaOk(x ast.Expr, t0 Type, t1 Type) {
	for {
		if tv, recorded := check.recordedAnswer(x); recorded {
			pos := x.Pos()
			tv.Type = NewTuple(
				newVar(LocalVar, pos, check.pkg, "", t0),
				newVar(LocalVar, pos, check.pkg, "", t1),
			)
			check.recordVariational(x, tv)
		}
		parenthesized, isParenthesized := x.(*ast.ParenExpr)
		if !isParenthesized {
			return
		}
		x = parenthesized.X
	}
}

// recordedAnswer is x's answer recorded for the builds a write of x now holds in, and whether one answer holds in
// all of them.
func (check *Checker) recordedAnswer(x ast.Expr) (TypeAndValue, bool) {
	if check.Types != nil {
		builds, _ := check.infoBuilds(x.Pos())
		return check.variational.info.types.covering(x, builds)
	}
	mask := check.variational.all
	if check.variational.depth > 0 {
		mask = check.variational.mask
	}
	for answer := range check.variational.variation.Types[x].All() {
		if answer.Builds&mask == mask {
			return answer.TypeAndValue, true
		}
	}
	return TypeAndValue{}, false
}

// mergedLeftSide is a variable declaration's left side with each variable replaced by the package-level object of its
// name. Another build's declaration, merged into the name's first object, names its own file's variables on its left,
// which were folded away, and the declaration now declares the merged ones (golang.org/x/sys/unix's mapper, declared
// with an initializer in mremap.go and in mmap_nomremap.go).
func (check *Checker) mergedLeftSide(lhs []*Var) []*Var {
	if len(lhs) == 0 {
		return lhs
	}
	merged := make([]*Var, len(lhs))
	for index, variable := range lhs {
		merged[index] = variable
		if first, isVariable := check.pkg.scope.elems[variable.name].(*Var); isVariable {
			merged[index] = first
		}
	}
	return merged
}
