package huffman

import (
	"math/rand"
	"testing"
)

// TestPairCostTableMatchesDirectCost pins the precomputed cost tables to the
// straightforward per-pair calculation, for every table and a wide spread of
// magnitudes including every escape boundary.
func TestPairCostTableMatchesDirectCost(t *testing.T) {
	values := []int{0, 1, 2, 3, 5, 7, 14, 15, 16, 17, 22, 30, 31, 46, 78, 142, 270, 526, 2062, 8205, 8206}
	for _, x := range values {
		for _, y := range values {
			for _, sx := range []int{1, -1} {
				key := pairKey(sx*x, y)
				base := &pairCostTable[key&0xFF]
				esc := &escapeCostTable[key>>8&0xF]
				for tab := 0; tab < numTables; tab++ {
					got := base[tab] + esc[tab]
					want := pairCost(tab, sx*x, y)
					if want < 0 {
						if got < penalty {
							t.Fatalf("table %d, pair (%d,%d): tables give cost %d, want unrepresentable",
								tab, sx*x, y, got)
						}
						continue
					}
					if int(got) != want {
						t.Fatalf("table %d, pair (%d,%d): tables give %d bits, direct calculation gives %d",
							tab, sx*x, y, got, want)
					}
				}
			}
		}
	}
}

// TestCostSumsCannotReachPenalty checks the headroom the penalty scheme relies
// on: a full granule of the most expensive legal pairs must stay below it.
func TestCostSumsCannotReachPenalty(t *testing.T) {
	worst := 0
	for sym := 0; sym < 256; sym++ {
		for tab := 0; tab < numTables; tab++ {
			if c := pairCostTable[sym][tab]; c < penalty && int(c) > worst {
				worst = int(c)
			}
		}
	}
	if worst > maxPairBits {
		t.Errorf("a pair can cost %d bits, more than the assumed maximum of %d", worst, maxPairBits)
	}
	if total := MaxBigValues * maxPairBits; total >= penalty {
		t.Errorf("a granule can legitimately cost %d bits, which the penalty of %d cannot exceed",
			total, penalty)
	}
}

func randomRow(rng *rand.Rand, scale int32) *[numTables]int32 {
	var row [numTables]int32
	for i := range row {
		row[i] = rng.Int31n(scale)
	}
	return &row
}

// TestKernelsMatchPortable compares whatever kernels this architecture uses with
// the portable implementations.
func TestKernelsMatchPortable(t *testing.T) {
	rng := rand.New(rand.NewSource(21))

	t.Run("accumulate", func(t *testing.T) {
		for iter := 0; iter < 300; iter++ {
			keys := make([]uint32, rng.Intn(40))
			for i := range keys {
				// Mostly ordinary pairs, with escapes often enough to exercise
				// the second table row.
				need := 0
				if rng.Intn(3) == 0 {
					need = rng.Intn(16)
				}
				keys[i] = uint32(rng.Intn(256)) | uint32(need)<<8
			}
			var got, want [numTables]int32
			if iter%2 == 0 {
				// Start from a non-zero accumulator half the time.
				for i := range got {
					v := rng.Int31n(1 << 20)
					got[i], want[i] = v, v
				}
			}
			accumulate(&got, keys)
			accumulateGo(&want, keys)
			if got != want {
				t.Fatalf("iteration %d with %d keys:\n got %v\nwant %v", iter, len(keys), got, want)
			}
		}
	})

	t.Run("bestTable", func(t *testing.T) {
		for iter := 0; iter < 2000; iter++ {
			scale := int32(1 << 10)
			if iter%3 == 0 {
				scale = penalty * 8 // exercise the impossible-region path
			}
			from := randomRow(rng, scale)
			to := randomRow(rng, scale)
			for i := range to {
				to[i] += from[i] // prefix sums never decrease
			}
			if got, want := bestTable(from, to), bestTableGo(from, to); got != want {
				gt, gc := unpackBest(got)
				wt, wc := unpackBest(want)
				t.Fatalf("iteration %d: got table %d cost %d, want table %d cost %d",
					iter, gt, gc, wt, wc)
			}
		}
	})

	t.Run("bestTable ties", func(t *testing.T) {
		// Every lane equal: both implementations must pick the lowest index.
		var from, to [numTables]int32
		for i := range to {
			to[i] = 100
		}
		if got, want := bestTable(&from, &to), bestTableGo(&from, &to); got != want {
			t.Fatalf("got %#x, want %#x", got, want)
		}
	})
}

func BenchmarkKernelAccumulate(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	keys := make([]uint32, 288)
	for i := range keys {
		keys[i] = uint32(rng.Intn(256))
	}
	var acc [numTables]int32
	b.Run("asm", func(b *testing.B) {
		for b.Loop() {
			accumulate(&acc, keys)
		}
	})
	b.Run("go", func(b *testing.B) {
		for b.Loop() {
			accumulateGo(&acc, keys)
		}
	})
}

func BenchmarkKernelBestTable(b *testing.B) {
	rng := rand.New(rand.NewSource(2))
	from, to := randomRow(rng, 1<<10), randomRow(rng, 1<<12)
	b.Run("asm", func(b *testing.B) {
		for b.Loop() {
			bestTable(from, to)
		}
	})
	b.Run("go", func(b *testing.B) {
		for b.Loop() {
			bestTableGo(from, to)
		}
	})
}
