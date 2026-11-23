//go:build amd64 || arm64

// This file is cohere's, for the variational check (variational.go): the identity of the goroutine running, so the
// reads that never see a Checker find the state of the check running on their goroutine.

package types

// goroutineIdentity is the running goroutine's g, as a number: unique among the goroutines alive, and the same for one
// goroutine across every thread it runs on. It is read from the register or thread-local slot the runtime keeps it in,
// which the Go assembler names in any package (goroutine_amd64.s, goroutine_arm64.s), and it is never dereferenced, so
// it depends on no field of the runtime's structure and needs no change from one Go release to the next.
func goroutineIdentity() uintptr
