package huffman

import "testing"

// TestKernelsSSE2Fallback re-runs the whole kernel comparison with the packed
// minimum disabled. Every x86 made since 2008 takes the SSE4.1 path, so without
// this the emulated-minimum fallback would never be exercised anywhere.
func TestKernelsSSE2Fallback(t *testing.T) {
	if !hasSSE41 {
		t.Skip("this CPU already takes the SSE2 path")
	}
	hasSSE41 = false
	t.Cleanup(func() { hasSSE41 = true })
	TestKernelsMatchPortable(t)
}
