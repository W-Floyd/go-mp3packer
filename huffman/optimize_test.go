package huffman

import (
	"math/rand"
	"testing"
)

// exhaustiveOptimize is a deliberately naive version of Optimize: no memoisation,
// no early exits, every combination costed from scratch. It exists to pin down
// what the fast search is supposed to return.
func exhaustiveOptimize(s *Spectrum, orig Config, sampleRate int) int {
	b := bands(sampleRate)
	last := lastNonZero(s)
	lastBig := 0
	for pair := MaxBigValues - 1; pair >= 0; pair-- {
		if abs(s[2*pair]) > 1 || abs(s[2*pair+1]) > 1 {
			lastBig = pair + 1
			break
		}
	}
	maxBV := min(max((last+1)/2, lastBig), MaxBigValues)

	regionBits := func(from, to int) (int, bool) {
		if from >= to {
			return 0, true
		}
		best := -1
		for t := 0; t < 32; t++ {
			if maxQuant[t] < 0 {
				continue
			}
			sum, ok := 0, true
			for p := from; p < to; p++ {
				c := pairCost(t, s[2*p], s[2*p+1])
				if c < 0 {
					ok = false
					break
				}
				sum += c
			}
			if ok && (best < 0 || sum < best) {
				best = sum
			}
		}
		return best, best >= 0
	}

	count1Bits := func(bv int) (int, bool) {
		best := -1
		for _, table := range []int{count1Table32, count1Table33} {
			sum, pos := 0, 2*bv
			for ; pos <= NumCoefficients-4 && pos < last; pos += 4 {
				sym := 0
				for i := 0; i < 4; i++ {
					if s[pos+i] != 0 {
						sym |= 1 << uint(3-i)
						sum++
					}
				}
				sum += encodeTables[table][sym].length
			}
			if pos < last {
				return 0, false
			}
			if best < 0 || sum < best {
				best = sum
			}
		}
		return best, best >= 0
	}

	total := -1
	for bv := lastBig; bv <= maxBV; bv++ {
		c1, ok := count1Bits(bv)
		if !ok {
			continue
		}
		try := func(splits ...int) {
			sum := c1
			from := 0
			for _, to := range append(splits, bv) {
				bits, ok := regionBits(from, to)
				if !ok {
					return
				}
				sum += bits
				from = to
			}
			if total < 0 || sum < total {
				total = sum
			}
		}
		if orig.WindowSwitching {
			bound := b[8] / 2
			if orig.BlockType == 2 {
				bound = bandsShort(sampleRate)[3] / 2 * 3
			}
			try(min(bv, bound))
			continue
		}
		for r0 := 0; r0 < 16; r0++ {
			for r1 := 0; r1 < 8; r1++ {
				try(min(bv, b[r0+1]/2), min(bv, b[min(r0+r1+2, 22)]/2))
			}
		}
	}
	return total
}

// TestOptimizeMatchesExhaustiveSearch checks that the memoised region costing and
// the loop pruning do not cost us any compression.
func TestOptimizeMatchesExhaustiveSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, rate := range []int{44100, 32000, 22050} {
		for iter := 0; iter < 12; iter++ {
			s := randomSpectrum(rng, 1+rng.Intn(50))
			orig := Config{Count1Table: count1Table32}
			if iter%3 == 1 {
				orig.WindowSwitching, orig.BlockType = true, 2
			} else if iter%3 == 2 {
				orig.WindowSwitching, orig.BlockType = true, 3
			}
			_, got := Optimize(&s, orig, rate)
			want := exhaustiveOptimize(&s, orig, rate)
			if got != want {
				t.Fatalf("%dHz iteration %d: Optimize found %d bits, exhaustive search found %d",
					rate, iter, got, want)
			}
		}
	}
}

// TestOptimizeIsReusable guards the pooled scratch space: a stale entry from a
// previous granule must never leak into the next one.
func TestOptimizeIsReusable(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	spectra := make([]Spectrum, 8)
	want := make([]int, len(spectra))
	for i := range spectra {
		spectra[i] = randomSpectrum(rng, 1+rng.Intn(80))
		_, want[i] = Optimize(&spectra[i], Config{Count1Table: count1Table32}, 44100)
	}
	// Interleave the same spectra in a different order, reusing the scratch.
	for round := 0; round < 3; round++ {
		for i := len(spectra) - 1; i >= 0; i-- {
			if _, got := Optimize(&spectra[i], Config{Count1Table: count1Table32}, 44100); got != want[i] {
				t.Fatalf("round %d spectrum %d: got %d bits, want %d", round, i, got, want[i])
			}
		}
	}
}
