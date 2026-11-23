// This file is cohere's (goroutine.go): the running goroutine's g, which amd64 keeps in a thread-local slot. The two
// instructions are what the runtime's own get_tls and g macros spell (runtime/go_tls.h), and the assembler lays the
// slot's address out for each operating system.

#include "textflag.h"

// func goroutineIdentity() uintptr
TEXT ·goroutineIdentity(SB), NOSPLIT, $0-8
	MOVQ	TLS, CX
	MOVQ	0(CX)(TLS*1), AX
	MOVQ	AX, ret+0(FP)
	RET
