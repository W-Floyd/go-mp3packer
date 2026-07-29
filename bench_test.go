package mp3packer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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

func BenchmarkRecompressSingleWorker(b *testing.B) {
	data := read(b, "testdata/vbr-joint.mp3")
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, _, err := Process(data, Options{Recompress: true, Workers: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLayoutOnly(b *testing.B) {
	data := read(b, "testdata/vbr-joint.mp3")
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, _, err := Process(data, Options{}); err != nil {
			b.Fatal(err)
		}
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
