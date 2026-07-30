//go:build !arm64 && !amd64

package huffman

// accumulate adds the per-table cost of each pair in keys to acc.
func accumulate(acc *[numTables]int32, keys []uint32) {
	accumulateGo(acc, keys)
}

// bestTails computes the cheapest table for the span between each row and acc.
func bestTails(rows []int32, acc *[numTables]int32, out []uint32) {
	bestTailsGo(rows, acc, out)
}

// bestTable returns the cheapest table for the region between two prefix rows,
// packed as cost<<costShift | table.
func bestTable(from, to *[numTables]int32) uint32 {
	return bestTableGo(from, to)
}
