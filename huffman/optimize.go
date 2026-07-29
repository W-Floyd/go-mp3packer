package huffman

import "sync"

// numBands is the number of scalefactor band boundaries a long block has.
const numBands = 23

// Prefix rows are addressed by slot. Slots 0..numBands-1 are the long-block band
// boundaries; short blocks put region0's boundary somewhere else entirely, so it
// gets its own slot; and the last slot is wherever big_values currently sits,
// which is the accumulator itself.
const (
	shortSlot = numBands
	bvSlot    = numBands + 1
	numSlots  = numBands + 2
)

// scratch is the working set of one Optimize call. It is pooled because the
// search runs once per granule — tens of thousands of times per second — and the
// arrays are far too large to keep reallocating.
type scratch struct {
	// keys[p] is the packed cost-table index of coefficient pair p.
	keys [MaxBigValues]uint32

	// acc is the running per-table cost of every pair below the big_values
	// currently being considered, and rows[i] the same total at band boundary i.
	// Only these 24 rows are ever needed, so the search never has to materialise
	// a cost per pair: a query is two rows, laid out so that all 32 tables sit
	// next to each other.
	acc  [numTables]int32
	rows [numSlots - 1][numTables]int32

	// Cheapest table per region, memoised against the slots that delimit it. Any
	// region ending at big_values has to be recomputed as it moves; every other
	// entry stays valid for the whole call, which is what makes the search
	// affordable. One struct per entry, so a lookup is one cache line.
	//
	// Region 0 always starts at pair zero and region 2 always ends at big_values,
	// so those two get flat arrays: the middle region is the only one that needs
	// both endpoints as a key.
	head [numSlots]memoEntry
	mid  [numSlots][numSlots]memoEntry
	tail [numSlots]memoEntry

	// prefix[j] is the cheapest way to cover pairs [0, boundary j) with regions 0
	// and 1, over every region0 boundary the side info can pair with j. Neither
	// region moves with big_values, so this is computed once per boundary and then
	// reused for every big_values above it.
	prefix [numBands]prefixSplit

	// Cost of coding the tail of the spectrum as count1 quadruples starting at
	// each even coefficient, with either count1 table.
	c1Bits32 [NumCoefficients + 4]int32
	c1Bits33 [NumCoefficients + 4]int32
	c1Usable [NumCoefficients + 4]bool
}

// prefixSplit is the best two-region cover of everything below a boundary: its
// cost, where region0 ends, and the tables both regions use.
type prefixSplit struct {
	bits    int32
	ready   bool
	ok      bool
	region0 int32 // the region0 boundary index that achieved it
	table0  int8
	table1  int8
}

// memoEntry caches one region's cheapest table. stamp records which big_values
// the entry was computed for, or stampAlways if it cannot go stale.
type memoEntry struct {
	stamp int32
	bits  int32
	table int8
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

const (
	stampUnset  = -1 // entry not computed
	stampAlways = -2 // entry independent of big_values
)

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

	for p := 0; p < maxBV; p++ {
		sc.keys[p] = pairKey(s[2*p], s[2*p+1])
	}
	for i := range sc.acc {
		sc.acc[i] = 0
	}
	for i := range sc.prefix {
		sc.prefix[i].ready = false
	}
	for i := range sc.mid {
		sc.head[i].stamp, sc.tail[i].stamp = stampUnset, stampUnset
		for j := range sc.mid[i] {
			sc.mid[i][j].stamp = stampUnset
		}
	}
	buildCount1Costs(sc, s, last)

	b := bands(sampleRate)
	var boundary [numBands]int32
	for i := range boundary {
		boundary[i] = int32(b[i] / 2)
	}
	shortBound := int32(bandsShort(sampleRate)[3] / 2 * 3)

	// Every prefix the search needs a snapshot of, in the order the accumulator
	// will reach them.
	var snapPos, snapSlot [numSlots - 1]int32
	n, inserted := 0, false
	for i := 0; i < numBands; i++ {
		if !inserted && shortBound < boundary[i] {
			snapPos[n], snapSlot[n] = shortBound, shortSlot
			n, inserted = n+1, true
		}
		snapPos[n], snapSlot[n] = boundary[i], int32(i)
		n++
	}
	if !inserted {
		snapPos[n], snapSlot[n] = shortBound, shortSlot
	}

	row := func(slot int32) *[numTables]int32 {
		if slot == bvSlot {
			return &sc.acc
		}
		return &sc.rows[slot]
	}

	// The three region lookups. Each returns the cheapest table for its span and
	// caches the answer; entries that cannot go stale as big_values moves are
	// stamped once and reused for the rest of the call.
	lookup := func(e *memoEntry, i, j, from, to, bv int32, stable bool) (int, int32, bool) {
		if from >= to {
			return 0, 0, true
		}
		if e.stamp == stampAlways || e.stamp == bv {
			return int(e.table), e.bits, e.bits >= 0
		}
		tab, cost := unpackBest(bestTable(row(i), row(j)))
		e.bits, e.table, e.stamp = cost, int8(tab), bv
		if stable {
			e.stamp = stampAlways
		}
		return tab, cost, cost >= 0
	}

	// The winner is remembered as plain numbers: copying a Config for every one
	// of the tens of thousands of combinations considered costs more than the
	// comparison does.
	bestBits := -1
	var bestBV, bestR0, bestR1, bestC1 int
	var bestTables [3]int

	// The accumulator walks the pairs once, in step with big_values, snapshotting
	// each band boundary as it passes: every prefix the search can ask about is
	// either behind it (a snapshot) or exactly at it.
	pos, nextSnap := 0, 0
	advance := func(to int) {
		for nextSnap < len(snapPos) {
			at := int(snapPos[nextSnap])
			if at > to {
				break
			}
			if at > pos {
				accumulate(&sc.acc, sc.keys[pos:at])
				pos = at
			}
			sc.rows[snapSlot[nextSnap]] = sc.acc
			nextSnap++
		}
		if to > pos {
			accumulate(&sc.acc, sc.keys[pos:to])
			pos = to
		}
	}

	for bv := lastBig; bv <= maxBV; bv++ {
		bv32 := int32(bv)

		c1Table, c1Bits, ok := count1Cost(sc, bv)
		if !ok {
			continue
		}
		advance(bv)

		if orig.WindowSwitching {
			// Only region0's boundary and two tables are ours to choose; the
			// geometry comes from the block type.
			split, slot, stable := boundary[8], int32(8), true
			if orig.BlockType == 2 {
				split, slot = shortBound, shortSlot
			}
			if split >= bv32 {
				split, slot, stable = bv32, bvSlot, false
			}
			t0, bits0, ok0 := lookup(&sc.head[slot], 0, slot, 0, split, bv32, stable)
			t1, bits1, ok1 := lookup(&sc.tail[slot], slot, bvSlot, split, bv32, bv32, false)
			if !ok0 || !ok1 {
				continue
			}
			if total := int(bits0+bits1) + c1Bits; bestBits < 0 || total < bestBits {
				bestBits, bestBV, bestC1 = total, bv, c1Table
				bestTables = [3]int{t0, t1, 0}
			}
			continue
		}

		// Everything below is a split of the pairs [0, big_values) into one, two or
		// three regions at band boundaries, so enumerate by shape rather than by
		// (region0_count, region1_count): only regions that end at big_values move
		// with it, and the rest are already cached.
		//
		// Ties are resolved towards the lower region counts, and towards the lower
		// big_values, which is the order the field values would be searched in.
		consider := func(bits int32, r0, r1, t0, t1, t2 int) {
			total := int(bits) + c1Bits
			if bestBits >= 0 {
				if total > bestBits {
					return
				}
				if total == bestBits && (bv != bestBV || r0 > bestR0 || (r0 == bestR0 && r1 >= bestR1)) {
					return
				}
			}
			bestBits, bestBV, bestC1 = total, bv, c1Table
			bestR0, bestR1 = r0, r1
			bestTables = [3]int{t0, t1, t2}
		}

		// One region: the first region0 boundary at or beyond big_values swallows
		// everything, and no later one can do anything different.
		firstAbove := -1
		for i := 1; i <= 16; i++ {
			if boundary[i] >= bv32 {
				firstAbove = i
				break
			}
		}
		if firstAbove >= 0 {
			if t0, bits0, ok0 := lookup(&sc.head[bvSlot], 0, bvSlot, 0, bv32, bv32, false); ok0 {
				consider(bits0, firstAbove-1, 0, t0, 0, 0)
			}
		}

		last0 := 16
		if firstAbove >= 0 {
			last0 = firstAbove - 1 // beyond this, region0 already covers everything
		}

		// Two regions: region0 up to a boundary, region1 from there to big_values,
		// which needs a region1_count large enough to reach at or past it.
		for i := 1; i <= last0; i++ {
			if boundary[min(i+8, numBands-1)] < bv32 {
				continue // region2 cannot be empty for this region0 boundary
			}
			t0, bits0, ok0 := lookup(&sc.head[int32(i)], 0, int32(i), 0, boundary[i], bv32, true)
			if !ok0 {
				continue
			}
			r1 := 0
			for ; r1 < 8; r1++ {
				if boundary[min(i+r1+1, numBands-1)] >= bv32 {
					break
				}
			}
			if t1, bits1, ok1 := lookup(&sc.tail[int32(i)], int32(i), bvSlot, boundary[i], bv32, bv32, false); ok1 {
				consider(bits0+bits1, i-1, r1, t0, t1, 0)
			}
		}

		// Three regions: regions 0 and 1 cover everything below a boundary and
		// neither moves with big_values, so the best pair of them is computed once
		// per boundary and reused; only the tail has to be recomputed.
		for j := 2; j <= numBands-1; j++ {
			if boundary[j] >= bv32 {
				break
			}
			p := &sc.prefix[j]
			if !p.ready {
				*p = prefixSplit{ready: true}
				for i := max(1, j-8); i <= min(16, j-1); i++ {
					t0, bits0, ok0 := lookup(&sc.head[int32(i)], 0, int32(i), 0, boundary[i], bv32, true)
					if !ok0 {
						continue
					}
					t1, bits1, ok1 := lookup(&sc.mid[i][j], int32(i), int32(j), boundary[i], boundary[j], bv32, true)
					if !ok1 {
						continue
					}
					if !p.ok || bits0+bits1 < p.bits {
						p.ok, p.bits = true, bits0+bits1
						p.region0, p.table0, p.table1 = int32(i), int8(t0), int8(t1)
					}
				}
			}
			if !p.ok {
				continue
			}
			t2, bits2, ok2 := lookup(&sc.tail[int32(j)], int32(j), bvSlot, boundary[j], bv32, bv32, false)
			if !ok2 {
				continue
			}
			i := int(p.region0)
			consider(p.bits+bits2, i-1, j-i-1, int(p.table0), int(p.table1), t2)
		}
	}
	if bestBits < 0 {
		return orig, -1
	}
	best := orig
	best.BigValues = bestBV
	best.TableSelect = bestTables
	best.Count1Table = bestC1
	if !orig.WindowSwitching {
		best.Region0Count, best.Region1Count = bestR0, bestR1
	}
	return best, bestBits
}

// buildCount1Costs fills in the cost of coding the spectrum tail as count1
// quadruples from every even start position. Each start is one quadruple plus the
// cost four coefficients later, so one backward walk serves every candidate
// big_values.
func buildCount1Costs(sc *scratch, s *Spectrum, last int) {
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
}

func count1Cost(sc *scratch, bv int) (table, bits int, ok bool) {
	pos := 2 * bv
	if !sc.c1Usable[pos] {
		return 0, 0, false
	}
	if sc.c1Bits33[pos] < sc.c1Bits32[pos] {
		return count1Table33, int(sc.c1Bits33[pos]), true
	}
	return count1Table32, int(sc.c1Bits32[pos]), true
}
