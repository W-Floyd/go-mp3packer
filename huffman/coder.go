package huffman

import "github.com/W-Floyd/go-mp3packer/internal/bitio"

// NumCoefficients is the number of quantized spectral values per granule.
const NumCoefficients = 576

// MaxBigValues is the largest number of big-value pairs a granule can declare
// (the field is 9 bits but the spectrum is only 576 values wide).
const MaxBigValues = NumCoefficients / 2

// Spectrum holds one granule's quantized coefficients. Values are the integer
// quantizer outputs; the recompressor never changes them.
type Spectrum [NumCoefficients]int

// Config is everything in the side info that determines how a granule's
// spectrum is Huffman coded. Recompression rewrites the first seven fields and
// leaves the block geometry alone.
type Config struct {
	BigValues    int
	Region0Count int
	Region1Count int
	TableSelect  [3]int
	Count1Table  int // count1Table32 or count1Table33

	WindowSwitching bool
	BlockType       int
	MixedBlock      bool
}

// regionPairs splits the big-values region into the three Huffman regions,
// measured in coefficient pairs.
//
// For window-switched granules the standard fixes the boundaries: short blocks
// put the first three short bands (times three windows) in region 0, other
// switched blocks use the ninth long band, and neither has a third region.
func (c Config) regionPairs(sampleRate int) (r0, r1, r2 int) {
	bv := min(c.BigValues, MaxBigValues)
	if c.WindowSwitching {
		var boundary int
		if c.BlockType == 2 {
			boundary = bandsShort(sampleRate)[3] / 2 * 3
		} else {
			boundary = bands(sampleRate)[8] / 2
		}
		r0 = min(boundary, bv)
		return r0, bv - r0, 0
	}
	b := bands(sampleRate)
	r0 = min(bv, b[min(c.Region0Count+1, 22)]/2)
	r1 = min(bv, b[min(c.Region0Count+c.Region1Count+2, 22)]/2) - r0
	return r0, r1, bv - r0 - r1
}

// Decode reads a granule's spectrum into dst. r must be positioned at the first
// Huffman bit (that is, past the scalefactors) and maxBits is the number of
// Huffman bits the granule declares.
//
// The result reports whether the granule decoded cleanly: the big-values
// region has to fit within maxBits, and every symbol has to be a defined code.
// A false result means the granule cannot be safely recompressed — most often
// because the frame references reservoir data that the file does not contain.
func Decode(dst *Spectrum, cfg Config, r *bitio.Reader, sampleRate, maxBits int) bool {
	// The bit position is carried through these calls in a local rather than in the
	// Reader. Going through Peek64 and Skip for every symbol loads the stored
	// position three times and stores it once, and the store-to-load turnaround
	// lands on the loop's critical path; the Reader is resynchronised once at the
	// end instead.
	bp := r.Tell()
	limit := bp + maxBits
	pos := 0

	ok := true
	r0, r1, r2 := cfg.regionPairs(sampleRate)
	regions := [3][2]int{{r0, cfg.TableSelect[0]}, {r1, cfg.TableSelect[1]}, {r2, cfg.TableSelect[2]}}
	for _, reg := range regions {
		pos, bp, ok = decodeRegion(dst, r, pos, reg[0], reg[1], limit, bp)
		if !ok {
			break
		}
	}

	// Everything past the big values is coded as quadruples of -1, 0 or 1 until
	// the granule's bits run out.
	if ok {
		pos, bp, ok = decodeCount1(dst, r, pos, cfg.Count1Table, limit, bp)
	}

	r.Seek(bp)
	if bp > r.Len() {
		ok = false // ran off the end of the available reservoir data
	}
	if !ok {
		// dst is only meaningful when the granule decoded, so leave the partial
		// result rather than pay to tidy it.
		return false
	}
	// Every coefficient below pos was written before it could be read, so only the
	// tail above it has to be cleared. Clearing the whole spectrum up front would
	// rewrite 4.6kB per granule to no purpose: a dense granule leaves almost
	// nothing above pos.
	clear(dst[pos:])
	return true
}

// decodeRegion decodes one Huffman region: up to pairs coefficient pairs with
// table idx, starting at coefficient pos and bit position bp. It returns both
// positions and whether the region decoded cleanly.
//
// The bit limit is checked once per pair, never mid-symbol: a codeword that
// starts inside the granule is read whole, matching how decoders behave. A whole
// pair — code, escape magnitudes and signs — is at most 47 bits, so it comes out
// of one peeked word rather than bit by bit, and the first pairBits of it almost
// always resolve the codeword and both its magnitudes in a single lookup.
func decodeRegion(dst *Spectrum, r *bitio.Reader, pos, pairs, idx, limit, bp int) (int, int, bool) {
	tree := trees[idx]
	// A pair needs two coefficients, so the spectrum's own end bounds the region
	// as much as the declared pair count does. Folding them together leaves one
	// comparison per pair instead of two.
	end := min(pos+2*pairs, NumCoefficients-1)
	// Both of the next two depend only on the table, so they are settled before
	// the loop rather than re-tested for every pair. Table 0 codes nothing, so it
	// consumes no bits and cannot overrun the limit.
	if len(tree) == 0 {
		return pos, bp, pos >= end
	}
	lut := &pairTables[idx]
	linbits := uint(tables[idx].linbits)
	checkLimit := idx != 0
	// A magnitude of 15 escapes to linbits, but only for tables that have any.
	// Setting the trigger out of range for the others folds "does this table
	// escape" into the comparison the loop was making anyway.
	escape := 15
	if linbits == 0 {
		escape = 16
	}
	for pos < end {
		if checkLimit && bp >= limit {
			return pos, bp, false
		}
		w := r.PeekAt(bp)
		var x, y, used int
		var nx, ny uint
		// One load settles the codeword and both magnitudes it stands for. The
		// prefixes it cannot settle are the ones no codeword of pairBits bits
		// covers — one symbol in a hundred on real material — and those are walked
		// from the root rather than resumed part-way, since carrying a resume
		// position would cost a second table the fast path would have to share its
		// cache with. Measured: resuming was 3% slower than starting over.
		if e := lut[w>>(64-pairBits)]; !e.isSlow() {
			// The codeword length comes out first: everything below waits on the
			// window having moved past the codeword, and nothing waits on the
			// magnitudes.
			used = e.length()
			w <<= uint(used)
			x, y = e.x(), e.y()
			nx, ny = e.nx(), e.ny()
		} else {
			node, sym := 0, 0
			for {
				v := tree[node]
				if v >= 0 {
					sym = int(v)
					break
				}
				node++
				if w>>63 != 0 {
					node -= int(v)
				}
				w <<= 1
				used++
				if node >= len(tree) {
					return pos, bp + used, false
				}
			}
			d := &pairDecode[sym]
			x, y = int(d.x), int(d.y)
			nx, ny = uint(d.nx), uint(d.ny)
		}

		// Neither the sign bits nor whether a value is zero can be predicted, so
		// neither is branched on. Applying a sign is an xor and an add — x^-1+1 is
		// -x, x^0+0 is x — and it is a no-op on zero for either sign bit, which
		// leaves only the bit advance to depend on the value: a shift by the sign
		// count, zero or one.
		if x == escape {
			x += int(w >> (64 - linbits))
			w <<= linbits
			used += int(linbits)
		}
		sx := int(w >> 63)
		x = (x ^ -sx) + sx
		w <<= nx
		used += int(nx)

		if y == escape {
			y += int(w >> (64 - linbits))
			w <<= linbits
			used += int(linbits)
		}
		sy := int(w >> 63)
		y = (y ^ -sy) + sy
		used += int(ny)

		dst[pos], dst[pos+1] = x, y
		pos += 2
		bp += used
	}
	return pos, bp, true
}

// decodeCount1 decodes the quadruple-coded tail from coefficient pos and bit
// position bp until the granule's bits run out or the spectrum ends.
func decodeCount1(dst *Spectrum, r *bitio.Reader, pos, table, limit, bp int) (int, int, bool) {
	lut := &decodeTables[table]
	for pos <= NumCoefficients-4 && bp < limit {
		w := r.PeekAt(bp)
		e := lut[w>>56]
		if e.isLong() {
			return pos, bp, false // no count1 codeword exceeds six bits
		}
		sym, used := e.symbol(), e.length()
		w <<= uint(used)
		// One lookup covers the pattern and its signs together; the sign bits sit
		// at the top of w now that the codeword has been shifted off.
		q := &count1Quad[sym<<4|int(w>>60)]
		dst[pos] = int(q[0])
		dst[pos+1] = int(q[1])
		dst[pos+2] = int(q[2])
		dst[pos+3] = int(q[3])
		pos += 4
		bp += used + int(count1Signs[sym])
	}
	return pos, bp, true
}

// Encode writes a spectrum using cfg. cfg must be able to represent every
// coefficient, which is guaranteed for the Config returned by Optimize.
func Encode(s *Spectrum, cfg Config, w *bitio.Writer, sampleRate int) {
	encode(s, cfg, w, sampleRate, lastNonZero(s))
}

// Encode writes s with the coding the search chose for it. It is [Encode] for a
// caller that has just searched the same spectrum, and cheaper by the walk of
// the trailing zero run that the search already did.
//
// s must be the spectrum c was searched from. A Coding whose Bits is negative
// has no coding to write and must not be used.
func (c Coding) Encode(s *Spectrum, w *bitio.Writer, sampleRate int) {
	encode(s, c.Config, w, sampleRate, c.last)
}

// encode writes the spectrum. last is one past the highest non-zero coefficient,
// which is where the count1 region stops.
func encode(s *Spectrum, cfg Config, w *bitio.Writer, sampleRate, last int) {
	pos := 0
	// The accumulator is carried in these two locals for the whole of the
	// granule and handed back at the end, so that appending a field is a shift
	// and an or in registers rather than a call into the Writer and a round trip
	// through its state. Same reason the decoder keeps its bit position in a
	// local: with the field group already assembled, the write side's critical
	// path was the load-modify-store of acc and nacc, once per pair.
	acc, nacc := w.Pending()
	r0, r1, r2 := cfg.regionPairs(sampleRate)
	regions := [3][2]int{{r0, cfg.TableSelect[0]}, {r1, cfg.TableSelect[1]}, {r2, cfg.TableSelect[2]}}
	for _, reg := range regions {
		pairs, idx := reg[0], reg[1]
		if idx == 0 {
			pos += 2 * pairs // table 0 codes nothing
			continue
		}
		// Everything that depends only on the table is settled per region: the
		// per-pair loop then touches one code entry and nothing else global.
		linbits := tables[idx].linbits
		tab := &encodeTables[idx]
		for i := 0; i < pairs; i++ {
			// A pair's code, escape magnitudes and signs come to at most 47 bits,
			// so they are assembled in a register and handed over in one call.
			x, y := s[pos], s[pos+1]
			ax, ay := abs(x), abs(y)
			sx, sy := min(ax, 15), min(ay, 15)
			c := tab[sx<<4|sy]
			word, n := uint64(c.bits), c.length
			if sx == 15 && linbits > 0 {
				word = word<<uint(linbits) | uint64(ax-15)
				n += linbits
			}
			// Whether a coefficient is zero is not predictable, so its sign bit is
			// appended by shifting the word by zero or one instead of branching:
			// masking the bit by the same count leaves the word untouched when
			// there is no sign to write.
			nx := uint(b2u(ax != 0))
			word = word<<nx | uint64(x)>>63&uint64(nx)
			n += int(nx)
			if sy == 15 && linbits > 0 {
				word = word<<uint(linbits) | uint64(ay-15)
				n += linbits
			}
			ny := uint(b2u(ay != 0))
			word = word<<ny | uint64(y)>>63&uint64(ny)
			n += int(ny)

			if nacc+n > 64 {
				acc, nacc = w.Store(acc, nacc)
			}
			acc |= (word & (1<<uint(n) - 1)) << uint(64-nacc-n)
			nacc += n
			pos += 2
		}
	}

	tab := &encodeTables[cfg.Count1Table]
	for pos <= NumCoefficients-4 && pos < last {
		// The quadruple's symbol and its sign bits are built in the same pass, both
		// branch-free: a zero coefficient contributes no symbol bit and shifts the
		// word by nothing.
		q := s[pos : pos+4 : pos+4]
		sym := 0
		var signs uint64
		var ns uint
		for i, v := range q {
			nz := uint(b2u(v != 0))
			sym |= int(nz) << uint(3-i)
			signs = signs<<nz | uint64(v)>>63&uint64(nz)
			ns += nz
		}
		c := tab[sym]
		word, n := uint64(c.bits)<<ns|signs, c.length+int(ns)
		if nacc+n > 64 {
			acc, nacc = w.Store(acc, nacc)
		}
		acc |= (word & (1<<uint(n) - 1)) << uint(64-nacc-n)
		nacc += n
		pos += 4
	}
	w.Resume(acc, nacc)
}

// lastNonZero returns one past the index of the highest non-zero coefficient.
func lastNonZero(s *Spectrum) int {
	for i := NumCoefficients - 1; i >= 0; i-- {
		if s[i] != 0 {
			return i + 1
		}
	}
	return 0
}

func signBit(v int) uint32 {
	if v < 0 {
		return 1
	}
	return 0
}
