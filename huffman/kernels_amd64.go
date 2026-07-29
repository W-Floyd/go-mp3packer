package huffman

// The reduction kernels have three amd64 forms, picked once at startup.
//
// SSE2 is the baseline but has no 32-bit minimum at all, so each one costs six
// instructions to emulate. SSE4.1 does it in one. AVX2 goes further: the lanes
// are 32 int32 wide, which is four 256-bit registers instead of eight 128-bit
// ones, and the VEX encoding is three-operand with a memory source, so the
// register-to-register copies that two-operand SSE needs before every
// destructive op disappear entirely.
var (
	hasAVX2  = cpuHasAVX2()
	hasSSE41 = cpuHasSSE41()
)

func cpuHasAVX2() bool
func cpuHasSSE41() bool
