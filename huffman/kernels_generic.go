//go:build !arm64 && !amd64

package huffman

// accumulate adds the per-table cost of each pair in keys to acc.
func accumulate(acc *[numTables]int32, keys []uint32) {
	accumulateGo(acc, keys)
}

// bestTable returns the cheapest table for the region between two prefix rows,
// packed as cost<<5 | table.
func bestTable(from, to *[numTables]int32) uint32 {
	return bestTableGo(from, to)
}
