// This file is cohere's, for the variational check (variational.go): the scope tree of a variationally checked
// package, by build. While the check runs, a scope is made as a plain check makes one and joins its parent's
// children, a split undoes none of that, and a body that runs again for a group of builds runs in a scope of its own
// that is no one's child (bodyScope). What tells which scope is which build's is the Scopes log (variational_info.go),
// which a split rolls back and a finished declaration keeps. So when the check ends, its scope tree is settled from
// that log: a scope no answer names was a run's that a split abandoned and leaves its parent's children, a function's
// first scope keeps the scopes its body ran again in beside it, and each scope that isn't every build's knows its
// builds. A read as one build (InBuild, or the host's with no state) then answers a scope's children, a function's
// scope and an object's parent as that build's own. During the check nothing here is set, and nothing answers by it.

package types

import (
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

// childrenHere are s's children as a read as one build has them: the build's own, in the order their runs made
// them. An agreement being gathered (Agreeing) keeps the builds that have the same children.
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

// settleScopes settles the checked package's scope tree from the Scopes log, of a check of all's builds, as this
// file's comment has it.
func (logs *infoLogs) settleScopes(all Mask) {
	builds := map[*Scope]Mask{}
	for _, scope := range logs.scopes.plain {
		builds[scope] |= all
	}
	for _, answers := range logs.scopes.staged {
		builds[answers.first.value] |= answers.first.builds
		for _, answer := range answers.more {
			builds[answer.value] |= answer.builds
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
