package mp3packer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/W-Floyd/go-mp3packer/mp3"
)

func BenchmarkRecompress(b *testing.B) {
	for _, path := range testFiles(b) {
		data := read(b, path)
		b.Run(filepath.Base(path), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, _, err := Process(data, Options{Recompress: true}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecompressSingleWorker measures the recompression kernels themselves:
// with one worker the numbers are far less noisy than the parallel run, and they
// scale directly with any change to the search.
func BenchmarkRecompressSingleWorker(b *testing.B) {
	data := read(b, "testdata/bench-vbr.mp3")
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, _, err := Process(data, Options{Recompress: true, Workers: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

// layoutBenchN is how many times the protected corpus file is repeated to make
// the layout benchmark's input. 16 copies is ~320 KB and ~770 frames, enough
// that the measurement settles to well under a percent.
const layoutBenchN = 16

// layoutBenchInput returns the input for BenchmarkLayoutOnly: a CRC-protected
// stream long enough to measure.
//
// Both properties matter. Relaying the main data moves every granule's
// reservoir offset, so a protected frame's side info changes and its CRC has to
// be recomputed — on an unprotected file that work does not happen at all, and
// the benchmark is blind to it. The committed corpus has exactly one protected
// file and it is 20 KB, small enough that the whole run was ~29 µs and swung
// ±8%; twice now that noise hid a real change to layout. Repeating its frames
// gets a realistic length without committing a large file.
func layoutBenchInput(b testing.TB) []byte {
	b.Helper()
	if path := os.Getenv("MP3PACKER_BENCH_FILE"); path != "" {
		return read(b, path)
	}
	data := read(b, "testdata/cbr-crc.mp3")
	f, err := mp3.Parse(data)
	if err != nil {
		b.Fatal(err)
	}
	last := f.Frames[len(f.Frames)-1]
	// Frame bytes only: dropping the junk either side keeps the repeats
	// contiguous, and starting the repeats at frame 1 leaves a single leading
	// Xing/Info frame, where a stream with one every 48 frames is not
	// representative of anything.
	audio := data[f.Frames[0].Offset : last.Offset+last.Size()]
	repeat := data[f.Frames[1].Offset : last.Offset+last.Size()]
	out := make([]byte, 0, len(audio)+(layoutBenchN-1)*len(repeat))
	out = append(out, audio...)
	for i := 1; i < layoutBenchN; i++ {
		out = append(out, repeat...)
	}
	return out
}

// TestLayoutBenchInput pins what BenchmarkLayoutOnly relies on: the repeated
// stream parses as one continuous run of protected frames, and laying it out is
// lossless. If a corpus change quietly drops the CRCs the benchmark stops
// measuring the CRC recomputation and nothing else notices.
func TestLayoutBenchInput(t *testing.T) {
	t.Setenv("MP3PACKER_BENCH_FILE", "")
	data := layoutBenchInput(t)
	f, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.SyncErrors != 0 {
		t.Errorf("%d sync errors: the repeats do not join cleanly", f.SyncErrors)
	}
	if len(f.Frames) < 500 {
		t.Errorf("only %d frames: too short to time layout stably", len(f.Frames))
	}
	for i, fr := range f.Frames {
		if !fr.Header.CRC {
			t.Fatalf("frame %d is not CRC-protected", i)
		}
	}

	out, _, err := Process(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	before, after := spectra(t, data), spectra(t, out)
	if len(before) != len(after) {
		t.Fatalf("granule count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("granule %d changed", i)
		}
	}
	outFile, err := mp3.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	for i, fr := range outFile.Frames {
		if !fr.CRCValid() {
			t.Fatalf("output frame %d has a stale CRC", i)
		}
	}
}

// BenchmarkLayoutOnly times Process with recompression off, which is the whole
// of the serial path: parse and build the reservoir view, then choose frame
// sizes and write the stream back out. That is what caps parallel scaling, so
// timing it as one number is the point — but layout is only about a quarter of
// it, so a change to layout alone shows up at a quarter of its true size. Build
// with -tags mp3timing to get the two stages reported separately.
func BenchmarkLayoutOnly(b *testing.B) {
	data := layoutBenchInput(b)
	b.SetBytes(int64(len(data)))
	var prepare, layout time.Duration
	for b.Loop() {
		_, stats, err := Process(data, Options{})
		if err != nil {
			b.Fatal(err)
		}
		if timingEnabled {
			prepare += stats.Prepare
			layout += stats.Layout
		}
	}
	if timingEnabled {
		b.ReportMetric(float64(prepare.Nanoseconds())/float64(b.N), "prepare-ns/op")
		b.ReportMetric(float64(layout.Nanoseconds())/float64(b.N), "layout-ns/op")
	}
}

// payloadBits totals the coded audio in a file, ignoring headers, side info and
// padding. This is what recompression actually shrinks.
func payloadBits(t *testing.T, data []byte) int {
	t.Helper()
	f, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	bits := 0
	for _, fr := range f.Frames {
		bits += fr.MainDataBits()
	}
	return bits
}

func firstFrameSize(t *testing.T, data []byte) int {
	t.Helper()
	f, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return f.Frames[0].Size()
}

// BenchmarkProcessFile repacks through the file entry point, so the read, the
// write and the rename are all in the figure. Process alone leaves them out, and
// on the eight-second file they are a third of the work.
func BenchmarkProcessFile(b *testing.B) {
	dir := b.TempDir()
	for _, path := range testFiles(b) {
		data := read(b, path)
		out := filepath.Join(dir, filepath.Base(path))
		b.Run(filepath.Base(path), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := ProcessFile(path, out, Options{Recompress: true}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// mp3packerBinary builds the command, so that it can be timed the way a user
// actually invokes it.
func mp3packerBinary(tb testing.TB) string {
	tb.Helper()
	bin := filepath.Join(tb.TempDir(), "mp3packer")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/mp3packer").CombinedOutput(); err != nil {
		tb.Fatalf("building the command: %v: %s", err, out)
	}
	return bin
}

// BenchmarkCLI runs the built command as a subprocess.
//
// This is the only like-for-like comparison with BenchmarkReference, and the
// reason it exists: that one execs another implementation, so it pays for a
// process start and for reading and writing a file, and neither Process nor
// ProcessFile pays for a process start. Comparing either against it flatters us
// by the whole cost of exec, which on a file this small is not a rounding error.
func BenchmarkCLI(b *testing.B) {
	bin := mp3packerBinary(b)
	dir := b.TempDir()
	for _, path := range testFiles(b) {
		data := read(b, path)
		out := filepath.Join(dir, filepath.Base(path))
		b.Run(filepath.Base(path), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if err := exec.Command(bin, "-q", "-f", path, out).Run(); err != nil {
					b.Fatalf("%s: %v", bin, err)
				}
			}
		})
	}
}

// referenceBinary returns the path to another mp3packer implementation to compare
// against, taken from $MP3PACKER_REFERENCE. It is expected to accept
// "-z <in> <out>", which both the original OCaml mp3packer and the C++ port do.
func referenceBinary(tb testing.TB) string {
	tb.Helper()
	path := os.Getenv("MP3PACKER_REFERENCE")
	if path == "" {
		tb.Skip("set MP3PACKER_REFERENCE to another implementation to compare against")
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		tb.Skipf("MP3PACKER_REFERENCE=%q: %v", path, err)
	}
	return resolved
}

// BenchmarkReference times the reference implementation over the same files, so
// that `go test -bench 'Recompress|Reference'` produces a like-for-like
// comparison. Process reads from memory while the reference reads and writes
// files, which flatters us slightly; the recompression search dominates both.
func BenchmarkReference(b *testing.B) {
	bin := referenceBinary(b)
	dir := b.TempDir()
	for _, path := range testFiles(b) {
		data := read(b, path)
		out := filepath.Join(dir, filepath.Base(path))
		b.Run(filepath.Base(path), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if err := exec.Command(bin, "-z", path, out).Run(); err != nil {
					b.Fatalf("%s: %v", bin, err)
				}
			}
		})
	}
}

// TestReferenceCompression reports how our output size compares with the
// reference implementation's on the same inputs, and fails only if we are
// meaningfully worse.
func TestReferenceCompression(t *testing.T) {
	bin := referenceBinary(t)
	dir := t.TempDir()
	for _, path := range testFiles(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			in := read(t, path)
			// The reference discards frame CRCs, so match it for the comparison
			// to be about compression rather than about what is kept.
			ours, _, err := Process(in, Options{Recompress: true, StripCRC: true})
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, name)
			if err := exec.Command(bin, "-z", path, out).Run(); err != nil {
				t.Fatalf("%s: %v", bin, err)
			}
			theirs := read(t, out)
			t.Logf("input %d, ours %d (%.2f%%), reference %d (%.2f%%)",
				len(in), len(ours), 100*float64(len(in)-len(ours))/float64(len(in)),
				len(theirs), 100*float64(len(in)-len(theirs))/float64(len(in)))

			// Coded audio is the like-for-like measure: file size also reflects
			// what each implementation chooses to keep. We preserve a leading
			// Xing/Info frame byte for byte, where the reference truncates it to
			// the smallest frame that fits, so the reference can come out up to
			// one frame smaller on files that have one.
			if payloadBits(t, ours) > payloadBits(t, theirs) {
				t.Errorf("our coded audio is larger: %d vs %d bits",
					payloadBits(t, ours), payloadBits(t, theirs))
			}
			if slack := firstFrameSize(t, in); len(ours) > len(theirs)+slack {
				t.Errorf("our output is %d bytes larger than the reference's, more than the %d bytes a rewritten header frame explains",
					len(ours)-len(theirs), slack)
			}
		})
	}
}

// BenchmarkRecompressFile times a file named by MP3PACKER_BENCH_FILE, so that
// material longer than the committed corpus can be measured without carrying it
// in the repository.
func BenchmarkRecompressFile(b *testing.B) {
	path := os.Getenv("MP3PACKER_BENCH_FILE")
	if path == "" {
		b.Skip("set MP3PACKER_BENCH_FILE to an mp3 to benchmark")
	}
	data := read(b, path)
	for _, workers := range []int{1, 0} {
		name := "allcores"
		if workers == 1 {
			name = "1worker"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, _, err := Process(data, Options{Recompress: true, Workers: workers}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecompressWorkers times one file across a range of worker counts, so
// that parallel scaling is measurable without a separate harness. Parsing and
// laying the frames back out do not shrink with workers, and on a long file they
// are most of the all-cores wall clock, so the curve flattens well before the
// core count — read it against BenchmarkLayoutOnly rather than expecting linear.
func BenchmarkRecompressWorkers(b *testing.B) {
	path := os.Getenv("MP3PACKER_BENCH_FILE")
	if path == "" {
		path = "testdata/bench-vbr.mp3"
	}
	data := read(b, path)
	for _, workers := range []int{1, 2, 4, 8, 12, 0} {
		name := fmt.Sprintf("j%d", workers)
		if workers == 0 {
			name = "jall"
		}
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, _, err := Process(data, Options{Recompress: true, Workers: workers}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
