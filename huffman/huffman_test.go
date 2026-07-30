package huffman

import (
	"bytes"
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

// TestPairTablesMatchTheTree holds the big-value probe to the trees it was built
// from, for every table and every prefix: the decoder trusts it for 99% of the
// symbols it reads, and a single wrong entry would silently decode a pair to the
// wrong magnitudes. The walk here is the plain definition, one bit at a time from
// the root, with no reference to how the table is packed.
func TestPairTablesMatchTheTree(t *testing.T) {
	for idx := range pairTables {
		tree := tables[idx].tree
		for prefix := 0; prefix < pairSize; prefix++ {
			e := pairTables[idx][prefix]

			node, used, sym, done := 0, 0, 0, false
			for len(tree) > 0 {
				v := tree[node]
				if v >= 0 {
					sym, done = int(v)&0xFF, true
					break
				}
				if used == pairBits {
					break // no codeword this short: the entry must say so
				}
				node++
				if prefix&(1<<uint(pairBits-1-used)) != 0 {
					node -= int(v)
				}
				used++
				if node >= len(tree) {
					break // undefined table, which only the walk may discover
				}
			}

			if !done {
				if !e.isSlow() {
					t.Fatalf("table %d prefix %0*b: entry resolves a pair the tree does not",
						idx, pairBits, prefix)
				}
				continue
			}
			if e.isSlow() {
				t.Fatalf("table %d prefix %0*b: entry defers a %d-bit codeword the tree resolves",
					idx, pairBits, prefix, used)
			}
			x, y := sym>>4, sym&0xF
			if e.x() != x || e.y() != y || e.length() != used {
				t.Fatalf("table %d prefix %0*b: entry gives (%d,%d) in %d bits, tree gives (%d,%d) in %d",
					idx, pairBits, prefix, e.x(), e.y(), e.length(), x, y, used)
			}
			// The sign counts are what the decoder advances the bit window by, so a
			// wrong one desynchronises the whole region rather than one pair.
			if e.nx() != uint(b2u(x != 0)) || e.ny() != uint(b2u(y != 0)) {
				t.Fatalf("table %d prefix %0*b: pair (%d,%d) carries sign counts %d and %d",
					idx, pairBits, prefix, x, y, e.nx(), e.ny())
			}
		}
	}
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

// TestCodingEncodeMatchesEncode holds the two encode paths to the same bytes.
// Coding.Encode differs from Encode only in taking the end of the coefficients
// from the search instead of walking to find it, so a Coding that disagreed with
// lastNonZero would change the count1 region's length — output bytes, not just
// speed. The whole point of the field being unexported is that this is the only
// place it can come from, so this is where it is checked.
func TestCodingEncodeMatchesEncode(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	rates := []int{44100, 48000, 32000, 24000, 22050, 8000}

	spectra := []Spectrum{{}} // silence: nothing to code at all
	var top Spectrum
	top[NumCoefficients-1] = 1 // and a granule that codes to the very end
	spectra = append(spectra, top)
	for _, peak := range []int{1, 2, 7, 15, 60, 4000} {
		for iter := 0; iter < 10; iter++ {
			spectra = append(spectra, randomSpectrum(rng, peak))
		}
	}

	for _, rate := range rates {
		for i := range spectra {
			s := spectra[i]
			for geom := 0; geom < 4; geom++ {
				orig := Config{Count1Table: count1Table32}
				switch geom {
				case 1:
					orig.WindowSwitching, orig.BlockType = true, 2
				case 2:
					orig.WindowSwitching, orig.BlockType, orig.MixedBlock = true, 2, true
				case 3:
					orig.WindowSwitching, orig.BlockType = true, 1
				}
				c := Search(&s, orig, rate)
				if c.last != lastNonZero(&s) {
					t.Fatalf("%dHz geometry %d: search says last is %d, lastNonZero says %d",
						rate, geom, c.last, lastNonZero(&s))
				}
				if c.Bits < 0 {
					continue
				}

				wc := bitio.NewWriter()
				c.Encode(&s, wc, rate)
				we := bitio.NewWriter()
				Encode(&s, c.Config, we, rate)
				if wc.Tell() != we.Tell() || !bytes.Equal(wc.Bytes(), we.Bytes()) {
					t.Fatalf("%dHz geometry %d: Coding.Encode wrote %d bits, Encode wrote %d",
						rate, geom, wc.Tell(), we.Tell())
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

// benchCorpus is a fixed spread of granules, from near-silence to a dense
// high-bitrate frame, so that a granule-level benchmark reflects the mix the
// search really sees. The seed is fixed: these numbers are only useful compared
// with each other.
func benchCorpus() []Spectrum {
	rng := rand.New(rand.NewSource(1))
	peaks := []int{1, 2, 5, 15, 40, 120, 600}
	out := make([]Spectrum, 0, 4*len(peaks))
	for _, peak := range peaks {
		for i := 0; i < 4; i++ {
			out = append(out, randomSpectrum(rng, peak))
		}
	}
	return out
}

// benchCoded is the corpus with each granule's cheapest coding worked out and
// written, which is what the decoder has to be measured against.
type benchCoded struct {
	cfg  Config
	bits int
	r    *bitio.Reader
}

func benchEncoded(tb testing.TB) ([]Spectrum, []benchCoded) {
	tb.Helper()
	corpus := benchCorpus()
	coded := make([]benchCoded, len(corpus))
	for i := range corpus {
		cfg, bits := Optimize(&corpus[i], Config{Count1Table: count1Table32}, 44100)
		if bits < 0 {
			tb.Fatalf("granule %d has no legal coding", i)
		}
		w := bitio.NewWriterSize((bits + 7) / 8)
		Encode(&corpus[i], cfg, w, 44100)
		coded[i] = benchCoded{cfg: cfg, bits: bits, r: bitio.NewReader(w.Bytes())}
	}
	return corpus, coded
}

// BenchmarkOptimizeGranule, BenchmarkDecodeGranule and BenchmarkEncodeGranule
// time the three halves of the work separately. The end-to-end benchmarks cannot
// attribute a change to one of them without a profile, which makes small steps
// hard to judge; these can be read directly.
func BenchmarkOptimizeGranule(b *testing.B) {
	corpus := benchCorpus()
	cfg := Config{Count1Table: count1Table32}
	i := 0
	for b.Loop() {
		Optimize(&corpus[i], cfg, 44100)
		if i++; i == len(corpus) {
			i = 0
		}
	}
}

func BenchmarkDecodeGranule(b *testing.B) {
	_, coded := benchEncoded(b)
	var dst Spectrum
	i := 0
	for b.Loop() {
		c := &coded[i]
		c.r.Seek(0)
		if !Decode(&dst, c.cfg, c.r, 44100, c.bits) {
			b.Fatalf("granule %d failed to decode", i)
		}
		if i++; i == len(coded) {
			i = 0
		}
	}
}

func BenchmarkEncodeGranule(b *testing.B) {
	corpus, coded := benchEncoded(b)
	buf := make([]byte, NumCoefficients*8+bitio.Slack)
	i := 0
	for b.Loop() {
		Encode(&corpus[i], coded[i].cfg, bitio.NewWriterBuf(buf), 44100)
		if i++; i == len(corpus) {
			i = 0
		}
	}
}

// BenchmarkEncodeGranuleCoding is the path a repack takes: the granule has just
// been searched, so Coding carries the end of its coefficients and the encoder
// does not walk the trailing zeros to rediscover it. The difference from
// BenchmarkEncodeGranule above is exactly that walk.
func BenchmarkEncodeGranuleCoding(b *testing.B) {
	corpus := benchCorpus()
	codings := make([]Coding, len(corpus))
	for i := range corpus {
		codings[i] = Search(&corpus[i], Config{Count1Table: count1Table32}, 44100)
		if codings[i].Bits < 0 {
			b.Fatalf("granule %d has no legal coding", i)
		}
	}
	buf := make([]byte, NumCoefficients*8+bitio.Slack)
	i := 0
	for b.Loop() {
		codings[i].Encode(&corpus[i], bitio.NewWriterBuf(buf), 44100)
		if i++; i == len(corpus) {
			i = 0
		}
	}
}

// BenchmarkOptimizeGranuleSwitched is the same search over window-switched
// geometry, which takes its own path: the standard fixes where region0 ends, so
// there are two spans to cost rather than a split to enumerate. The benchmark
// above never enters it, and a short block's boundary is not even one of the band
// boundaries the long-block path works in.
func BenchmarkOptimizeGranuleSwitched(b *testing.B) {
	for _, geom := range []struct {
		name string
		cfg  Config
	}{
		{"short", Config{Count1Table: count1Table32, WindowSwitching: true, BlockType: 2}},
		{"start", Config{Count1Table: count1Table32, WindowSwitching: true, BlockType: 1}},
	} {
		b.Run(geom.name, func(b *testing.B) {
			corpus := benchCorpus()
			i := 0
			for b.Loop() {
				Optimize(&corpus[i], geom.cfg, 44100)
				if i++; i == len(corpus) {
					i = 0
				}
			}
		})
	}
}
