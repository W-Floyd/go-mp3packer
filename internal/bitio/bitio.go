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

	// The last bytes of the data, zero-filled to a full word, so that a peek near
	// the end can be the same load as any other. tailFrom is the byte index pad[0]
	// stands for and lastWord is the highest index a whole word can be loaded from.
	lastWord int
	tailFrom int
	pad      [16]byte
}

func NewReader(data []byte) *Reader {
	r := &Reader{}
	r.reset(data)
	return r
}

// reset points the reader at data and builds the padded copy of its tail. Kept
// separate only so that NewReader stays small enough to inline, which is what
// keeps a Reader on its caller's stack.
func (r *Reader) reset(data []byte) {
	r.data = data
	r.pos = 0
	r.lastWord = len(data) - 8
	r.tailFrom = max(len(data)-7, 0)
	clear(r.pad[:])
	copy(r.pad[:], data[r.tailFrom:])
}

// Peek64 returns the next 64 bits without consuming them, most significant bit
// first, zero-filled past the end of the data. Only the top 57 bits are
// guaranteed to be present, which is more than the 47 a single MP3 coefficient
// pair can occupy.
//
// This is called once per coefficient pair, so it has to inline, and what used to
// stop it was not the load but the call handling the zero-filled tail: a call the
// inliner cannot see through costs 57 of its budget of 80. Reading the tail from a
// padded copy instead leaves the whole function branch-plus-load, at a cost of 44.
// Every index at or past the end of the data selects the pad's zero region, so
// clamping is all the bounds checking the far tail needs.
func (r *Reader) Peek64() uint64 { return r.PeekAt(r.pos) }

// PeekAt is Peek64 at an explicit bit position, leaving the Reader's own position
// alone. Decoding a run of symbols through Peek64 and Skip loads the stored
// position three times and stores it once per symbol, which puts a round trip
// through memory on the loop's critical path; a caller that keeps the position in
// a local and hands it in here pays none of that.
func (r *Reader) PeekAt(pos int) uint64 {
	idx := pos >> 3
	b := r.data
	if idx > r.lastWord {
		b, idx = r.pad[:], min(idx-r.tailFrom, 7)
	}
	return binary.BigEndian.Uint64(b[idx:]) << uint(pos&7)
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

// Writer accumulates bits MSB-first into a growing byte slice. Pending bits are
// held in a register-sized accumulator and reach the buffer eight bytes at a
// time, so appending a field costs a shift and an or; nothing is ever read back
// out of the buffer. The buffer carries eight bytes of slack past the output
// because a flush always stores a whole word, of which it commits only the
// complete bytes; Bytes trims it back.
type Writer struct {
	buf   []byte
	acc   uint64 // pending bits, most significant first, zero below nacc
	nacc  int    // how many of acc's bits are pending; under eight after a flush
	nbyte int    // bytes already committed to buf
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
// empty; its contents need not be zeroed, since every byte of the result is one
// the Writer stored itself. A buffer of n+Slack bytes holds an n-byte result; a
// Writer given less simply allocates when it runs out, so the capacity is a hint
// rather than a limit.
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

// maxPut is the widest field put takes in one go: a flush leaves at most seven
// bits pending, and those plus the field itself must fit in 64.
const maxPut = 57

// put appends up to maxPut bits.
func (w *Writer) put(v uint64, n int) {
	if n == 0 {
		return
	}
	if w.nacc+n > 64 {
		w.flush()
	}
	w.acc |= (v & (1<<uint(n) - 1)) << uint(64-w.nacc-n)
	w.nacc += n
}

// flush stores the accumulator and keeps back whatever did not fill a whole byte.
// It writes eight bytes whatever the state and commits only the complete ones, so
// the next flush overwrites the remainder: every byte the Writer ever returns is
// one it wrote itself, which is why the buffer needs slack but needs neither
// zeroing nor reading back.
func (w *Writer) flush() {
	if w.nbyte+writerSlack > len(w.buf) {
		w.grow(w.nbyte + writerSlack)
	}
	binary.BigEndian.PutUint64(w.buf[w.nbyte:], w.acc)
	whole := w.nacc >> 3
	w.nbyte += whole
	w.acc <<= uint(whole * 8)
	w.nacc -= whole * 8
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

func (w *Writer) Tell() int { return w.nbyte*8 + w.nacc }

// Bytes returns the written bits, zero-padded up to a byte boundary. The pending
// bits are stored without being committed, so a caller can read back what it has
// written so far and then carry on writing — which the verification pass does,
// once per granule.
func (w *Writer) Bytes() []byte {
	if w.nacc > 0 {
		if w.nbyte+writerSlack > len(w.buf) {
			w.grow(w.nbyte + writerSlack)
		}
		binary.BigEndian.PutUint64(w.buf[w.nbyte:], w.acc)
	}
	n := (w.Tell() + 7) / 8
	if n > len(w.buf) {
		return w.buf
	}
	return w.buf[:n]
}
