package mp3

// crcTable[b] is the frame CRC of the single byte b from a zero register, which
// is what lets the whole byte be folded in at once rather than a bit at a time.
var crcTable [256]uint16

// crcSlice[k][b] is the CRC of byte b followed by k zero bytes, so that four
// bytes can be folded in by four independent lookups instead of four dependent
// ones. crcSlice[0] is crcTable.
//
// Byte at a time, each step needs the previous step's crc to index the table, so
// the loop runs at the latency of a load plus a shift and an xor however wide the
// machine is. Slicing breaks that chain: the four lookups depend only on the
// state as it was four bytes ago and on the input, so they issue together.
var crcSlice [4][256]uint16

func init() {
	for i := range crcTable {
		crc := uint16(i) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
		crcTable[i] = crc
	}
	crcSlice[0] = crcTable
	for k := 1; k < len(crcSlice); k++ {
		for b := range crcSlice[k] {
			// Advance the previous row by one zero byte.
			c := crcSlice[k-1][b]
			crcSlice[k][b] = c<<8 ^ crcTable[c>>8]
		}
	}
}

// CRC16 computes the MPEG audio frame CRC: generator polynomial 0x8005
// (x^16 + x^15 + x^2 + 1), initial value 0xFFFF, MSB first, no final xor.
//
// A protected frame's CRC covers its side info, so this runs over about thirty
// bytes per frame — bit at a time that was the single largest cost of laying a
// file back out, and the layout pass is serial, so it bounded what every worker
// could add up to. TestCRC16MatchesBitwise holds the table to the definition.
func CRC16(chunks ...[]byte) uint16 {
	crc := uint16(0xFFFF)
	for _, chunk := range chunks {
		// The register is only sixteen bits wide, so it is fully consumed after
		// two bytes: the third and fourth bytes of a group index their tables by
		// themselves, and only the first two are mixed with the state.
		for len(chunk) >= 4 {
			// The parentheses are not decoration: | and ^ share a precedence
			// level in Go, so without them the low byte is or-ed into the state
			// instead of xor-ed, which is wrong exactly when the state's low
			// byte is not zero.
			u := crc ^ (uint16(chunk[0])<<8 | uint16(chunk[1]))
			crc = crcSlice[3][u>>8] ^ crcSlice[2][u&0xFF] ^
				crcSlice[1][chunk[2]] ^ crcSlice[0][chunk[3]]
			chunk = chunk[4:]
		}
		for _, b := range chunk {
			crc = crc<<8 ^ crcTable[byte(crc>>8)^b]
		}
	}
	return crc
}

// FrameCRC computes the CRC stored between the header and the side info of a
// protected frame. It covers the last two header bytes and the whole side
// information block; the main data is not protected.
func FrameCRC(header [4]byte, sideInfo []byte) uint16 {
	return CRC16(header[2:4], sideInfo)
}

// lameCRC16 is the reflected CRC-16 (polynomial 0xA001, initial value 0) used
// for the Info/Xing tag's own checksum field. Unrelated to the frame CRC above.
func lameCRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
