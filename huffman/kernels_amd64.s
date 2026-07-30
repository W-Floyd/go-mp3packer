#include "textflag.h"

// Three forms of each kernel, dispatched on the feature bytes set at startup.
// SSE2 is the amd64 baseline and has no 32-bit minimum, so each one costs six
// instructions to emulate; SSE4.1 has PMINUD; AVX2 halves the register count
// (32 int32 lanes are four 256-bit registers, not eight 128-bit ones) and its
// three-operand encoding takes a memory source directly, which removes the
// copy that two-operand SSE needs before every destructive operation.
//
// Every AVX2 path ends in VZEROUPPER. Returning to Go's SSE code with the upper
// halves of the YMM registers live costs far more on this hardware than the
// kernel saves.

// func cpuHasSSE41() bool
TEXT ·cpuHasSSE41(SB), NOSPLIT, $0-1
	MOVL $1, AX
	XORL CX, CX
	CPUID
	SHRL $19, CX // ECX bit 19 reports SSE4.1
	ANDL $1, CX
	MOVB CX, ret+0(FP)
	RET

// func cpuHasAVX2() bool
TEXT ·cpuHasAVX2(SB), NOSPLIT, $0-1
	// The instruction bit alone is not enough: the YMM registers are only usable
	// if the OS has enabled saving them, which is what OSXSAVE and XGETBV report.
	MOVL $1, AX
	XORL CX, CX
	CPUID
	ANDL $(1<<27), CX // OSXSAVE
	JZ   avx2none
	XORL CX, CX
	XGETBV            // enabled state mask into AX
	ANDL $6, AX       // XMM and YMM state both saved
	CMPL AX, $6
	JNE  avx2none
	MOVL $7, AX
	XORL CX, CX
	CPUID
	SHRL $5, BX // CPUID.(EAX=7,ECX=0):EBX bit 5 reports AVX2
	ANDL $1, BX
	MOVB BX, ret+0(FP)
	RET

avx2none:
	MOVB $0, ret+0(FP)
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

	CMPB ·hasAVX2(SB), $0
	JNE  accavx2

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
	RET

// Y0-Y3 hold the running totals. Each row is added straight out of memory, so a
// pair costs four adds rather than eight loads and eight adds.
accavx2:
	VMOVDQU 0(AX), Y0
	VMOVDQU 32(AX), Y1
	VMOVDQU 64(AX), Y2
	VMOVDQU 96(AX), Y3

accavx2loop:
	MOVL (SI), DX
	ADDQ $4, SI

	MOVL DX, BX
	ANDL $0xFF, BX
	SHLQ $7, BX
	ADDQ R8, BX

	VPADDD 0(BX), Y0, Y0
	VPADDD 32(BX), Y1, Y1
	VPADDD 64(BX), Y2, Y2
	VPADDD 96(BX), Y3, Y3

	MOVL DX, DI
	ANDL $0xF00, DI
	JZ   accavx2next

	SHRL $8, DI
	SHLQ $7, DI
	ADDQ R9, DI

	VPADDD 0(DI), Y0, Y0
	VPADDD 32(DI), Y1, Y1
	VPADDD 64(DI), Y2, Y2
	VPADDD 96(DI), Y3, Y3

accavx2next:
	DECQ CX
	JNZ  accavx2loop

	VMOVDQU Y0, 0(AX)
	VMOVDQU Y1, 32(AX)
	VMOVDQU Y2, 64(AX)
	VMOVDQU Y3, 96(AX)
	VZEROUPPER
	RET

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

	CMPB ·hasAVX2(SB), $0
	JNE  keybestavx2
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

// Four 256-bit registers cover all 32 lanes, and both the subtrahend and the
// lane labels come straight from memory.
keybestavx2:
	VMOVDQU 0(BX), Y0
	VMOVDQU 32(BX), Y1
	VMOVDQU 64(BX), Y2
	VMOVDQU 96(BX), Y3
	VPSUBD 0(AX), Y0, Y0
	VPSUBD 32(AX), Y1, Y1
	VPSUBD 64(AX), Y2, Y2
	VPSUBD 96(AX), Y3, Y3
	VPOR 0(DI), Y0, Y0
	VPOR 32(DI), Y1, Y1
	VPOR 64(DI), Y2, Y2
	VPOR 96(DI), Y3, Y3

	VPMINUD Y1, Y0, Y0
	VPMINUD Y3, Y2, Y2
	VPMINUD Y2, Y0, Y0

	// Fold 8 lanes to 1: across the two halves, then pairs, then singles.
	VEXTRACTI128 $1, Y0, X1
	VPMINUD X1, X0, X0
	VPSHUFD $0xB1, X0, X1
	VPMINUD X1, X0, X0
	VPSHUFD $0x4E, X0, X1
	VPMINUD X1, X0, X0

	VMOVD X0, ret+16(FP)
	VZEROUPPER
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

	CMPB ·hasAVX2(SB), $0
	JNE  tailsavx2

	// acc and the lane labels stay in registers for the whole run: X0-X7 hold acc
	// with the lane already folded in, so each row costs one subtract per group and
	// the table index falls out of the minimum. Every cost arrives scaled, so there
	// is nothing to shift here.
	MOVOU 0(AX), X0
	MOVOU 16(AX), X1
	MOVOU 32(AX), X2
	MOVOU 48(AX), X3
	MOVOU 64(AX), X4
	MOVOU 80(AX), X5
	MOVOU 96(AX), X6
	MOVOU 112(AX), X7
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
	// Every cost is scaled: (acc | lane) - row is (acc-row) | lane, because the
	// scaling leaves the low five bits clear.
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

// TAILROW reduces one row against the accumulator, leaving its 32 lanes folded
// down to the 8 of dst. Both the load and the subtract are the one instruction,
// so a row costs four of them and three minimums.
#define TAILROW(dst) \
	VPSUBD 0(SI), Y0, Y8   \
	VPSUBD 32(SI), Y1, Y9  \
	VPSUBD 64(SI), Y2, Y10 \
	VPSUBD 96(SI), Y3, Y11 \
	VPMINUD Y9, Y8, Y8     \
	VPMINUD Y11, Y10, Y10  \
	VPMINUD Y10, Y8, dst   \
	ADDQ $128, SI

// Y0-Y3 keep the labelled accumulator for the whole run. A row is
// then four subtracts straight from memory and three minimums, with no loads and
// no register copies of its own.
tailsavx2:
	VMOVDQU 0(AX), Y0
	VMOVDQU 32(AX), Y1
	VMOVDQU 64(AX), Y2
	VMOVDQU 96(AX), Y3
	VPOR 0(BX), Y0, Y0
	VPOR 32(BX), Y1, Y1
	VPOR 64(BX), Y2, Y2
	VPOR 96(BX), Y3, Y3

	CMPQ CX, $4
	JLT  tailsavx2one

// Four rows at a time. A row on its own ends in a six-instruction serial fold
// and a 4-byte store, which is what the kernel was spending its time on once the
// subtracts came free. Reducing four rows to four vectors first and then
// transposing lets one chain finish all four: the unpacks work inside each
// 128-bit half, so both halves transpose at once and a single cross-half minimum
// at the end covers all eight lanes of every row.
tailsavx2four:
	TAILROW(Y4)
	TAILROW(Y5)
	TAILROW(Y6)
	TAILROW(Y7)

	VPUNPCKLDQ Y5, Y4, Y8
	VPUNPCKHDQ Y5, Y4, Y9
	VPUNPCKLDQ Y7, Y6, Y10
	VPUNPCKHDQ Y7, Y6, Y11
	VPUNPCKLQDQ Y10, Y8, Y12
	VPUNPCKHQDQ Y10, Y8, Y13
	VPUNPCKLQDQ Y11, Y9, Y14
	VPUNPCKHQDQ Y11, Y9, Y15

	VPMINUD Y13, Y12, Y12
	VPMINUD Y15, Y14, Y14
	VPMINUD Y14, Y12, Y12

	// Lane i of each half now belongs to row i; fold the halves together.
	VEXTRACTI128 $1, Y12, X13
	VPMINUD X13, X12, X12

	VMOVDQU X12, (DI)
	ADDQ    $16, DI
	SUBQ    $4, CX
	CMPQ    CX, $4
	JGE     tailsavx2four

	TESTQ CX, CX
	JZ    tailsavx2done

// The leftover rows, one at a time.
tailsavx2one:
	TAILROW(Y4)

	VEXTRACTI128 $1, Y4, X5
	VPMINUD X5, X4, X4
	VPSHUFD $0xB1, X4, X5
	VPMINUD X5, X4, X4
	VPSHUFD $0x4E, X4, X5
	VPMINUD X5, X4, X4

	VMOVD X4, (DI)
	ADDQ  $4, DI
	DECQ  CX
	JNZ   tailsavx2one

tailsavx2done:
	VZEROUPPER
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
