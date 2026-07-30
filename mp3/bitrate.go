package mp3

// Padded reports whether frame number n of a constant-bitrate stream carries a
// padding byte.
//
// A frame holds a whole number of bytes but the bitrate divides the sample rate
// unevenly, so the exact length 144000·kbps/rate has a fractional part that the
// padding bit makes up for: a frame is padded whenever the running total has
// accumulated another whole byte. Over the cycle the average frame is exactly the
// declared length, which is what makes the stream the bitrate it claims.
//
// Only the 44.1 kHz family needs this. The other rates divide evenly at every
// bitrate, so nothing is ever padded and this returns false for all of them.
func Padded(v Version, sampleRate, bitrateKbps, n int) bool {
	mult := 72000
	if v == MPEG1 {
		mult = 144000
	}
	total := mult * bitrateKbps
	// Both ends of the frame, in exact bytes scaled by sampleRate: a frame is
	// padded when a byte boundary falls between them.
	from := n * total / sampleRate
	to := (n + 1) * total / sampleRate
	return to-from > total/sampleRate
}

// CapacityFloor is a per-frame lower bound on frame size, as a minimum bitrate
// implies one. Its zero value bounds nothing.
//
// The bound moves from frame to frame, because a constant bitrate that does not
// divide the sample rate evenly is a cycle of padded and unpadded frames rather
// than one size (see [Padded]).
type CapacityFloor struct {
	index    int
	dataSize int  // capacity of the unpadded frame at that index
	always   bool // pad every frame rather than following the cycle
	cycle    bool // follow the constant-bitrate padding cycle

	version    Version
	sampleRate int
	bitrate    int
}

// CapacityFloor reads a requested minimum bitrate in kbps the way mp3packer's
// -b does, which is quirkier than it looks but is the interface people know:
//
//   - an exact bitrate (128) floors every frame at that bitrate, padded or not
//     according to the constant-bitrate cycle — so the output is CBR at 128;
//   - one more than an exact bitrate (129) floors every frame at a padded frame
//     of that bitrate, which is CBR at the next size up from 128;
//   - more than the maximum bitrate floors every frame at a padded maximum
//     frame;
//   - anything else (117) rounds up to the next valid bitrate, unpadded.
//
// Zero, or anything below the lowest bitrate, bounds nothing.
func (h Header) CapacityFloor(kbps int) CapacityFloor {
	rates := bitrateTable[h.Version]
	if kbps <= 0 {
		return CapacityFloor{}
	}
	f := CapacityFloor{version: h.Version, sampleRate: h.SampleRate}
	maxRate := rates[MaxBitrateIndex]
	switch {
	case kbps > maxRate:
		f.index, f.always = MaxBitrateIndex, true
	default:
		for idx := 1; idx <= MaxBitrateIndex; idx++ {
			if kbps > rates[idx]+1 {
				continue
			}
			f.index = idx
			switch kbps {
			case rates[idx]:
				f.cycle = true
			case rates[idx] + 1:
				f.always = true
			}
			break
		}
	}
	f.bitrate = rates[f.index]
	f.dataSize = unpaddedFrameSize(h.Version, h.SampleRate, f.bitrate) - h.overhead()
	return f
}

// Bitrate is the bitrate the floor settled on, which is the requested one only
// when the request was a valid bitrate. Zero means the floor bounds nothing.
func (f CapacityFloor) Bitrate() int { return f.bitrate }

// At is the smallest frame the floor allows for frame number n. A zero
// CapacityFloor allows any frame, so its capacity is zero.
func (f CapacityFloor) At(n int) Capacity {
	if f.index == 0 {
		return Capacity{}
	}
	pad := f.always
	if f.cycle {
		pad = Padded(f.version, f.sampleRate, f.bitrate, n)
	}
	return Capacity{Index: f.index, Padding: pad, DataSize: f.dataSize + boolInt(pad)}
}

// SmallestCBRBitrate is the lowest constant bitrate that can carry payloads,
// where payloads[i] is the number of main-data bytes frame i needs. It returns
// zero if even the maximum bitrate cannot.
//
// A constant bitrate hands every frame the same room whether it needs it or not,
// so the question is not whether the total fits but whether the bit reservoir
// ever runs dry: a frame may spend what earlier frames banked, but only up to
// what its side info can point back to. This walks the stream keeping that
// balance and rejects a bitrate as soon as it goes negative.
//
// from is the position of payloads[0] in the output stream, which is 1 when a
// header frame precedes it: the padding cycle is a property of the stream, so
// the walk has to enter it at the same point the layout will.
func (h Header) SmallestCBRBitrate(payloads []int, from int) int {
	rates := bitrateTable[h.Version]
	overhead := h.overhead()
	maxBack := h.MaxMainDataBegin()
	for idx := 1; idx <= MaxBitrateIndex; idx++ {
		base := unpaddedFrameSize(h.Version, h.SampleRate, rates[idx]) - overhead
		banked, ok := 0, true
		for i, need := range payloads {
			// Reservoir carried into this frame, capped by the back-reference the
			// side info can express; anything above that is unreachable.
			banked = min(banked, maxBack)
			size := base
			if Padded(h.Version, h.SampleRate, rates[idx], from+i) {
				size++
			}
			banked += size - need
			if banked < 0 {
				ok = false
				break
			}
		}
		if ok {
			return rates[idx]
		}
	}
	return 0
}
