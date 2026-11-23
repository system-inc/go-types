// This file is cohere's, for the variational check (variational.go): the Info maps a caller gives a variational
// check, answered by build. Types is answered in Variation.Types; this file answers Defs, Uses, Implicits, Selections
// and Instances. While the check runs, each answer is kept with the builds it holds in, and undone with its unit when
// the unit restarts, as Types' answers are. When the check ends, the caller's maps get the host's answers (build 0's),
// so a reader of the plain maps reads the host's plain check, and Variation.Overlays gets, for each other build, only
// the answers in which that build differs from the host. go/types never reads these maps back while it checks, so
// holding them aside until the end changes no answer.

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
	Defs       Overlay[*ast.Ident, Object]
	Uses       Overlay[*ast.Ident, Object]
	Implicits  Overlay[ast.Node, Object]
	Selections Overlay[*ast.SelectorExpr, *Selection]
	Instances  Overlay[*ast.Ident, Instance]
}

// Len is how many answers of every map the build has that differ from the host's.
func (overlays *BuildOverlays) Len() int {
	return overlays.Defs.Len() + overlays.Uses.Len() + overlays.Implicits.Len() + overlays.Selections.Len() +
		overlays.Instances.Len()
}

// maskedAnswer is an answer and the builds it holds in.
type maskedAnswer[V any] struct {
	builds Mask
	value  V
}

// keyAnswers are one key's answers by build. Nearly every key has one, kept inline so the map needs no slice for it.
type keyAnswers[V any] struct {
	first maskedAnswer[V]
	more  []maskedAnswer[V]
}

// with is answers after value is written for builds, which it takes from every earlier answer, as a plain check's later
// write replaces its earlier one. answers itself is never changed, so an undo can put it back.
func (answers keyAnswers[V]) with(builds Mask, value V) keyAnswers[V] {
	if answers.first.builds == 0 || len(answers.more) == 0 && answers.first.builds&^builds == 0 {
		return keyAnswers[V]{first: maskedAnswer[V]{builds, value}}
	}
	kept := make([]maskedAnswer[V], 0, 2+len(answers.more))
	for _, earlier := range append([]maskedAnswer[V]{answers.first}, answers.more...) {
		earlier.builds &^= builds
		if earlier.builds != 0 {
			kept = append(kept, earlier)
		}
	}
	kept = append(kept, maskedAnswer[V]{builds, value})
	return keyAnswers[V]{first: kept[0], more: kept[1:]}
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

// infoUndo is what one write replaced, so a unit that restarts can put it back.
type infoUndo[K comparable, V any] struct {
	key      K
	had      bool
	previous keyAnswers[V]
}

// infoAnswers are one Info map's answers by build while the check runs, and what each write inside a unit replaced.
type infoAnswers[K comparable, V any] struct {
	answers map[K]keyAnswers[V]
	undo    []infoUndo[K, V]
}

// record writes key's answer value for builds, keeping what it replaced when undoable.
func (log *infoAnswers[K, V]) record(key K, value V, builds Mask, undoable bool) {
	if log.answers == nil {
		log.answers = map[K]keyAnswers[V]{}
	}
	previous, had := log.answers[key]
	if undoable {
		log.undo = append(log.undo, infoUndo[K, V]{key: key, had: had, previous: previous})
	}
	log.answers[key] = previous.with(builds, value)
}

// rollback puts back every write after mark.
func (log *infoAnswers[K, V]) rollback(mark int) {
	for index := len(log.undo) - 1; index >= mark; index-- {
		undo := log.undo[index]
		if undo.had {
			log.answers[undo.key] = undo.previous
		} else {
			delete(log.answers, undo.key)
		}
	}
	log.undo = log.undo[:mark]
}

// project gives plain the host's answers and each other build of all its overlay through overlayOf, comparing a
// build's answer with the host's by same.
// Each answer is first given as build has it by inBuild, when one is given.
func (log *infoAnswers[K, V]) project(plain map[K]V, all Mask, overlayOf func(build int) *Overlay[K, V],
	same func(V, V) bool, inBuild func(V, Mask) V,
) {
	for key, answers := range log.answers {
		hostValue, hostHas := answers.at(1)
		if hostHas && inBuild != nil {
			hostValue = inBuild(hostValue, 1)
		}
		if hostHas {
			plain[key] = hostValue
		}
		for build := 1; build < 8; build++ {
			if all&(1<<build) == 0 {
				continue
			}
			value, has := answers.at(1 << build)
			if has && inBuild != nil {
				value = inBuild(value, 1<<build)
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
}

// infoLogs are the answers of every Info map the check records by build.
type infoLogs struct {
	defs       infoAnswers[*ast.Ident, Object]
	uses       infoAnswers[*ast.Ident, Object]
	implicits  infoAnswers[ast.Node, Object]
	selections infoAnswers[*ast.SelectorExpr, *Selection]
	instances  infoAnswers[*ast.Ident, Instance]
	// fileBuilds is the builds each file compiles in, for an answer written outside every unit, which holds where the
	// file of its key compiles.
	fileBuilds map[*token.File]Mask
}

// infoMarks are each log's undo length at a unit's start.
type infoMarks [5]int

func (logs *infoLogs) mark() infoMarks {
	return infoMarks{len(logs.defs.undo), len(logs.uses.undo), len(logs.implicits.undo), len(logs.selections.undo),
		len(logs.instances.undo)}
}

func (logs *infoLogs) rollback(marks infoMarks) {
	logs.defs.rollback(marks[0])
	logs.uses.rollback(marks[1])
	logs.implicits.rollback(marks[2])
	logs.selections.rollback(marks[3])
	logs.instances.rollback(marks[4])
}

// keep drops the undo entries after marks, so what they wrote stays whatever happens around them.
func (logs *infoLogs) keep(marks infoMarks) {
	logs.defs.undo = logs.defs.undo[:marks[0]]
	logs.uses.undo = logs.uses.undo[:marks[1]]
	logs.implicits.undo = logs.implicits.undo[:marks[2]]
	logs.selections.undo = logs.selections.undo[:marks[3]]
	logs.instances.undo = logs.instances.undo[:marks[4]]
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

// startInfo readies the check's Info answers: which builds each file compiles in.
func (check *Checker) startInfo() {
	state := check.variational
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
	sameObject := func(left Object, right Object) bool { return left == right }
	if check.Defs != nil {
		logs.defs.project(check.Defs, all, func(build int) *Overlay[*ast.Ident, Object] {
			return &variation.Overlays[build].Defs
		}, sameObject, nil)
	}
	if check.Uses != nil {
		logs.uses.project(check.Uses, all, func(build int) *Overlay[*ast.Ident, Object] {
			return &variation.Overlays[build].Uses
		}, sameObject, nil)
	}
	if check.Implicits != nil {
		logs.implicits.project(check.Implicits, all, func(build int) *Overlay[ast.Node, Object] {
			return &variation.Overlays[build].Implicits
		}, sameObject, nil)
	}
	if check.Selections != nil {
		logs.selections.project(check.Selections, all, func(build int) *Overlay[*ast.SelectorExpr, *Selection] {
			return &variation.Overlays[build].Selections
		}, sameSelection, selectionInBuild)
	}
	if check.Instances != nil {
		logs.instances.project(check.Instances, all, func(build int) *Overlay[*ast.Ident, Instance] {
			return &variation.Overlays[build].Instances
		}, sameInstance, nil)
	}
	state.info = infoLogs{}
}

// sameSelection reports whether two selections answer alike: one kind, one object reached the same way, and receivers
// identical. Each run of a unit makes its own Selection, so they compare by what they say.
func sameSelection(left *Selection, right *Selection) bool {
	return left.kind == right.kind && left.obj == right.obj && left.indirect == right.indirect &&
		slices.Equal(left.index, right.index) && Identical(left.recv, right.recv)
}

// selectionInBuild is selection as build has it: a method's index among its named type's methods counted as build's
// own methods are, since the merged named type holds every build's methods in one list. The index of a field, or of an
// interface's method, is the build's already, read under the unit's builds.
func selectionInBuild(selection *Selection, build Mask) *Selection {
	method, isMethod := selection.obj.(*Func)
	if !isMethod || selection.kind == FieldVal || len(selection.index) == 0 {
		return selection
	}
	receiver := method.Signature().Recv()
	if receiver == nil {
		return selection
	}
	receiverType := Unalias(receiver.Type())
	if pointer, isPointer := receiverType.(*Pointer); isPointer {
		receiverType = Unalias(pointer.Elem())
	}
	named, isNamed := receiverType.(*Named)
	if !isNamed {
		return selection
	}
	index, found := methodIndexIn(method.Origin(), named, build)
	last := len(selection.index) - 1
	if !found || selection.index[last] == index {
		return selection
	}
	inBuild := *selection
	inBuild.index = append(slices.Clone(selection.index[:last]), index)
	return &inBuild
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
