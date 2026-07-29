// Package mp3packer losslessly recompresses MP3 files.
//
// It re-codes each frame's Huffman data with the cheapest tables the format
// allows and then re-lays the result out across frames, minimising both the
// coded size of the audio and the padding needed to carry it. The quantized
// spectrum is never altered, so the decoded audio is bit-identical to the input:
// this is the MP3 equivalent of recompressing a ZIP archive at a higher setting.
//
// The entry points are [Process] for in-memory data and [ProcessFile].
package mp3packer

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/W-Floyd/go-mp3packer/huffman"
	"github.com/W-Floyd/go-mp3packer/internal/bitio"
	"github.com/W-Floyd/go-mp3packer/mp3"
)

// Options controls a repack.
type Options struct {
	// Recompress enables the brute-force Huffman search. Without it, only the
	// frame layout and padding are optimised, which is fast; with it, every
	// granule is re-coded, which is where most of the gain is.
	Recompress bool

	// StripCRC drops the optional 16-bit frame CRC, freeing two bytes per frame
	// for audio. Nothing in the audio depends on it — it only lets a decoder
	// notice a damaged frame — but it is part of the input, so removing it is
	// opt-in. Protected frames keep their CRC by default, recomputed to match
	// the rewritten side info.
	StripCRC bool

	// Workers is the number of goroutines used for recompression. Zero means
	// one per CPU. Ignored unless Recompress is set.
	Workers int

	// Log, if set, receives progress and per-frame diagnostics.
	Log func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.NumCPU()
}

// Stats reports what a repack did.
type Stats struct {
	InputSize    int
	OutputSize   int
	Frames       int
	Recompressed int // frames whose Huffman data got smaller
	Unchanged    int // frames already optimal, or not worth rewriting
	Abandoned    int // frames that could not be safely recompressed
	SyncErrors   int
	PayloadBits  int // total part2_3_length in the input
	NewPayload   int // total part2_3_length in the output
}

// Saved is the number of bytes the repack removed. It can be negative only if
// the input was pathological.
func (s Stats) Saved() int { return s.InputSize - s.OutputSize }

// ErrReservoirOverflow means the audio cannot be laid out even at the maximum
// bitrate, which indicates a corrupt input rather than a repacking failure.
var ErrReservoirOverflow = errors.New("mp3packer: frame data does not fit at the maximum bitrate")

// frameWork is the per-frame outcome of the recompression stage.
type frameWork struct {
	data      []byte       // the frame's own main data, packed and byte-aligned
	side      mp3.SideInfo // side info to emit, main_data_begin not yet set
	rewritten bool         // side info fields changed, so it must be re-serialized
	abandoned bool
	newBits   int
}

// Process repacks an MP3 held in memory and returns the new file.
//
// Bytes before the first frame and after the last (ID3 tags and the like) are
// preserved verbatim. A leading Xing/Info/VBRI header frame is preserved too,
// with its stream size, seek table and checksum updated to match the new layout.
func Process(data []byte, opt Options) ([]byte, Stats, error) {
	file, err := mp3.Parse(data)
	if err != nil {
		return nil, Stats{}, err
	}
	stats := Stats{
		InputSize:  len(data),
		Frames:     len(file.Frames),
		SyncErrors: file.SyncErrors,
	}
	opt.logf("parsed %d frames, %d bytes of leading data, %d trailing, %d sync errors",
		len(file.Frames), len(file.StartJunk), len(file.EndJunk), file.SyncErrors)

	// The reservoir view of the input: every frame's data slots end to end. A
	// frame's own audio starts main_data_begin bytes before its own slots, which
	// may be several frames back. It cannot exceed the input it is copied from, so
	// it is sized once rather than grown. A Frame is over a kilobyte, which is why
	// this and the loops below index instead of ranging by value.
	pool := make([]byte, 0, len(data))
	starts := make([]int, len(file.Frames))
	for i := range file.Frames {
		fr := &file.Frames[i]
		starts[i] = len(pool)
		pool = append(pool, fr.MainData...)
		stats.PayloadBits += fr.MainDataBits()
	}

	// A leading header frame carries metadata rather than audio. Preserving it
	// byte for byte is both simpler and safer than re-deriving it, and it keeps
	// whatever gapless-playback information the encoder stored there.
	first := &file.Frames[0]
	firstRaw := data[first.Offset : first.Offset+first.Size()]
	var tag *mp3.InfoTag
	if first.MainDataBits() == 0 {
		tag = mp3.FindInfoTag(firstRaw, first.Header)
	}
	audio := file.Frames
	if tag != nil {
		audio = file.Frames[1:]
		starts = starts[1:]
		opt.logf("preserving leading %s header frame verbatim", tag.Kind)
		if len(audio) == 0 {
			return nil, stats, mp3.ErrNoFrames
		}
	}

	if opt.StripCRC {
		// Decided before the layout runs, so that the freed bytes count towards
		// each frame's capacity.
		for i := range audio {
			audio[i].Header.CRC = false
		}
	}

	// Every frame's output is bounded by its input, so the whole per-frame stage
	// writes into one buffer carved up in advance: a frame's slot is its own and
	// nothing else touches it, which keeps the stage allocation-free even with a
	// worker per CPU.
	slots := make([]int, len(audio)+1)
	arenaSize := 0
	for i := range audio {
		slots[i] = arenaSize
		arenaSize += audio[i].MainDataBytes() + bitio.Slack
	}
	slots[len(audio)] = arenaSize
	arena := make([]byte, arenaSize)

	work := recompressAll(audio, pool, starts, arena, slots, opt, &stats)
	for i := range work {
		stats.NewPayload += work[i].newBits
	}

	out := make([]byte, 0, len(data))
	out = append(out, file.StartJunk...)
	streamStart := len(out)
	if tag != nil {
		out = append(out, firstRaw...)
	}
	framePos := make([]int, 0, len(audio))
	out, err = layout(out, audio, work, streamStart, &framePos, opt)
	if err != nil {
		return nil, stats, err
	}
	streamBytes := len(out) - streamStart
	if tag != nil {
		tag.Repair(out[streamStart:streamStart+len(firstRaw)], streamBytes, framePos)
	}
	out = append(out, file.EndJunk...)

	stats.OutputSize = len(out)
	return out, stats, nil
}

// recompressAll runs the per-frame stage, in parallel when asked to.
func recompressAll(frames []mp3.Frame, pool []byte, starts []int, arena []byte, slots []int, opt Options, stats *Stats) []frameWork {
	work := make([]frameWork, len(frames))
	run := func(i int) {
		buf := arena[slots[i]:slots[i]:slots[i+1]]
		work[i] = recompressFrame(&frames[i], pool, starts[i], buf, opt)
	}
	if !opt.Recompress || opt.workers() <= 1 || len(frames) < 2 {
		for i := range frames {
			run(i)
		}
	} else {
		var next atomic.Int64
		var wg sync.WaitGroup
		for w := 0; w < opt.workers(); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1)) - 1
					if i >= len(frames) {
						return
					}
					run(i)
				}
			}()
		}
		wg.Wait()
	}
	for i := range work {
		w := &work[i]
		switch {
		case w.abandoned:
			stats.Abandoned++
			opt.logf("frame %d: left as-is (data not recompressible)", i)
		case w.rewritten:
			stats.Recompressed++
		default:
			stats.Unchanged++
		}
	}
	return work
}

// mainData extracts a frame's own audio from the input reservoir into buf,
// zero-filling any part that lies outside the file.
func mainData(fr *mp3.Frame, pool []byte, start int, buf []byte) []byte {
	n := fr.MainDataBytes()
	out := buf[:n]
	from := start - fr.SideInfo.MainDataBegin
	// The part of the span that lies inside the reservoir is one copy; the rest
	// is zeroed rather than tested for per byte, which is what this used to do.
	// buf may already have been written to by an abandoned recompression, so the
	// zeroing is not something the caller can be trusted to have done.
	lo, hi := max(from, 0), min(from+n, len(pool))
	if lo >= hi {
		clear(out)
		return out
	}
	clear(out[:lo-from])
	copy(out[lo-from:], pool[lo:hi])
	clear(out[hi-from:])
	return out
}

// recompressFrame re-codes one frame's granules. On any difficulty it falls back
// to copying the frame's data through unchanged, which is always safe: the
// repack still gains from the new layout.
func recompressFrame(fr *mp3.Frame, pool []byte, start int, buf []byte, opt Options) frameWork {
	verbatim := func(abandoned bool) frameWork {
		return frameWork{
			data:      mainData(fr, pool, start, buf),
			side:      fr.SideInfo,
			abandoned: abandoned,
			newBits:   fr.MainDataBits(),
		}
	}
	if !opt.Recompress {
		return verbatim(false)
	}
	h := fr.Header
	from := start - fr.SideInfo.MainDataBegin
	if from < 0 {
		// The frame refers to data before the beginning of the file; the audio
		// is already damaged, so do not compound it.
		return verbatim(true)
	}

	r := bitio.NewReader(pool)
	r.Seek(from * 8)
	// The result is only kept if it is smaller than the input, so the frame's
	// current size is a hard upper bound on what the writer will need, and buf
	// was reserved with exactly that in mind.
	w := bitio.NewWriterBuf(buf)
	side := fr.SideInfo
	// One spectrum for decoding and one for the verification pass, reused across
	// the frame's granules: they are 4.6kB each, too big to keep copying.
	var spectrum, roundTrip huffman.Spectrum

	for gr := 0; gr < h.Granules(); gr++ {
		for ch := 0; ch < h.Channels(); ch++ {
			g := &side.Gr[gr][ch]
			origBits := g.Part23Length
			inStart, outStart := r.Tell(), w.Tell()

			// Scalefactors are copied bit for bit: only their length matters
			// here, and re-deriving their values would risk changing them.
			sfBits := mp3.ScalefactorBits(h, fr.SideInfo, gr, ch)
			if sfBits > origBits {
				return verbatim(true)
			}
			w.Copy(r, sfBits)

			cfg := granuleConfig(*g)
			if !huffman.Decode(&spectrum, cfg, r, h.SampleRate, origBits-sfBits) {
				return verbatim(true)
			}
			best, bits := huffman.Optimize(&spectrum, cfg, h.SampleRate)
			if bits < 0 {
				return verbatim(true)
			}

			huffStart := w.Tell()
			huffman.Encode(&spectrum, best, w, h.SampleRate)

			// Trust nothing: decode what was just written and require it to
			// reproduce the spectrum exactly before accepting it.
			check := bitio.NewReader(w.Bytes())
			check.Seek(huffStart)
			if !huffman.Decode(&roundTrip, best, check, h.SampleRate, w.Tell()-huffStart) || roundTrip != spectrum {
				return verbatim(true)
			}

			newBits := w.Tell() - outStart
			if newBits > 4095 { // part2_3_length is a 12-bit field
				return verbatim(true)
			}
			applyConfig(g, best)
			g.Part23Length = newBits

			r.Seek(inStart + origBits)
		}
	}

	newData := w.Bytes()
	if len(newData) >= fr.MainDataBytes() {
		// No whole byte was saved, so keep the original bits and leave the side
		// info untouched.
		return verbatim(false)
	}
	bits := 0
	for gr := 0; gr < h.Granules(); gr++ {
		for ch := 0; ch < h.Channels(); ch++ {
			bits += side.Gr[gr][ch].Part23Length
		}
	}
	return frameWork{data: newData, side: side, rewritten: true, newBits: bits}
}

func granuleConfig(g mp3.GranuleInfo) huffman.Config {
	cfg := huffman.Config{
		BigValues:       g.BigValues,
		Region0Count:    g.Region0Count,
		Region1Count:    g.Region1Count,
		TableSelect:     g.TableSelect,
		Count1Table:     32,
		WindowSwitching: g.WindowSwitching,
		BlockType:       g.BlockType,
		MixedBlock:      g.MixedBlock,
	}
	if g.Count1TableSelect {
		cfg.Count1Table = 33
	}
	return cfg
}

func applyConfig(g *mp3.GranuleInfo, cfg huffman.Config) {
	g.BigValues = cfg.BigValues
	g.TableSelect = cfg.TableSelect
	g.Count1TableSelect = cfg.Count1Table == 33
	if !g.WindowSwitching {
		g.Region0Count = cfg.Region0Count
		g.Region1Count = cfg.Region1Count
	}
}

// layout chooses a frame size for every frame and writes the stream.
//
// Each frame gets the smallest size that can hold what is left of its own audio
// after the bit reservoir contributes, while still leaving room for any later
// frame that is too large to fit in a single frame of its own. Since only whole
// frame sizes exist, this minimises the file.
func layout(out []byte, frames []mp3.Frame, work []frameWork, streamStart int, framePos *[]int, opt Options) ([]byte, error) {
	n := len(frames)

	// How many bytes of reservoir each frame needs to have banked before it,
	// because its own data is larger than one frame can carry.
	need := make([]int, n+1)
	total := 0
	for i := n - 1; i >= 0; i-- {
		need[i] = max(0, len(work[i].data)+need[i+1]-frames[i].Header.MaxDataSize())
		total += len(work[i].data)
	}
	if need[0] > 0 {
		return nil, fmt.Errorf("%w: first frame is short by %d bytes", ErrReservoirOverflow, need[0])
	}
	for i := range frames {
		if maxMDB := frames[i].Header.MaxMainDataBegin(); need[i] > maxMDB {
			return nil, fmt.Errorf("%w: frame %d needs %d bytes of reservoir but only %d can be addressed",
				ErrReservoirOverflow, i, need[i], maxMDB)
		}
	}

	// Place the data first, then emit frames around it. stream is the new
	// reservoir: frame data end to end, with explicit gaps wherever the
	// reservoir would otherwise overflow the 9-bit back-reference.
	stream := make([]byte, 0, total)
	capacity := 0
	chosen := make([]mp3.Capacity, n)
	mdb := make([]int, n)
	gaps := 0
	for i := range frames {
		h := frames[i].Header
		avail := capacity - len(stream)
		if avail > h.MaxMainDataBegin() {
			// More reservoir than the side info can point back to: the excess
			// becomes unused padding inside earlier frames.
			pad := avail - h.MaxMainDataBegin()
			stream = appendZeros(stream, pad)
			avail = h.MaxMainDataBegin()
			gaps += pad
		}
		if avail < 0 {
			return nil, fmt.Errorf("%w: frame %d overran its own frame by %d bytes",
				ErrReservoirOverflow, i, -avail)
		}
		want := len(work[i].data) + need[i+1] - avail
		chosen[i] = smallestCapacity(h, want)
		mdb[i] = avail
		stream = append(stream, work[i].data...)
		capacity += chosen[i].DataSize
	}
	if gaps > 0 {
		opt.logf("%d bytes could not be packed into the reservoir", gaps)
	}

	cursor := 0
	for i := range frames {
		fr := &frames[i]
		*framePos = append(*framePos, len(out)-streamStart)

		h := fr.Header
		h.BitrateIndex = chosen[i].Index
		h.Padding = chosen[i].Padding
		header := h.Bytes()

		side := work[i].side
		side.MainDataBegin = mdb[i]
		var sideRaw []byte
		if work[i].rewritten {
			sideRaw = side.Serialize(h)
		} else {
			// Untouched frames keep their exact side info bits, so nothing but
			// the reservoir pointer can drift.
			sideRaw = mp3.PatchMainDataBegin(h, fr.SideInfoRaw, mdb[i])
		}

		out = append(out, header[:]...)
		if h.CRC {
			// The stored CRC covers the header and side info, both of which we
			// have just rewritten.
			crc := mp3.FrameCRC(header, sideRaw)
			out = append(out, byte(crc>>8), byte(crc))
		}
		out = append(out, sideRaw...)

		end := min(cursor+chosen[i].DataSize, len(stream))
		out = append(out, stream[cursor:end]...)
		if pad := chosen[i].DataSize - (end - cursor); pad > 0 {
			out = appendZeros(out, pad) // tail of the last frames
		}
		cursor = end
	}
	if cursor != len(stream) {
		return nil, fmt.Errorf("%w: %d bytes left over", ErrReservoirOverflow, len(stream)-cursor)
	}
	return out, nil
}

// smallestCapacity returns the cheapest frame size holding at least want bytes,
// or the largest available size if none can.
func smallestCapacity(h mp3.Header, want int) mp3.Capacity {
	return h.SmallestCapacity(want)
}

// appendZeros extends b with n zero bytes, without allocating a throwaway slice
// to copy them from. Padding runs are short but there is one per frame.
func appendZeros(b []byte, n int) []byte {
	var zeros [64]byte
	for n > len(zeros) {
		b = append(b, zeros[:]...)
		n -= len(zeros)
	}
	return append(b, zeros[:n]...)
}

func (s Stats) String() string {
	return fmt.Sprintf("%d frames: %d recompressed, %d unchanged, %d abandoned; %d -> %d bytes (%d saved)",
		s.Frames, s.Recompressed, s.Unchanged, s.Abandoned, s.InputSize, s.OutputSize, s.Saved())
}
