package mp3

import "github.com/W-Floyd/go-mp3packer/internal/bitio"

// GranuleInfo is the per-granule, per-channel part of the side information.
type GranuleInfo struct {
	Part23Length      int // scalefactors + Huffman data, in bits
	BigValues         int // number of coefficient pairs coded with the big-value tables
	GlobalGain        int
	ScalefacCompress  int
	WindowSwitching   bool
	BlockType         int
	MixedBlock        bool
	TableSelect       [3]int
	SubblockGain      [3]int
	Region0Count      int
	Region1Count      int
	Preflag           bool // MPEG-1 only
	ScalefacScale     bool
	Count1TableSelect bool
}

// SideInfo is the decoded side information block that follows the header.
type SideInfo struct {
	MainDataBegin int // bytes to step back into the bit reservoir
	PrivateBits   uint32
	SCFSI         [2][4]bool // MPEG-1 only
	Gr            [2][2]GranuleInfo
}

func (h Header) privateBitCount() int {
	if h.Version == MPEG1 {
		if h.Mode == Mono {
			return 5
		}
		return 3
	}
	if h.Mode == Mono {
		return 1
	}
	return 2
}

func (h Header) mainDataBeginBits() int {
	if h.Version == MPEG1 {
		return 9
	}
	return 8
}

func (h Header) scalefacCompressBits() int {
	if h.Version == MPEG1 {
		return 4
	}
	return 9
}

// ParseSideInfo decodes a side information block. raw must be exactly
// h.SideInfoSize() bytes long; the field layout is fully determined by the
// header, so parsing cannot fail.
func ParseSideInfo(h Header, raw []byte) SideInfo {
	r := bitio.NewReader(raw)
	var si SideInfo
	si.MainDataBegin = int(r.Read(h.mainDataBeginBits()))
	si.PrivateBits = r.Read(h.privateBitCount())
	if h.Version == MPEG1 {
		for ch := 0; ch < h.Channels(); ch++ {
			for band := 0; band < 4; band++ {
				si.SCFSI[ch][band] = r.Read(1) != 0
			}
		}
	}
	for gr := 0; gr < h.Granules(); gr++ {
		for ch := 0; ch < h.Channels(); ch++ {
			g := &si.Gr[gr][ch]
			g.Part23Length = int(r.Read(12))
			g.BigValues = int(r.Read(9))
			g.GlobalGain = int(r.Read(8))
			g.ScalefacCompress = int(r.Read(h.scalefacCompressBits()))
			g.WindowSwitching = r.Read(1) != 0
			if g.WindowSwitching {
				g.BlockType = int(r.Read(2))
				g.MixedBlock = r.Read(1) != 0
				g.TableSelect[0] = int(r.Read(5))
				g.TableSelect[1] = int(r.Read(5))
				for i := range g.SubblockGain {
					g.SubblockGain[i] = int(r.Read(3))
				}
				// region counts are implied for window-switched granules
				if g.BlockType == 2 && !g.MixedBlock {
					g.Region0Count = 8
				} else {
					g.Region0Count = 7
				}
				g.Region1Count = 20 - g.Region0Count
			} else {
				for i := range g.TableSelect {
					g.TableSelect[i] = int(r.Read(5))
				}
				g.Region0Count = int(r.Read(4))
				g.Region1Count = int(r.Read(3))
			}
			if h.Version == MPEG1 {
				g.Preflag = r.Read(1) != 0
			}
			g.ScalefacScale = r.Read(1) != 0
			g.Count1TableSelect = r.Read(1) != 0
		}
	}
	return si
}

// Serialize writes the side information back out. The result is always exactly
// h.SideInfoSize() bytes; the standard field layout fills the block exactly, so
// Serialize(Parse(x)) == x for any well-formed x.
func (si SideInfo) Serialize(h Header) []byte {
	w := bitio.NewWriterSize(h.SideInfoSize())
	w.Write(uint32(si.MainDataBegin), h.mainDataBeginBits())
	w.Write(si.PrivateBits, h.privateBitCount())
	if h.Version == MPEG1 {
		for ch := 0; ch < h.Channels(); ch++ {
			for band := 0; band < 4; band++ {
				w.Write(boolBit(si.SCFSI[ch][band]), 1)
			}
		}
	}
	for gr := 0; gr < h.Granules(); gr++ {
		for ch := 0; ch < h.Channels(); ch++ {
			g := si.Gr[gr][ch]
			w.Write(uint32(g.Part23Length), 12)
			w.Write(uint32(g.BigValues), 9)
			w.Write(uint32(g.GlobalGain), 8)
			w.Write(uint32(g.ScalefacCompress), h.scalefacCompressBits())
			w.Write(boolBit(g.WindowSwitching), 1)
			if g.WindowSwitching {
				w.Write(uint32(g.BlockType), 2)
				w.Write(boolBit(g.MixedBlock), 1)
				w.Write(uint32(g.TableSelect[0]), 5)
				w.Write(uint32(g.TableSelect[1]), 5)
				for _, sg := range g.SubblockGain {
					w.Write(uint32(sg), 3)
				}
			} else {
				for _, t := range g.TableSelect {
					w.Write(uint32(t), 5)
				}
				w.Write(uint32(g.Region0Count), 4)
				w.Write(uint32(g.Region1Count), 3)
			}
			if h.Version == MPEG1 {
				w.Write(boolBit(g.Preflag), 1)
			}
			w.Write(boolBit(g.ScalefacScale), 1)
			w.Write(boolBit(g.Count1TableSelect), 1)
		}
	}
	out := w.Bytes()
	for len(out) < h.SideInfoSize() {
		out = append(out, 0)
	}
	return out[:h.SideInfoSize()]
}

// PatchMainDataBegin returns a copy of a raw side info block with only the
// main_data_begin field replaced. Preferred over Serialize for frames we did
// not otherwise modify, since it cannot perturb any other bit.
func PatchMainDataBegin(h Header, raw []byte, mdb int) []byte {
	out := make([]byte, len(raw))
	copy(out, raw)
	if h.Version == MPEG1 {
		out[0] = byte(mdb >> 1)
		out[1] = out[1]&0x7F | byte(mdb&1)<<7
	} else {
		out[0] = byte(mdb)
	}
	return out
}

// MPEG-1 scalefactor lengths, indexed by scalefac_compress (ISO 11172-3 table
// B.4 / 2.4.2.7).
var (
	mpeg1Slen1 = [16]int{0, 0, 0, 0, 3, 1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4}
	mpeg1Slen2 = [16]int{0, 1, 2, 3, 0, 1, 2, 3, 1, 2, 3, 1, 2, 3, 2, 3}
)

// ScalefactorBits returns part2_length: how many bits of the granule's
// part2_3_length are scalefactors rather than Huffman-coded spectrum. The
// recompressor needs the split point but never the scalefactor values, which it
// copies through bit for bit.
func ScalefactorBits(h Header, si SideInfo, gr, ch int) int {
	g := si.Gr[gr][ch]
	if h.Version != MPEG1 {
		return lsfScalefactorBits(h, g, ch)
	}
	slen1, slen2 := mpeg1Slen1[g.ScalefacCompress], mpeg1Slen2[g.ScalefacCompress]
	if g.WindowSwitching && g.BlockType == 2 {
		if g.MixedBlock {
			// 8 long bands plus short bands 3..5 at slen1, short bands 6..11 at slen2
			return 17*slen1 + 18*slen2
		}
		return 18*slen1 + 18*slen2
	}
	// long blocks: four scalefactor groups of 6, 5, 5 and 5 bands, any of which
	// the second granule may inherit from the first via scfsi
	bits := 0
	counts := [4]int{6, 5, 5, 5}
	slens := [4]int{slen1, slen1, slen2, slen2}
	for grp := 0; grp < 4; grp++ {
		if gr == 1 && si.SCFSI[ch][grp] {
			continue
		}
		bits += counts[grp] * slens[grp]
	}
	return bits
}

// MPEG-2 LSF scalefactor band group counts, indexed by [block category][slen
// set][group] (ISO 13818-3 table B.1). Block category is 0 for long blocks,
// 1 for pure short blocks and 2 for mixed blocks.
var lsfBandCounts = [3][6][4]int{
	{{6, 5, 5, 5}, {6, 5, 7, 3}, {11, 10, 0, 0}, {7, 7, 7, 0}, {6, 6, 6, 3}, {8, 8, 5, 0}},
	{{9, 9, 9, 9}, {9, 9, 12, 6}, {18, 18, 0, 0}, {12, 12, 12, 0}, {12, 9, 9, 6}, {15, 12, 9, 0}},
	{{6, 9, 9, 9}, {6, 9, 12, 6}, {15, 18, 0, 0}, {6, 15, 12, 0}, {6, 12, 9, 6}, {6, 15, 9, 0}},
}

// The LSF scalefac_compress field is a compound index: it selects both a group
// of scalefactor band counts and the bit length used within each group. Both
// mappings are generated rather than tabulated, exactly as the decomposition is
// specified in ISO 13818-3 2.4.3.2. Each entry packs four 3-bit lengths in bits
// 0..11 and the band-count set in bits 12..14.
var (
	lsfNormalSlen    [512]uint32
	lsfIntensitySlen [256]uint32
)

func init() {
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			for k := 0; k < 4; k++ {
				for l := 0; l < 4; l++ {
					n := l + k*4 + j*16 + i*80
					lsfNormalSlen[n] = uint32(i | j<<3 | k<<6 | l<<9 | 0<<12)
				}
			}
		}
	}
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			for k := 0; k < 4; k++ {
				n := k + j*4 + i*20
				lsfNormalSlen[n+400] = uint32(i | j<<3 | k<<6 | 1<<12)
			}
		}
	}
	for i := 0; i < 5; i++ {
		for j := 0; j < 6; j++ {
			for k := 0; k < 6; k++ {
				n := k + j*6 + i*36
				lsfIntensitySlen[n] = uint32(i | j<<3 | k<<6 | 2<<12)
			}
		}
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			for k := 0; k < 4; k++ {
				n := k + j*4 + i*16
				lsfIntensitySlen[n+180] = uint32(i | j<<3 | k<<6 | 3<<12)
			}
		}
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 3; j++ {
			n := j + i*3
			lsfIntensitySlen[n+244] = uint32(i | j<<3 | 4<<12)
			lsfNormalSlen[n+500] = uint32(i | j<<3 | 1<<12 | 5<<15)
		}
	}
}

func lsfScalefactorBits(h Header, g GranuleInfo, ch int) int {
	var slen uint32
	if h.IntensityStereo() && ch == 1 {
		slen = lsfIntensitySlen[(g.ScalefacCompress>>1)&0xFF]
	} else {
		slen = lsfNormalSlen[g.ScalefacCompress&0x1FF]
	}
	category := 0
	if g.WindowSwitching && g.BlockType == 2 {
		category = 1
		if g.MixedBlock {
			category = 2
		}
	}
	counts := lsfBandCounts[category][(slen>>12)&7]
	bits := 0
	for i := 0; i < 4; i++ {
		bits += counts[i] * int(slen&7)
		slen >>= 3
	}
	return bits
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
