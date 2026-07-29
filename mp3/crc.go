package mp3

// crcTable[b] is the frame CRC of the single byte b from a zero register, which
// is what lets the whole byte be folded in at once rather than a bit at a time.
var crcTable [256]uint16

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
