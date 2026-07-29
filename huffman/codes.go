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

var (
	encodeTables [34]encodeTable

	// decodeTables[table][first 8 bits] short-circuits the tree walk. Most
	// codewords are eight bits or fewer, so most symbols cost one lookup.
	decodeTables [34][256]decodeEntry
	// maxQuant is the largest absolute coefficient a table can represent, or -1
	// for the two undefined tables, which must never be selected.
	maxQuant [34]int
)

func init() {
	for i := range tables {
		encodeTables[i] = buildEncodeTable(i)
		maxQuant[i] = tableMaxQuant(i, &encodeTables[i])
		buildDecodeTable(i, &decodeTables[i])
	}
}

// buildEncodeTable inverts a decode tree into a symbol-to-codeword map by
// walking every root-to-leaf path.
func buildEncodeTable(idx int) encodeTable {
	var out encodeTable
	tree := tables[idx].tree
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
	tree := tables[idx].tree
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
