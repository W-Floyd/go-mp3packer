package huffman

import (
	"math"
	"math/rand"
	"testing"
)

// TestPairCostTableMatchesDirectCost pins the precomputed cost tables to the
// straightforward per-pair calculation, for every table and a wide spread of
// magnitudes including every escape boundary. Both tables hold their costs scaled
// by costShift, so the comparison unscales; that the shift is exact — no cost
// with a bit set below it — is part of what is being checked, since a stray low
// bit would read as a table index.
func TestPairCostTableMatchesDirectCost(t *testing.T) {
	values := []int{0, 1, 2, 3, 5, 7, 14, 15, 16, 17, 22, 30, 31, 46, 78, 142, 270, 526, 2062, 8205, 8206}
	for _, x := range values {
		for _, y := range values {
			for _, sx := range []int{1, -1} {
				key := pairKey(sx*x, y)
				base := &pairCostTable[key&0xFF]
				esc := &escapeCostTable[key>>8&0xF]
				for tab := 0; tab < numTables; tab++ {
					scaled := base[tab] + esc[tab]
					if scaled&(1<<costShift-1) != 0 {
						t.Fatalf("table %d, pair (%d,%d): cost %d is not a multiple of %d",
							tab, sx*x, y, scaled, 1<<costShift)
					}
					got := scaled >> costShift
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
// on at both ends: a full granule of the most expensive legal pairs must stay
// below the penalty, and a full granule of penalties must still fit an int32 once
// scaled, which is what the accumulator and every row hold.
func TestCostSumsCannotReachPenalty(t *testing.T) {
	worst := 0
	for sym := 0; sym < 256; sym++ {
		for tab := 0; tab < numTables; tab++ {
			if c := pairCostTable[sym][tab] >> costShift; c < penalty && int(c) > worst {
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
	// The worst a lane can accumulate is every pair unrepresentable in both cost
	// tables at once, and it has to survive being scaled.
	if worst := int64(MaxBigValues) * 2 * penalty << costShift; worst > math.MaxInt32 {
		t.Errorf("a lane can reach %d once scaled, which an int32 cannot hold", worst)
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
				// Prefix sums never decrease, and rows are stored pre-scaled.
				from[i] *= 32
				to[i] = from[i] + to[i]*32
			}
			if got, want := bestTable(from, to), bestTableGo(from, to); got != want {
				gt, gc := unpackBest(got)
				wt, wc := unpackBest(want)
				t.Fatalf("iteration %d: got table %d cost %d, want table %d cost %d",
					iter, gt, gc, wt, wc)
			}
		}
	})

	t.Run("bestTails", func(t *testing.T) {
		for iter := 0; iter < 500; iter++ {
			n := 1 + rng.Intn(numRows)
			scale := int32(1 << 10)
			if iter%3 == 0 {
				scale = penalty * 8
			}
			acc := randomRow(rng, scale*int32(n))
			rows := make([]int32, n*numTables)
			for i := 0; i < n; i++ {
				// Prefix sums only grow and never pass the accumulator, and every
				// cost is scaled, endpoint included.
				for t := range acc {
					rows[i*numTables+t] = rng.Int31n(acc[t]+1) * 32
				}
			}
			for t := range acc {
				acc[t] *= 32
			}
			got := make([]uint32, n)
			want := make([]uint32, n)
			bestTails(rows, acc, got)
			bestTailsGo(rows, acc, want)
			for i := range want {
				if got[i] != want[i] {
					gt, gc := unpackBest(got[i])
					wt, wc := unpackBest(want[i])
					t.Fatalf("iteration %d row %d of %d: got table %d cost %d, want table %d cost %d",
						iter, i, n, gt, gc, wt, wc)
				}
			}
		}
	})

	t.Run("bestTable ties", func(t *testing.T) {
		// Every lane equal: both implementations must pick the lowest index.
		var from, to [numTables]int32
		for i := range to {
			to[i] = 100 * 32
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

func BenchmarkKernelBestTails(b *testing.B) {
	rng := rand.New(rand.NewSource(3))
	const n = 22
	acc := randomRow(rng, 1<<14)
	rows := make([]int32, n*numTables)
	for i := range rows {
		rows[i] = rng.Int31n(1<<12) * 32
	}
	for t := range acc {
		acc[t] *= 32 // the endpoint is scaled like every other cost
	}
	out := make([]uint32, n)
	b.Run("asm", func(b *testing.B) {
		for b.Loop() {
			bestTails(rows, acc, out)
		}
	})
	b.Run("go", func(b *testing.B) {
		for b.Loop() {
			bestTailsGo(rows, acc, out)
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
