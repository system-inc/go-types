// This file is cohere's (goroutine.go): the running goroutine's g, which arm64 keeps in the g register (R28).

#include "textflag.h"

// func goroutineIdentity() uintptr
TEXT ·goroutineIdentity(SB), NOSPLIT, $0-8
	MOVD	g, R0
	MOVD	R0, ret+0(FP)
	RET
