package huffman

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"slices"
	"testing"
)

// The decoder's fallback walk wants a code tree, not a grid. This is the
// generator that derives one from the constants in tables.go: a negative entry
// is an interior node whose 0-branch is the next slot and whose 1-branch is that
// many slots further on again, and a non-negative entry is a leaf holding the
// symbol.
// Keeping the standard's numbers in one file and this representation of them in
// another means neither has to be read to check the other.
//
// It runs at build time rather than at startup, because deriving the trees costs
// about a hundred microseconds and a small file's whole repack is only a few
// milliseconds — 1.3% of one, for work whose answer never changes. The output is
// trees_gen.go, and TestTreesGenerated both checks it is current and rewrites it.

// buildTrees derives every table's tree. Tables that select the same grid share
// one, since linbits does not reach the codewords.
func buildTrees() [34][]int16 {
	var out [34][]int16
	built := map[*grid][]int16{}
	for i := range tables {
		g := tables[i].g
		t, ok := built[g]
		if !ok {
			t = buildTree(g)
			built[g] = t
		}
		out[i] = t
	}
	return out
}

// trieNode is one node of the code tree while it is still being assembled. leaf
// is the symbol at a leaf and -1 anywhere else.
type trieNode struct {
	leaf int
	kids [2]*trieNode
}

// buildTree inserts every codeword of a grid into a trie and then flattens it.
//
// The empty table is the one special case: it defines a single zero-length
// codeword for the pair (0, 0), which is not a path through a trie at all, and it
// comes out as the one-slot tree the decoder reads as "this table codes nothing".
func buildTree(g *grid) []int16 {
	n := 0
	for _, c := range g.cw {
		if c.length() > 0 {
			n++
		}
	}
	if n == 0 {
		return []int16{0}
	}

	// Nodes come from one block so that building a trie is a few thousand stores
	// rather than a few thousand allocations. Pointers into it stay valid because
	// it is never appended to.
	arena := make([]trieNode, 2*n)
	used := 0
	alloc := func() *trieNode {
		if used == len(arena) {
			panic("huffman: code tree needs more nodes than a complete code has")
		}
		p := &arena[used]
		used++
		p.leaf = -1
		return p
	}

	root := alloc()
	for x := 0; x < g.xlen; x++ {
		for y := 0; y < g.ylen; y++ {
			c := g.cw[x*g.ylen+y]
			if c.length() == 0 {
				continue
			}
			// The count1 tables tabulate a single 4-bit symbol rather than a pair, so
			// their one row is indexed by y alone.
			sym := x<<4 | y
			if g.xlen == 1 {
				sym = y
			}
			cur := root
			for b := c.length() - 1; b >= 0; b-- {
				bit := c.code() >> uint(b) & 1
				if cur.kids[bit] == nil {
					cur.kids[bit] = alloc()
				}
				cur = cur.kids[bit]
			}
			cur.leaf = sym
		}
	}
	return layTree(root, make([]int16, 0, 2*n-1))
}

// layTree flattens a trie depth-first onto out. A node writes itself, then its
// whole 0-subtree, then its whole 1-subtree, so the 0-branch is always the next
// slot and the distance to the 1-branch is the size of the 0-subtree — which is
// what the node stores, negated to mark it as interior. The distance is not known
// until the 0-subtree is down, so the slot is left blank and filled in after.
func layTree(n *trieNode, out []int16) []int16 {
	if n.leaf >= 0 {
		return append(out, int16(n.leaf))
	}
	if n.kids[0] == nil || n.kids[1] == nil {
		// No table the standard defines has a half-used interior node; if one ever
		// did, the walk would need a way to report the missing branch rather than
		// silently decode the other.
		panic("huffman: code tree has an interior node with one branch")
	}
	at := len(out)
	out = append(out, 0)
	out = layTree(n.kids[0], out)
	out[at] = int16(-(len(out) - at - 1))
	return layTree(n.kids[1], out)
}

// updateTrees rewrites trees_gen.go instead of checking it, for when the grids
// change. The same switch the README's generated tables use.
var updateTrees = flag.Bool("update-trees", false, "rewrite huffman/trees_gen.go from the code grids")

// TestTreesGenerated holds trees_gen.go to the grids it is supposed to come from.
// The file is committed rather than built at startup, so nothing at run time would
// notice it drifting; this is what notices.
func TestTreesGenerated(t *testing.T) {
	built := buildTrees()

	var b bytes.Buffer
	fmt.Fprint(&b, `package huffman

// Code generated from the grids in tables.go by TestTreesGenerated. DO NOT EDIT.
//
// Each tree is the depth-first flattening of its table's codewords: a negative
// entry is an interior node whose 0-branch is the next slot and whose 1-branch is
// that many slots further on again, and a non-negative entry is a leaf holding
// (x<<4)|y, or the bare 4-bit symbol for the two count1 tables.
//
// Nothing here is a choice. A complete prefix code over n codewords has exactly
// 2n-1 nodes and one depth-first order, so these numbers follow from the standard
// and could not have been written differently.
`)
	name := map[int]string{}
	for i := range built {
		if _, ok := name[i]; ok {
			continue
		}
		for j := range built {
			if j > i && slices.Equal(built[i], built[j]) {
				name[j] = fmt.Sprintf("tree%d", i)
			}
		}
		name[i] = fmt.Sprintf("tree%d", i)
		fmt.Fprintf(&b, "\nvar tree%d = []int16{", i)
		for k, v := range built[i] {
			if k%20 == 0 {
				fmt.Fprint(&b, "\n\t")
			}
			fmt.Fprintf(&b, "%d, ", v)
		}
		fmt.Fprint(&b, "\n}\n")
	}
	fmt.Fprint(&b, "\n// trees[i] is table i's code tree; tables sharing a grid share one.\nvar trees = [34][]int16{\n")
	for i := range built {
		fmt.Fprintf(&b, "\t%s,\n", name[i])
	}
	fmt.Fprint(&b, "}\n")

	want, err := format.Source(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	const path = "trees_gen.go"
	if *updateTrees {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote", path)
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte{'\r', '\n'}, []byte{'\n'})
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not what the grids produce; re-run with -update-trees", path)
	}

	// And the shipped variable has to be the generated one, not something that
	// merely parses.
	for i, tr := range trees {
		if !slices.Equal(tr, built[i]) {
			t.Errorf("trees[%d] is not the tree its grid builds", i)
		}
	}
}
