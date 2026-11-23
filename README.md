# go-types

Go's type checker, `go/types`, with the packages it and its export-data reader need, taken from
[golang/go](https://github.com/golang/go) so [cohere](https://github.com/system-inc/cohere) can check a Go program
once for every platform it builds for. Upstream's code is BSD-licensed (`LICENSE`, `PATENTS`), and our changes are
commits on top of it.

## Where it comes from

Taken at go1.27.4, upstream commit `7b2881024c95a11ddb86663a616340aa3a88b67f` ("[release-branch.go1.27] go1.27.4"), laid
over go1.27.1 (`862c888e612ac346c7c4d99c9392bdfd265f33b0`), the first commit's release.
Test files and testdata are left out.

| Here                    | Upstream                     |
| ----------------------- | ---------------------------- |
| `types`                 | `src/go/types`               |
| `internal/types/errors` | `src/internal/types/errors`  |
| `internal/goversion`    | `src/internal/goversion`     |
| `gcimporter`            | `src/go/internal/gcimporter` |
| `exportdata`            | `src/internal/exportdata`    |
| `pkgbits`               | `src/internal/pkgbits`       |
| `saferio`               | `src/internal/saferio`       |

The first commit is upstream's text with only the import paths rewritten to this module. Every change of ours after
it is a commit of one concern.

## Our changes

- `types` drops two linkname shims, which only Go's own tree can satisfy.
- `types` checks a package once for every build at once (`types/variational.go` says how). A package-level object
  keeps one identity across builds and only its properties vary by build. A unit of work runs under the builds it
  holds in, and runs again for each group of builds whose reads differ.
- `types` runs many variational checks at once, each by its own Checker on its own goroutine. Each check holds its own
  state, and the reads that never see a Checker (`Named.Underlying`, `Object.Type`, `Const.Val`, `Scope.Lookup`, a
  method's presence, a type set's memo) find it by the goroutine they run on, read from the runtime's g register or
  thread-local slot by two instructions of assembly (`types/goroutine_amd64.s`, `types/goroutine_arm64.s`), which
  depend on no field of the runtime and need nothing from one Go release to the next. Other architectures read the
  goroutine's id from its stack trace (`types/goroutine_other.go`).
- `gcimporter` reads each object of a package's export data the first time it is looked up, and a named type's
  declaration (its type parameters, underlying type and methods) the first time any of it is needed, through
  `types.NewTypeNameLazy`, which `types` exports as types2 does.
- `types.Outside` writes nothing outside every unit, so goroutines resolving imported objects while no check runs
  share no write.
- `types` offers an undefined name's near match only from the names present in the unit's builds.
- `types.CompleteWithMethods` gives an imported interface its type set whole, for an importer whose interface embeds
  merged interfaces that differ by build.
- `canonical` is ours: it spells the fork's types as cohere's platform census spells go/types', so a check projected
  to one build compares exactly with that build's plain answers.

## Taking in a Go release

1. Fetch upstream at the new tag:
   `git clone --depth 1 --branch go1.N.M --filter=blob:none --sparse https://github.com/golang/go`, then
   `git sparse-checkout set` the seven upstream directories above.
2. On a branch of this repository off the first commit's line, lay upstream's non-test files out as the table says
   and rewrite the import paths, the same way the first commit did. Commit that as "go1.N.M as upstream has it".
3. Re-apply our commits on top, one at a time, with `git cherry-pick`. `go/types` is generated from
   `cmd/compile/internal/types2`, so a conflict is usually a changed line beside one of our marked sites (each says
   "cohere's variational spike"). Resolve each against the commit's own message.
4. Run the fork's tests, and cohere's driver proof over its root on every build. A release is in only when every build
   projects exactly.
5. Move cohere's submodule at `libraries/go/go-types` to the new head.
