// Package mp3 parses and re-serializes the structural parts of an MPEG-1/2/2.5
// layer III bitstream: frame headers, side information and the CRC. It
// deliberately knows nothing about the audio itself.
package mp3

import "fmt"

type Version uint8

const (
	MPEG1  Version = iota
	MPEG2          // MPEG-2 LSF
	MPEG25         // MPEG-2.5 (unofficial extension)
)

func (v Version) String() string {
	switch v {
	case MPEG1:
		return "MPEG1"
	case MPEG2:
		return "MPEG2"
	default:
		return "MPEG2.5"
	}
}

type ChannelMode uint8

const (
	Stereo ChannelMode = iota
	JointStereo
	DualChannel
	Mono
)

func (c ChannelMode) String() string {
	switch c {
	case Stereo:
		return "Stereo"
	case JointStereo:
		return "JointStereo"
	case DualChannel:
		return "DualChannel"
	default:
		return "Mono"
	}
}

var bitrateTable = [3][16]int{
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}, // MPEG1
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},     // MPEG2
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},     // MPEG2.5
}

var sampleRateTable = [3][3]int{
	{44100, 48000, 32000},
	{22050, 24000, 16000},
	{11025, 12000, 8000},
}

// MaxBitrateIndex is the highest usable bitrate index (15 is "reserved").
const MaxBitrateIndex = 14

// Header is a decoded 32-bit MPEG audio frame header.
type Header struct {
	Version      Version
	CRC          bool // protection_bit clear: a 16-bit CRC follows the header
	BitrateIndex int
	SampleRate   int
	Padding      bool
	Private      bool
	Mode         ChannelMode
	ModeExt      uint8 // joint stereo: bit 1 = MS stereo, bit 0 = intensity stereo
	Copyright    bool
	Original     bool
	Emphasis     uint8
}

// ParseHeader decodes a header from the first four bytes of b. It rejects
// anything that is not a layer III frame with a usable bitrate and samplerate.
func ParseHeader(b []byte) (Header, bool) {
	var h Header
	if len(b) < 4 {
		return h, false
	}
	w := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if w&0xFFE00000 != 0xFFE00000 {
		return h, false
	}
	switch (w >> 19) & 3 {
	case 0:
		h.Version = MPEG25
	case 2:
		h.Version = MPEG2
	case 3:
		h.Version = MPEG1
	default: // 1 is reserved
		return h, false
	}
	if (w>>17)&3 != 1 { // layer III only
		return h, false
	}
	h.CRC = (w>>16)&1 == 0
	h.BitrateIndex = int((w >> 12) & 0xF)
	if h.BitrateIndex == 0 || h.BitrateIndex > MaxBitrateIndex {
		return h, false // 0 is "free format", 15 is reserved
	}
	srIdx := int((w >> 10) & 3)
	if srIdx == 3 {
		return h, false
	}
	h.SampleRate = sampleRateTable[h.Version][srIdx]
	h.Padding = (w>>9)&1 != 0
	h.Private = (w>>8)&1 != 0
	h.Mode = ChannelMode((w >> 6) & 3)
	h.ModeExt = uint8((w >> 4) & 3) // only meaningful for joint stereo, but preserved regardless
	h.Copyright = (w>>3)&1 != 0
	h.Original = (w>>2)&1 != 0
	h.Emphasis = uint8(w & 3)
	return h, true
}

// Bytes re-serializes the header.
func (h Header) Bytes() [4]byte {
	var w uint32 = 0xFFE00000
	switch h.Version {
	case MPEG1:
		w |= 3 << 19
	case MPEG2:
		w |= 2 << 19
	}
	w |= 1 << 17 // layer III
	if !h.CRC {
		w |= 1 << 16
	}
	w |= uint32(h.BitrateIndex) << 12
	w |= uint32(h.sampleRateIndex()) << 10
	if h.Padding {
		w |= 1 << 9
	}
	if h.Private {
		w |= 1 << 8
	}
	w |= uint32(h.Mode) << 6
	w |= uint32(h.ModeExt) << 4
	if h.Copyright {
		w |= 1 << 3
	}
	if h.Original {
		w |= 1 << 2
	}
	w |= uint32(h.Emphasis)
	return [4]byte{byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w)}
}

func (h Header) sampleRateIndex() int {
	for i, sr := range sampleRateTable[h.Version] {
		if sr == h.SampleRate {
			return i
		}
	}
	return 0
}

func (h Header) Bitrate() int { return bitrateTable[h.Version][h.BitrateIndex] }

// Channels is 1 for mono, 2 otherwise.
func (h Header) Channels() int {
	if h.Mode == Mono {
		return 1
	}
	return 2
}

// Granules is 2 for MPEG-1 and 1 for the low sampling frequency extensions.
func (h Header) Granules() int {
	if h.Version == MPEG1 {
		return 2
	}
	return 1
}

// IntensityStereo reports whether the second channel's scalefactors use the
// intensity-stereo table set (MPEG-2 LSF only).
func (h Header) IntensityStereo() bool {
	return h.Mode == JointStereo && h.ModeExt&1 != 0
}

// SideInfoSize is the fixed side information length in bytes.
func (h Header) SideInfoSize() int {
	if h.Version == MPEG1 {
		if h.Mode == Mono {
			return 17
		}
		return 32
	}
	if h.Mode == Mono {
		return 9
	}
	return 17
}

func (h Header) crcSize() int {
	if h.CRC {
		return 2
	}
	return 0
}

// FrameSize is the total on-disk size of the frame in bytes, padding included.
func (h Header) FrameSize() int {
	return unpaddedFrameSize(h.Version, h.SampleRate, h.Bitrate()) + boolInt(h.Padding)
}

// DataSize is the number of main-data bytes the frame can carry.
func (h Header) DataSize() int {
	return h.FrameSize() - 4 - h.crcSize() - h.SideInfoSize()
}

// MaxMainDataBegin is the largest back-reference into the bit reservoir the
// side info can express: 9 bits for MPEG-1, 8 bits for the LSF versions.
func (h Header) MaxMainDataBegin() int {
	if h.Version == MPEG1 {
		return 511
	}
	return 255
}

func unpaddedFrameSize(v Version, sampleRate, bitrateKbps int) int {
	mult := 72000
	if v == MPEG1 {
		mult = 144000
	}
	return mult * bitrateKbps / sampleRate
}

// Capacity describes one candidate frame size: a bitrate index plus the
// padding bit, and how many main-data bytes that combination carries.
type Capacity struct {
	Index    int
	Padding  bool
	DataSize int
}

// Capacities lists every frame size available to a frame with this header's
// version, samplerate, channel mode and CRC setting, in ascending order of
// data capacity (each bitrate unpadded, then padded).
func (h Header) Capacities() []Capacity {
	overhead := h.overhead()
	caps := make([]Capacity, 0, 2*MaxBitrateIndex)
	for idx := 1; idx <= MaxBitrateIndex; idx++ {
		base := unpaddedFrameSize(h.Version, h.SampleRate, bitrateTable[h.Version][idx]) - overhead
		caps = append(caps,
			Capacity{Index: idx, Padding: false, DataSize: base},
			Capacity{Index: idx, Padding: true, DataSize: base + 1},
		)
	}
	return caps
}

func (h Header) overhead() int { return 4 + h.crcSize() + h.SideInfoSize() }

// MaxDataSize is the data capacity of the largest frame this header can
// describe, which is the last entry Capacities would return.
func (h Header) MaxDataSize() int {
	return unpaddedFrameSize(h.Version, h.SampleRate, bitrateTable[h.Version][MaxBitrateIndex]) -
		h.overhead() + 1
}

// SmallestCapacity is the cheapest frame size that can carry want bytes of main
// data, or the largest available if none can. It answers the same question as
// scanning Capacities without building the slice, which matters because the
// layout pass asks once per frame.
func (h Header) SmallestCapacity(want int) Capacity {
	overhead := h.overhead()
	last := Capacity{}
	for idx := 1; idx <= MaxBitrateIndex; idx++ {
		base := unpaddedFrameSize(h.Version, h.SampleRate, bitrateTable[h.Version][idx]) - overhead
		if base >= want {
			return Capacity{Index: idx, Padding: false, DataSize: base}
		}
		if base+1 >= want {
			return Capacity{Index: idx, Padding: true, DataSize: base + 1}
		}
		last = Capacity{Index: idx, Padding: true, DataSize: base + 1}
	}
	return last
}

// SamplesPerFrame is 1152 for MPEG-1 and 576 for the LSF versions.
func (h Header) SamplesPerFrame() int { return 576 * h.Granules() }

// SameStream reports whether two headers can belong to the same logical
// stream. Following mp3packer, per-frame changes of CRC, copyright, emphasis
// and the stereo flavour are tolerated; version, samplerate and channel count
// are not.
func (h Header) SameStream(o Header) bool {
	return h.Version == o.Version && h.SampleRate == o.SampleRate && h.Channels() == o.Channels()
}

func (h Header) String() string {
	return fmt.Sprintf("%s layer3 %dkbps %dHz %s", h.Version, h.Bitrate(), h.SampleRate, h.Mode)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
