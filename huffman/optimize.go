package huffman

import "sync"

// numBands is the number of scalefactor band boundaries a long block has.
const numBands = 23

// Prefix rows are addressed by slot. Slots 0..numBands-1 are the long-block band
// boundaries; short blocks put region0's boundary somewhere else entirely, so it
// gets its own slot.
const (
	shortSlot = numBands
	numRows   = numBands + 1
)

// prefixSplit is the best two-region cover of every pair below a boundary: its
// cost, where region0 ends, and the tables both regions use. None of it moves
// with big_values, so it is computed once per boundary.
type prefixSplit struct {
	bits    int32
	ok      bool
	region0 int8
	table0  int8
	table1  int8
}

// region is one span's cheapest table and what it costs.
type region struct {
	bits  int32
	table int8
}

// scratch is the working set of one Optimize call. It is pooled because the
// search runs once per granule — hundreds of thousands of times per second — and
// the arrays are far too large to keep reallocating.
type scratch struct {
	// keys[p] is the packed cost-table index of coefficient pair p.
	keys [MaxBigValues]uint32

	// acc is the running per-table cost of every pair below the big_values under
	// consideration, and rows holds the same totals at each boundary, one 32-lane
	// row per slot. Only these rows are ever needed, so the search never has to
	// materialise a cost per pair.
	//
	// The rows are stored pre-scaled by 32 to leave room for a table index in the
	// low bits, which is how the kernels return the cheapest table alongside its
	// cost; doing it once per snapshot saves a shift per row per candidate.
	acc  [numTables]int32
	rows [numRows * numTables]int32

	// The same totals again, unscaled. The batched span kernel scales its shared
	// endpoint itself, so handing it one of these lets the prefix search reuse it
	// rather than need a second kernel that takes an already-scaled endpoint.
	raw [numRows * numTables]int32

	// tails[slot] is the cheapest coding of the span from that boundary up to
	// big_values, packed as cost<<5|table. These are the only region costs that
	// move as big_values does, so all of them are recomputed together in a single
	// vector pass per candidate.
	tails [numRows]uint32

	// head[i] covers the pairs below boundary i, which does not depend on
	// big_values. The head costs are wanted for every candidate, so they are
	// filled in one pass rather than on demand.
	head    [numRows]region
	headN   int
	prefix  [numBands]prefixSplit
	prefixN int

	// Cost of coding the tail of the spectrum as count1 quadruples starting at
	// each even coefficient, with either count1 table.
	c1Bits32 [NumCoefficients + 4]int32
	c1Bits33 [NumCoefficients + 4]int32
	c1Usable [NumCoefficients + 4]bool
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

func (sc *scratch) row(slot int) *[numTables]int32 {
	return (*[numTables]int32)(sc.rows[slot*numTables:])
}

func (sc *scratch) rawRow(slot int) *[numTables]int32 {
	return (*[numTables]int32)(sc.raw[slot*numTables:])
}

func (sc *scratch) snapshot(slot int) {
	row := sc.rows[slot*numTables : (slot+1)*numTables]
	copy(sc.raw[slot*numTables:(slot+1)*numTables], sc.acc[:])
	for i, v := range sc.acc {
		row[i] = v << 5
	}
}

// fillHeads computes the cost of covering the pairs below each boundary up to n
// with a single table. They do not depend on big_values, so each is computed once.
func (sc *scratch) fillHeads(n int) {
	for ; sc.headN < n; sc.headN++ {
		i := sc.headN
		tab, cost := unpackBest(bestTable(sc.row(0), sc.row(i)))
		sc.head[i].bits, sc.head[i].table = cost, int8(tab)
	}
}

// fillPrefixes settles the best two-region cover below each boundary up to n.
// Like the head costs, none of it moves with big_values, so each boundary is
// computed the first time the search can reach it and only read thereafter.
func (sc *scratch) fillPrefixes(n int) {
	var spans [8]uint32
	for ; sc.prefixN < n; sc.prefixN++ {
		j := sc.prefixN
		p := &sc.prefix[j]
		*p = prefixSplit{}
		lo, hi := max(1, j-8), min(16, j-1)
		if lo > hi {
			continue
		}
		// Every span considered here ends at the same boundary, which is the shape
		// the batched kernel exists for: one call that keeps the endpoint in
		// registers and walks the rows, instead of eight calls of which most was
		// call overhead.
		bestTails(sc.rows[lo*numTables:(hi+1)*numTables], sc.rawRow(j), spans[:hi-lo+1])
		for i := lo; i <= hi; i++ {
			t0, bits0 := int(sc.head[i].table), sc.head[i].bits
			if bits0 < 0 {
				continue
			}
			t1, bits1 := unpackBest(spans[i-lo])
			if bits1 < 0 {
				continue
			}
			if !p.ok || bits0+bits1 < p.bits {
				p.ok, p.bits = true, bits0+bits1
				p.region0, p.table0, p.table1 = int8(i), int8(t0), int8(t1)
			}
		}
	}
}

// Coding is the outcome of a search: the configuration to write a granule with,
// what it costs, and what the search learned along the way that the encoder
// would otherwise have to work out again.
type Coding struct {
	// Config codes the spectrum in Bits bits. Bits is -1 if no legal coding was
	// found, in which case Config is the one the search started from.
	Config Config
	Bits   int

	// last is one past the highest non-zero coefficient, which is where the
	// count1 region stops. The search needs it to bound big_values, and Encode
	// needs it for the same spectrum a moment later; carrying it is what stops
	// the trailing zero run being walked twice. It stays unexported so that it
	// can only have come from a search of the spectrum it is used with.
	last int
}

// Optimize searches for the cheapest legal Huffman coding of s and returns the
// configuration that achieves it together with its size in bits.
//
// It is Search without the part only Encode has any use for; a caller that goes
// on to encode should prefer Search and [Coding.Encode].
func Optimize(s *Spectrum, orig Config, sampleRate int) (Config, int) {
	c := Search(s, orig, sampleRate)
	return c.Config, c.Bits
}

// Search finds the cheapest legal Huffman coding of s.
//
// The search is exhaustive over everything the side info can express: the
// big-values split point, both region boundaries, the code table for each
// region, and the count1 table. Encoders typically pick these heuristically, so
// there is usually a little room left, and finding it is pure profit — the
// spectrum, and hence the audio, is untouched.
//
// orig supplies the block geometry, which is not ours to change: the region
// boundaries of a window-switched granule are fixed by the standard.
func Search(s *Spectrum, orig Config, sampleRate int) Coding {
	sc := scratchPool.Get().(*scratch)
	defer scratchPool.Put(sc)

	// Coefficients above 1 in magnitude cannot live in the count1 region, so
	// they set a floor on big_values; nothing above the last non-zero
	// coefficient needs coding at all, which sets the ceiling.
	//
	// The two bounds share their walk: no pair above the last non-zero
	// coefficient can hold a magnitude over 1, so the floor's scan starts where
	// the ceiling's ended rather than at the top of the spectrum. Granules
	// usually code well under half their coefficients, so that tail is the
	// larger part of both scans.
	last := lastNonZero(s)
	lastBig := 0
	for pair := (last - 1) / 2; pair >= 0; pair-- {
		if abs(s[2*pair]) > 1 || abs(s[2*pair+1]) > 1 {
			lastBig = pair + 1
			break
		}
	}
	maxBV := min(max((last+1)/2, lastBig), MaxBigValues)

	for p := 0; p < maxBV; p++ {
		sc.keys[p] = pairKey(s[2*p], s[2*p+1])
	}
	sc.acc = [numTables]int32{}
	sc.headN = 0
	sc.prefixN = 0
	buildCount1Costs(sc, s, last, 2*lastBig)

	b := bands(sampleRate)
	var boundary [numRows]int32
	for i := 0; i < numBands; i++ {
		boundary[i] = int32(b[i] / 2)
	}
	boundary[shortSlot] = int32(bandsShort(sampleRate)[3] / 2 * 3)

	// Every prefix the search needs a snapshot of, in the order the accumulator
	// will reach them.
	var snapPos, snapSlot [numRows]int32
	n, inserted := 0, false
	for i := 0; i < numBands; i++ {
		if !inserted && boundary[shortSlot] < boundary[i] {
			snapPos[n], snapSlot[n] = boundary[shortSlot], shortSlot
			n, inserted = n+1, true
		}
		snapPos[n], snapSlot[n] = boundary[i], int32(i)
		n++
	}
	if !inserted {
		snapPos[n], snapSlot[n] = boundary[shortSlot], shortSlot
	}

	// The accumulator walks the pairs once, in step with big_values, snapshotting
	// each boundary as it passes: every prefix the search can ask about is either
	// behind it or exactly at it.
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
			sc.snapshot(int(snapSlot[nextSnap]))
			nextSnap++
		}
		if to > pos {
			accumulate(&sc.acc, sc.keys[pos:to])
			pos = to
		}
	}

	// The winner is remembered as plain numbers: copying a Config for every one of
	// the combinations considered costs more than the comparison does. Ties go to
	// the lower region counts and the lower big_values, which is the order the
	// field values themselves would be searched in.
	bestBits := -1
	var bestBV, bestR0, bestR1, bestC1 int
	var bestTables [3]int
	bv, c1Table, c1Bits := 0, 0, 0
	// Most candidates lose outright, and consider cannot inline: it carries the
	// tie-break and every winner field, and reaching it spills six arguments. So
	// the losing test is made at the call sites through this, which can inline, and
	// the call happens only for a candidate that might really win.
	canWin := func(total int) bool { return bestBits < 0 || total <= bestBits }
	consider := func(total, r0, r1, t0, t1, t2 int) {
		if total == bestBits && (bv != bestBV || r0 > bestR0 || (r0 == bestR0 && r1 >= bestR1)) {
			return
		}
		bestBits, bestBV, bestC1 = total, bv, c1Table
		bestR0, bestR1 = r0, r1
		bestTables = [3]int{t0, t1, t2}
	}

	// A window-switched granule's geometry is not ours to choose: region0 ends at
	// one fixed boundary, the ninth long band or the third short band across all
	// three windows, and there is no third region. That is a different search
	// rather than a special case of this one — two spans to cost, no split to
	// enumerate — so it gets its own loop. Sharing one loop costs the long-block
	// path a test and a page of unrelated code between it and the candidate
	// enumeration, which measured as 3%.
	if orig.WindowSwitching {
		slot := 8
		if orig.BlockType == 2 {
			slot = shortSlot
		}
		// The span below the boundary does not move with big_values, so it is
		// settled the first time a candidate reaches past the boundary at all.
		headBits, headTable, headKnown := int32(0), 0, false
		for bv = lastBig; bv <= maxBV; bv++ {
			var ok bool
			c1Table, c1Bits, ok = count1Cost(sc, bv)
			if !ok {
				continue
			}
			advance(bv)

			if boundary[slot] >= int32(bv) {
				// Region0 already covers every pair there is.
				if bv == 0 {
					if canWin(c1Bits) {
						consider(c1Bits, 0, 0, 0, 0, 0)
					}
					continue
				}
				bestTails(sc.rows[:numTables], &sc.acc, sc.tails[:1])
				if t0, bits0 := unpackBest(sc.tails[0]); bits0 >= 0 {
					if total := int(bits0) + c1Bits; canWin(total) {
						consider(total, 0, 0, t0, 0, 0)
					}
				}
				continue
			}
			if !headKnown {
				// Both rows are snapshots, so this is the cost fillHeads would have
				// arrived at, for the one boundary that can be asked about.
				headTable, headBits = unpackBest(bestTable(sc.row(0), sc.row(slot)))
				headKnown = true
			}
			if headBits < 0 {
				continue
			}
			// Only the span up to big_values moves, and there is one of it. The
			// batched kernel is still what computes it, as a batch of one: it is the
			// form that takes an unscaled endpoint.
			bestTails(sc.rows[slot*numTables:(slot+1)*numTables], &sc.acc, sc.tails[slot:slot+1])
			t1, bits1 := unpackBest(sc.tails[slot])
			if bits1 < 0 {
				continue
			}
			if total := int(headBits+bits1) + c1Bits; canWin(total) {
				consider(total, 0, 0, headTable, t1, 0)
			}
		}
		return winner(orig, last, bestBits, bestBV, bestC1, bestTables, bestR0, bestR1)
	}

	// nTail is how many band boundaries lie strictly below big_values. Because
	// boundary is sorted, it is the only thing the candidate enumeration needs to
	// know about bv: "boundary[i] >= bv" is exactly "i >= nTail". Every test below
	// is that identity applied, which is why none of them scan. And since bv only
	// grows, nTail is carried across candidates rather than recounted.
	nTail := 0
	for bv = lastBig; bv <= maxBV; bv++ {
		bv32 := int32(bv)
		for nTail < numBands && boundary[nTail] < bv32 {
			nTail++
		}

		var ok bool
		c1Table, c1Bits, ok = count1Cost(sc, bv)
		if !ok {
			continue
		}
		advance(bv)

		// Spans ending at big_values are the only ones that move with it, and they
		// all share that endpoint, so they are computed together: one pass over the
		// snapshot rows, 32 tables at a time.
		if nTail > 0 {
			bestTails(sc.rows[:nTail*numTables], &sc.acc, sc.tails[:nTail])
			// Both fillers memoise on how far they have already run, but the test
			// has to be out here: they hold loops, so neither can inline, and every
			// candidate would otherwise pay a call to be told there is nothing to
			// do. Only a candidate that reached a new boundary has anything to add.
			if sc.headN < nTail {
				sc.fillHeads(nTail)
			}
		}

		// Every remaining candidate is a split of the pairs [0, big_values) into
		// one, two or three regions at band boundaries, enumerated by shape rather
		// than by (region0_count, region1_count).

		// One region: the first region0 boundary at or beyond big_values swallows
		// everything, and no larger region0_count can do anything different. That
		// boundary is max(1, nTail), if region0_count can reach it at all.
		firstAbove := -1
		if nTail <= 16 {
			firstAbove = max(1, nTail)
		}
		if firstAbove >= 0 {
			if bv == 0 {
				if canWin(c1Bits) {
					consider(c1Bits, firstAbove-1, 0, 0, 0, 0)
				}
			} else if t0, bits0 := unpackBest(sc.tails[0]); bits0 >= 0 {
				if total := int(bits0) + c1Bits; canWin(total) {
					consider(total, firstAbove-1, 0, t0, 0, 0)
				}
			}
		}
		last0 := 16
		if firstAbove >= 0 {
			last0 = firstAbove - 1
		}

		// Two regions: region1 runs from a boundary to big_values, which needs a
		// region1_count large enough to reach at or past it.
		// region1_count tops out at 7, so a region0 boundary below nTail-8 can never
		// stretch region1 up to big_values; starting there skips the dead prefix
		// instead of testing and rejecting it.
		for i := max(1, nTail-8); i <= last0; i++ {
			if min(i+8, numBands-1) < nTail {
				continue // region2 cannot be empty for this region0 boundary
			}
			t0, bits0 := int(sc.head[i].table), sc.head[i].bits
			if bits0 < 0 {
				continue
			}
			// The smallest region1_count whose boundary reaches big_values. The
			// test above guarantees region1_count 7 does, so this cannot overflow
			// the field.
			r1 := max(0, nTail-i-1)
			if t1, bits1 := unpackBest(sc.tails[i]); bits1 >= 0 {
				if total := int(bits0+bits1) + c1Bits; canWin(total) {
					consider(total, i-1, r1, t0, t1, 0)
				}
			}
		}

		// Three regions: regions 0 and 1 cover everything below a boundary and
		// neither moves with big_values, so the best pair of them is computed once
		// per boundary and reused; only the tail has to be recomputed.
		if sc.prefixN < nTail {
			sc.fillPrefixes(nTail)
		}
		for j := 2; j < nTail; j++ {
			p := &sc.prefix[j]
			if !p.ok {
				continue
			}
			t2, bits2 := unpackBest(sc.tails[j])
			if bits2 < 0 {
				continue
			}
			if total := int(p.bits+bits2) + c1Bits; canWin(total) {
				i := int(p.region0)
				consider(total, i-1, j-i-1, int(p.table0), int(p.table1), t2)
			}
		}
	}
	return winner(orig, last, bestBits, bestBV, bestC1, bestTables, bestR0, bestR1)
}

// winner assembles the configuration the search settled on. Both searches end
// this way, and neither runs it more than once, so it is a function rather than
// repeated at each exit.
func winner(orig Config, last, bits, bv, c1Table int, tables [3]int, r0, r1 int) Coding {
	if bits < 0 {
		return Coding{Config: orig, Bits: -1, last: last}
	}
	best := orig
	best.BigValues = bv
	best.TableSelect = tables
	best.Count1Table = c1Table
	if !orig.WindowSwitching {
		best.Region0Count, best.Region1Count = r0, r1
	}
	return Coding{Config: best, Bits: bits, last: last}
}

// buildCount1Costs fills in the cost of coding the spectrum tail as count1
// quadruples from every even start position down to from. Each start is one
// quadruple plus the cost four coefficients later, so one backward walk serves
// every candidate big_values.
func buildCount1Costs(sc *scratch, s *Spectrum, last, from int) {
	// Only positions up to twice the largest big_values are ever asked about, and
	// that is at most last+1, so starting at the top of the spectrum writes zeros
	// for a tail nobody reads — most of the walk on a granule that codes half its
	// coefficients. Two base-case positions above the recurrence are enough to seed
	// it, since each step reaches four coefficients up and the walk steps by two.
	top := min((last+3)&^1, NumCoefficients) // least even position at or above last+2
	for pos := top; pos >= from; pos -= 2 {
		switch {
		case pos >= last:
			sc.c1Bits32[pos], sc.c1Bits33[pos], sc.c1Usable[pos] = 0, 0, true
		case pos > NumCoefficients-4 || !sc.c1Usable[pos+4]:
			// A quadruple cannot start inside the final one, so a split here
			// would drop a coefficient.
			sc.c1Usable[pos] = false
		default:
			q := s[pos : pos+4 : pos+4]
			sym := 0
			if q[0] != 0 {
				sym |= 8
			}
			if q[1] != 0 {
				sym |= 4
			}
			if q[2] != 0 {
				sym |= 2
			}
			if q[3] != 0 {
				sym |= 1
			}
			d := &count1Delta[sym]
			sc.c1Usable[pos] = count1Valid[sym]
			sc.c1Bits32[pos] = sc.c1Bits32[pos+4] + d[0]
			sc.c1Bits33[pos] = sc.c1Bits33[pos+4] + d[1]
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
