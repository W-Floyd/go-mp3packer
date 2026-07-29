// Package bitio provides MSB-first bit-level reading and writing over byte
// slices, which is the bit order used throughout the MPEG audio bitstream.
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

// Read consumes n bits (0 <= n <= 32) and returns them right-aligned.
func (r *Reader) Read(n int) uint32 {
	var v uint32
	for n > 0 {
		idx := r.pos >> 3
		off := r.pos & 7
		avail := 8 - off
		take := avail
		if n < take {
			take = n
		}
		var b byte
		if idx < len(r.data) {
			b = r.data[idx]
		}
		chunk := (uint32(b) >> uint(avail-take)) & (1<<uint(take) - 1)
		v = v<<uint(take) | chunk
		r.pos += take
		n -= take
	}
	return v
}

// Peek64 returns the next 64 bits without consuming them, most significant bit
// first, zero-filled past the end of the data. Only the top 57 bits are
// guaranteed to be present, which is more than the 47 a single MP3 coefficient
// pair can occupy: reading one word and shifting through it locally is far
// cheaper than fetching a bit at a time.
func (r *Reader) Peek64() uint64 {
	idx := r.pos >> 3
	var w uint64
	if idx+8 <= len(r.data) {
		w = binary.BigEndian.Uint64(r.data[idx:])
	} else {
		for i := 0; i < 8; i++ {
			var b byte
			if idx+i < len(r.data) {
				b = r.data[idx+i]
			}
			w = w<<8 | uint64(b)
		}
	}
	return w << uint(r.pos&7)
}

func (r *Reader) Skip(n int)     { r.pos += n }
func (r *Reader) Tell() int      { return r.pos }
func (r *Reader) Seek(bit int)   { r.pos = bit }
func (r *Reader) Len() int       { return len(r.data) * 8 }
func (r *Reader) Remaining() int { return r.Len() - r.pos }

// Writer accumulates bits MSB-first into a growing byte slice. The final byte
// is zero-padded, matching the per-frame byte alignment of MP3 main data.
type Writer struct {
	buf []byte
	pos int
}

func NewWriter() *Writer { return &Writer{} }

// NewWriterSize returns a Writer with room for n bytes already reserved, which
// avoids regrowing the buffer when the eventual size is known in advance.
func NewWriterSize(n int) *Writer { return &Writer{buf: make([]byte, 0, n)} }

// Write appends the low n bits of v (0 <= n <= 32), most significant first.
func (w *Writer) Write(v uint32, n int) {
	for n > 0 {
		idx := w.pos >> 3
		off := w.pos & 7
		avail := 8 - off
		take := avail
		if n < take {
			take = n
		}
		if idx >= len(w.buf) {
			w.buf = append(w.buf, make([]byte, idx+1-len(w.buf))...)
		}
		chunk := byte((v >> uint(n-take)) & (1<<uint(take) - 1))
		w.buf[idx] |= chunk << uint(avail-take)
		w.pos += take
		n -= take
	}
}

// Write64 appends the low n bits of v (0 <= n <= 64), most significant first.
// Callers that assemble a whole field group in a register can hand it over in one
// call instead of several.
func (w *Writer) Write64(v uint64, n int) {
	if n > 32 {
		w.Write(uint32(v>>uint(n-32)), 32)
		n -= 32
	}
	w.Write(uint32(v)&^(^uint32(0)<<uint(n)), n)
}

// Copy moves n bits from r to w without interpreting them. Used for
// scalefactors, which are re-emitted verbatim: their layout depends on
// scalefac_compress tables we only need to size, never to decode.
func (w *Writer) Copy(r *Reader, n int) {
	for n > 0 {
		chunk := 32
		if n < chunk {
			chunk = n
		}
		w.Write(r.Read(chunk), chunk)
		n -= chunk
	}
}

func (w *Writer) Tell() int { return w.pos }

// Bytes returns the written bits, zero-padded up to a byte boundary.
func (w *Writer) Bytes() []byte { return w.buf }
