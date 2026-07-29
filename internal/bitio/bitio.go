// Package bitio provides MSB-first bit-level reading and writing over byte
// slices, which is the bit order used throughout the MPEG audio bitstream.
//
// Both directions work through a single 64-bit access per operation rather than
// walking bytes: no MP3 field is wider than 32 bits, and a whole coefficient pair
// fits in 47, so one load or read-modify-write covers any of them.
package bitio

import "encoding/binary"

// Reader reads bits MSB-first from a fixed byte slice. Reads past the end of
// the slice yield zero bits, so callers that care must compare Tell against
// Len; the MP3 bitstream is self-delimiting via part2_3_length, and a truncated
// final frame is better handled as zero-filled than as a hard error.
type Reader struct {
	data []byte
	pos  int // absolute bit position
}

func NewReader(data []byte) *Reader {
	return &Reader{data: data}
}

// Peek64 returns the next 64 bits without consuming them, most significant bit
// first, zero-filled past the end of the data. Only the top 57 bits are
// guaranteed to be present, which is more than the 47 a single MP3 coefficient
// pair can occupy.
// The zero-filled tail case is kept out of line so that the hot path is just the
// aligned load, which the compiler fuses into one load and a byte swap. It is
// still a call: the inliner scores the pre-optimisation form at 89 against a
// budget of 80, so it does not quite fit however this is arranged.
func (r *Reader) Peek64() uint64 {
	idx := r.pos >> 3
	if idx+8 <= len(r.data) {
		return binary.BigEndian.Uint64(r.data[idx:]) << uint(r.pos&7)
	}
	return r.peekTail()
}

//go:noinline
func (r *Reader) peekTail() uint64 {
	idx := r.pos >> 3
	var w uint64
	for i := 0; i < 8; i++ {
		var b byte
		if idx+i < len(r.data) {
			b = r.data[idx+i]
		}
		w = w<<8 | uint64(b)
	}
	return w << uint(r.pos&7)
}

// Read consumes n bits (0 <= n <= 32) and returns them right-aligned.
func (r *Reader) Read(n int) uint32 {
	if n == 0 {
		return 0
	}
	v := uint32(r.Peek64() >> uint(64-n))
	r.pos += n
	return v
}

func (r *Reader) Skip(n int)     { r.pos += n }
func (r *Reader) Tell() int      { return r.pos }
func (r *Reader) Seek(bit int)   { r.pos = bit }
func (r *Reader) Len() int       { return len(r.data) * 8 }
func (r *Reader) Remaining() int { return r.Len() - r.pos }

// Writer accumulates bits MSB-first into a growing byte slice. The buffer always
// carries eight zeroed bytes of slack past the write position so that any field
// can be merged in with a single read-modify-write; Bytes trims it back.
type Writer struct {
	buf []byte
	pos int
}

const writerSlack = 8

// Slack is how much capacity past its output a Writer needs, so that a caller
// providing its own buffer to NewWriterBuf can size it exactly.
const Slack = writerSlack

func NewWriter() *Writer { return &Writer{} }

// NewWriterSize returns a Writer with room for n bytes already reserved, which
// avoids regrowing the buffer when the eventual size is known in advance.
func NewWriterSize(n int) *Writer {
	return &Writer{buf: make([]byte, 0, n+writerSlack)}
}

// NewWriterBuf returns a Writer that builds its output in buf, which must be
// empty and zeroed: fields are merged in with a read-modify-write, so anything
// already there would show up in the output. A buffer of n+Slack bytes holds an
// n-byte result; a Writer given less simply allocates when it runs out, so the
// capacity is a hint rather than a limit.
func NewWriterBuf(buf []byte) *Writer {
	return &Writer{buf: buf[:0]}
}

// Write appends the low n bits of v (0 <= n <= 32), most significant first.
func (w *Writer) Write(v uint32, n int) { w.put(uint64(v), n) }

// Write64 appends the low n bits of v (0 <= n <= 64), most significant first.
// Callers that assemble a whole field group in a register can hand it over in one
// call instead of several.
func (w *Writer) Write64(v uint64, n int) {
	// Anything up to 57 bits still lands inside a single 64-bit window whatever
	// the misalignment, so it needs only one read-modify-write. Nothing in the
	// bitstream is wider than that — a coefficient pair is at most 47 bits — so
	// the split path exists only for completeness.
	if n > maxPut {
		w.put(v>>32, n-32)
		n = 32
	}
	w.put(v, n)
}

// maxPut is the widest field put can merge in one access: 7 bits of possible
// misalignment plus the field itself must fit in 64.
const maxPut = 57

// put merges up to maxPut bits into the buffer.
func (w *Writer) put(v uint64, n int) {
	if n == 0 {
		return
	}
	idx := w.pos >> 3
	if idx+writerSlack > len(w.buf) {
		w.grow(idx + writerSlack)
	}
	v &= 1<<uint(n) - 1
	word := binary.BigEndian.Uint64(w.buf[idx:]) | v<<uint(64-(w.pos&7)-n)
	binary.BigEndian.PutUint64(w.buf[idx:], word)
	w.pos += n
}

func (w *Writer) grow(size int) {
	if size <= cap(w.buf) {
		w.buf = w.buf[:size]
		return
	}
	next := 2 * cap(w.buf)
	if next < size {
		next = size
	}
	buf := make([]byte, size, next)
	copy(buf, w.buf)
	w.buf = buf
}

// Copy moves n bits from r to w without interpreting them. Used for
// scalefactors, which are re-emitted verbatim: their layout depends on
// scalefac_compress tables we only need to size, never to decode.
func (w *Writer) Copy(r *Reader, n int) {
	for ; n >= 32; n -= 32 {
		w.put(uint64(r.Read(32)), 32)
	}
	w.put(uint64(r.Read(n)), n)
}

func (w *Writer) Tell() int { return w.pos }

// Bytes returns the written bits, zero-padded up to a byte boundary.
func (w *Writer) Bytes() []byte {
	n := (w.pos + 7) / 8
	if n > len(w.buf) {
		return w.buf
	}
	return w.buf[:n]
}
