package bitio

import (
	"math/rand"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	type item struct {
		v uint32
		n int
	}
	var items []item
	w := NewWriter()
	for i := 0; i < 5000; i++ {
		n := rng.Intn(33)
		v := rng.Uint32()
		if n < 32 {
			v &= 1<<uint(n) - 1
		}
		items = append(items, item{v, n})
		w.Write(v, n)
	}
	r := NewReader(w.Bytes())
	for i, it := range items {
		if got := r.Read(it.n); got != it.v {
			t.Fatalf("item %d (%d bits): got %#x want %#x", i, it.n, got, it.v)
		}
	}
}

func TestWriterIsByteAligned(t *testing.T) {
	w := NewWriter()
	w.Write(0x1F, 5)
	if len(w.Bytes()) != 1 {
		t.Fatalf("5 bits should occupy one byte, got %d", len(w.Bytes()))
	}
	if w.Bytes()[0] != 0xF8 {
		t.Fatalf("bits must be left-aligned: got %#x want 0xF8", w.Bytes()[0])
	}
	if w.Tell() != 5 {
		t.Fatalf("Tell = %d, want 5", w.Tell())
	}
}

func TestReadPastEndReturnsZeros(t *testing.T) {
	r := NewReader([]byte{0xFF})
	if got := r.Read(16); got != 0xFF00 {
		t.Fatalf("got %#x, want 0xFF00", got)
	}
	if r.Tell() != 16 || r.Remaining() != -8 {
		t.Fatalf("Tell = %d, Remaining = %d", r.Tell(), r.Remaining())
	}
}

func TestCopyPreservesBits(t *testing.T) {
	src := NewWriter()
	src.Write(0b1011010, 7)
	src.Write(0xABCDEF12, 32)

	r := NewReader(src.Bytes())
	w := NewWriter()
	w.Write(0b101, 3) // offset the destination so the copy is not byte-aligned
	w.Copy(r, 39)

	out := NewReader(w.Bytes())
	if got := out.Read(3); got != 0b101 {
		t.Fatalf("prefix = %#b", got)
	}
	if got := out.Read(7); got != 0b1011010 {
		t.Fatalf("first copied field = %#b", got)
	}
	if got := out.Read(32); got != 0xABCDEF12 {
		t.Fatalf("second copied field = %#x", got)
	}
}
