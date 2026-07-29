package huffman

import "testing"

// TestKernelsDispatchPaths runs the whole kernel comparison against every amd64
// form this CPU can reach. Only the widest one runs by default, so without this
// the narrower paths would go unexercised -- and for SSE4.1 that means every
// machine made since about 2013.
func TestKernelsDispatchPaths(t *testing.T) {
	avx2, sse41 := hasAVX2, hasSSE41
	t.Cleanup(func() { hasAVX2, hasSSE41 = avx2, sse41 })

	cases := []struct {
		name        string
		avx2, sse41 bool
	}{
		{"avx2", true, true},
		{"sse41", false, true},
		{"sse2", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.avx2 && !avx2 {
				t.Skip("no AVX2 on this CPU")
			}
			if c.sse41 && !sse41 {
				t.Skip("no SSE4.1 on this CPU")
			}
			hasAVX2, hasSSE41 = c.avx2, c.sse41
			TestKernelsMatchPortable(t)
		})
	}
}
