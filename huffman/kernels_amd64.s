#include "textflag.h"

// SSE2 only, which is baseline on amd64: the newer packed-minimum and blend
// instructions would shorten bestTable a little, but not enough to justify a
// runtime feature check.

// func accumulate(acc *[32]int32, keys []uint32)
TEXT ·accumulate(SB), NOSPLIT, $0-32
	MOVQ acc+0(FP), AX
	MOVQ keys_base+8(FP), SI
	MOVQ keys_len+16(FP), CX
	TESTQ CX, CX
	JZ    done

	LEAQ ·pairCostTable(SB), R8
	LEAQ ·escapeCostTable(SB), R9

	// X0-X7 hold the running per-table totals for the whole loop.
	MOVOU 0(AX), X0
	MOVOU 16(AX), X1
	MOVOU 32(AX), X2
	MOVOU 48(AX), X3
	MOVOU 64(AX), X4
	MOVOU 80(AX), X5
	MOVOU 96(AX), X6
	MOVOU 112(AX), X7

loop:
	MOVL (SI), DX
	ADDQ $4, SI

	// &pairCostTable[key&0xFF]: 32 lanes of int32 is 128 bytes per row.
	MOVL DX, BX
	ANDL $0xFF, BX
	SHLQ $7, BX
	ADDQ R8, BX

	// The escape row is all zeros whenever the pair needs no linbits, which is
	// the overwhelmingly common case, so skip it rather than add zeros.
	MOVL DX, DI
	ANDL $0xF00, DI
	JZ   noescape

	SHRL $8, DI
	SHLQ $7, DI
	ADDQ R9, DI

	MOVOU 0(BX), X8
	MOVOU 0(DI), X9
	PADDL X9, X8
	PADDL X8, X0
	MOVOU 16(BX), X8
	MOVOU 16(DI), X9
	PADDL X9, X8
	PADDL X8, X1
	MOVOU 32(BX), X8
	MOVOU 32(DI), X9
	PADDL X9, X8
	PADDL X8, X2
	MOVOU 48(BX), X8
	MOVOU 48(DI), X9
	PADDL X9, X8
	PADDL X8, X3
	MOVOU 64(BX), X8
	MOVOU 64(DI), X9
	PADDL X9, X8
	PADDL X8, X4
	MOVOU 80(BX), X8
	MOVOU 80(DI), X9
	PADDL X9, X8
	PADDL X8, X5
	MOVOU 96(BX), X8
	MOVOU 96(DI), X9
	PADDL X9, X8
	PADDL X8, X6
	MOVOU 112(BX), X8
	MOVOU 112(DI), X9
	PADDL X9, X8
	PADDL X8, X7

	DECQ CX
	JNZ  loop
	JMP  store

noescape:
	MOVOU 0(BX), X8
	PADDL X8, X0
	MOVOU 16(BX), X8
	PADDL X8, X1
	MOVOU 32(BX), X8
	PADDL X8, X2
	MOVOU 48(BX), X8
	PADDL X8, X3
	MOVOU 64(BX), X8
	PADDL X8, X4
	MOVOU 80(BX), X8
	PADDL X8, X5
	MOVOU 96(BX), X8
	PADDL X8, X6
	MOVOU 112(BX), X8
	PADDL X8, X7

	DECQ CX
	JNZ  loop

store:
	MOVOU X0, 0(AX)
	MOVOU X1, 16(AX)
	MOVOU X2, 32(AX)
	MOVOU X3, 48(AX)
	MOVOU X4, 64(AX)
	MOVOU X5, 80(AX)
	MOVOU X6, 96(AX)
	MOVOU X7, 112(AX)

done:
	RET

// keyMin folds one group of four lanes into the running minimum in X0.
// X1 receives the group's packed keys, X2 and X3 are scratch.
#define KEYMIN(off) \
	MOVOU off(BX), X1     \
	MOVOU off(AX), X2     \
	PSUBL X2, X1          \
	PSLLL $5, X1          \
	MOVOU off(DI), X2     \
	POR   X2, X1          \
	MOVOU X0, X2          \
	PXOR  X1, X2          \
	MOVOU X0, X3          \
	PCMPGTL X1, X3        \
	PAND  X3, X2          \
	PXOR  X2, X0

// func bestTable(from, to *[32]int32) uint32
TEXT ·bestTable(SB), NOSPLIT, $0-20
	MOVQ from+0(FP), AX
	MOVQ to+8(FP), BX
	LEAQ ·laneIndex(SB), DI

	// key = (to - from) << 5 | lane. Costs are non-negative and bounded well
	// below 2^26, so the shift cannot reach the sign bit and a signed comparison
	// orders the keys correctly.
	MOVOU 0(BX), X0
	MOVOU 0(AX), X2
	PSUBL X2, X0
	PSLLL $5, X0
	MOVOU 0(DI), X2
	POR   X2, X0

	KEYMIN(16)
	KEYMIN(32)
	KEYMIN(48)
	KEYMIN(64)
	KEYMIN(80)
	KEYMIN(96)
	KEYMIN(112)

	// Fold the four remaining lanes: swap pairs, then halves.
	PSHUFD $0xB1, X0, X1
	MOVOU  X0, X2
	PXOR   X1, X2
	MOVOU  X0, X3
	PCMPGTL X1, X3
	PAND   X3, X2
	PXOR   X2, X0

	PSHUFD $0x4E, X0, X1
	MOVOU  X0, X2
	PXOR   X1, X2
	MOVOU  X0, X3
	PCMPGTL X1, X3
	PAND   X3, X2
	PXOR   X2, X0

	MOVL X0, ret+16(FP)
	RET
