package huffman

import "sync"

// numBands is the number of scalefactor band boundaries a long block has. Every
// Huffman region boundary is one of these, clamped to big_values.
const numBands = 23

// scratch is the working set of one Optimize call. It is pooled because the
// search runs once per granule — thousands of times per second — and the arrays
// are far too large to keep reallocating.
type scratch struct {
	// Per-table prefix sums over coefficient pairs, so the cost of any region is
	// one subtraction. Pairs a table cannot represent are counted separately
	// rather than folded in as a large cost, which keeps the "impossible" test
	// exact.
	cost [32][MaxBigValues + 1]int32
	bad  [32][MaxBigValues + 1]int32

	// Cheapest table per region, indexed by the band boundaries that delimit it.
	// Index numBands means "up to big_values", the only endpoint that moves as
	// the search varies big_values; every other entry stays valid for the whole
	// call, which is what makes the search affordable.
	bits  [numBands + 1][numBands + 1]int32
	table [numBands + 1][numBands + 1]int8
	stamp [numBands + 1][numBands + 1]int32

	// Cost of coding the tail of the spectrum as count1 quadruples starting at
	// each even coefficient, with either count1 table. Built once per call by
	// walking backwards, since a quadruple's cost never depends on big_values.
	c1Bits32 [NumCoefficients + 4]int32
	c1Bits33 [NumCoefficients + 4]int32
	c1Usable [NumCoefficients + 4]bool
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

const (
	stampUnset  = -1 // entry not computed
	stampAlways = -2 // entry independent of big_values
)

var usableTables = bigValueTables()

// Optimize searches for the cheapest legal Huffman coding of s and returns the
// configuration that achieves it together with its size in bits.
//
// The search is exhaustive over everything the side info can express: the
// big-values split point, both region boundaries, the code table for each
// region, and the count1 table. Encoders typically pick these heuristically, so
// there is usually a little room left, and finding it is pure profit — the
// spectrum, and hence the audio, is untouched.
//
// orig supplies the block geometry, which is not ours to change: the region
// boundaries of a window-switched granule are fixed by the standard.
func Optimize(s *Spectrum, orig Config, sampleRate int) (Config, int) {
	sc := scratchPool.Get().(*scratch)
	defer scratchPool.Put(sc)

	// Coefficients above 1 in magnitude cannot live in the count1 region, so
	// they set a floor on big_values; nothing above the last non-zero
	// coefficient needs coding at all, which sets the ceiling.
	lastBig := 0
	for pair := MaxBigValues - 1; pair >= 0; pair-- {
		if abs(s[2*pair]) > 1 || abs(s[2*pair+1]) > 1 {
			lastBig = pair + 1
			break
		}
	}
	last := lastNonZero(s)
	maxBV := min(max((last+1)/2, lastBig), MaxBigValues)

	for _, tab := range usableTables {
		cost, bad := &sc.cost[tab], &sc.bad[tab]
		cost[0], bad[0] = 0, 0
		for pair := 0; pair < maxBV; pair++ {
			c := pairCost(tab, s[2*pair], s[2*pair+1])
			cost[pair+1], bad[pair+1] = cost[pair], bad[pair]
			if c < 0 {
				bad[pair+1]++
			} else {
				cost[pair+1] += int32(c)
			}
		}
	}
	for i := range sc.stamp {
		for j := range sc.stamp[i] {
			sc.stamp[i][j] = stampUnset
		}
	}

	// bestRegion picks the cheapest table for the pairs [from, to), memoised
	// against the boundary indices it came from. bvStable says whether both
	// endpoints are independent of the current big_values.
	bestRegion := func(i, j, from, to, bv int32, bvStable bool) (tab int, bits int32, ok bool) {
		if from >= to {
			return 0, 0, true
		}
		if sc.stamp[i][j] == stampAlways || sc.stamp[i][j] == bv {
			return int(sc.table[i][j]), sc.bits[i][j], sc.bits[i][j] >= 0
		}
		bits = -1
		for _, t := range usableTables {
			if sc.bad[t][to]-sc.bad[t][from] > 0 {
				continue
			}
			c := sc.cost[t][to] - sc.cost[t][from]
			if bits < 0 || c < bits {
				bits, tab = c, t
			}
		}
		sc.bits[i][j], sc.table[i][j] = bits, int8(tab)
		if bvStable {
			sc.stamp[i][j] = stampAlways
		} else {
			sc.stamp[i][j] = bv
		}
		return tab, bits, bits >= 0
	}

	// The count1 region codes quadruples from 2*big_values up to the last
	// non-zero coefficient. Every candidate big_values needs this cost, so build
	// it for all of them at once: each start position is one quadruple plus the
	// cost of the position four coefficients later.
	for pos := NumCoefficients; pos >= 0; pos -= 2 {
		switch {
		case pos >= last:
			sc.c1Bits32[pos], sc.c1Bits33[pos], sc.c1Usable[pos] = 0, 0, true
		case pos > NumCoefficients-4 || !sc.c1Usable[pos+4]:
			// A quadruple cannot start inside the final one, so a split here
			// would drop a coefficient.
			sc.c1Usable[pos] = false
		default:
			sym, signs := 0, int32(0)
			for i := 0; i < 4; i++ {
				if s[pos+i] != 0 {
					sym |= 1 << uint(3-i)
					signs++
				}
			}
			c32, c33 := encodeTables[count1Table32][sym], encodeTables[count1Table33][sym]
			sc.c1Usable[pos] = c32.valid && c33.valid
			sc.c1Bits32[pos] = sc.c1Bits32[pos+4] + int32(c32.length) + signs
			sc.c1Bits33[pos] = sc.c1Bits33[pos+4] + int32(c33.length) + signs
		}
	}
	count1Cost := func(bv int) (table, bits int, ok bool) {
		pos := 2 * bv
		if !sc.c1Usable[pos] {
			return 0, 0, false
		}
		if sc.c1Bits33[pos] < sc.c1Bits32[pos] {
			return count1Table33, int(sc.c1Bits33[pos]), true
		}
		return count1Table32, int(sc.c1Bits32[pos]), true
	}

	best := orig
	bestBits := -1
	consider := func(cfg Config, bits int) {
		if bestBits < 0 || bits < bestBits {
			best, bestBits = cfg, bits
		}
	}

	b := bands(sampleRate)
	// Pair index of each band boundary, and the last index that is not clamped
	// away by a given big_values.
	var boundary [numBands]int32
	for i := range boundary {
		boundary[i] = int32(b[i] / 2)
	}

	for bv := lastBig; bv <= maxBV; bv++ {
		c1Table, c1Bits, ok := count1Cost(bv)
		if !ok {
			continue
		}
		bv32 := int32(bv)
		// clampIndex reports the boundary as a pair count together with the cache
		// slot to use: boundaries at or beyond big_values all collapse onto it.
		clampIndex := func(i int) (int32, int32, bool) {
			if boundary[i] >= bv32 {
				return bv32, numBands, false
			}
			return boundary[i], int32(i), true
		}

		if orig.WindowSwitching {
			// Only region0's boundary and two tables are ours to choose; the
			// geometry comes from the block type.
			var bound int32
			if orig.BlockType == 2 {
				bound = int32(bandsShort(sampleRate)[3] / 2 * 3)
			} else {
				bound = boundary[8]
			}
			split, slot, stable := bound, int32(8), true
			if split >= bv32 {
				split, slot, stable = bv32, numBands, false
			}
			t0, bits0, ok0 := bestRegion(0, slot, 0, split, bv32, stable)
			t1, bits1, ok1 := bestRegion(slot, numBands, split, bv32, bv32, false)
			if !ok0 || !ok1 {
				continue
			}
			cfg := orig
			cfg.BigValues = bv
			cfg.TableSelect = [3]int{t0, t1, 0}
			cfg.Count1Table = c1Table
			consider(cfg, int(bits0+bits1)+c1Bits)
			continue
		}

		for r0 := 0; r0 < 16; r0++ {
			end0, slot0, stable0 := clampIndex(r0 + 1)
			t0, bits0, ok0 := bestRegion(0, slot0, 0, end0, bv32, stable0)
			if !ok0 {
				continue
			}
			for r1 := 0; r1 < 8; r1++ {
				end1, slot1, stable1 := clampIndex(min(r0+r1+2, numBands-1))
				t1, bits1, ok1 := bestRegion(slot0, slot1, end0, end1, bv32, stable0 && stable1)
				t2, bits2, ok2 := bestRegion(slot1, numBands, end1, bv32, bv32, false)
				if ok1 && ok2 {
					cfg := orig
					cfg.BigValues = bv
					cfg.Region0Count = r0
					cfg.Region1Count = r1
					cfg.TableSelect = [3]int{t0, t1, t2}
					cfg.Count1Table = c1Table
					consider(cfg, int(bits0+bits1+bits2)+c1Bits)
				}
				if end1 == bv32 {
					// The band table is monotonic, so every larger r1 gives the
					// same split with an empty region 2.
					break
				}
			}
			if end0 == bv32 {
				break
			}
		}
	}
	return best, bestBits
}
