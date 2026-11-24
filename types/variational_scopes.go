// This file is cohere's, for the variational check (variational.go): the scope tree of a variationally checked
// package, by build. While the check runs, a scope is made as a plain check makes one and joins its parent's
// children, a split undoes none of that, and a body that runs again for a group of builds runs in a scope of its own
// that is no one's child (bodyScope). What tells which scope is which build's is the Scopes log (variational_info.go),
// which a split rolls back and a finished declaration keeps. So when the check ends, its scope tree is settled from
// that log: a scope no answer names was a run's that a split abandoned and leaves its parent's children, a function's
// first scope keeps the scopes its body ran again in beside it, and each scope that isn't every build's knows its
// builds. A read as one build (InBuild, or the host's with no state) then answers a scope's children, a function's
// scope and an object's parent as that build's own. During the check nothing here is set, and nothing answers by it.
//
// The order of a scope's children is the fork's own, and one for every build: the order they are written in, by
// where their nodes start, and for the package's scope the order the checker was given the files (orderChildren). A
// plain check has them in the order it happens to check its declarations in, types before functions and a method
// when it is first selected, which one check of every build can't make for each of them, so no order by build is
// kept through the check. The children a build has are the same scopes its plain check has, in this order.

package types

import (
	"cmp"
	"go/token"
	"math/bits"
	"slices"
)

// scopeBuilds is what a scope holds, once its check has ended, when it or one of its children isn't every build's.
type scopeBuilds struct {
	// builds are the builds this scope is its node's own scope in. A function's first scope whose body ran again for
	// every group of its builds is no build's own, and stands among its siblings for the reruns.
	builds Mask
	// reruns are the scopes a function's body ran again in, each with its builds.
	reruns []maskedScope
	// children are the scope's children as each build has them, when they aren't its children whole: a child in the
	// builds it is its node's own scope in, and in a function's first scope's place the build's rerun.
	children *[8][]*Scope
}

// maskedScope is a scope and the builds it holds in.
type maskedScope struct {
	builds Mask
	scope  *Scope
}

// bodyReruns are the scopes a function's body ran again in after its first run was abandoned for a split, and the
// names its signature declared, which are all its first scope keeps.
type bodyReruns struct {
	declared map[string]bool
	scopes   []*Scope
}

// here is s as a read as one build has it: the scope the build's body ran again in when s is a function's first
// scope that stands for its reruns, and else s. An agreement being gathered (Agreeing) keeps the builds that share
// the scope answered.
func (s *Scope) here() *Scope {
	if s == nil || s.byBuild == nil || len(s.byBuild.reruns) == 0 {
		return s
	}
	state := viewState()
	if state == nil {
		return s
	}
	if s.byBuild.builds&state.mask != 0 {
		if state.agreeing != nil {
			*state.agreeing &= s.byBuild.builds
		}
		return s
	}
	covered := s.byBuild.builds
	for _, rerun := range s.byBuild.reruns {
		if rerun.builds&state.mask != 0 {
			if state.agreeing != nil {
				*state.agreeing &= rerun.builds
			}
			return rerun.scope
		}
		covered |= rerun.builds
	}
	// The build declares the function nowhere, and reads its first scope with every other such build.
	if state.agreeing != nil {
		*state.agreeing &^= covered
	}
	return s
}

// childrenHere are s's children as a read as one build has them: the build's own, in the order they are written in,
// which the settle put them in (orderChildren). An agreement being gathered (Agreeing) keeps the builds that have
// the same children. Every build's list is taken from the one ordered list, so the lists share one order, and two
// builds with the same children have equal lists.
func (s *Scope) childrenHere() []*Scope {
	if s.byBuild == nil || s.byBuild.children == nil {
		return s.children
	}
	state := viewState()
	if state == nil {
		return s.children
	}
	build := bits.TrailingZeros8(uint8(state.mask))
	if state.agreeing != nil {
		for other := range s.byBuild.children {
			if !slices.Equal(s.byBuild.children[other], s.byBuild.children[build]) {
				*state.agreeing &^= 1 << other
			}
		}
	}
	return s.byBuild.children[build]
}

// inBuild is s as build, a single build, has it among its parent's children: s where it is its node's own scope,
// the build's rerun where s stands for its reruns, and nil where the build has neither.
func (s *Scope) inBuild(build Mask) *Scope {
	if s.byBuild == nil || s.byBuild.builds&build != 0 {
		return s
	}
	for _, rerun := range s.byBuild.reruns {
		if rerun.builds&build != 0 {
			return rerun.scope
		}
	}
	return nil
}

// noteRerun notes that rerun is a scope first's body ran again in, and what first's signature declared.
func (logs *infoLogs) noteRerun(first *Scope, rerun *Scope) {
	if logs.reruns == nil {
		logs.reruns = map[*Scope]*bodyReruns{}
	}
	reruns := logs.reruns[first]
	if reruns == nil {
		reruns = &bodyReruns{declared: map[string]bool{}}
		logs.reruns[first] = reruns
		for name := range rerun.elems {
			reruns.declared[name] = true
		}
	}
	reruns.scopes = append(reruns.scopes, rerun)
}

// orderChildren puts parent's children in the order they are written in, by where the node the Scopes log names
// each for starts (positions), whatever order the check made them in. The package's scope keeps its children, the
// file scopes, in the order the checker was given the files: a position orders what lies in one file, as every
// other scope's children do, and across files it follows the order the files joined the file set in. Children at
// one position, a node's scopes in different builds, keep the order they were made in, a function's first scope
// that stands for its reruns is where they are, and a child the log names nowhere keeps its place.
func orderChildren(parent *Scope, positions map[*Scope]token.Pos) {
	if parent.parent == Universe {
		return
	}
	positionOf := func(child *Scope) (token.Pos, bool) {
		if position, named := positions[child]; named {
			return position, true
		}
		// No answer names a first scope whose body ran again for every group of its builds, and each rerun the scope
		// keeps is named for its node.
		if child.byBuild != nil && len(child.byBuild.reruns) > 0 {
			position, named := positions[child.byBuild.reruns[0].scope]
			return position, named
		}
		return token.NoPos, false
	}
	// Inside a function every scope's children are made in the order they are written in, so nearly every parent
	// needs no sort, which one pass finds. A file's are the ones made out of it: its types' before its functions',
	// and a method's when it is first selected.
	ordered := true
	last := token.NoPos
	for _, child := range parent.children {
		if position, named := positionOf(child); named {
			if position < last {
				ordered = false
				break
			}
			last = position
		}
	}
	if ordered {
		return
	}
	type placedChild struct {
		position token.Pos
		scope    *Scope
	}
	// The children the log names are ordered among the slots they hold, so one it doesn't name stays where it is.
	placed := make([]placedChild, 0, len(parent.children))
	slots := make([]int, 0, len(parent.children))
	for index, child := range parent.children {
		if position, named := positionOf(child); named {
			placed = append(placed, placedChild{position, child})
			slots = append(slots, index)
		}
	}
	slices.SortStableFunc(placed, func(left placedChild, right placedChild) int {
		return cmp.Compare(left.position, right.position)
	})
	for index, slot := range slots {
		parent.children[slot] = placed[index].scope
	}
}

// settleScopes settles the checked package's scope tree from the Scopes log, of a check of all's builds, as this
// file's comment has it.
func (logs *infoLogs) settleScopes(all Mask) {
	builds := map[*Scope]Mask{}
	// positions is where the node the log names each scope for starts. Nearly every node has one scope.
	positions := make(map[*Scope]token.Pos, len(logs.scopes.plain)+len(logs.scopes.staged))
	for node, scope := range logs.scopes.plain {
		builds[scope] |= all
		positions[scope] = node.Pos()
	}
	for node, answers := range logs.scopes.staged {
		builds[answers.first.value] |= answers.first.builds
		positions[answers.first.value] = node.Pos()
		for _, answer := range answers.more {
			builds[answer.value] |= answer.builds
			positions[answer.value] = node.Pos()
		}
	}
	settled := map[*Scope]bool{}
	parents := map[*Scope]bool{}
	for _, scope := range logs.made {
		if settled[scope] {
			continue
		}
		settled[scope] = true
		var reruns []maskedScope
		if again := logs.reruns[scope]; again != nil {
			for _, rerun := range again.scopes {
				if builds[rerun] != 0 {
					reruns = append(reruns, maskedScope{builds[rerun], rerun})
				}
			}
			// Its first run was abandoned, and what that run declared and opened goes with it.
			for name := range scope.elems {
				if !again.declared[name] {
					delete(scope.elems, name)
				}
			}
			scope.children = scope.children[:scope.bodyChildren:scope.bodyChildren]
		}
		if builds[scope] != all || len(reruns) > 0 {
			scope.byBuild = &scopeBuilds{builds: builds[scope], reruns: reruns}
		}
		if scope.parent != nil {
			parents[scope.parent] = true
		}
	}
	for parent := range parents {
		// An abandoned run's scopes are no one's children, so its own children are never read.
		if byBuild := parent.byBuild; byBuild != nil && byBuild.builds == 0 && len(byBuild.reruns) == 0 && settled[parent] {
			continue
		}
		whole := true
		parent.children = slices.DeleteFunc(parent.children, func(child *Scope) bool {
			if child.byBuild == nil {
				return false
			}
			whole = false
			return child.byBuild.builds == 0 && len(child.byBuild.reruns) == 0
		})
		orderChildren(parent, positions)
		for index, child := range parent.children {
			// A rerun keeps the number of the first scope it stands in for, as bodyScope gave it.
			child.number = index + 1
			if child.byBuild != nil {
				for _, rerun := range child.byBuild.reruns {
					rerun.scope.number = index + 1
				}
			}
		}
		if whole {
			continue
		}
		if parent.byBuild == nil {
			// A parent that is every build's: the package's scope, or one the log holds for all of them.
			parent.byBuild = &scopeBuilds{builds: ^Mask(0)}
		}
		parent.byBuild.children = &[8][]*Scope{}
		for build := range parent.byBuild.children {
			for _, child := range parent.children {
				if own := child.inBuild(1 << build); own != nil {
					parent.byBuild.children[build] = append(parent.byBuild.children[build], own)
				}
			}
		}
		noteVariationsExist()
	}
}
