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

func TestPeek64MatchesRead(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(rng.Intn(256))
	}
	for _, size := range []int{40, 9, 8, 7, 3, 1, 0} {
		for start := 0; start <= size*8; start++ {
			a := NewReader(data[:size])
			a.Seek(start)
			w := a.Peek64()
			if a.Tell() != start {
				t.Fatalf("Peek64 advanced the reader")
			}
			b := NewReader(data[:size])
			b.Seek(start)
			// Only the top 57 bits are guaranteed; check every one of them.
			for i := 0; i < 57; i++ {
				want := b.Read(1)
				if got := uint32(w >> uint(63-i) & 1); got != want {
					t.Fatalf("size %d start %d: bit %d is %d, want %d", size, start, i, got, want)
				}
			}
		}
	}
}

func TestWrite64MatchesWrite(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	batched, single := NewWriter(), NewWriter()
	for i := 0; i < 2000; i++ {
		n := rng.Intn(65)
		v := rng.Uint64()
		if n < 64 {
			v &= 1<<uint(n) - 1
		}
		batched.Write64(v, n)
		// The same bits, most significant first, one word at a time.
		if n > 32 {
			single.Write(uint32(v>>uint(n-32)), 32)
			single.Write(uint32(v)&^(^uint32(0)<<uint(n-32)), n-32)
		} else {
			single.Write(uint32(v), n)
		}
		if batched.Tell() != single.Tell() {
			t.Fatalf("item %d (%d bits): position %d vs %d", i, n, batched.Tell(), single.Tell())
		}
	}
	a, b := batched.Bytes(), single.Bytes()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d: %#02x vs %#02x", i, a[i], b[i])
		}
	}
}

// TestPendingMatchesWrite64 checks that a caller holding the accumulator in
// locals writes the same bits as one going through the Writer, and that it can
// hand the accumulator back and forth mid-stream. The runs are deliberately
// uneven so that Store lands at a different point in each of them.
func TestPendingMatchesWrite64(t *testing.T) {
	rng := rand.New(rand.NewSource(29))
	local, direct := NewWriter(), NewWriter()
	for run := 0; run < 200; run++ {
		acc, nacc := local.Pending()
		for i := rng.Intn(40); i >= 0; i-- {
			n := rng.Intn(maxPut + 1)
			v := rng.Uint64()
			if n < 64 {
				v &= 1<<uint(n) - 1
			}
			if nacc+n > 64 {
				acc, nacc = local.Store(acc, nacc)
			}
			acc |= v << uint(64-nacc-n)
			nacc += n
			direct.Write64(v, n)
		}
		local.Resume(acc, nacc)
		// Between runs the Writer is used directly, which only works if Resume
		// left it in a state its own put can carry on from.
		local.Write64(uint64(run), 8)
		direct.Write64(uint64(run), 8)
		if local.Tell() != direct.Tell() {
			t.Fatalf("run %d: position %d vs %d", run, local.Tell(), direct.Tell())
		}
	}
	a, b := local.Bytes(), direct.Bytes()
	if len(a) != len(b) {
		t.Fatalf("length %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d: %#02x vs %#02x", i, a[i], b[i])
		}
	}
}
