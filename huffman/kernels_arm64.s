#include "textflag.h"

// The cost machinery keeps all 32 code tables side by side, so both kernels are
// pure 32-lane vector work: eight Q registers cover a whole row, and the only
// scalar work is deriving two table addresses from a packed key.

// func accumulate(acc *[32]int32, keys []uint32)
TEXT ·accumulate(SB), NOSPLIT, $0-32
	MOVD acc+0(FP), R0
	MOVD keys_base+8(FP), R1
	MOVD keys_len+16(FP), R2
	CBZ  R2, done

	MOVD $·pairCostTable(SB), R3
	MOVD $·escapeCostTable(SB), R4

	// V0-V7 hold the running per-table totals for the whole loop.
	MOVD   R0, R5
	VLD1.P 64(R5), [V0.S4, V1.S4, V2.S4, V3.S4]
	VLD1   (R5), [V4.S4, V5.S4, V6.S4, V7.S4]

loop:
	MOVWU (R1), R6
	ADD   $4, R1

	// &pairCostTable[key&0xFF]: 32 lanes of int32 is 128 bytes per row.
	AND $0xFF, R6, R7
	LSL $7, R7, R7
	ADD R3, R7, R7

	VLD1.P 64(R7), [V8.S4, V9.S4, V10.S4, V11.S4]
	VLD1   (R7), [V12.S4, V13.S4, V14.S4, V15.S4]

	// The escape row is all zeros whenever the pair needs no linbits, which is
	// the overwhelmingly common case, so skip it rather than add zeros.
	AND $0xF00, R6, R8
	CBZ R8, noescape

	LSR $8, R8, R8
	LSL $7, R8, R8
	ADD R4, R8, R8

	VLD1.P 64(R8), [V16.S4, V17.S4, V18.S4, V19.S4]
	VLD1   (R8), [V20.S4, V21.S4, V22.S4, V23.S4]

	VADD V16.S4, V8.S4, V8.S4
	VADD V17.S4, V9.S4, V9.S4
	VADD V18.S4, V10.S4, V10.S4
	VADD V19.S4, V11.S4, V11.S4
	VADD V20.S4, V12.S4, V12.S4
	VADD V21.S4, V13.S4, V13.S4
	VADD V22.S4, V14.S4, V14.S4
	VADD V23.S4, V15.S4, V15.S4

noescape:
	VADD V8.S4, V0.S4, V0.S4
	VADD V9.S4, V1.S4, V1.S4
	VADD V10.S4, V2.S4, V2.S4
	VADD V11.S4, V3.S4, V3.S4
	VADD V12.S4, V4.S4, V4.S4
	VADD V13.S4, V5.S4, V5.S4
	VADD V14.S4, V6.S4, V6.S4
	VADD V15.S4, V7.S4, V7.S4

	SUB   $1, R2
	CBNZ  R2, loop

	VST1.P [V0.S4, V1.S4, V2.S4, V3.S4], 64(R0)
	VST1   [V4.S4, V5.S4, V6.S4, V7.S4], (R0)

done:
	RET

// func bestTable(from, to *[32]int32) uint32
TEXT ·bestTable(SB), NOSPLIT, $0-20
	MOVD from+0(FP), R0
	MOVD to+8(FP), R1
	MOVD $·laneIndex(SB), R2

	VLD1.P 64(R0), [V0.S4, V1.S4, V2.S4, V3.S4]
	VLD1   (R0), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R1), [V8.S4, V9.S4, V10.S4, V11.S4]
	VLD1   (R1), [V12.S4, V13.S4, V14.S4, V15.S4]
	VLD1.P 64(R2), [V16.S4, V17.S4, V18.S4, V19.S4]
	VLD1   (R2), [V20.S4, V21.S4, V22.S4, V23.S4]

	// Both rows arrive scaled by 32, so their difference already has room for the
	// lane label. Costs are non-negative and bounded well below 2^26, so nothing
	// reaches the sign bit and an unsigned minimum is also the signed one.
	VSUB V0.S4, V8.S4, V8.S4
	VSUB V1.S4, V9.S4, V9.S4
	VSUB V2.S4, V10.S4, V10.S4
	VSUB V3.S4, V11.S4, V11.S4
	VSUB V4.S4, V12.S4, V12.S4
	VSUB V5.S4, V13.S4, V13.S4
	VSUB V6.S4, V14.S4, V14.S4
	VSUB V7.S4, V15.S4, V15.S4

	VORR V16.B16, V8.B16, V8.B16
	VORR V17.B16, V9.B16, V9.B16
	VORR V18.B16, V10.B16, V10.B16
	VORR V19.B16, V11.B16, V11.B16
	VORR V20.B16, V12.B16, V12.B16
	VORR V21.B16, V13.B16, V13.B16
	VORR V22.B16, V14.B16, V14.B16
	VORR V23.B16, V15.B16, V15.B16

	VUMIN V9.S4, V8.S4, V8.S4
	VUMIN V11.S4, V10.S4, V10.S4
	VUMIN V13.S4, V12.S4, V12.S4
	VUMIN V15.S4, V14.S4, V14.S4
	VUMIN V10.S4, V8.S4, V8.S4
	VUMIN V14.S4, V12.S4, V12.S4
	VUMIN V12.S4, V8.S4, V8.S4

	// Fold the four remaining lanes: swap within each half, then across halves.
	VREV64 V8.S4, V9.S4
	VUMIN  V9.S4, V8.S4, V8.S4
	VEXT   $8, V8.B16, V8.B16, V9.B16
	VUMIN  V9.S4, V8.S4, V8.S4

	VMOV V8.S[0], R3
	MOVW R3, ret+16(FP)
	RET

// func bestTails(rows []int32, acc *[32]int32, out []uint32)
TEXT ·bestTails(SB), NOSPLIT, $0-56
	MOVD rows_base+0(FP), R0
	MOVD acc+24(FP), R1
	MOVD out_base+32(FP), R2
	MOVD out_len+40(FP), R3
	CBZ  R3, tailsdone

	MOVD $·laneIndex(SB), R4

	// acc and the lane labels stay in registers for the whole run: V0-V7 hold
	// acc<<5 with the lane already folded in, so each row costs one subtract per
	// group and the table index falls out of the minimum.
	VLD1.P 64(R1), [V0.S4, V1.S4, V2.S4, V3.S4]
	VLD1   (R1), [V4.S4, V5.S4, V6.S4, V7.S4]
	VLD1.P 64(R4), [V16.S4, V17.S4, V18.S4, V19.S4]
	VLD1   (R4), [V20.S4, V21.S4, V22.S4, V23.S4]

	VSHL $5, V0.S4, V0.S4
	VSHL $5, V1.S4, V1.S4
	VSHL $5, V2.S4, V2.S4
	VSHL $5, V3.S4, V3.S4
	VSHL $5, V4.S4, V4.S4
	VSHL $5, V5.S4, V5.S4
	VSHL $5, V6.S4, V6.S4
	VSHL $5, V7.S4, V7.S4

	VORR V16.B16, V0.B16, V0.B16
	VORR V17.B16, V1.B16, V1.B16
	VORR V18.B16, V2.B16, V2.B16
	VORR V19.B16, V3.B16, V3.B16
	VORR V20.B16, V4.B16, V4.B16
	VORR V21.B16, V5.B16, V5.B16
	VORR V22.B16, V6.B16, V6.B16
	VORR V23.B16, V7.B16, V7.B16

// ROWMIN reduces one row against the accumulator, leaving the 32 lanes folded
// down to four in dst. The rows are already scaled: (acc<<5 | lane) - (row<<5)
// is (acc-row)<<5 | lane, because the shift leaves the low five bits clear.
#define ROWMIN(dst)                                  \
	VLD1.P 64(R0), [V8.S4, V9.S4, V10.S4, V11.S4]  \
	VLD1.P 64(R0), [V12.S4, V13.S4, V14.S4, V15.S4] \
	VSUB  V8.S4, V0.S4, V8.S4                       \
	VSUB  V9.S4, V1.S4, V9.S4                       \
	VSUB  V10.S4, V2.S4, V10.S4                     \
	VSUB  V11.S4, V3.S4, V11.S4                     \
	VSUB  V12.S4, V4.S4, V12.S4                     \
	VSUB  V13.S4, V5.S4, V13.S4                     \
	VSUB  V14.S4, V6.S4, V14.S4                     \
	VSUB  V15.S4, V7.S4, V15.S4                     \
	VUMIN V9.S4, V8.S4, V8.S4                       \
	VUMIN V11.S4, V10.S4, V10.S4                    \
	VUMIN V13.S4, V12.S4, V12.S4                    \
	VUMIN V15.S4, V14.S4, V14.S4                    \
	VUMIN V10.S4, V8.S4, V8.S4                      \
	VUMIN V14.S4, V12.S4, V12.S4                    \
	VUMIN V12.S4, V8.S4, dst

	// Four rows at a time. Reducing each row's last four lanes on its own would
	// cost a serial fold plus a lane-to-register move per row; transposing four
	// partial results instead turns all four folds into three minimums and lets
	// the answers leave as one 16-byte store.
	CMP $4, R3
	BLT tailsone

tailsfour:
	ROWMIN(V24.S4)
	ROWMIN(V25.S4)
	ROWMIN(V26.S4)
	ROWMIN(V27.S4)

	// Transpose the 4x4 of partial minimums, so that lane i of each result
	// vector belongs to row i, and fold it.
	VTRN1 V25.S4, V24.S4, V16.S4
	VTRN2 V25.S4, V24.S4, V17.S4
	VTRN1 V27.S4, V26.S4, V18.S4
	VTRN2 V27.S4, V26.S4, V19.S4
	VUMIN V17.S4, V16.S4, V20.S4
	VUMIN V19.S4, V18.S4, V21.S4
	VZIP1 V21.D2, V20.D2, V22.D2
	VZIP2 V21.D2, V20.D2, V23.D2
	VUMIN V23.S4, V22.S4, V22.S4

	VST1.P [V22.S4], 16(R2)
	SUB    $4, R3
	CMP    $4, R3
	BGE    tailsfour

	CBZ R3, tailsdone

tailsone:
	ROWMIN(V8.S4)

	VREV64 V8.S4, V9.S4
	VUMIN  V9.S4, V8.S4, V8.S4
	VEXT   $8, V8.B16, V8.B16, V9.B16
	VUMIN  V9.S4, V8.S4, V8.S4

	VMOV  V8.S[0], R5
	MOVW  R5, (R2)
	ADD   $4, R2
	SUB   $1, R3
	CBNZ  R3, tailsone

tailsdone:
	RET
