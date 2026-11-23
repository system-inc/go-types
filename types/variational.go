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
// A goroutine that runs no variational check finds none and reads every property as its first build's. go/types
// starts no goroutine inside a check, and the merging importer resolves an object on the goroutine whose check reads
// it, so a check's reads are all on its goroutine. Threading the state through every such read instead would change
// 234 declarations and 875 calls of the fork, and every caller of go/types' API beside it (#dfnzqjb, from the
// concurrency census of #y5mh5he); this finds it on the few reads that meet a property that differs by build.

package types

import (
	"go/ast"
	"go/constant"
	"iter"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
)

// Mask is a set of builds, bit b for build b. The spike holds at most eight.
type Mask uint8

// Variation is what a variational check is given and what it answers. Builds is every build the check covers, and
// Files says which of them compile each file, a file it doesn't name compiling in every one. Types is the answer:
// every expression's type and value, with the builds each holds in.
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

// variationalState is the state of one variational check in progress, held by its Checker.
type variationalState struct {
	all       Mask
	mask      Mask   // the builds the current unit runs under
	depth     int    // how many units are running; a read outside every unit never splits
	narrowed  uint64 // how many reads have answered for the current unit's builds alone
	variation *Variation
	info      infoLogs // the Info maps' answers by build until the check ends (variational_info.go)
	// outer is the check this goroutine was running when this one began, put back when this one ends: an importer
	// may check a package from source in the middle of another check.
	outer *variationalState
}

// runningChecks counts the variational checks in progress anywhere, so a read in a process running none pays one
// atomic load and nothing more. checksByGoroutine holds each one's state by the goroutine it runs on.
var (
	runningChecks     atomic.Int32
	checksByGoroutine sync.Map // goroutineIdentity() to *variationalState
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

// readState is the state a read on check's behalf answers under: check's own when check is given, and the goroutine's
// when it isn't (a type set computed for an interface whose checker is gone).
func (check *Checker) readState() *variationalState {
	if check != nil {
		return check.variational
	}
	return runningState()
}

// recordTypes reports whether check records expressions' types: when its Info has a Types map, and always in a
// variational check, which records its answers in Variation.Types. go/types skips recording builtins and comma-ok
// types without it.
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
	}
	return obj.variation
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
	bases []*TypeName
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

// UnderlyingVariant is one of a named type's underlying types, for the merging importer.
type UnderlyingVariant struct {
	Builds     Mask
	Underlying Type
}

// SetUnderlyingVariants gives n its underlying types by build. Alike ones are kept as one, and when one is left n is
// left as it is.
func SetUnderlyingVariants(n *Named, variants []UnderlyingVariant) {
	var alike []UnderlyingVariant
	for _, variant := range variants {
		merged := false
		for index := range alike {
			if Identical(alike[index].Underlying, variant.Underlying) {
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
		kept := make([]namedVariant, len(alike))
		for index, variant := range alike {
			kept[index].mask = variant.Builds
			kept[index].rhs = variant.Underlying
			kept[index].underlying.Store(&resolvedUnderlying{variant.Underlying})
		}
		n.variation = &namedVariation{variants: kept}
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
		variants[index] = UnderlyingVariant{Builds: variant.mask, Underlying: variant.rhs}
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
func Outside(work func()) {
	state := runningState()
	if state == nil || state.depth == 0 {
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

// readingState is the state of the variational check running on this goroutine when a read must answer by build,
// since the check is in a unit, and nil when it needn't.
func readingState() *variationalState {
	state := runningState()
	if state == nil || state.depth == 0 {
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
	state.narrowed++
	mask := state.mask
	first := -1
	var parts []Mask
	for index, variant := range variation.variants {
		if variant.mask&mask == 0 {
			continue
		}
		parts = append(parts, variant.mask&mask)
		if first < 0 {
			first = index
		}
	}
	if len(parts) > 1 {
		splitAt(state, parts)
	}
	if first < 0 {
		return variation.variants[0], true
	}
	if variation.variants[first].mask&mask != mask {
		// The answer holds in some of the unit's builds and the object is absent in the rest.
		splitAt(state, parts)
	}
	return variation.variants[first], true
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
	state.narrowed++
	mask := state.mask
	first := -1
	var parts []Mask
	for index := range variation.variants {
		builds := variation.variants[index].mask & mask
		if builds == 0 {
			continue
		}
		parts = append(parts, builds)
		if first < 0 {
			first = index
		}
	}
	if len(parts) > 1 {
		splitAt(state, parts)
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
		before = state.narrowed
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
	if state != nil && state.narrowed == before {
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
// could restart.
func (check *Checker) recordVariational(x ast.Expr, tv TypeAndValue) {
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

// rewriteVariational replaces x's answer for the current unit's builds, as recordCommaOkTypes rewrites a recorded type.
func (check *Checker) rewriteVariational(x ast.Expr, tv TypeAndValue) {
	check.recordVariational(x, tv)
}

// startVariational begins a variational check of check's package, if its configuration asks for one.
func (check *Checker) startVariational() bool {
	variation := check.conf.Variation
	if variation == nil {
		return false
	}
	if variation.Types == nil {
		variation.Types = map[ast.Expr]Answers{}
	}
	check.variational = &variationalState{all: variation.Builds, mask: variation.Builds, variation: variation}
	goroutine := goroutineIdentity()
	if outer, running := checksByGoroutine.Load(goroutine); running {
		check.variational.outer = outer.(*variationalState)
	}
	checksByGoroutine.Store(goroutine, check.variational)
	runningChecks.Add(1)
	check.startInfo()
	check.recordOrder = -1
	return true
}

// endVariational ends the check. Its answers are already in the variation, written as they came.
func (check *Checker) endVariational() {
	check.vundo = nil
	check.pendingErrors = nil
	check.endInfo()
	goroutine := goroutineIdentity()
	if outer := check.variational.outer; outer != nil {
		checksByGoroutine.Store(goroutine, outer)
	} else {
		checksByGoroutine.Delete(goroutine)
	}
	runningChecks.Add(-1)
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
		firstVariation.decls = []declVariant{{mask: firstDecl.mask, decl: firstDecl, iota: iotaOf(first)}}
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
	firstVariation.decls = append(firstVariation.decls, declVariant{mask: d.mask, decl: d, iota: iotaOf(obj)})
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
	var decls []declVariant
	if variation != nil && len(variation.decls) > 0 {
		decls = variation.decls
	} else {
		mask := d.mask
		if mask == 0 {
			mask = check.variational.all
		}
		decls = []declVariant{{mask: mask, decl: d, iota: iotaOf(obj)}}
	}
	typeName, isTypeName := obj.(*TypeName)
	isDefinedType := isTypeName && !decls[0].decl.tdecl.Assign.IsValid()
	if isDefinedType || len(decls) > 1 {
		variation = base.ensureVariation()
	}
	var answers []objectVariant
	// An n:1 variable declaration types every variable on its left at once, so each run starts them all untyped and
	// keeps each one's answer.
	var siblings []*Var
	siblingAnswers := map[*Var][]objectVariant{}
	if variable, isVariable := obj.(*Var); isVariable && len(decls) == 1 && len(decls[0].decl.lhs) > 1 {
		for _, sibling := range decls[0].decl.lhs {
			if sibling != variable {
				siblings = append(siblings, sibling)
			}
		}
	}
	every := Mask(0)
	for _, declared := range decls {
		every |= declared.mask
		check.runUnit(declared.mask, func() {
			builds := check.variational.mask
			base.typ = nil
			for _, sibling := range siblings {
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
				for _, sibling := range siblings {
					siblingAnswers[sibling] = append(siblingAnswers[sibling], objectVariant{mask: builds, typ: sibling.typ})
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
		var siblingCollapsed []objectVariant
		for _, answer := range siblingAnswers[sibling] {
			siblingCollapsed = appendVariant(siblingCollapsed, answer)
		}
		if len(siblingCollapsed) == 0 {
			continue
		}
		sibling.typ = siblingCollapsed[0].typ
		if len(siblingCollapsed) > 1 {
			sibling.ensureVariation().variants = siblingCollapsed
		}
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

// resetForVariant readies n to be declared again, for another build's declaration or another group of builds after a
// split: its right-hand side, underlying type and type parameters are taken again.
func (n *Named) resetForVariant() {
	n.fromRHS = nil
	n.underlying = nil
	n.tparams = nil
	n.variation = nil
	n.state_ = 0
}

// freshBodyScope puts sig's scope back to what the signature declared in it, when its body runs again: the receiver,
// parameters, results and type parameters, and no child scope the last run opened.
func freshBodyScope(sig *Signature) {
	scope := sig.scope
	if !scope.bodyRan {
		scope.bodyRan = true
		scope.bodyChildren = len(scope.children)
		return
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
	for name, obj := range scope.elems {
		if !own[resolve(name, obj)] {
			delete(scope.elems, name)
		}
	}
	scope.children = scope.children[:scope.bodyChildren]
}

// rewriteCommaOk is recordCommaOkTypes for a variational check without a Types map: x's answer for the current
// unit's builds, and each parenthesized expression inside it, becomes the comma-ok tuple.
func (check *Checker) rewriteCommaOk(x ast.Expr, t0 Type, t1 Type) {
	mask := check.variational.all
	if check.variational.depth > 0 {
		mask = check.variational.mask
	}
	for {
		for answer := range check.variational.variation.Types[x].All() {
			if answer.Builds&mask == mask {
				tv := answer.TypeAndValue
				pos := x.Pos()
				tv.Type = NewTuple(
					newVar(LocalVar, pos, check.pkg, "", t0),
					newVar(LocalVar, pos, check.pkg, "", t1),
				)
				check.recordVariational(x, tv)
				break
			}
		}
		parenthesized, isParenthesized := x.(*ast.ParenExpr)
		if !isParenthesized {
			return
		}
		x = parenthesized.X
	}
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
