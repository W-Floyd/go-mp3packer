package mp3packer

import (
	"fmt"

	"github.com/W-Floyd/go-mp3packer/internal/bitio"
	"github.com/W-Floyd/go-mp3packer/mp3"
)

// ErrNoConstantBitrate means no constant bitrate can carry the audio, which
// takes a frame needing more than a maximum-bitrate frame and a full reservoir
// together. Such a file cannot be laid out at all, constant bitrate or not.
var ErrNoConstantBitrate = fmt.Errorf("mp3packer: no constant bitrate can carry this audio")

// SmallestCBRBitrate reports the lowest constant bitrate, in kbps, that data can
// be repacked to under opt — the number to hand back as [Options.MinBitrate] to
// get the smallest constant-bitrate file this input allows.
//
// opt.Recompress matters: the Huffman search shrinks the payload, and a smaller
// payload can fit a lower bitrate, so the answer is only valid for a repack that
// makes the same choice. It is measured, not estimated — with Recompress set,
// this runs the search.
//
// This deliberately walks the input again rather than sharing Process's prepare
// stage: that stage is the serial floor of a repack and the subject of its own
// tuning, and it is not worth slowing down for a diagnostic that runs once.
func SmallestCBRBitrate(data []byte, opt Options) (int, error) {
	file, err := mp3.Parse(data)
	if err != nil {
		return 0, err
	}

	pool := make([]byte, 0, len(data))
	starts := make([]int, len(file.Frames))
	for i := range file.Frames {
		starts[i] = len(pool)
		pool = append(pool, file.Frames[i].MainData...)
	}

	// A leading header frame carries no audio and no repack reads reservoir from
	// it, so it is not part of the walk — but it does occupy a place in the
	// padding cycle, which is what from records.
	first := &file.Frames[0]
	audio, from := file.Frames, 0
	if first.MainDataBits() == 0 && mp3.FindInfoTag(data[first.Offset:first.Offset+first.Size()], first.Header) != nil {
		audio, starts, from = file.Frames[1:], starts[1:], 1
	}
	if len(audio) == 0 {
		return 0, mp3.ErrNoFrames
	}
	if opt.StripCRC {
		// Two bytes a frame that the audio can have instead, so it can lower the
		// answer by a whole bitrate step.
		for i := range audio {
			audio[i].Header.CRC = false
		}
	}

	payloads := make([]int, len(audio))
	if opt.Recompress {
		slots := make([]int, len(audio)+1)
		size := 0
		for i := range audio {
			slots[i] = size
			size += audio[i].MainDataBytes() + bitio.Slack
		}
		slots[len(audio)] = size
		var stats Stats
		work := recompressAll(audio, pool, starts, make([]byte, size), slots, opt, &stats)
		for i := range work {
			payloads[i] = len(work[i].data)
		}
	} else {
		for i := range audio {
			payloads[i] = audio[i].MainDataBytes()
		}
	}

	bitrate := audio[0].Header.SmallestCBRBitrate(payloads, from)
	if bitrate == 0 {
		return 0, ErrNoConstantBitrate
	}
	return bitrate, nil
}
