package mp3

import "encoding/binary"

// Xing tag flag bits.
const (
	xingFlagFrames  = 1
	xingFlagBytes   = 2
	xingFlagTOC     = 4
	xingFlagQuality = 8
)

// InfoTag describes a Xing/Info (or VBRI) header frame. Such a frame carries no
// audio, only stream metadata, and its byte count and seek table go stale as
// soon as the file is repacked.
type InfoTag struct {
	Kind      string // "Xing", "Info" or "VBRI"
	MagicAt   int    // offset of the magic word from the start of the frame
	Flags     uint32
	BytesAt   int // offset of the 32-bit stream byte count, or -1
	TOCAt     int // offset of the 100-byte seek table, or -1
	LameCRCAt int // offset of the Info tag's own CRC field, or -1 if absent/invalid
}

// FindInfoTag looks for a Xing/Info/VBRI header in a frame and locates the
// fields that a repack invalidates. frame must be the frame's complete bytes.
func FindInfoTag(frame []byte, h Header) *InfoTag {
	// The magic word sits at the start of the main data for Xing/Info and at a
	// fixed offset for VBRI, but encoders disagree about the presence of a CRC,
	// so search the region where it can legally appear.
	limit := min(len(frame), 4+2+h.SideInfoSize()+40)
	for at := 4; at+4 <= limit; at++ {
		kind := string(frame[at : at+4])
		if kind != "Xing" && kind != "Info" && kind != "VBRI" {
			continue
		}
		tag := &InfoTag{Kind: kind, MagicAt: at, BytesAt: -1, TOCAt: -1, LameCRCAt: -1}
		if kind == "VBRI" {
			// VBRI is Fraunhofer's format: version(2) delay(2) quality(2)
			// bytes(4) frames(4) at fixed offsets after the magic.
			if at+14 <= len(frame) {
				tag.BytesAt = at + 10
			}
			tag.LameCRCAt = lameTagCRCOffset(frame)
			return tag
		}
		if at+8 > len(frame) {
			return tag
		}
		tag.Flags = binary.BigEndian.Uint32(frame[at+4:])
		off := at + 8
		if tag.Flags&xingFlagFrames != 0 {
			off += 4
		}
		if tag.Flags&xingFlagBytes != 0 {
			if off+4 <= len(frame) {
				tag.BytesAt = off
			}
			off += 4
		}
		if tag.Flags&xingFlagTOC != 0 {
			if off+100 <= len(frame) {
				tag.TOCAt = off
			}
			off += 100
		}
		tag.LameCRCAt = lameTagCRCOffset(frame)
		return tag
	}
	return nil
}

// lameTagCRCOffset returns the offset of the LAME extension's checksum field if
// the frame carries one that currently validates, else -1. Only a checksum we
// can confirm is worth rewriting: a wrong one is worse than an absent one.
func lameTagCRCOffset(frame []byte) int {
	const crcAt = 190 // LAME tag CRC covers the first 190 bytes of the frame
	if len(frame) < crcAt+2 {
		return -1
	}
	if binary.BigEndian.Uint16(frame[crcAt:]) != lameCRC16(frame[:crcAt]) {
		return -1
	}
	return crcAt
}

// Repair updates a header frame in place after the stream around it has been
// rewritten. streamBytes is the total size of the MP3 stream (first frame
// through last, tags excluded) and framePos lists the offset of every audio
// frame relative to the start of this header frame.
func (t *InfoTag) Repair(frame []byte, streamBytes int, framePos []int) {
	if t.BytesAt >= 0 {
		binary.BigEndian.PutUint32(frame[t.BytesAt:], uint32(streamBytes))
	}
	if t.TOCAt >= 0 && len(framePos) > 0 && streamBytes > 0 {
		toc := frame[t.TOCAt : t.TOCAt+100]
		toc[0] = 0
		for i := 1; i < 100; i++ {
			fi := i * len(framePos) / 100
			if fi >= len(framePos) {
				fi = len(framePos) - 1
			}
			v := 256 * framePos[fi] / streamBytes
			toc[i] = byte(min(v, 255))
		}
	}
	if t.LameCRCAt >= 0 {
		binary.BigEndian.PutUint16(frame[t.LameCRCAt:], lameCRC16(frame[:t.LameCRCAt]))
	}
}
