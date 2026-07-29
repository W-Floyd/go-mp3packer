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

// Decode reads a granule's spectrum. r must be positioned at the first Huffman
// bit (that is, past the scalefactors) and maxBits is the number of Huffman bits
// the granule declares.
//
// The returned bool reports whether the granule decoded cleanly: the big-values
// region has to fit within maxBits, and every symbol has to be a defined code.
// A false result means the granule cannot be safely recompressed — most often
// because the frame references reservoir data that the file does not contain.
func Decode(cfg Config, r *bitio.Reader, sampleRate, maxBits int) (Spectrum, bool) {
	var s Spectrum
	limit := r.Tell() + maxBits
	pos := 0
	ok := true

	decodeSymbol := func(idx int) (int, bool) {
		tree := tables[idx].tree
		if len(tree) == 0 {
			return 0, false
		}
		p := 0
		for {
			v := tree[p]
			if v >= 0 {
				return int(v), true
			}
			p++
			if r.Read(1) != 0 {
				p -= int(v)
			}
			if p >= len(tree) {
				return 0, false
			}
		}
	}

	// The bit limit is checked once per pair, never mid-symbol: a codeword that
	// starts inside the granule is read whole, matching how decoders behave.
	region := func(pairs, idx int) bool {
		end := pos + 2*pairs
		for pos < end && pos < NumCoefficients-1 {
			if idx != 0 && r.Tell() >= limit {
				return false
			}
			sym, good := decodeSymbol(idx)
			if !good {
				return false
			}
			linbits := tables[idx].linbits
			x, y := (sym>>4)&0xF, sym&0xF
			if x > 0 {
				if x == 15 && linbits > 0 {
					x += int(r.Read(linbits))
				}
				if r.Read(1) != 0 {
					x = -x
				}
			}
			if y > 0 {
				if y == 15 && linbits > 0 {
					y += int(r.Read(linbits))
				}
				if r.Read(1) != 0 {
					y = -y
				}
			}
			s[pos] = x
			s[pos+1] = y
			pos += 2
		}
		return true
	}

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
		for pos <= NumCoefficients-4 && r.Tell() < limit {
			sym, good := decodeSymbol(cfg.Count1Table)
			if !good {
				ok = false
				break
			}
			for bit := 3; bit >= 0; bit-- {
				v := 0
				if sym&(1<<uint(bit)) != 0 {
					v = 1
					if r.Read(1) != 0 {
						v = -1
					}
				}
				s[pos] = v
				pos++
			}
		}
	}
	if r.Tell() > r.Len() {
		ok = false // ran off the end of the available reservoir data
	}
	return s, ok
}

// Encode writes a spectrum using cfg. cfg must be able to represent every
// coefficient, which is guaranteed for the Config returned by Optimize.
func Encode(s *Spectrum, cfg Config, w *bitio.Writer, sampleRate int) {
	pos := 0
	writePair := func(x, y, idx int) {
		if idx == 0 {
			return
		}
		linbits := tables[idx].linbits
		ax, ay := abs(x), abs(y)
		sx, sy := min(ax, 15), min(ay, 15)
		c := encodeTables[idx][sx<<4|sy]
		w.Write(c.bits, c.length)
		if sx == 15 && linbits > 0 {
			w.Write(uint32(ax-15), linbits)
		}
		if ax != 0 {
			w.Write(signBit(x), 1)
		}
		if sy == 15 && linbits > 0 {
			w.Write(uint32(ay-15), linbits)
		}
		if ay != 0 {
			w.Write(signBit(y), 1)
		}
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
		w.Write(c.bits, c.length)
		for i := 0; i < 4; i++ {
			if v := s[pos+i]; v != 0 {
				w.Write(signBit(v), 1)
			}
		}
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
