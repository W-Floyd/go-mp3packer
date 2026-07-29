package mp3packer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/W-Floyd/go-mp3packer/huffman"
	"github.com/W-Floyd/go-mp3packer/internal/bitio"
	"github.com/W-Floyd/go-mp3packer/mp3"
)

func testFiles(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob("testdata/*.mp3")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	return files
}

func read(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// spectra decodes every granule in a file, following the bit reservoir exactly
// as a decoder would. Two files with the same spectra decode to the same audio,
// which is the property a lossless repack has to preserve.
func spectra(t *testing.T, data []byte) []huffman.Spectrum {
	t.Helper()
	out, err := decodeSpectra(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// errNotSelfContained reports a granule whose coding runs past the length it
// declares. Nothing can be asserted about repacking such a granule; see the
// comment at the check itself.
var errNotSelfContained = errors.New("granule reads past part2_3_length")

// decodeSpectra is spectra without the test dependency: a granule that does not
// decode is an error rather than a failure, which is what the fuzz target needs
// — arbitrary bytes not decoding is the expected outcome, not a bug.
func decodeSpectra(data []byte) ([]huffman.Spectrum, error) {
	f, err := mp3.Parse(data)
	if err != nil {
		return nil, err
	}
	var pool []byte
	starts := make([]int, len(f.Frames))
	for i, fr := range f.Frames {
		starts[i] = len(pool)
		pool = append(pool, fr.MainData...)
	}
	var out []huffman.Spectrum
	var s huffman.Spectrum
	for i, fr := range f.Frames {
		h := fr.Header
		from := starts[i] - fr.SideInfo.MainDataBegin
		if from < 0 {
			return nil, fmt.Errorf("frame %d points before the start of the file", i)
		}
		pos := from * 8
		for gr := 0; gr < h.Granules(); gr++ {
			for ch := 0; ch < h.Channels(); ch++ {
				g := fr.SideInfo.Gr[gr][ch]
				sf := mp3.ScalefactorBits(h, fr.SideInfo, gr, ch)

				// Decode from a copy of exactly the bits the granule declares,
				// which a Reader zero-fills past the end of. A codeword is read
				// whole once it has started, so one that straddles
				// part2_3_length would otherwise pull in whatever follows the
				// granule — and what follows is ancillary data, which a repack
				// is free to drop. Two files that decode to the same audio can
				// differ there, so reading it would make this compare something
				// that is not the audio.
				src := bitio.NewReader(pool)
				src.Seek(pos)
				buf := bitio.NewWriterSize((g.Part23Length + 7) / 8)
				buf.Copy(src, g.Part23Length)
				r := bitio.NewReader(buf.Bytes())

				r.Skip(sf)
				cfg := granuleConfig(g)
				if !huffman.Decode(&s, cfg, r, h.SampleRate, g.Part23Length-sf) {
					return nil, fmt.Errorf("frame %d granule %d/%d did not decode", i, gr, ch)
				}
				if r.Tell() > g.Part23Length {
					// A codeword that begins inside the granule and ends outside
					// it makes the granule's own audio depend on bytes that are
					// not part of it. No encoder produces that and a repack
					// cannot promise anything about it, since those bytes are
					// ancillary data or another frame's reservoir.
					return nil, fmt.Errorf("frame %d granule %d/%d reads %d bits past its %d: %w",
						i, gr, ch, r.Tell()-g.Part23Length, g.Part23Length, errNotSelfContained)
				}
				out = append(out, s)
				pos += g.Part23Length
			}
		}
	}
	return out, nil
}

func TestProcessIsLossless(t *testing.T) {
	for _, path := range testFiles(t) {
		for _, recompress := range []bool{false, true} {
			name := filepath.Base(path)
			if recompress {
				name += "/-z"
			}
			t.Run(name, func(t *testing.T) {
				in := read(t, path)
				out, stats, err := Process(in, Options{Recompress: recompress})
				if err != nil {
					t.Fatal(err)
				}
				if len(out) > len(in) {
					t.Errorf("output grew: %d -> %d bytes", len(in), len(out))
				}
				if stats.Abandoned != 0 {
					t.Errorf("%d frames could not be recompressed", stats.Abandoned)
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

				inFile, _ := mp3.Parse(in)
				outFile, _ := mp3.Parse(out)
				if len(inFile.Frames) != len(outFile.Frames) {
					t.Errorf("frame count changed: %d -> %d", len(inFile.Frames), len(outFile.Frames))
				}
				if !bytes.Equal(inFile.StartJunk, outFile.StartJunk) {
					t.Error("leading tag data changed")
				}
				if !bytes.Equal(inFile.EndJunk, outFile.EndJunk) {
					t.Error("trailing tag data changed")
				}
				for i, fr := range outFile.Frames {
					if !fr.CRCValid() {
						t.Fatalf("output frame %d has a bad CRC", i)
					}
				}
			})
		}
	}
}

func TestRecompressBeatsLayoutOnly(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			in := read(t, path)
			plain, _, err := Process(in, Options{})
			if err != nil {
				t.Fatal(err)
			}
			packed, stats, err := Process(in, Options{Recompress: true})
			if err != nil {
				t.Fatal(err)
			}
			if stats.NewPayload > stats.PayloadBits {
				t.Errorf("payload grew: %d -> %d bits", stats.PayloadBits, stats.NewPayload)
			}
			if len(packed) > len(in) {
				t.Errorf("output grew past the input: %d vs %d", len(packed), len(in))
			}
			// A smaller payload only shrinks the file while the frames are still
			// carrying data: once every frame is already at the minimum size, the
			// space freed up becomes padding instead, and can even cost a byte or
			// two more of it. Only insist on a win when there is one to be had.
			saved := stats.PayloadBits - stats.NewPayload
			if saved > stats.PayloadBits/100 && len(packed) >= len(plain) {
				t.Errorf("payload shrank by %d bits but the file did not: %d vs %d",
					saved, len(packed), len(plain))
			}
		})
	}
}

// TestProcessConverges checks that a repacked file is a fixed point: running the
// tool twice must not keep changing the file.
func TestProcessConverges(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			once, _, err := Process(read(t, path), Options{Recompress: true})
			if err != nil {
				t.Fatal(err)
			}
			twice, stats, err := Process(once, Options{Recompress: true})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(once, twice) {
				t.Errorf("second pass changed %d bytes (%d frames recompressed again)",
					len(twice)-len(once), stats.Recompressed)
			}
		})
	}
}

func TestWorkerCountDoesNotAffectOutput(t *testing.T) {
	in := read(t, "testdata/vbr-joint.mp3")
	want, _, err := Process(in, Options{Recompress: true, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{2, 3, 8} {
		got, _, err := Process(in, Options{Recompress: true, Workers: n})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%d workers produced different output", n)
		}
	}
}

func TestProcessRejectsNonAudio(t *testing.T) {
	if _, _, err := Process(bytes.Repeat([]byte{0x41}, 4096), Options{}); err == nil {
		t.Error("expected an error for a file with no frames")
	}
}

// TestDecodedAudioIsIdentical compares the decoded waveform before and after,
// using ffmpeg as an independent decoder. Skipped when ffmpeg is unavailable.
func TestDecodedAudioIsIdentical(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	decode := func(path string) []byte {
		out, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-").Output()
		if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		return out
	}
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, _, err := Process(read(t, path), Options{Recompress: true})
			if err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(t.TempDir(), "packed.mp3")
			if err := os.WriteFile(tmp, out, 0o644); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decode(path), decode(tmp)) {
				t.Error("decoded audio differs")
			}
		})
	}
}
