package mp3

import (
	"os"
	"path/filepath"
	"testing"
)

func testFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../testdata/*.mp3")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files: %v", err)
	}
	return files
}

func TestHeaderRoundTrip(t *testing.T) {
	// Every bit combination that parses must serialize back to itself.
	for hi := int64(0xFFE00000); hi <= 0xFFFFFFFF; hi += 0x400 {
		for _, low := range []int64{0, 0x3FF, 0x155, 0x2AA} {
			raw := uint32(hi | low)
			b := []byte{byte(raw >> 24), byte(raw >> 16), byte(raw >> 8), byte(raw)}
			h, ok := ParseHeader(b)
			if !ok {
				continue
			}
			got := h.Bytes()
			if got != [4]byte{b[0], b[1], b[2], b[3]} {
				t.Fatalf("%08x round-tripped to %08x", raw, got)
			}
			if h.FrameSize() <= 4+h.SideInfoSize() {
				t.Fatalf("%08x: frame size %d cannot hold its own side info", raw, h.FrameSize())
			}
		}
	}
}

func TestParseRealFiles(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Frames) < 10 {
				t.Fatalf("only %d frames parsed", len(f.Frames))
			}
			if f.SyncErrors != 0 {
				t.Errorf("%d sync errors in a file written by an encoder", f.SyncErrors)
			}
			// The frames must tile the file exactly, with junk only at the ends.
			pos := len(f.StartJunk)
			for i, fr := range f.Frames {
				if fr.Offset != pos {
					t.Fatalf("frame %d starts at %d, expected %d", i, fr.Offset, pos)
				}
				pos += fr.Size()
			}
			if pos+len(f.EndJunk) != len(data) {
				t.Fatalf("frames plus junk cover %d of %d bytes", pos+len(f.EndJunk), len(data))
			}
		})
	}
}

func TestSideInfoRoundTrip(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			for i, fr := range f.Frames {
				out := fr.SideInfo.Serialize(fr.Header)
				if len(out) != len(fr.SideInfoRaw) {
					t.Fatalf("frame %d: serialized %d bytes, want %d", i, len(out), len(fr.SideInfoRaw))
				}
				for b := range out {
					if out[b] != fr.SideInfoRaw[b] {
						t.Fatalf("frame %d: side info byte %d is %#02x, want %#02x",
							i, b, out[b], fr.SideInfoRaw[b])
					}
				}
			}
		})
	}
}

func TestScalefactorBitsFitInGranule(t *testing.T) {
	for _, path := range testFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for i, fr := range f.Frames {
				for gr := 0; gr < fr.Header.Granules(); gr++ {
					for ch := 0; ch < fr.Header.Channels(); ch++ {
						p23 := fr.SideInfo.Gr[gr][ch].Part23Length
						if p23 == 0 {
							continue // header frames and silent granules
						}
						sf := ScalefactorBits(fr.Header, fr.SideInfo, gr, ch)
						if sf > p23 {
							t.Fatalf("frame %d granule %d/%d: %d scalefactor bits exceed part2_3_length %d",
								i, gr, ch, sf, p23)
						}
						checked++
					}
				}
			}
			if checked == 0 {
				t.Fatal("no granules checked")
			}
		})
	}
}

func TestFrameCRCMatchesEncoder(t *testing.T) {
	data, err := os.ReadFile("../testdata/cbr-crc.mp3")
	if err != nil {
		t.Skip("no CRC-protected test file")
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	protected := 0
	for i, fr := range f.Frames {
		if !fr.Header.CRC {
			continue
		}
		protected++
		if !fr.CRCValid() {
			t.Fatalf("frame %d: computed CRC does not match the one the encoder stored", i)
		}
	}
	if protected == 0 {
		t.Fatal("expected CRC-protected frames")
	}
}

func TestFindInfoTag(t *testing.T) {
	data, err := os.ReadFile("../testdata/vbr-joint.mp3")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	first := f.Frames[0]
	tag := FindInfoTag(data[first.Offset:first.Offset+first.Size()], first.Header)
	if tag == nil {
		t.Fatal("no Xing header found in a LAME VBR file")
	}
	if tag.Kind != "Xing" {
		t.Errorf("kind = %q, want Xing", tag.Kind)
	}
	if tag.BytesAt < 0 || tag.TOCAt < 0 {
		t.Errorf("expected byte count and seek table, got %+v", tag)
	}
	if tag.LameCRCAt < 0 {
		t.Error("expected a valid LAME tag checksum")
	}
	if first.MainDataBits() != 0 {
		t.Errorf("a header frame should carry no audio, got %d bits", first.MainDataBits())
	}
}
