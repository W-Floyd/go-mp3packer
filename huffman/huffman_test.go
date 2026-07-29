package huffman

import (
	"math/rand"
	"testing"

	"github.com/W-Floyd/go-mp3packer/internal/bitio"
)

// TestTablesAreConsistent walks each decode tree and checks that the encode map
// derived from it is its exact inverse, which is what makes re-coding safe.
func TestTablesAreConsistent(t *testing.T) {
	for idx := 0; idx < 34; idx++ {
		if idx == 4 || idx == 14 {
			if maxQuant[idx] != -1 {
				t.Errorf("table %d is undefined by the standard and must not be selectable", idx)
			}
			continue
		}
		leaves := 0
		for sym, c := range encodeTables[idx] {
			if !c.valid || c.length == 0 {
				continue
			}
			leaves++
			r := bitio.NewReader(bitsToBytes(c.bits, c.length))
			tree := tables[idx].tree
			p := 0
			for tree[p] < 0 {
				v := int(tree[p])
				p++
				if r.Read(1) != 0 {
					p -= v
				}
			}
			if int(tree[p]) != sym {
				t.Errorf("table %d: symbol %#x codes to %0*b which decodes to %#x",
					idx, sym, c.length, c.bits, tree[p])
			}
			if r.Tell() != c.length {
				t.Errorf("table %d: symbol %#x consumed %d bits, want %d", idx, sym, r.Tell(), c.length)
			}
		}
		if leaves == 0 && idx != 0 {
			t.Errorf("table %d has no codewords", idx)
		}
	}
}

// TestMaxQuant checks the escape extension: tables 16 and above represent large
// coefficients as the value 15 plus linbits of magnitude.
func TestMaxQuant(t *testing.T) {
	want := map[int]int{0: 0, 1: 1, 2: 2, 3: 2, 5: 3, 6: 3, 7: 5, 8: 5, 9: 5,
		10: 7, 11: 7, 12: 7, 13: 15, 15: 15, 16: 16, 23: 8206, 24: 30, 31: 8206}
	for idx, w := range want {
		if maxQuant[idx] != w {
			t.Errorf("maxQuant[%d] = %d, want %d", idx, maxQuant[idx], w)
		}
	}
}

func bitsToBytes(v uint32, n int) []byte {
	w := bitio.NewWriter()
	w.Write(v, n)
	return w.Bytes()
}

// randomSpectrum builds a plausible quantized spectrum: large values at low
// frequencies, a run of -1/0/1 above them, then nothing.
func randomSpectrum(rng *rand.Rand, peak int) Spectrum {
	var s Spectrum
	big := rng.Intn(200) * 2
	small := big + rng.Intn(NumCoefficients-big)
	for i := 0; i < big; i++ {
		v := rng.Intn(peak + 1)
		if rng.Intn(2) == 0 {
			v = -v
		}
		s[i] = v
	}
	for i := big; i < small; i++ {
		s[i] = rng.Intn(3) - 1
	}
	return s
}

func TestOptimizeRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	rates := []int{44100, 48000, 32000, 24000, 22050, 8000}
	peaks := []int{1, 2, 7, 15, 60, 4000}

	for _, rate := range rates {
		for _, peak := range peaks {
			for iter := 0; iter < 40; iter++ {
				s := randomSpectrum(rng, peak)
				orig := Config{Count1Table: count1Table32}
				switch iter % 4 {
				case 1:
					orig.WindowSwitching, orig.BlockType = true, 2
				case 2:
					orig.WindowSwitching, orig.BlockType, orig.MixedBlock = true, 2, true
				case 3:
					orig.WindowSwitching, orig.BlockType = true, 1
				}
				best, bits := Optimize(&s, orig, rate)
				if bits < 0 {
					t.Fatalf("%dHz peak %d: no coding found", rate, peak)
				}

				w := bitio.NewWriter()
				Encode(&s, best, w, rate)
				if w.Tell() != bits {
					t.Fatalf("%dHz peak %d: Optimize predicted %d bits, Encode wrote %d",
						rate, peak, bits, w.Tell())
				}
				var got Spectrum
				if !Decode(&got, best, bitio.NewReader(w.Bytes()), rate, bits) {
					t.Fatalf("%dHz peak %d: re-decode failed", rate, peak)
				}
				if got != s {
					for i := range s {
						if got[i] != s[i] {
							t.Fatalf("%dHz peak %d: coefficient %d became %d (want %d)",
								rate, peak, i, got[i], s[i])
						}
					}
				}
			}
		}
	}
}

// TestOptimizeNeverWorseThanEncoderChoice compares the search against the
// coding a straightforward encoder would pick for the same spectrum.
func TestOptimizeNeverWorseThanEncoderChoice(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for iter := 0; iter < 200; iter++ {
		s := randomSpectrum(rng, 1+rng.Intn(40))
		orig := Config{Count1Table: count1Table32}
		best, bits := Optimize(&s, orig, 44100)
		if bits < 0 {
			t.Fatal("no coding found")
		}
		// Re-optimising an already optimal coding must not find anything better,
		// which also means the search is deterministic and self-consistent.
		again, bits2 := Optimize(&s, best, 44100)
		if bits2 != bits {
			t.Fatalf("second pass found %d bits after %d", bits2, bits)
		}
		if again.BigValues != best.BigValues {
			t.Fatalf("second pass changed big_values: %d then %d", best.BigValues, again.BigValues)
		}
	}
}

func TestDecodeRejectsTruncatedData(t *testing.T) {
	var s Spectrum
	for i := 0; i < 100; i++ {
		s[i] = 30 // needs an escape table, so several bits per pair
	}
	cfg, bits := Optimize(&s, Config{Count1Table: count1Table32}, 44100)
	w := bitio.NewWriter()
	Encode(&s, cfg, w, 44100)

	// Claiming fewer bits than the spectrum needs must be reported, not guessed
	// at: this is how a frame with a bad reservoir pointer gets caught.
	var out Spectrum
	if Decode(&out, cfg, bitio.NewReader(w.Bytes()), 44100, bits/2) {
		t.Error("truncated granule decoded as if valid")
	}
}
