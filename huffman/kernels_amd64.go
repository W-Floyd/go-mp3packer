package huffman

// hasSSE41 selects the packed-minimum path in the reduction kernels. Every
// 32-bit minimum costs six instructions to emulate on plain SSE2 and one with
// SSE4.1, and the reductions are almost nothing but minimums, so this is the one
// place a feature check earns its keep. PMINUD has been present on x86 since
// 2008, so the SSE2 path is a fallback rather than the expected case.
var hasSSE41 = cpuHasSSE41()

func cpuHasSSE41() bool
