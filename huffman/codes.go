// Package huffman implements the lossless part of MP3 recompression: decoding a
// granule's quantized spectrum, searching for the cheapest legal way to code
// the same spectrum, and writing it back out.
//
// Nothing here touches the quantizer, so every transformation is exactly
// reversible: the decoded coefficients, and therefore the decoded audio, are
// bit-identical before and after.
package huffman

// code is one encodable symbol of a code table.
type code struct {
	bits   uint32
	length int
	valid  bool
}

// encodeTable maps a packed (x<<4)|y pair to its codeword. Index 0..255 covers
// the big-value tables; the count1 tables use only indices 0..15.
type encodeTable [256]code

// decodeEntry resolves the first eight bits of a codeword in one lookup: either
// the whole symbol, or where in the tree to carry on from. Packed into a word so
// the decoder loads it in one go — symbol in bits 0..7, code length in 8..12, the
// long-code flag in 13, and the tree position to resume from in 16..31.
type decodeEntry uint32

const decodeLong = 1 << 13

func (e decodeEntry) isLong() bool { return e&decodeLong != 0 }
func (e decodeEntry) symbol() int  { return int(e) & 0xFF }
func (e decodeEntry) length() int  { return int(e>>8) & 0x1F }
func (e decodeEntry) node() int    { return int(e >> 16) }

// pairBits is how many bits of the stream index the big-value probe, and it was
// chosen by measurement rather than by coverage. On real material 92% of
// codewords are eight bits or fewer, 99.0% ten or fewer and 99.9% eleven, but the
// table has to stay small enough to sit in L1 next to everything else the search
// is touching: one worker's repack of the long track came out 4.1% faster than the
// eight-bit table at nine bits, 8.6% at ten, 8.4% at eleven and only 6.0% at
// twelve. Ten it is — the same as eleven for half the footprint, and the wider
// tables lose to their own cache misses however much of the tail they cover.
const (
	pairBits = 10
	pairSize = 1 << pairBits
)

// pairEntry is a resolved big-value pair: not the symbol, but the two magnitudes
// the symbol stands for, so that the decoder's per-symbol work is one load rather
// than a load of the symbol followed by a dependent load of its split. That
// second load was worth more than the width of the table — folding it in is 14.6%
// off a granule decode where widening alone was 4.6%.
//
// Magnitudes are the clamped 0..15 the code table encodes, x in bits 0..3 and y
// in 4..7, the codeword length in 8..11, and each magnitude's sign-bit count in
// 12 and 13. pairSlow marks a prefix no codeword of pairBits bits or fewer
// resolves, which the tree walk then handles from the root.
type pairEntry uint16

const pairSlow = 1 << 15

func (e pairEntry) x() int       { return int(e) & 0xF }
func (e pairEntry) y() int       { return int(e>>4) & 0xF }
func (e pairEntry) length() int  { return int(e>>8) & 0xF }
func (e pairEntry) nx() uint     { return uint(e>>12) & 1 }
func (e pairEntry) ny() uint     { return uint(e>>13) & 1 }
func (e pairEntry) isSlow() bool { return e >= pairSlow }

var (
	encodeTables [34]encodeTable

	// decodeTables[table][first 8 bits] short-circuits the tree walk. The
	// big-value regions go through pairTables instead; what is left for this one is
	// the count1 tail, whose codewords are six bits at most and so always resolve
	// in the one lookup.
	decodeTables [34][256]decodeEntry

	// pairTables[table][first pairBits bits] resolves a big-value pair. Only the
	// 32 selectable big-value tables have one; the count1 tables are not indexed
	// here.
	pairTables [32][pairSize]pairEntry
	// maxQuant is the largest absolute coefficient a table can represent, or -1
	// for the two undefined tables, which must never be selected.
	maxQuant [34]int

	// count1Quad resolves a whole count1 quadruple in one lookup. The symbol says
	// which of the four values are non-zero and their sign bits follow in the same
	// order, so symbol and the next four bits of the stream together determine all
	// four coefficients. Bits past the symbol's sign count belong to whatever comes
	// next and must not matter, which is why every combination of them is
	// tabulated: entries that differ only in those bits are equal. int8 keeps the
	// whole table inside 1kB, since the values are only -1, 0 and 1.
	count1Quad [256][4]int8

	// count1Signs is how many sign bits a count1 symbol carries.
	count1Signs [16]uint8

	// pairDecode splits a big-value symbol into its two magnitudes and how many
	// sign bits each of them carries. Having the counts to hand lets the decoder
	// advance the bit window by a shift of zero or one instead of branching on
	// whether a value is zero, which is not predictable.
	pairDecode [256]pairSplit
)

type pairSplit struct {
	x, y   int8
	nx, ny uint8
}

func init() {
	for sym := 0; sym < 16; sym++ {
		n := 0
		for i := 0; i < 4; i++ {
			if sym&(8>>uint(i)) != 0 {
				n++
			}
		}
		count1Signs[sym] = uint8(n)
		for pat := 0; pat < 16; pat++ {
			var q [4]int8
			k := 0
			for i := 0; i < 4; i++ {
				if sym&(8>>uint(i)) == 0 {
					continue
				}
				q[i] = 1
				if pat&(8>>uint(k)) != 0 {
					q[i] = -1
				}
				k++
			}
			count1Quad[sym<<4|pat] = q
		}
	}
	for sym := range pairDecode {
		x, y := int8(sym>>4&0xF), int8(sym&0xF)
		pairDecode[sym] = pairSplit{x: x, y: y, nx: b2u(x != 0), ny: b2u(y != 0)}
	}
}

func b2u(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

func init() {
	for i := range tables {
		encodeTables[i] = buildEncodeTable(i)
		maxQuant[i] = tableMaxQuant(i, &encodeTables[i])
		buildDecodeTable(i, &decodeTables[i])
		if i < len(pairTables) {
			buildPairTable(i, &pairTables[i])
		}
	}
}

// buildPairTable walks every pairBits-wide prefix through the decode tree and
// records the pair it resolves to. A prefix the walk cannot finish in that many
// bits is marked slow, and so is one that runs off the end of the tree — which
// only the two tables the standard leaves undefined can do, and which the walk has
// to reach for itself to report the failure.
func buildPairTable(idx int, out *[pairSize]pairEntry) {
	tree := trees[idx]
	if len(tree) == 0 {
		for i := range out {
			out[i] = pairSlow
		}
		return
	}
	for prefix := 0; prefix < pairSize; prefix++ {
		node, used := 0, 0
		for {
			v := tree[node]
			if v >= 0 {
				sym := int(v) & 0xFF
				x, y := sym>>4, sym&0xF
				out[prefix] = pairEntry(x) | pairEntry(y)<<4 | pairEntry(used)<<8 |
					pairEntry(b2u(x != 0))<<12 | pairEntry(b2u(y != 0))<<13
				break
			}
			if used == pairBits {
				out[prefix] = pairSlow
				break
			}
			node++
			if prefix&(1<<uint(pairBits-1-used)) != 0 {
				node -= int(v)
			}
			used++
			if node >= len(tree) {
				out[prefix] = pairSlow
				break
			}
		}
	}
}

// buildEncodeTable inverts a decode tree into a symbol-to-codeword map by
// walking every root-to-leaf path.
func buildEncodeTable(idx int) encodeTable {
	var out encodeTable
	tree := trees[idx]
	if len(tree) == 0 || tree[0] == 0 {
		// The empty tables (0, 4, 14) code nothing but the all-zero pair, for
		// which they emit no bits at all.
		out[0] = code{valid: true}
		return out
	}
	type state struct {
		pos    int
		bits   uint32
		length int
	}
	stack := []state{{}}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		v := tree[s.pos]
		switch {
		case v < 0: // interior node
			stack = append(stack,
				state{s.pos + 1, s.bits << 1, s.length + 1},
				state{s.pos + 1 - int(v), s.bits<<1 | 1, s.length + 1})
		case v < 256: // leaf
			out[v] = code{bits: s.bits, length: s.length, valid: true}
		}
		// Leaves at or above 256 are escape entries with no encodable symbol.
	}
	return out
}

// buildDecodeTable walks each possible eight-bit prefix through the decode tree,
// recording the symbol if the walk finishes and the tree position if it does not.
func buildDecodeTable(idx int, out *[256]decodeEntry) {
	tree := trees[idx]
	if len(tree) == 0 {
		return
	}
	for prefix := 0; prefix < 256; prefix++ {
		node, used := 0, 0
		for {
			v := tree[node]
			if v >= 0 {
				out[prefix] = decodeEntry(uint32(v)&0xFF | uint32(used)<<8)
				break
			}
			if used == 8 {
				out[prefix] = decodeEntry(decodeLong | uint32(node)<<16)
				break
			}
			node++
			if prefix&(1<<uint(7-used)) != 0 {
				node -= int(v)
			}
			used++
			if node >= len(tree) {
				// Only reachable for the two tables the standard leaves
				// undefined, which no valid stream selects.
				out[prefix] = decodeEntry(decodeLong)
				break
			}
		}
	}
}

func tableMaxQuant(idx int, t *encodeTable) int {
	if idx == 4 || idx == 14 {
		return -1 // reserved by the standard
	}
	best := 0
	for sym, c := range t {
		if !c.valid {
			continue
		}
		best = max(best, max(sym>>4, sym&15))
	}
	if lb := tables[idx].linbits; lb > 0 {
		// The escape symbol 15 is extended by linbits of magnitude.
		best += 1<<uint(lb) - 1
	}
	return best
}

// bigValueTables lists the table indices selectable for a big-values region.
func bigValueTables() []int {
	out := make([]int, 0, 30)
	for i := 0; i < 32; i++ {
		if maxQuant[i] >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// pairCost returns the number of bits table idx needs for the coefficient pair
// (x, y), or -1 if the table cannot represent it.
func pairCost(idx, x, y int) int {
	if maxQuant[idx] < 0 {
		return -1
	}
	ax, ay := abs(x), abs(y)
	if ax > maxQuant[idx] || ay > maxQuant[idx] {
		return -1
	}
	sym, esc := min(ax, 15), tables[idx].linbits
	symY := min(ay, 15)
	c := encodeTables[idx][sym<<4|symY]
	if !c.valid {
		return -1
	}
	bits := c.length
	if sym == 15 {
		bits += esc
	}
	if symY == 15 {
		bits += esc
	}
	if ax != 0 {
		bits++ // sign
	}
	if ay != 0 {
		bits++
	}
	return bits
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
