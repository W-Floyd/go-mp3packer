package mp3

import "errors"

// Frame is one parsed MPEG audio frame.
type Frame struct {
	Offset      int // byte offset of the frame within the input
	Header      Header
	CRCBytes    [2]byte // valid only when Header.CRC
	SideInfoRaw []byte
	SideInfo    SideInfo
	MainData    []byte // the frame's main-data slots, not necessarily its own audio
}

// Size is the frame's length in bytes as it appeared in the input.
func (f Frame) Size() int { return f.Header.FrameSize() }

// File is a parsed MP3 file: a frame sequence plus whatever non-audio bytes
// bracket it.
type File struct {
	StartJunk  []byte // everything before the first frame (ID3v2 tags, garbage)
	EndJunk    []byte // everything after the last frame (ID3v1, APE, garbage)
	Frames     []Frame
	SyncErrors int // times a frame did not directly follow the previous one
}

// ErrNoFrames is returned when a file contains no decodable layer III frames.
var ErrNoFrames = errors.New("mp3: no valid MPEG layer III frames found")

// chainLength counts how many consecutive valid frames start at off, up to
// want. Requiring a short chain avoids locking onto a false sync word inside a
// tag or inside the audio payload itself.
func chainLength(data []byte, off, want int) int {
	var first Header
	for n := 0; n < want; n++ {
		h, ok := ParseHeader(data[min(off, len(data)):])
		if !ok {
			return n
		}
		if n == 0 {
			first = h
		} else if !first.SameStream(h) {
			return n
		}
		size := h.FrameSize()
		if size < 4+h.crcSize()+h.SideInfoSize() {
			return n
		}
		if off+size > len(data) {
			// A truncated final frame still counts as a sync point; the caller
			// decides whether to keep it.
			return n + 1
		}
		off += size
	}
	return want
}

// findSync scans forward from off for a position that begins a plausible run of
// frames, returning -1 if there is none.
func findSync(data []byte, off int) int {
	for p := max(off, 0); p+4 <= len(data); p++ {
		if data[p] != 0xFF || data[p+1]&0xE0 != 0xE0 {
			continue
		}
		want := 3
		if p+4 >= len(data)-64 {
			want = 1 // near EOF there is no room for a chain
		}
		if chainLength(data, p, want) >= want {
			return p
		}
	}
	return -1
}

// id3v2Size returns the total length of an ID3v2 tag at the start of data, or 0.
func id3v2Size(data []byte) int {
	if len(data) < 10 || string(data[0:3]) != "ID3" {
		return 0
	}
	for _, b := range data[6:10] {
		if b&0x80 != 0 {
			return 0 // not a valid syncsafe integer
		}
	}
	size := int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
	total := 10 + size
	if data[5]&0x10 != 0 {
		total += 10 // footer
	}
	if total > len(data) {
		return 0
	}
	return total
}

// Parse reads every frame in data. Non-audio bytes before the first frame and
// after the last are preserved; junk between frames is dropped but counted as a
// sync error, which is what the original mp3packer does.
func Parse(data []byte) (*File, error) {
	f := &File{}
	start := findSync(data, id3v2Size(data))
	if start < 0 {
		return nil, ErrNoFrames
	}
	f.StartJunk = data[:start]

	// A Frame is over a kilobyte, so growing the slice by append alone copies
	// megabytes for a file of any length. Every frame in a stream shares a
	// sample rate and hence a duration, so the first frame's size is a good
	// estimate of the rest: exact for CBR, and for VBR close enough that the
	// slice grows once rather than a dozen times.
	if h, ok := ParseHeader(data[start:]); ok {
		if size := h.FrameSize(); size > 0 {
			f.Frames = make([]Frame, 0, (len(data)-start)/size+1)
		}
	}

	pos := start
	for pos < len(data) {
		h, ok := ParseHeader(data[pos:])
		if !ok || pos+h.FrameSize() > len(data) {
			next := findSync(data, pos+1)
			if next < 0 {
				break
			}
			f.SyncErrors++
			pos = next
			continue
		}
		size := h.FrameSize()
		sideSize := h.SideInfoSize()
		off := pos + 4
		fr := Frame{Offset: pos, Header: h}
		if h.CRC {
			fr.CRCBytes = [2]byte{data[off], data[off+1]}
			off += 2
		}
		fr.SideInfoRaw = data[off : off+sideSize]
		fr.SideInfo = ParseSideInfo(h, fr.SideInfoRaw)
		fr.MainData = data[off+sideSize : pos+size]
		f.Frames = append(f.Frames, fr)
		pos += size
	}
	if len(f.Frames) == 0 {
		return nil, ErrNoFrames
	}
	f.EndJunk = data[pos:]
	return f, nil
}

// MainDataBytes is the byte length of the frame's own audio data once its
// granules are packed together, which is how much space the reservoir must find
// for it.
func (f Frame) MainDataBytes() int {
	return (f.MainDataBits() + 7) / 8
}

// MainDataBits is the total part2_3_length across the frame's granules.
func (f Frame) MainDataBits() int {
	bits := 0
	for gr := 0; gr < f.Header.Granules(); gr++ {
		for ch := 0; ch < f.Header.Channels(); ch++ {
			bits += f.SideInfo.Gr[gr][ch].Part23Length
		}
	}
	return bits
}

// CRCValid reports whether a protected frame's stored CRC matches its contents.
func (f Frame) CRCValid() bool {
	if !f.Header.CRC {
		return true
	}
	want := uint16(f.CRCBytes[0])<<8 | uint16(f.CRCBytes[1])
	return FrameCRC(f.Header.Bytes(), f.SideInfoRaw) == want
}
