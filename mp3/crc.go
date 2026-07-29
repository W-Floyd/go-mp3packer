package mp3

// CRC16 computes the MPEG audio frame CRC: generator polynomial 0x8005
// (x^16 + x^15 + x^2 + 1), initial value 0xFFFF, MSB first, no final xor.
func CRC16(chunks ...[]byte) uint16 {
	crc := uint16(0xFFFF)
	for _, chunk := range chunks {
		for _, b := range chunk {
			for i := 7; i >= 0; i-- {
				bit := uint16(b>>uint(i)) & 1
				if crc>>15 != bit {
					crc = crc<<1 ^ 0x8005
				} else {
					crc <<= 1
				}
			}
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
