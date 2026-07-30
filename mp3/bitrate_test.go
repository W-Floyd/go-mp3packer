package mp3

import "testing"

// TestPaddingCycleIsExactlyTheBitrate is the property that makes a padded stream
// the bitrate it declares: the first n frames occupy exactly the number of whole
// bytes n frames of that bitrate are worth, for every n.
//
// It also pins the cycle's phase — frame 0 is never padded — since the layout and
// the feasibility walk have to agree on where in the cycle a frame sits.
func TestPaddingCycleIsExactlyTheBitrate(t *testing.T) {
	for _, v := range []Version{MPEG1, MPEG2, MPEG25} {
		mult := 72000
		if v == MPEG1 {
			mult = 144000
		}
		for _, rate := range sampleRateTable[v] {
			for idx := 1; idx <= MaxBitrateIndex; idx++ {
				kbps := bitrateTable[v][idx]
				total := mult * kbps
				if Padded(v, rate, kbps, 0) {
					t.Errorf("v=%v rate=%d %dkbps: frame 0 is padded", v, rate, kbps)
				}
				bytes := 0
				for n := 0; n < 200; n++ {
					bytes += unpaddedFrameSize(v, rate, kbps)
					if Padded(v, rate, kbps, n) {
						bytes++
					}
					if want := (n + 1) * total / rate; bytes != want {
						t.Fatalf("v=%v rate=%d %dkbps: %d frames are %d bytes, want %d",
							v, rate, kbps, n+1, bytes, want)
					}
				}
			}
		}
	}
}

// TestPaddingOnlyAt44100Family checks the shortcut the rest of the code relies
// on: every other sample rate divides evenly at every bitrate, so no frame is
// ever padded and a constant bitrate is a single frame size.
func TestPaddingOnlyAt44100Family(t *testing.T) {
	for _, v := range []Version{MPEG1, MPEG2, MPEG25} {
		for _, rate := range sampleRateTable[v] {
			needs := false
			for idx := 1; idx <= MaxBitrateIndex; idx++ {
				for n := 0; n < 49; n++ {
					if Padded(v, rate, bitrateTable[v][idx], n) {
						needs = true
					}
				}
			}
			if want := rate == 44100 || rate == 22050 || rate == 11025; needs != want {
				t.Errorf("rate %d: padding needed = %v, want %v", rate, needs, want)
			}
		}
	}
}

func TestCapacityFloor(t *testing.T) {
	h := Header{Version: MPEG1, SampleRate: 44100, Mode: JointStereo, BitrateIndex: 9}
	unpadded := func(kbps int) int { return unpaddedFrameSize(MPEG1, 44100, kbps) - h.overhead() }

	for _, c := range []struct {
		kbps    int
		bitrate int
		first   int  // capacity the floor allows frame 0
		padded1 bool // and whether it pads frame 1
	}{
		{0, 0, 0, false},
		{128, 128, unpadded(128), true},     // exact: the constant-bitrate cycle
		{129, 128, unpadded(128) + 1, true}, // one over: padded always
		{117, 128, unpadded(128), false},    // between: round up, never padded
		{112, 112, unpadded(112), true},     // exact again, a different cycle
		{400, 320, unpadded(320) + 1, true}, // over the top: padded maximum
		{1, 32, unpadded(32), false},        // below the lowest: round up to it
		{32, 32, unpadded(32), false},       // 32kbps pads neither of the first two
	} {
		f := h.CapacityFloor(c.kbps)
		if f.Bitrate() != c.bitrate {
			t.Errorf("-b %d: bitrate %d, want %d", c.kbps, f.Bitrate(), c.bitrate)
		}
		if got := f.At(0); got.DataSize != c.first {
			t.Errorf("-b %d: frame 0 capacity %d, want %d", c.kbps, got.DataSize, c.first)
		}
		if got := f.At(1); got.Padding != c.padded1 {
			t.Errorf("-b %d: frame 1 padded = %v, want %v", c.kbps, got.Padding, c.padded1)
		}
	}
}

func TestSmallestCBRBitrate(t *testing.T) {
	h := Header{Version: MPEG1, SampleRate: 44100, Mode: JointStereo, BitrateIndex: 9}
	at := func(kbps int) int { return unpaddedFrameSize(MPEG1, 44100, kbps) - h.overhead() }

	// Every frame exactly filling a 128kbps unpadded frame needs 128kbps: the
	// padding the cycle adds is slack, but no lower bitrate can carry it.
	flat := make([]int, 100)
	for i := range flat {
		flat[i] = at(128)
	}
	if got := h.SmallestCBRBitrate(flat, 0); got != 128 {
		t.Errorf("flat 128kbps payloads: got %d, want 128", got)
	}
	// One byte more than a frame holds is fine — the reservoir covers it — but a
	// hundred such frames in a row is not, since nothing ever banks anything.
	for i := range flat {
		flat[i] = at(128) + 1
	}
	if got := h.SmallestCBRBitrate(flat, 0); got != 160 {
		t.Errorf("slightly over 128kbps: got %d, want 160", got)
	}
	// A single spike is absorbed by what the quiet frames before it banked, up to
	// the 511 bytes a back-reference can reach.
	spike := make([]int, 100)
	for i := range spike {
		spike[i] = 10
	}
	spike[99] = at(32) + 400
	if got := h.SmallestCBRBitrate(spike, 0); got != 32 {
		t.Errorf("spike within reservoir reach: got %d, want 32", got)
	}
	// The same spike first, with nothing banked, does not fit at 32kbps.
	spike[0], spike[99] = spike[99], spike[0]
	if got := h.SmallestCBRBitrate(spike, 0); got == 32 {
		t.Error("spike in the first frame: got 32, want more")
	}
	// Nothing carries a frame bigger than a maximum frame plus a full reservoir.
	if got := h.SmallestCBRBitrate([]int{at(320) + 512}, 0); got != 0 {
		t.Errorf("impossible payload: got %d, want 0", got)
	}
}
