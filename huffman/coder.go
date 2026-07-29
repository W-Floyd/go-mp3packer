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
	// Every coefficient below pos is written before it is read, so only the tail
	// above it has to be cleared, and that is done once at the end. Clearing the
	// whole spectrum up front would rewrite 4.6kB per granule to no purpose: a
	// dense granule leaves almost nothing above pos.
	limit := r.Tell() + maxBits
	pos := 0

	// The bit limit is checked once per pair, never mid-symbol: a codeword that
	// starts inside the granule is read whole, matching how decoders behave. A
	// whole pair — code, escape magnitudes and signs — is at most 47 bits, so it
	// comes out of one peeked word rather than bit by bit, and the first eight bits
	// usually resolve the codeword in a single table lookup.
	region := func(pairs, idx int) bool {
		tree := tables[idx].tree
		lut := &decodeTables[idx]
		linbits := uint(tables[idx].linbits)
		end := pos + 2*pairs
		// Both of these depend only on the table, so they are settled before the
		// loop rather than re-tested for every pair. Table 0 codes nothing, so it
		// consumes no bits and cannot overrun the limit.
		if len(tree) == 0 {
			return pos >= end || pos >= NumCoefficients-1
		}
		checkLimit := idx != 0
		for pos < end && pos < NumCoefficients-1 {
			if checkLimit && r.Tell() >= limit {
				return false
			}
			w := r.Peek64()
			var sym, used int
			if e := lut[w>>56]; !e.isLong() {
				sym, used = e.symbol(), e.length()
				w <<= uint(used)
			} else {
				node := e.node()
				used, w = 8, w<<8
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
						r.Skip(used)
						return false
					}
				}
			}

			x, y := sym>>4&0xF, sym&0xF
			if x > 0 {
				if x == 15 && linbits > 0 {
					x += int(w >> (64 - linbits))
					w <<= linbits
					used += int(linbits)
				}
				if w>>63 != 0 {
					x = -x
				}
				w <<= 1
				used++
			}
			if y > 0 {
				if y == 15 && linbits > 0 {
					y += int(w >> (64 - linbits))
					w <<= linbits
					used += int(linbits)
				}
				if w>>63 != 0 {
					y = -y
				}
				used++
			}
			dst[pos], dst[pos+1] = x, y
			pos += 2
			r.Skip(used)
		}
		return true
	}

	ok := true
	r0, r1, r2 := cfg.regionPairs(sampleRate)
	for _, reg := range [][2]int{{r0, cfg.TableSelect[0]}, {r1, cfg.TableSelect[1]}, {r2, cfg.TableSelect[2]}} {
		if !region(reg[0], reg[1]) {
			ok = false
			break
		}
	}

	// Everything past the big values is coded as quadruples of -1, 0 or 1 until
	// the granule's bits run out.
	if ok {
		lut := &decodeTables[cfg.Count1Table]
		for pos <= NumCoefficients-4 && r.Tell() < limit {
			w := r.Peek64()
			e := lut[w>>56]
			if e.isLong() {
				ok = false // no count1 codeword exceeds six bits
				break
			}
			sym, used := e.symbol(), e.length()
			w <<= uint(used)
			for bit := 3; bit >= 0; bit-- {
				val := 0
				if sym&(1<<uint(bit)) != 0 {
					val = 1
					if w>>63 != 0 {
						val = -1
					}
					w <<= 1
					used++
				}
				dst[pos] = val
				pos++
			}
			r.Skip(used)
		}
	}
	if r.Tell() > r.Len() {
		ok = false // ran off the end of the available reservoir data
	}
	if !ok {
		// dst is only meaningful when the granule decoded, so leave the partial
		// result rather than pay to tidy it.
		return false
	}
	clear(dst[pos:])
	return true
}

// Encode writes a spectrum using cfg. cfg must be able to represent every
// coefficient, which is guaranteed for the Config returned by Optimize.
func Encode(s *Spectrum, cfg Config, w *bitio.Writer, sampleRate int) {
	pos := 0
	// A pair's code, escape magnitudes and signs come to at most 47 bits, so they
	// are assembled in a register and handed over in one call.
	writePair := func(x, y, idx int) {
		if idx == 0 {
			return
		}
		linbits := tables[idx].linbits
		ax, ay := abs(x), abs(y)
		sx, sy := min(ax, 15), min(ay, 15)
		c := encodeTables[idx][sx<<4|sy]
		word, n := uint64(c.bits), c.length
		if sx == 15 && linbits > 0 {
			word = word<<uint(linbits) | uint64(ax-15)
			n += linbits
		}
		if ax != 0 {
			word = word<<1 | uint64(signBit(x))
			n++
		}
		if sy == 15 && linbits > 0 {
			word = word<<uint(linbits) | uint64(ay-15)
			n += linbits
		}
		if ay != 0 {
			word = word<<1 | uint64(signBit(y))
			n++
		}
		w.Write64(word, n)
	}

	r0, r1, r2 := cfg.regionPairs(sampleRate)
	for _, reg := range [][2]int{{r0, cfg.TableSelect[0]}, {r1, cfg.TableSelect[1]}, {r2, cfg.TableSelect[2]}} {
		for i := 0; i < reg[0]; i++ {
			writePair(s[pos], s[pos+1], reg[1])
			pos += 2
		}
	}

	last := lastNonZero(s)
	for pos <= NumCoefficients-4 && pos < last {
		sym := 0
		for i := 0; i < 4; i++ {
			if s[pos+i] != 0 {
				sym |= 1 << uint(3-i)
			}
		}
		c := encodeTables[cfg.Count1Table][sym]
		word, n := uint64(c.bits), c.length
		for i := 0; i < 4; i++ {
			if v := s[pos+i]; v != 0 {
				word = word<<1 | uint64(signBit(v))
				n++
			}
		}
		w.Write64(word, n)
		pos += 4
	}
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
