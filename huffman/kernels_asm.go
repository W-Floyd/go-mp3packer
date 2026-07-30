//go:build arm64 || amd64

package huffman

// laneIndex labels each table's lane so that a plain minimum over packed
// cost<<costShift|table values yields both the cheapest cost and which table
// achieved it, breaking ties towards the lower index exactly as the portable code
// does.
var laneIndex = [numTables]int32{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31,
}

// accumulate adds the per-table cost of each pair in keys to acc. Both tables it
// reads are indexed by small fields of the key, so the whole loop is 32 lanes of
// load-add with no gathers.
//
//go:noescape
func accumulate(acc *[numTables]int32, keys []uint32)

// bestTable returns the cheapest table for the region between two prefix rows,
// packed as cost<<costShift | table. Every cost arrives pre-scaled, so their
// difference already leaves the low bits free for the lane.
//
//go:noescape
func bestTable(from, to *[numTables]int32) uint32

// bestTails is bestTable over a run of rows sharing the same upper endpoint, acc:
// one answer per entry of out, from the matching 32-lane row. Batching them keeps
// acc in registers and pays the call cost once instead of two dozen times.
//
//go:noescape
func bestTails(rows []int32, acc *[numTables]int32, out []uint32)
