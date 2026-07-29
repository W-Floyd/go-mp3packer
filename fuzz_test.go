package mp3packer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/W-Floyd/go-mp3packer/mp3"
)

// FuzzProcess drives the whole repack from arbitrary bytes.
//
// Process is an exported entry point that takes untrusted input and then indexes
// its way through it: the pad clamp in Peek64, the arena slot bounds, the
// reservoir clamps in mainData, PatchMainDataBegin, FindInfoTag. Every one of
// those has moved in the last dozen commits, and unit tests only ever show them
// well-formed frames.
//
// Rejecting an input is always allowed — most random bytes are not an MP3, and
// even a real frame can be unrepackable. What is not allowed is panicking, or
// returning success alongside something that is not a valid file. Both are
// checked for each of the option combinations the command line exposes, since
// the layout path and the search path fail differently.
func FuzzProcess(f *testing.F) {
	for _, path := range testFiles(f) {
		f.Add(read(f, path))
	}
	// Two shapes no corpus file has: nothing at all, and a frame header with
	// nothing behind it.
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xFB, 0x90, 0x00})

	options := []struct {
		name string
		opt  Options
	}{
		{"layout", Options{}},
		{"recompress", Options{Recompress: true, Workers: 1}},
		{"nocrc", Options{Recompress: true, Workers: 1, StripCRC: true}},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, o := range options {
			out, stats, err := Process(data, o.opt)
			if err != nil {
				continue // refusing an input is a legitimate outcome
			}
			checkRepack(t, o.name, data, out, stats, o.opt)
		}
	})
}

// checkRepack asserts what a successful Process has to have produced, whatever
// went in.
func checkRepack(t *testing.T, name string, data, out []byte, stats Stats, opt Options) {
	t.Helper()
	in, err := mp3.Parse(data)
	if err != nil {
		t.Fatalf("%s: the input parsed for Process but not for us: %v", name, err)
	}
	if _, err := mp3.Parse(out); err != nil {
		t.Fatalf("%s: output does not parse: %v", name, err)
	}

	// Everything outside the frames is copied, never rebuilt. This is asserted
	// against the bytes rather than against a re-parse of the output: where the
	// junk itself contains something that looks like a frame header, which is a
	// shape the fuzzer reaches easily, the parser has two readings of the result
	// and no heuristic can be relied on to pick the same one twice. Carving the
	// frame region out by the lengths the input parse gave us takes that
	// ambiguity out of everything below.
	if !bytes.HasPrefix(out, in.StartJunk) {
		t.Fatalf("%s: leading %d bytes not preserved", name, len(in.StartJunk))
	}
	if !bytes.HasSuffix(out, in.EndJunk) {
		t.Fatalf("%s: trailing %d bytes not preserved", name, len(in.EndJunk))
	}
	if len(out) < len(in.StartJunk)+len(in.EndJunk) {
		t.Fatalf("%s: output of %d bytes cannot hold %d bytes of preserved junk",
			name, len(out), len(in.StartJunk)+len(in.EndJunk))
	}
	inAudio := data[len(in.StartJunk) : len(data)-len(in.EndJunk)]
	outAudio := out[len(in.StartJunk) : len(out)-len(in.EndJunk)]

	// The input region has to be checked for the same mis-lock as the output
	// one, and for the same reason. Trimming the trailing junk changes where the
	// best reading of the remainder starts: one case here parses as a frame at
	// offset 0 whole, and as a different frame at offset 3 once four trailing
	// bytes are gone. Comparing that against what Process actually worked from
	// compares two different files.
	inFile, err := mp3.Parse(inAudio)
	if err != nil || len(inFile.StartJunk) != 0 || len(inFile.Frames) != len(in.Frames) {
		return
	}

	outFile, err := mp3.Parse(outAudio)
	if err != nil {
		t.Fatalf("%s: output frame region does not parse: %v", name, err)
	}
	if len(outFile.StartJunk) != 0 || len(in.Frames) != len(outFile.Frames) {
		// Not necessarily a fault, and the two tests are one condition: the
		// region was carved to begin at a frame, so a parse that puts anything
		// in front of the first one has mis-locked, whatever the frame count
		// then comes to.
		//
		// Parsing is a search — a frame's payload can
		// hold bytes that read as a plausible header, and the repack changes
		// which reading scores best by changing the payload's length and
		// position. A real encoder does not produce such a payload; the fuzzer
		// produces little else. Everything below compares one parse against
		// another, so none of it means anything once the two disagree about
		// where the frames are, and the checks above — output parses, junk
		// preserved byte for byte — are what still holds here.
		return
	}
	// A leading Xing/Info frame is metadata, not audio: it is copied byte for
	// byte, so it keeps whatever CRC it arrived with and StripCRC does not reach
	// it. Everything after it is ours to answer for.
	audio := 0
	if len(in.Frames) > 0 && in.Frames[0].MainDataBits() == 0 {
		raw := data[in.Frames[0].Offset : in.Frames[0].Offset+in.Frames[0].Size()]
		if mp3.FindInfoTag(raw, in.Frames[0].Header) != nil {
			audio = 1
		}
	}
	for i := audio; i < len(outFile.Frames) && i < len(in.Frames); i++ {
		fr := outFile.Frames[i]
		if opt.StripCRC && fr.Header.CRC {
			t.Errorf("%s: frame %d kept its CRC", name, i)
		}
		// Only meaningful where the input's own CRC was right to begin with: a
		// frame we abandon is copied through, corrupt checksum and all.
		if in.Frames[i].CRCValid() && !fr.CRCValid() {
			t.Errorf("%s: frame %d has a stale CRC", name, i)
		}
	}

	// The point of the whole program: if the input decoded, the output has to
	// decode to exactly the same coefficients. Frames the repacker gave up on
	// are copied through, so they cannot break this either.
	before, err := decodeSpectra(inAudio)
	if err != nil {
		return // an input we cannot decode says nothing about the output
	}
	after, err := decodeSpectra(outAudio)
	if errors.Is(err, errNotSelfContained) {
		// Our own encoder writes each granule's bits and then declares exactly
		// that many, so this should be unreachable — but it is a property of the
		// output rather than of the input, so it is worth saying out loud.
		t.Fatalf("%s: we emitted a granule that reads past its own length: %v", name, err)
	}
	if err != nil {
		t.Fatalf("%s: input decoded but output did not: %v", name, err)
	}
	if len(before) != len(after) {
		t.Fatalf("%s: granule count %d -> %d", name, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s: granule %d changed", name, i)
		}
	}
	if stats.Abandoned > len(in.Frames) {
		t.Errorf("%s: %d frames abandoned of %d", name, stats.Abandoned, len(in.Frames))
	}
}
