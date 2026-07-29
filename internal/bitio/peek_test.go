package bitio

import (
	"math/rand"
	"testing"
)

// peekReference is the definition Peek64 has to meet: the next 64 bits, MSB
// first, with every byte past the end of the data reading as zero. The padded
// tail Peek64 uses instead has to agree with this everywhere, including at
// positions past the end of the data, which a truncated final frame reaches.
func peekReference(data []byte, pos int) uint64 {
	idx := pos >> 3
	var w uint64
	for i := 0; i < 8; i++ {
		var b byte
		if p := idx + i; p >= 0 && p < len(data) {
			b = data[p]
		}
		w = w<<8 | uint64(b)
	}
	return w << uint(pos&7)
}

func TestPeek64MatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 0; n <= 40; n++ {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(rng.Intn(256))
		}
		r := NewReader(data)
		// Well past the end: callers bound themselves by Tell against Len, but a
		// peek beyond that must still be zeros rather than a panic.
		for pos := 0; pos <= n*8+128; pos++ {
			r.Seek(pos)
			if got, want := r.Peek64(), peekReference(data, pos); got != want {
				t.Fatalf("len %d pos %d: Peek64 = %016x, want %016x", n, pos, got, want)
			}
		}
	}
}

func TestPeek64AllOnesTail(t *testing.T) {
	// All-ones data makes any byte wrongly taken from the pad's zero region, or
	// wrongly skipped, visible in the result.
	for n := 0; n <= 24; n++ {
		data := make([]byte, n)
		for i := range data {
			data[i] = 0xFF
		}
		r := NewReader(data)
		for pos := 0; pos <= n*8+64; pos++ {
			r.Seek(pos)
			if got, want := r.Peek64(), peekReference(data, pos); got != want {
				t.Fatalf("len %d pos %d: Peek64 = %016x, want %016x", n, pos, got, want)
			}
		}
	}
}
