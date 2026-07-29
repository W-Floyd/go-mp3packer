package huffman

import "math/bits"

// numTables is the number of big-value code tables. Indices 4 and 14 are
// undefined by the standard but keep their slots, so a table index is its own
// lane throughout the cost machinery.
const numTables = 32

// penalty is added to a table's cost for every coefficient pair it cannot
// represent. It exceeds the largest cost any legal coding of a whole granule can
// reach (288 pairs of at most 19 code bits, 26 escape bits and 2 sign bits), so
// a region cost at or above it means "impossible" — exactly, with no clamping,
// which is what lets the search work in prefix sums.
const penalty = 1 << 15

// maxPairBits is the most bits a single pair can legitimately cost.
const maxPairBits = 19 + 2*13 + 2

var (
	// pairCostTable[symbol][table] is the complete cost of coding a pair whose
	// magnitudes clamp to symbol: code length, escape length and sign bits
	// together, since all three follow from the symbol alone.
	pairCostTable [256][numTables]int32

	// escapeCostTable[n][table] penalises tables whose linbits cannot express a
	// magnitude n bits above the escape threshold. Splitting this out keeps the
	// per-pair cost to two table rows, both indexed by small integers, which is
	// what makes the accumulation loop vectorisable.
	escapeCostTable [16][numTables]int32

	// count1Delta[sym] is what a count1 quadruple whose non-zero pattern is sym
	// adds to the running cost under each of the two count1 tables: the codeword
	// length plus one sign bit per non-zero value. Folding the sign count into the
	// table leaves the tail walk two adds per position.
	count1Delta [16][2]int32
	count1Valid [16]bool
)

func init() {
	for sym := range pairCostTable {
		sx, sy := sym>>4, sym&15
		for tab := 0; tab < numTables; tab++ {
			pairCostTable[sym][tab] = int32(penalty)
			if maxQuant[tab] < 0 {
				continue // undefined table: never selectable
			}
			c := encodeTables[tab][sym]
			if !c.valid || sx > maxQuant[tab] || sy > maxQuant[tab] {
				continue
			}
			cost := c.length
			if sx == 15 {
				cost += tables[tab].linbits
			}
			if sy == 15 {
				cost += tables[tab].linbits
			}
			if sx != 0 {
				cost++ // sign
			}
			if sy != 0 {
				cost++
			}
			pairCostTable[sym][tab] = int32(cost)
		}
	}
	for sym := range count1Delta {
		c32, c33 := encodeTables[count1Table32][sym], encodeTables[count1Table33][sym]
		signs := int32(bits.OnesCount(uint(sym)))
		count1Valid[sym] = c32.valid && c33.valid
		count1Delta[sym][0] = int32(c32.length) + signs
		count1Delta[sym][1] = int32(c33.length) + signs
	}
	for need := range escapeCostTable {
		for tab := 0; tab < numTables; tab++ {
			if maxQuant[tab] >= 0 && tables[tab].linbits >= need {
				escapeCostTable[need][tab] = 0
			} else if need > 0 {
				escapeCostTable[need][tab] = int32(penalty)
			}
		}
	}
}

// pairKey packs a coefficient pair into the two row indices the cost kernel
// needs: the clamped symbol, and how many bits of magnitude sit above the escape
// threshold.
func pairKey(x, y int) uint32 {
	ax, ay := abs(x), abs(y)
	sym := min(ax, 15)<<4 | min(ay, 15)
	need := 0
	if m := max(ax, ay); m > 15 {
		need = min(bits.Len(uint(m-15)), 15)
	}
	return uint32(sym) | uint32(need)<<8
}

// accumulateGo is the portable implementation of accumulate.
func accumulateGo(acc *[numTables]int32, keys []uint32) {
	for _, key := range keys {
		base := &pairCostTable[key&0xFF]
		esc := &escapeCostTable[key>>8&0xF]
		for t := range acc {
			acc[t] += base[t] + esc[t]
		}
	}
}

// bestTailsGo is the portable implementation of bestTails.
func bestTailsGo(rows []int32, acc *[numTables]int32, out []uint32) {
	// Scaling and labelling the shared endpoint once is what lets each row cost
	// one subtraction: (acc<<5 | t) - (row<<5) is (acc-row)<<5 | t, because the
	// shift leaves the low five bits clear.
	var scaled [numTables]int32
	for t, v := range acc {
		scaled[t] = v<<5 | int32(t)
	}
	for i := range out {
		row := rows[i*numTables:]
		best := int32(1 << 30)
		for t := 0; t < numTables; t++ {
			if k := scaled[t] - row[t]; k < best {
				best = k
			}
		}
		out[i] = uint32(best)
	}
}

// bestTableGo is the portable implementation of bestTable. Both rows arrive
// pre-scaled by 32, so their difference already leaves room for the table index.
func bestTableGo(from, to *[numTables]int32) uint32 {
	best := int32(1 << 30)
	for t := 0; t < numTables; t++ {
		// Packing the cost above the table index turns "cheapest table, lowest
		// index on a tie" into a single minimum.
		if k := to[t] - from[t] | int32(t); k < best {
			best = k
		}
	}
	return uint32(best)
}

// unpackBest splits a bestTable result into its table and bit count. bits is
// negative when no table can code the region.
func unpackBest(packed uint32) (table int, cost int32) {
	cost = int32(packed) >> 5
	if cost >= penalty {
		return 0, -1
	}
	return int(packed & 31), cost
}
