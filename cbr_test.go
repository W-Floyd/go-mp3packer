package mp3packer

import (
	"path/filepath"
	"testing"

	"github.com/W-Floyd/go-mp3packer/mp3"
)

// bitrates reports the distinct bitrates a file's frames declare, and whether
// every frame's padding bit is what a constant-bitrate stream of that bitrate
// wants at its position.
func bitrates(t *testing.T, data []byte) (rates map[int]int, cyclic bool) {
	t.Helper()
	f, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	rates, cyclic = map[int]int{}, true
	for i := range f.Frames {
		h := f.Frames[i].Header
		rates[h.Bitrate()]++
		if h.Padding != mp3.Padded(h.Version, h.SampleRate, h.Bitrate(), i) {
			cyclic = false
		}
	}
	return rates, cyclic
}

// TestConstantBitrateIsConstant is the property the option exists for: one
// bitrate across the whole file, padded exactly where the standard's cycle says,
// and the same audio as before.
func TestConstantBitrateIsConstant(t *testing.T) {
	for _, path := range testFiles(t) {
		for _, recompress := range []bool{false, true} {
			name := filepath.Base(path)
			if recompress {
				name += "/-z"
			}
			t.Run(name, func(t *testing.T) {
				in := read(t, path)
				out, stats, err := Process(in, Options{Recompress: recompress, ConstantBitrate: true})
				if err != nil {
					t.Fatal(err)
				}
				if stats.Bitrate == 0 {
					t.Fatal("no bitrate reported")
				}
				rates, cyclic := bitrates(t, out)
				if len(rates) != 1 || rates[stats.Bitrate] == 0 {
					t.Errorf("output holds bitrates %v, want only %d", rates, stats.Bitrate)
				}
				if !cyclic {
					t.Error("padding does not follow the constant-bitrate cycle")
				}

				before, after := spectra(t, in), spectra(t, out)
				if len(before) != len(after) {
					t.Fatalf("granule count changed: %d -> %d", len(before), len(after))
				}
				for i := range before {
					if before[i] != after[i] {
						t.Fatalf("granule %d changed", i)
					}
				}
			})
		}
	}
}

// TestConstantBitrateIsLowest checks that the chosen bitrate is not one step
// higher than it needs to be: laying the same audio out one bitrate lower must
// leave some frame unable to keep to the floor.
func TestConstantBitrateIsLowest(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := read(t, path)
			_, stats, err := Process(in, Options{Recompress: true, ConstantBitrate: true})
			if err != nil {
				t.Fatal(err)
			}
			file, err := mp3.Parse(in)
			if err != nil {
				t.Fatal(err)
			}
			h := file.Frames[len(file.Frames)-1].Header
			lower := 0
			for idx := 1; idx <= mp3.MaxBitrateIndex; idx++ {
				if got := bitrateOf(h, idx); got < stats.Bitrate {
					lower = max(lower, got)
				}
			}
			if lower == 0 {
				t.Skipf("%d kbps is the lowest bitrate there is", stats.Bitrate)
			}
			first := file.Frames[0]
			if first.MainDataBits() == 0 && first.Header.Bitrate() > lower {
				t.Skipf("the header frame's own %d kbps sets the floor, not the audio", first.Header.Bitrate())
			}

			out, _, err := Process(in, Options{Recompress: true, MinBitrate: lower})
			if err != nil {
				t.Fatal(err)
			}
			rates, _ := bitrates(t, out)
			if len(rates) == 1 && rates[lower] > 0 {
				t.Errorf("%d kbps was enough after all, but %d was chosen", lower, stats.Bitrate)
			}
		})
	}
}

// bitrateOf is the bitrate a capacity's index names, which Capacity itself does
// not carry.
func bitrateOf(h mp3.Header, index int) int {
	h.BitrateIndex = index
	return h.Bitrate()
}

// TestSmallestCBRBitrateAgreesWithRepack holds the number reported before a
// repack to the one the repack settles on, since the point of reporting it is to
// be able to ask for it.
func TestSmallestCBRBitrateAgreesWithRepack(t *testing.T) {
	for _, path := range testFiles(t) {
		for _, recompress := range []bool{false, true} {
			name := filepath.Base(path)
			if recompress {
				name += "/-z"
			}
			t.Run(name, func(t *testing.T) {
				in := read(t, path)
				opt := Options{Recompress: recompress}
				reported, err := SmallestCBRBitrate(in, opt)
				if err != nil {
					t.Fatal(err)
				}
				opt.ConstantBitrate = true
				_, stats, err := Process(in, opt)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case reported == stats.Bitrate:
				case reported < stats.Bitrate:
					// The only reason to end up higher: the preserved header frame
					// is bigger than the audio needs.
					file, err := mp3.Parse(in)
					if err != nil {
						t.Fatal(err)
					}
					if first := file.Frames[0].Header; first.Bitrate() != stats.Bitrate {
						t.Errorf("repacked at %d kbps, %d was reported, and the header frame is %d",
							stats.Bitrate, reported, first.Bitrate())
					}
				default:
					t.Errorf("reported %d kbps but repacked at %d", reported, stats.Bitrate)
				}
			})
		}
	}
}

// TestMinBitrateOnlyRaises checks the floor never makes a frame smaller than the
// free layout would have: it is a minimum, so a file whose audio wants more than
// the floor keeps the bitrate its audio wants.
func TestMinBitrateOnlyRaises(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := read(t, path)
			free, _, err := Process(in, Options{Recompress: true})
			if err != nil {
				t.Fatal(err)
			}
			floored, _, err := Process(in, Options{Recompress: true, MinBitrate: 32})
			if err != nil {
				t.Fatal(err)
			}
			if len(floored) < len(free) {
				t.Errorf("a 32 kbps floor made the file smaller: %d < %d", len(floored), len(free))
			}
			before, after := spectra(t, in), spectra(t, floored)
			if len(before) != len(after) {
				t.Fatalf("granule count changed: %d -> %d", len(before), len(after))
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("granule %d changed", i)
				}
			}
		})
	}
}
