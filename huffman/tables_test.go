package huffman

import "testing"

// TestGridsAreValidCodeTables checks the constants in tables.go as constants: not
// against the tree, the probe or the encode map, all of which are derived from
// them and so would agree with a wrong grid, but against the properties the
// standard's tables have. What this is really for is making a transcription error
// visible as one, rather than as a corrupt output file weeks later.
func TestGridsAreValidCodeTables(t *testing.T) {
	seen := map[*grid]bool{}
	for idx, spec := range tables {
		g := spec.g
		if seen[g] {
			continue
		}
		seen[g] = true

		if len(g.cw) != g.xlen*g.ylen {
			t.Fatalf("table %d: %d codewords for a %dx%d grid", idx, len(g.cw), g.xlen, g.ylen)
		}

		// Every codeword must fit the length it declares, or the spare high bits
		// would silently become part of some other code's prefix.
		var codes []codeword
		for i, c := range g.cw {
			if c.length() == 0 {
				continue
			}
			if c.length() > 19 {
				t.Errorf("table %d cell %d: %d-bit codeword, longer than the standard defines", idx, i, c.length())
			}
			if c.code() >= 1<<uint(c.length()) {
				t.Errorf("table %d cell %d: code %#x does not fit %d bits", idx, i, c.code(), c.length())
			}
			codes = append(codes, c)
		}

		// A prefix code: no codeword may be a prefix of another, which is the
		// property that makes the stream decodable without separators. Checked
		// pairwise against the definition rather than by building a trie, since a
		// trie is what the code under test builds.
		for i, a := range codes {
			for _, b := range codes[i+1:] {
				short, long := a, b
				if short.length() > long.length() {
					short, long = long, short
				}
				if long.code()>>uint(long.length()-short.length()) == short.code() {
					t.Errorf("table %d: %0*b is a prefix of %0*b",
						idx, short.length(), short.code(), long.length(), long.code())
				}
			}
		}

		// Kraft equality: a complete prefix code sums to exactly 1. Less than 1
		// means a bit pattern decodes to nothing, which the decoder's walk would
		// have to run off the end of the tree to discover.
		var kraft float64
		for _, c := range codes {
			kraft += 1 / float64(int64(1)<<uint(c.length()))
		}
		if len(codes) > 0 && kraft != 1 {
			t.Errorf("table %d: Kraft sum is %v, not 1 — the code is %s", idx, kraft,
				map[bool]string{true: "incomplete", false: "over-subscribed"}[kraft < 1])
		}
	}
}

// TestTreesDecodeTheGrids closes the loop between the two representations: every
// codeword the standard gives, walked one bit at a time through the tree built
// from it, must arrive at the symbol it was tabulated under.
func TestTreesDecodeTheGrids(t *testing.T) {
	for idx, spec := range tables {
		g, tree := spec.g, trees[idx]
		for x := 0; x < g.xlen; x++ {
			for y := 0; y < g.ylen; y++ {
				c := g.cw[x*g.ylen+y]
				if c.length() == 0 {
					continue
				}
				want := x<<4 | y
				if g.xlen == 1 {
					want = y
				}
				node := 0
				for b := c.length() - 1; b >= 0; b-- {
					v := tree[node]
					if v >= 0 {
						t.Fatalf("table %d: codeword for (%d,%d) hit leaf %d with %d bits left", idx, x, y, v, b+1)
					}
					node++
					if c.code()>>uint(b)&1 != 0 {
						node -= int(v)
					}
				}
				if got := tree[node]; int(got) != want {
					t.Errorf("table %d: codeword for (%d,%d) decodes to %d", idx, x, y, got)
				}
			}
		}
	}
}
