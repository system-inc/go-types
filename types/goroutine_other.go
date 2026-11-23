//go:build !amd64 && !arm64

// This file is cohere's (goroutine.go), for the architectures without its assembly.

package types

import (
	"runtime"
	"strconv"
	"strings"
)

// goroutineIdentity is the running goroutine's id, read from the head of its own stack trace ("goroutine 18 [..."):
// slower than the assembly by a microsecond a call, and only paid on a read of something that differs by build.
func goroutineIdentity() uintptr {
	var trace [64]byte
	head := string(trace[:runtime.Stack(trace[:], false)])
	head = strings.TrimPrefix(head, "goroutine ")
	if end := strings.IndexByte(head, ' '); end >= 0 {
		head = head[:end]
	}
	identity, parseError := strconv.ParseUint(head, 10, 64)
	if parseError != nil {
		panic("go-types: no goroutine id in " + strconv.Quote(head))
	}
	return uintptr(identity)
}
