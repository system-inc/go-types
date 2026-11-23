// This file is cohere's, for the variational check (variational.go): the Info maps a caller gives a variational
// check, answered by build: Types, Defs, Uses, Implicits, Selections, Instances and Scopes. A caller that gives no
// Types map has every expression's answers in Variation.Types instead. While the check runs, each answer is kept with
// the builds it holds in, and undone with its unit when the unit restarts. When the check ends, the caller's maps get
// the host's answers (build 0's), so a reader of the plain maps reads the host's plain check, and Variation.Overlays
// gets, for each other build, only the answers in which that build differs from the host. go/types never reads these
// maps back while it checks, so holding them aside until the end changes no answer.

package types

import (
	"go/ast"
	"go/token"
	"slices"
)

// Overlay is one build's answers of one Info map where they differ from the host's: Set holds the build's answer for a
// key the host answers otherwise or not at all, and Removed a key the host answers and the build doesn't.
type Overlay[K comparable, V any] struct {
	Set     map[K]V
	Removed map[K]bool
}

// Len is how many keys the build answers differently from the host.
func (overlay Overlay[K, V]) Len() int { return len(overlay.Set) + len(overlay.Removed) }

// set notes the build's answer for key.
func (overlay *Overlay[K, V]) set(key K, value V) {
	if overlay.Set == nil {
		overlay.Set = map[K]V{}
	}
	overlay.Set[key] = value
}

// remove notes that the build has no answer for key.
func (overlay *Overlay[K, V]) remove(key K) {
	if overlay.Removed == nil {
		overlay.Removed = map[K]bool{}
	}
	overlay.Removed[key] = true
}

// BuildOverlays is one build's overlays, one for each Info map the check was given; the overlay of a map it wasn't
// given stays empty.
type BuildOverlays struct {
	Types      Overlay[ast.Expr, TypeAndValue]
	Defs       Overlay[*ast.Ident, Object]
	Uses       Overlay[*ast.Ident, Object]
	Implicits  Overlay[ast.Node, Object]
	Selections Overlay[*ast.SelectorExpr, *Selection]
	Instances  Overlay[*ast.Ident, Instance]
	Scopes     Overlay[ast.Node, *Scope]
	// InitOrder is the build's whole initialization order when it isn't the host's, which Info.InitOrder holds, and
	// nil when it is.
	InitOrder []*Initializer
	// Imports are the build's imports, in the order its files meet them, when they aren't the host's, which the
	// checked package's Imports holds, and nil when they are.
	Imports []*Package
}

// Len is how many answers of every map the build has that differ from the host's.
func (overlays *BuildOverlays) Len() int {
	return overlays.Types.Len() + overlays.Defs.Len() + overlays.Uses.Len() + overlays.Implicits.Len() +
		overlays.Selections.Len() + overlays.Instances.Len() + overlays.Scopes.Len()
}

// maskedAnswer is an answer, the builds it holds in, and the order it was written in (see check.recordOrder).
type maskedAnswer[V any] struct {
	builds Mask
	value  V
	order  int64
}

// unordered is the order of every write but a Types answer of a constant whose declarations merge across builds'
// files, which a plain check makes in its group's order and the variational check makes out of it.
const unordered int64 = -1

// keyAnswers are one key's answers by build. Nearly every key has one, kept inline so the map needs no slice for it.
type keyAnswers[V any] struct {
	first maskedAnswer[V]
	more  []maskedAnswer[V]
}

// with is answers after value is written for builds with order: it takes those builds from every earlier answer
// written at an order no higher, as a plain check's later write replaces its earlier one, and an earlier answer at a
// higher order keeps its builds (Answers.with). answers itself is never changed, so an undo can put it back.
func (answers keyAnswers[V]) with(builds Mask, value V, order int64) keyAnswers[V] {
	if answers.first.builds == 0 ||
		len(answers.more) == 0 && answers.first.builds&^builds == 0 && answers.first.order <= order {
		return keyAnswers[V]{first: maskedAnswer[V]{builds, value, order}}
	}
	kept := make([]maskedAnswer[V], 0, 2+len(answers.more))
	for _, earlier := range append([]maskedAnswer[V]{answers.first}, answers.more...) {
		if earlier.builds&builds != 0 {
			if earlier.order > order {
				builds &^= earlier.builds
			} else {
				earlier.builds &^= builds
			}
		}
		if earlier.builds != 0 {
			kept = append(kept, earlier)
		}
	}
	if builds != 0 {
		kept = append(kept, maskedAnswer[V]{builds, value, order})
	}
	if len(kept) == 0 {
		return keyAnswers[V]{}
	}
	return keyAnswers[V]{first: kept[0], more: kept[1:]}
}

// covering is the one answer that holds in every one of builds, and whether one does.
func (answers keyAnswers[V]) covering(builds Mask) (V, bool) {
	if answers.first.builds&builds == builds {
		return answers.first.value, true
	}
	for _, answer := range answers.more {
		if answer.builds&builds == builds {
			return answer.value, true
		}
	}
	var none V
	return none, false
}

// at is the answer that holds in build, and whether one does.
func (answers keyAnswers[V]) at(build Mask) (V, bool) {
	if answers.first.builds&build != 0 {
		return answers.first.value, true
	}
	for _, answer := range answers.more {
		if answer.builds&build != 0 {
			return answer.value, true
		}
	}
	var none V
	return none, false
}

// infoUndo is what one write replaced, in the caller's map and among the staged answers, so a unit that restarts can
// put it back.
type infoUndo[K comparable, V any] struct {
	key            K
	hadPlain       bool
	previousPlain  V
	hadStaged      bool
	previousStaged keyAnswers[V]
}

// infoAnswers are one Info map's answers by build while the check runs, and what each write inside a unit replaced. An
// answer that holds in every build, written unordered, goes straight into the caller's map, which is where the host's
// answers end up, and costs a plain check's write; only a key answered differently by build, or written at an order, is
// staged, and projected when the check ends.
type infoAnswers[K comparable, V any] struct {
	plain  map[K]V
	staged map[K]keyAnswers[V]
	undo   []infoUndo[K, V]
}

// record writes key's answer value for builds, of all the check's builds, keeping what it replaced when undoable.
func (log *infoAnswers[K, V]) record(key K, value V, builds Mask, all Mask, undoable bool) {
	log.recordAt(key, value, builds, all, unordered, undoable)
}

// recordAt is record of an answer written at order.
func (log *infoAnswers[K, V]) recordAt(key K, value V, builds Mask, all Mask, order int64, undoable bool) {
	staged, isStaged := log.staged[key]
	if builds == all && order == unordered && !isStaged {
		if undoable {
			previous, had := log.plain[key]
			log.undo = append(log.undo, infoUndo[K, V]{key: key, hadPlain: had, previousPlain: previous})
		}
		log.plain[key] = value
		return
	}
	previousPlain, hadPlain := log.plain[key]
	if undoable {
		log.undo = append(log.undo, infoUndo[K, V]{key: key, hadPlain: hadPlain, previousPlain: previousPlain,
			hadStaged: isStaged, previousStaged: staged})
	}
	answers := staged
	if hadPlain {
		// The key held one answer in every build; it is staged now, with that answer for the builds this write leaves.
		answers = keyAnswers[V]{first: maskedAnswer[V]{all, previousPlain, unordered}}
		delete(log.plain, key)
	}
	answers = answers.with(builds, value, order)
	if len(answers.more) == 0 && answers.first.builds == all && answers.first.order == unordered {
		log.plain[key] = answers.first.value
		delete(log.staged, key)
		return
	}
	if log.staged == nil {
		log.staged = map[K]keyAnswers[V]{}
	}
	log.staged[key] = answers
}

// covering is key's one answer in every one of builds, and whether one holds.
func (log *infoAnswers[K, V]) covering(key K, builds Mask) (V, bool) {
	if value, plain := log.plain[key]; plain {
		return value, true
	}
	return log.staged[key].covering(builds)
}

// remap replaces each answer that by answers otherwise, in the caller's map and among the staged answers. It runs
// as the check ends, when no undo reads the answers again, so it changes them in place.
func (log *infoAnswers[K, V]) remap(by func(V) (V, bool)) {
	for key, value := range log.plain {
		if replaced, changed := by(value); changed {
			log.plain[key] = replaced
		}
	}
	for key, answers := range log.staged {
		if replaced, changed := by(answers.first.value); changed {
			answers.first.value = replaced
			log.staged[key] = answers
		}
		for index := range answers.more {
			if replaced, changed := by(answers.more[index].value); changed {
				answers.more[index].value = replaced
			}
		}
	}
}

// rollback puts back every write after mark.
func (log *infoAnswers[K, V]) rollback(mark int) {
	for index := len(log.undo) - 1; index >= mark; index-- {
		undo := log.undo[index]
		if undo.hadPlain {
			log.plain[undo.key] = undo.previousPlain
		} else {
			delete(log.plain, undo.key)
		}
		if undo.hadStaged {
			log.staged[undo.key] = undo.previousStaged
		} else {
			delete(log.staged, undo.key)
		}
	}
	log.undo = log.undo[:mark]
}

// project gives the caller's map the host's answer of each staged key and each other build of all its overlay through
// overlayOf, comparing a build's answer with the host's by same. When inBuild is given, a build's answer is first
// given as that build has it, the answers in the caller's map included: one answer held in every build can still read
// otherwise in a build (a method selection's index).
func (log *infoAnswers[K, V]) project(all Mask, overlayOf func(build int) *Overlay[K, V], same func(V, V) bool,
	inBuild func(V, Mask) (V, bool),
) {
	for key, answers := range log.staged {
		hostValue, hostHas := answers.at(1)
		if hostHas && inBuild != nil {
			hostValue, _ = inBuild(hostValue, 1)
		}
		if hostHas {
			log.plain[key] = hostValue
		}
		for build := 1; build < 8; build++ {
			if all&(1<<build) == 0 {
				continue
			}
			value, has := answers.at(1 << build)
			if has && inBuild != nil {
				value, _ = inBuild(value, 1<<build)
			}
			switch {
			case has && hostHas && same(value, hostValue):
			case has:
				overlayOf(build).set(key, value)
			case hostHas:
				overlayOf(build).remove(key)
			}
		}
	}
	if inBuild == nil {
		return
	}
	for key, value := range log.plain {
		if _, isStaged := log.staged[key]; isStaged {
			continue
		}
		hostValue, hostChanged := inBuild(value, 1)
		if hostChanged {
			log.plain[key] = hostValue
		}
		for build := 1; build < 8; build++ {
			if all&(1<<build) == 0 {
				continue
			}
			buildValue, changed := inBuild(value, 1<<build)
			if !changed && !hostChanged {
				continue
			}
			if !changed {
				buildValue = value
			}
			if !same(buildValue, hostValue) {
				overlayOf(build).set(key, buildValue)
			}
		}
	}
}

// infoLogs are the answers of every Info map the check records by build.
type infoLogs struct {
	types      infoAnswers[ast.Expr, TypeAndValue]
	defs       infoAnswers[*ast.Ident, Object]
	uses       infoAnswers[*ast.Ident, Object]
	implicits  infoAnswers[ast.Node, Object]
	selections infoAnswers[*ast.SelectorExpr, *Selection]
	instances  infoAnswers[*ast.Ident, Instance]
	scopes     infoAnswers[ast.Node, *Scope]
	// initOrders are each build's initialization order where it isn't the host's (initOrderByBuild).
	initOrders [8][]*Initializer
	// folds are the objects and scopes a folded answer's type declares, each with the kept answer's in its place.
	folds typeFolds
	// functionNodes are the node each function's scope is recorded for, so a body that runs again records its own
	// scope there (bodyScope).
	functionNodes map[*Scope]ast.Node
	// fileBuilds is the builds each file compiles in, for an answer written outside every unit, which holds where the
	// file of its key compiles.
	fileBuilds map[*token.File]Mask
}

// infoMarks are each log's undo length at a unit's start.
type infoMarks [7]int

func (logs *infoLogs) mark() infoMarks {
	return infoMarks{len(logs.defs.undo), len(logs.uses.undo), len(logs.implicits.undo), len(logs.selections.undo),
		len(logs.instances.undo), len(logs.types.undo), len(logs.scopes.undo)}
}

func (logs *infoLogs) rollback(marks infoMarks) {
	logs.defs.rollback(marks[0])
	logs.uses.rollback(marks[1])
	logs.implicits.rollback(marks[2])
	logs.selections.rollback(marks[3])
	logs.instances.rollback(marks[4])
	logs.types.rollback(marks[5])
	logs.scopes.rollback(marks[6])
}

// keep drops the undo entries after marks, so what they wrote stays whatever happens around them.
func (logs *infoLogs) keep(marks infoMarks) {
	logs.defs.undo = logs.defs.undo[:marks[0]]
	logs.uses.undo = logs.uses.undo[:marks[1]]
	logs.implicits.undo = logs.implicits.undo[:marks[2]]
	logs.selections.undo = logs.selections.undo[:marks[3]]
	logs.instances.undo = logs.instances.undo[:marks[4]]
	logs.types.undo = logs.types.undo[:marks[5]]
	logs.scopes.undo = logs.scopes.undo[:marks[6]]
}

// infoBuilds is the builds an answer for a key at pos holds in, and whether to keep what it replaces: the current
// unit's builds inside a unit, which may restart, and outside every unit the builds the key's file compiles in.
func (check *Checker) infoBuilds(pos token.Pos) (Mask, bool) {
	state := check.variational
	if state.depth > 0 {
		return state.mask, true
	}
	if builds, named := state.info.fileBuilds[check.fset.File(pos)]; named {
		return builds, false
	}
	return state.all, false
}

// startInfo readies the check's Info answers: each log writes into the caller's map of its kind, and which builds
// each file compiles in.
func (check *Checker) startInfo() {
	state := check.variational
	state.info.types.plain = check.Types
	state.info.defs.plain = check.Defs
	state.info.uses.plain = check.Uses
	state.info.implicits.plain = check.Implicits
	state.info.selections.plain = check.Selections
	state.info.instances.plain = check.Instances
	state.info.scopes.plain = check.Scopes
	state.info.functionNodes = map[*Scope]ast.Node{}
	state.info.fileBuilds = make(map[*token.File]Mask, len(state.variation.Files))
	for file, builds := range state.variation.Files {
		if tokenFile := check.fset.File(file.Pos()); tokenFile != nil {
			state.info.fileBuilds[tokenFile] = builds
		}
	}
}

// endInfo gives the caller's Info maps the host's answers and Variation.Overlays each other build's differences.
func (check *Checker) endInfo() {
	state := check.variational
	variation := state.variation
	variation.Overlays = make([]BuildOverlays, 8)
	all := state.all
	logs := &state.info
	for build, order := range logs.initOrders {
		variation.Overlays[build].InitOrder = order
	}
	sameObject := func(left Object, right Object) bool { return left == right }
	logs.folds.apply(logs)
	if check.Types != nil {
		logs.types.project(all, func(build int) *Overlay[ast.Expr, TypeAndValue] {
			return &variation.Overlays[build].Types
		}, sameTypeAndValue, nil)
	}
	if check.Defs != nil {
		logs.defs.project(all, func(build int) *Overlay[*ast.Ident, Object] {
			return &variation.Overlays[build].Defs
		}, sameObject, nil)
	}
	if check.Uses != nil {
		logs.uses.project(all, func(build int) *Overlay[*ast.Ident, Object] {
			return &variation.Overlays[build].Uses
		}, sameObject, nil)
	}
	if check.Implicits != nil {
		logs.implicits.project(all, func(build int) *Overlay[ast.Node, Object] {
			return &variation.Overlays[build].Implicits
		}, sameObject, nil)
	}
	if check.Selections != nil {
		logs.selections.project(all, func(build int) *Overlay[*ast.SelectorExpr, *Selection] {
			return &variation.Overlays[build].Selections
		}, sameSelection, (&methodRemaps{}).selectionInBuild)
	}
	if check.Instances != nil {
		logs.instances.project(all, func(build int) *Overlay[*ast.Ident, Instance] {
			return &variation.Overlays[build].Instances
		}, sameInstance, nil)
	}
	// Each run of a unit makes its own scopes, which hold that run's objects, so a scope is the same answer only as
	// itself: a build answered by another run's scope gets its own in its overlay.
	if check.Scopes != nil {
		logs.scopes.project(all, func(build int) *Overlay[ast.Node, *Scope] {
			return &variation.Overlays[build].Scopes
		}, func(left *Scope, right *Scope) bool { return left == right }, nil)
	}
	state.info = infoLogs{}
}

// sameTypeAndValue reports whether two answers of an expression are alike: one mode, identical types, and constant
// values of one kind and one exact value. Each run of a unit makes its own types, so they compare by what they say.
func sameTypeAndValue(left TypeAndValue, right TypeAndValue) bool {
	if left.mode != right.mode || (left.Type == nil) != (right.Type == nil) ||
		(left.Value == nil) != (right.Value == nil) {
		return false
	}
	if left.Type != nil && !Identical(left.Type, right.Type) {
		return false
	}
	return left.Value == nil ||
		left.Value.Kind() == right.Value.Kind() && left.Value.ExactString() == right.Value.ExactString()
}

// typeFolds map what an answer folded into an alike answer of another run declares (its fields, an interface's
// methods) to what the kept answer declares in its place. Identical types have one shape, so the two are walked side
// by side. A function type's parameters stay each run's own: nothing but their own declaration and their function
// type's scope names them, and identical signatures may name them otherwise.
type typeFolds struct {
	objects map[Object]Object
}

// add maps what folded declares to what kept declares in its place.
func (folds *typeFolds) add(folded Type, kept Type) {
	if folded == nil || kept == nil || folded == kept {
		return
	}
	object := func(folded Object, kept Object) {
		if folded == kept || folded == nil || kept == nil {
			return
		}
		if folds.objects == nil {
			folds.objects = map[Object]Object{}
		}
		folds.objects[folded] = kept
	}
	tuple := func(folded *Tuple, kept *Tuple) {
		if folded.Len() != kept.Len() {
			return
		}
		for index := range folded.Len() {
			folds.add(folded.At(index).typ, kept.At(index).typ)
		}
	}
	switch folded := folded.(type) {
	case *Struct:
		if kept, isStruct := kept.(*Struct); isStruct && len(kept.fields) == len(folded.fields) {
			for index, field := range folded.fields {
				object(field, kept.fields[index])
				folds.add(field.typ, kept.fields[index].typ)
			}
		}
	case *Pointer:
		if kept, isPointer := kept.(*Pointer); isPointer {
			folds.add(folded.base, kept.base)
		}
	case *Slice:
		if kept, isSlice := kept.(*Slice); isSlice {
			folds.add(folded.elem, kept.elem)
		}
	case *Array:
		if kept, isArray := kept.(*Array); isArray {
			folds.add(folded.elem, kept.elem)
		}
	case *Chan:
		if kept, isChan := kept.(*Chan); isChan {
			folds.add(folded.elem, kept.elem)
		}
	case *Map:
		if kept, isMap := kept.(*Map); isMap {
			folds.add(folded.key, kept.key)
			folds.add(folded.elem, kept.elem)
		}
	case *Signature:
		if kept, isSignature := kept.(*Signature); isSignature {
			tuple(folded.params, kept.params)
			tuple(folded.results, kept.results)
		}
	case *Interface:
		if kept, isInterface := kept.(*Interface); isInterface && len(kept.methods) == len(folded.methods) {
			for index, method := range folded.methods {
				if method.name == kept.methods[index].name {
					object(method, kept.methods[index])
					folds.add(method.typ, kept.methods[index].typ)
				}
			}
			if len(kept.embeddeds) == len(folded.embeddeds) {
				for index := range folded.embeddeds {
					folds.add(folded.embeddeds[index], kept.embeddeds[index])
				}
			}
		}
	}
}

// apply names the kept answer's objects in every Info answer that names a folded answer's.
func (folds *typeFolds) apply(logs *infoLogs) {
	if len(folds.objects) == 0 {
		return
	}
	object := func(value Object) (Object, bool) {
		kept, folded := folds.objects[value]
		return kept, folded
	}
	logs.defs.remap(object)
	logs.uses.remap(object)
	logs.selections.remap(func(selection *Selection) (*Selection, bool) {
		kept, folded := folds.objects[selection.obj]
		if !folded {
			return selection, false
		}
		remapped := *selection
		remapped.obj = kept
		return &remapped, true
	})
}

// sameSelection reports whether two selections answer alike: one kind, one object reached the same way, and receivers
// identical. Each run of a unit makes its own Selection, so they compare by what they say.
func sameSelection(left *Selection, right *Selection) bool {
	return left.kind == right.kind && left.obj == right.obj && left.indirect == right.indirect &&
		slices.Equal(left.index, right.index) && Identical(left.recv, right.recv)
}

// methodRemaps remember, for each named type a projection meets, whether its methods can sit at another index in a
// build than in its one list: only then is a selection of its methods counted again for each build.
type methodRemaps struct {
	byNamed map[*Named]bool
}

// differs reports whether named's methods can sit elsewhere in some build (methodsDifferByBuild).
func (remaps *methodRemaps) differs(named *Named) bool {
	if remaps.byNamed == nil {
		remaps.byNamed = map[*Named]bool{}
	}
	differs, known := remaps.byNamed[named]
	if !known {
		differs = methodsDifferByBuild(named)
		remaps.byNamed[named] = differs
	}
	return differs
}

// selectionInBuild is selection as build has it, and whether that differs from selection: a method's index among its
// named type's methods counted as build's own methods are, since the merged named type holds every build's methods in
// one list. The index of a field, or of an interface's method, is the build's already, read under the unit's builds.
func (remaps *methodRemaps) selectionInBuild(selection *Selection, build Mask) (*Selection, bool) {
	method, isMethod := selection.obj.(*Func)
	if !isMethod || selection.kind == FieldVal || len(selection.index) == 0 {
		return selection, false
	}
	receiver := method.Signature().Recv()
	if receiver == nil {
		return selection, false
	}
	receiverType := Unalias(receiver.Type())
	if pointer, isPointer := receiverType.(*Pointer); isPointer {
		receiverType = Unalias(pointer.Elem())
	}
	named, isNamed := receiverType.(*Named)
	if !isNamed || !remaps.differs(named.Origin()) {
		return selection, false
	}
	index, found := methodIndexIn(method.Origin(), named, build)
	last := len(selection.index) - 1
	if !found || selection.index[last] == index {
		return selection, false
	}
	inBuild := *selection
	inBuild.index = append(slices.Clone(selection.index[:last]), index)
	return &inBuild, true
}

// sameInstance reports whether two instances answer alike: identical type arguments and an identical type.
func sameInstance(left Instance, right Instance) bool {
	if left.TypeArgs.Len() != right.TypeArgs.Len() || !Identical(left.Type, right.Type) {
		return false
	}
	for index := range left.TypeArgs.Len() {
		if !Identical(left.TypeArgs.At(index), right.TypeArgs.At(index)) {
			return false
		}
	}
	return true
}
