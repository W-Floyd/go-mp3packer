#include "textflag.h"

// The accumulation kernel is pure SSE2, which is baseline on amd64. The two
// reduction kernels have a second version using SSE4.1's packed minimum: SSE2
// has no 32-bit minimum at all, so each one costs six instructions to emulate,
// and the reductions are almost nothing but minimums.

// func cpuHasSSE41() bool
TEXT ·cpuHasSSE41(SB), NOSPLIT, $0-1
	MOVL $1, AX
	XORL CX, CX
	CPUID
	SHRL $19, CX // ECX bit 19 reports SSE4.1
	ANDL $1, CX
	MOVB CX, ret+0(FP)
	RET

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

	CMPB ·hasSSE41(SB), $0
	JNE  keybest41

	// Both rows arrive scaled by 32, so their difference already has room for the
	// lane label. Costs are non-negative and bounded well below 2^26, so nothing
	// reaches the sign bit and a signed comparison orders the keys correctly.
	MOVOU 0(BX), X0
	MOVOU 0(AX), X2
	PSUBL X2, X0
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

// KEYMIN41 is KEYMIN with the emulated minimum replaced by the real one. Costs
// never reach the sign bit, so unsigned and signed order the keys alike.
#define KEYMIN41(off)  \
	MOVOU off(BX), X1 \
	MOVOU off(AX), X2 \
	PSUBL X2, X1      \
	MOVOU off(DI), X2 \
	POR   X2, X1      \
	PMINUD X1, X0

keybest41:
	MOVOU 0(BX), X0
	MOVOU 0(AX), X2
	PSUBL X2, X0
	MOVOU 0(DI), X2
	POR   X2, X0

	KEYMIN41(16)
	KEYMIN41(32)
	KEYMIN41(48)
	KEYMIN41(64)
	KEYMIN41(80)
	KEYMIN41(96)
	KEYMIN41(112)

	PSHUFD $0xB1, X0, X1
	PMINUD X1, X0
	PSHUFD $0x4E, X0, X1
	PMINUD X1, X0

	MOVL X0, ret+16(FP)
	RET

// tailMin folds one group of four lanes of a row into the running minimum in X8,
// where the group's accumulator is already shifted with its lanes folded in.
#define TAILMIN(reg, off) \
	MOVOU off(SI), X9  \
	MOVOU reg, X10     \
	PSUBL X9, X10      \
	MOVOU X8, X9       \
	PXOR  X10, X9      \
	MOVOU X8, X11      \
	PCMPGTL X10, X11   \
	PAND  X11, X9      \
	PXOR  X9, X8

// func bestTails(rows []int32, acc *[32]int32, out []uint32)
TEXT ·bestTails(SB), NOSPLIT, $0-56
	MOVQ rows_base+0(FP), SI
	MOVQ acc+24(FP), AX
	MOVQ out_base+32(FP), DI
	MOVQ out_len+40(FP), CX
	TESTQ CX, CX
	JZ    tailsdone

	LEAQ ·laneIndex(SB), BX

	// acc and the lane labels stay in registers for the whole run: X0-X7 hold
	// acc<<5 with the lane already folded in, so each row costs one subtract per
	// group and the table index falls out of the minimum.
	MOVOU 0(AX), X0
	MOVOU 16(AX), X1
	MOVOU 32(AX), X2
	MOVOU 48(AX), X3
	MOVOU 64(AX), X4
	MOVOU 80(AX), X5
	MOVOU 96(AX), X6
	MOVOU 112(AX), X7
	PSLLL $5, X0
	PSLLL $5, X1
	PSLLL $5, X2
	PSLLL $5, X3
	PSLLL $5, X4
	PSLLL $5, X5
	PSLLL $5, X6
	PSLLL $5, X7
	MOVOU 0(BX), X8
	POR   X8, X0
	MOVOU 16(BX), X8
	POR   X8, X1
	MOVOU 32(BX), X8
	POR   X8, X2
	MOVOU 48(BX), X8
	POR   X8, X3
	MOVOU 64(BX), X8
	POR   X8, X4
	MOVOU 80(BX), X8
	POR   X8, X5
	MOVOU 96(BX), X8
	POR   X8, X6
	MOVOU 112(BX), X8
	POR   X8, X7

	CMPB ·hasSSE41(SB), $0
	JNE  tailsloop41

tailsloop:
	// The rows are already scaled: (acc<<5 | lane) - (row<<5) is (acc-row)<<5 |
	// lane, because the shift leaves the low five bits clear.
	MOVOU 0(SI), X9
	MOVOU X0, X8
	PSUBL X9, X8

	TAILMIN(X1, 16)
	TAILMIN(X2, 32)
	TAILMIN(X3, 48)
	TAILMIN(X4, 64)
	TAILMIN(X5, 80)
	TAILMIN(X6, 96)
	TAILMIN(X7, 112)

	// Fold the four remaining lanes: swap pairs, then halves.
	PSHUFD $0xB1, X8, X9
	MOVOU  X8, X10
	PXOR   X9, X10
	MOVOU  X8, X11
	PCMPGTL X9, X11
	PAND   X11, X10
	PXOR   X10, X8

	PSHUFD $0x4E, X8, X9
	MOVOU  X8, X10
	PXOR   X9, X10
	MOVOU  X8, X11
	PCMPGTL X9, X11
	PAND   X11, X10
	PXOR   X10, X8

	MOVL  X8, (DI)
	ADDQ  $4, DI
	ADDQ  $128, SI
	DECQ  CX
	JNZ   tailsloop

tailsdone:
	RET

// TAILMIN41 is TAILMIN with the emulated minimum replaced by the real one.
#define TAILMIN41(reg, off) \
	MOVOU off(SI), X9  \
	MOVOU reg, X10     \
	PSUBL X9, X10      \
	PMINUD X10, X8

tailsloop41:
	MOVOU 0(SI), X9
	MOVOU X0, X8
	PSUBL X9, X8

	TAILMIN41(X1, 16)
	TAILMIN41(X2, 32)
	TAILMIN41(X3, 48)
	TAILMIN41(X4, 64)
	TAILMIN41(X5, 80)
	TAILMIN41(X6, 96)
	TAILMIN41(X7, 112)

	PSHUFD $0xB1, X8, X9
	PMINUD X9, X8
	PSHUFD $0x4E, X8, X9
	PMINUD X9, X8

	MOVL  X8, (DI)
	ADDQ  $4, DI
	ADDQ  $128, SI
	DECQ  CX
	JNZ   tailsloop41
	RET
