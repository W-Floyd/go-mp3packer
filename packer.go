// Package mp3packer losslessly recompresses MP3 files.
//
// It re-codes each frame's Huffman data with the cheapest tables the format
// allows and then re-lays the result out across frames, minimising both the
// coded size of the audio and the padding needed to carry it. The quantized
// spectrum, the scalefactors and every gain and window field are never altered,
// so the output means exactly the audio the input did: this is the MP3
// equivalent of recompressing a ZIP archive at a higher setting. Decoders render
// it identically too, bar one — see the README on macOS CoreAudio.
//
// The entry points are [Process] for in-memory data and [ProcessFile].
package mp3packer

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

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

	// MinBitrate is a floor on every frame's size, in kbps, read the way
	// mp3packer's -b switch reads it: see [mp3.Header.CapacityFloor]. Naming an
	// exact bitrate makes the output constant-bitrate at that rate, since after
	// recompression no frame wants more than the floor gives it. Zero, the
	// default, lets every frame be as small as its data allows.
	//
	// The audio is unaffected either way. A floor only adds padding, so it costs
	// size and buys a stream a player that cannot handle VBR will accept.
	MinBitrate int

	// ConstantBitrate lays the output out at the lowest constant bitrate the
	// recompressed audio fits in, which is MinBitrate with the number worked out
	// rather than given. It is decided from the payloads this same repack
	// produced, so it costs nothing beyond the search that was going to run
	// anyway — no need to ask [SmallestCBRBitrate] first and then repack.
	//
	// MinBitrate still applies as a floor, so setting both gives the lower bound
	// you asked for or the lowest that fits, whichever is larger.
	ConstantBitrate bool

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

	// Bitrate is the floor the frame layout worked to, in kbps, or zero if it was
	// free to make every frame as small as it could. With Options.ConstantBitrate
	// this is the bitrate that was chosen, and the output is that bitrate.
	Bitrate int

	// Wall time of each stage, zero unless built with -tags mp3timing. Only
	// Recompress uses more than one goroutine, so Prepare and Layout together
	// bound what any number of workers can achieve — on a long file they are most
	// of an all-cores repack, and a change that shows up in only one of the three
	// is easy to misread without these.
	Prepare    time.Duration // parsing, and building the reservoir view
	Recompress time.Duration // the per-frame stage, one goroutine per worker
	Layout     time.Duration // choosing frame sizes and writing the stream back out
}

// Serial is the part of a repack that does not shrink with more workers.
func (s Stats) Serial() time.Duration { return s.Prepare + s.Layout }

// clock reads the wall clock, or returns nothing at all when the stage timings
// are compiled out, which lets the whole of the recording fold away.
func clock() time.Time {
	if timingEnabled {
		return time.Now()
	}
	return time.Time{}
}

// Saved is the number of bytes the repack removed. It can be negative only if
// the input was pathological.
func (s Stats) Saved() int { return s.InputSize - s.OutputSize }

// ErrReservoirOverflow means the audio cannot be laid out even at the maximum
// bitrate, which indicates a corrupt input rather than a repacking failure.
var ErrReservoirOverflow = errors.New("mp3packer: frame data does not fit at the maximum bitrate")

// frameWork is the per-frame outcome of the recompression stage. New side info
// is written back into the frame rather than carried here: it is 536 bytes, and
// one per frame is three and a half megabytes of the repack's allocation on a
// long track. A worker owns its frame outright, and the write happens only once
// the granules are all coded and verified, so a frame that is given up on still
// has the side info it arrived with.
type frameWork struct {
	data      []byte // the frame's own main data, packed and byte-aligned
	rewritten bool   // side info fields changed, so it must be re-serialized
	abandoned bool
	newBits   int
}

// Process repacks an MP3 held in memory and returns the new file.
//
// Bytes before the first frame and after the last (ID3 tags and the like) are
// preserved verbatim. A leading Xing/Info/VBRI header frame is preserved too,
// with its stream size, seek table and checksum updated to match the new layout.
func Process(data []byte, opt Options) ([]byte, Stats, error) {
	tPrepare := clock()
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
	// worker per CPU. Nothing writes coded data without the search, though, and a
	// frame that comes through unchanged is read straight out of the reservoir
	// view, so with recompression off the arena is not allocated at all.
	var slots []int
	var arena []byte
	if opt.Recompress {
		slots = make([]int, len(audio)+1)
		arenaSize := 0
		for i := range audio {
			slots[i] = arenaSize
			arenaSize += audio[i].MainDataBytes() + bitio.Slack
		}
		slots[len(audio)] = arenaSize
		arena = make([]byte, arenaSize)
	}

	tRecompress := clock()
	work := recompressAll(audio, pool, starts, arena, slots, opt, &stats)
	tLayout := clock()
	for i := range work {
		stats.NewPayload += work[i].newBits
	}

	firstNum := 0
	if tag != nil {
		firstNum = 1 // the header frame takes the stream's first place in the padding cycle
	}
	if opt.ConstantBitrate {
		// Asked of the payloads this repack just produced, so the answer is the
		// real one rather than a bound taken from the input.
		payloads := make([]int, len(work))
		for i := range work {
			payloads[i] = len(work[i].data)
		}
		h := audio[0].Header
		bitrate := h.SmallestCBRBitrate(payloads, firstNum)
		if bitrate == 0 {
			return nil, stats, ErrNoConstantBitrate
		}
		if tag != nil && first.Header.Bitrate() > bitrate {
			// The header frame is preserved rather than rebuilt, so it can be
			// grown to the floor but not shrunk to it (its Xing payload and any
			// LAME extension have to stay where they are). A stream whose first
			// frame is larger than the rest is not constant bitrate, so the floor
			// rises to meet it instead.
			bitrate = first.Header.Bitrate()
			opt.logf("constant bitrate raised to the %s header frame's own %d kbps", tag.Kind, bitrate)
		}
		if h.CapacityFloor(bitrate).At(0).DataSize > h.CapacityFloor(opt.MinBitrate).At(0).DataSize {
			opt.MinBitrate = bitrate
		}
		opt.logf("constant bitrate: %d kbps", opt.MinBitrate)
	}
	stats.Bitrate = audio[0].Header.CapacityFloor(opt.MinBitrate).Bitrate()

	out := make([]byte, 0, len(data))
	out = append(out, file.StartJunk...)
	streamStart := len(out)
	headerFrame := firstRaw
	if tag != nil {
		// A stream is only constant-bitrate if this frame is that bitrate too, so
		// the floor applies here as much as to the audio — but nothing inside the
		// frame moves, so growing it is a new header and a longer tail.
		headerFrame = growToFloor(firstRaw, *first, opt)
		out = append(out, headerFrame...)
	}
	framePos := make([]int, 0, len(audio))
	out, err = layout(out, audio, work, streamStart, firstNum, &framePos, opt)
	if err != nil {
		return nil, stats, err
	}
	streamBytes := len(out) - streamStart
	if tag != nil {
		tag.Repair(out[streamStart:streamStart+len(headerFrame)], streamBytes, framePos)
	}
	out = append(out, file.EndJunk...)

	if timingEnabled {
		// Four reads for the whole repack, taken at the stage boundaries, so the
		// three durations are differences rather than six separate readings.
		tDone := clock()
		stats.Prepare = tRecompress.Sub(tPrepare)
		stats.Recompress = tLayout.Sub(tRecompress)
		stats.Layout = tDone.Sub(tLayout)
	}
	stats.OutputSize = len(out)
	return out, stats, nil
}

// recompressAll runs the per-frame stage, in parallel when asked to.
func recompressAll(frames []mp3.Frame, pool []byte, starts []int, arena []byte, slots []int, opt Options, stats *Stats) []frameWork {
	work := make([]frameWork, len(frames))
	run := func(i int) {
		var buf []byte
		if arena != nil {
			buf = arena[slots[i]:slots[i]:slots[i+1]]
		}
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

// mainData returns a frame's own audio, as bytes taken from the input reservoir
// and byte-aligned.
//
// Almost always the whole span lies inside the reservoir, and then the answer is
// a view of it rather than a copy: nothing downstream writes to a frame's data,
// so with recompression off this is the only form needed and buf can be nil.
// What is left is the two ends of the file — a frame pointing back before its
// start, and the last frame's rounding tail reaching past it — where the part
// outside has to be zero-filled into a buffer of its own.
func mainData(fr *mp3.Frame, pool []byte, start int, buf []byte) []byte {
	n := fr.MainDataBytes()
	from := start - fr.SideInfo.MainDataBegin
	if from >= 0 && from+n <= len(pool) {
		return pool[from : from+n : from+n]
	}
	if cap(buf) < n {
		// Only the handful of edge frames above reach this, so allocating for
		// them beats sizing the arena for a case that usually never arises.
		buf = make([]byte, n)
	}
	out := buf[:n]
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
			coding := huffman.Search(&spectrum, cfg, h.SampleRate)
			if coding.Bits < 0 {
				return verbatim(true)
			}
			best := coding.Config

			huffStart := w.Tell()
			coding.Encode(&spectrum, w, h.SampleRate)

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
	// Committed only here, with every granule coded and checked: until this
	// point each verbatim return above still sees the frame as it arrived.
	fr.SideInfo = side
	return frameWork{data: newData, rewritten: true, newBits: bits}
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

// growToFloor pads a leading header frame up to opt.MinBitrate's floor, so that
// a constant-bitrate output is constant across its first frame too. It returns
// raw unchanged when the frame already reaches the floor, which includes every
// repack that sets no floor at all.
//
// The frame carries no audio, so there is nothing to move: the Xing or VBRI
// payload keeps its offsets and the new room is zeros at the end. Only the
// header changes, and the frame CRC with it if the frame has one.
func growToFloor(raw []byte, fr mp3.Frame, opt Options) []byte {
	h := fr.Header
	floor := h.CapacityFloor(opt.MinBitrate).At(0)
	size := floor.DataSize + h.FrameSize() - h.DataSize()
	if size <= len(raw) {
		return raw
	}
	h.BitrateIndex = floor.Index
	h.Padding = floor.Padding
	header := h.Bytes()

	out := make([]byte, 0, size)
	out = append(out, header[:]...)
	rest := raw[4:]
	if h.CRC {
		crc := mp3.FrameCRC(header, rest[2:2+h.SideInfoSize()])
		out = append(out, byte(crc>>8), byte(crc))
		rest = rest[2:]
	}
	out = append(out, rest...)
	return appendZeros(out, size-len(out))
}

// layout chooses a frame size for every frame and writes the stream.
//
// Each frame gets the smallest size that can hold what is left of its own audio
// after the bit reservoir contributes, while still leaving room for any later
// frame that is too large to fit in a single frame of its own. Since only whole
// frame sizes exist, this minimises the file — unless opt.MinBitrate raises the
// floor, in which case a frame given more room than it needs banks the rest in
// the reservoir, and once even that is full the remainder is padding.
//
// firstNum is the position of frames[0] in the output stream, which is 1 when a
// header frame precedes it. Only the constant-bitrate padding cycle cares.
func layout(out []byte, frames []mp3.Frame, work []frameWork, streamStart, firstNum int, framePos *[]int, opt Options) ([]byte, error) {
	n := len(frames)

	// How many bytes of reservoir each frame needs to have banked before it,
	// because its own data is larger than one frame can carry.
	need := make([]int, n+1)
	for i := n - 1; i >= 0; i-- {
		need[i] = max(0, len(work[i].data)+need[i+1]-frames[i].Header.MaxDataSize())
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

	// Place the data first, then emit frames around it. The new reservoir is
	// the frames' data end to end, with explicit gaps wherever it would
	// otherwise overflow the 9-bit back-reference — but only its shape is
	// needed here, since deciding a frame's size takes lengths and not bytes.
	// Recording the pieces rather than concatenating them saves a second copy
	// of the whole audio and the allocation to hold it; the second pass reads
	// the frames' own buffers.
	stream := reservoir{segs: make([]segment, 0, n+8)}
	capacity := 0
	chosen := make([]mp3.Capacity, n)
	mdb := make([]int, n)
	gaps := 0
	for i := range frames {
		h := frames[i].Header
		avail := capacity - stream.len
		if avail > h.MaxMainDataBegin() {
			// More reservoir than the side info can point back to: the excess
			// becomes unused padding inside earlier frames.
			pad := avail - h.MaxMainDataBegin()
			stream.addGap(pad)
			avail = h.MaxMainDataBegin()
			gaps += pad
		}
		if avail < 0 {
			return nil, fmt.Errorf("%w: frame %d overran its own frame by %d bytes",
				ErrReservoirOverflow, i, -avail)
		}
		want := len(work[i].data) + need[i+1] - avail
		chosen[i] = smallestCapacity(h, want)
		if opt.MinBitrate > 0 {
			// Guarded rather than folded into the comparison: this is the serial
			// path, and a repack with no floor should not pay for the option.
			if floor := h.CapacityFloor(opt.MinBitrate).At(firstNum + i); floor.DataSize > chosen[i].DataSize {
				chosen[i] = floor
			}
		}
		mdb[i] = avail
		stream.addData(work[i].data)
		capacity += chosen[i].DataSize
	}
	if gaps > 0 {
		if opt.MinBitrate > 0 {
			// Expected rather than notable: a floor hands out more room than the
			// audio needs, and once the reservoir is as full as a back-reference
			// can reach, the rest of the slack has nowhere to go but padding.
			opt.logf("%d bytes of padding, the reservoir being full", gaps)
		} else {
			opt.logf("%d bytes could not be packed into the reservoir", gaps)
		}
	}

	cursor := 0
	for i := range frames {
		fr := &frames[i]
		*framePos = append(*framePos, len(out)-streamStart)

		h := fr.Header
		h.BitrateIndex = chosen[i].Index
		h.Padding = chosen[i].Padding
		header := h.Bytes()

		side := fr.SideInfo
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

		end := min(cursor+chosen[i].DataSize, stream.len)
		out = stream.appendTo(out, end-cursor)
		if pad := chosen[i].DataSize - (end - cursor); pad > 0 {
			out = appendZeros(out, pad) // tail of the last frames
		}
		cursor = end
	}
	if cursor != stream.len {
		return nil, fmt.Errorf("%w: %d bytes left over", ErrReservoirOverflow, stream.len-cursor)
	}
	return out, nil
}

// segment is one run of the new reservoir: a frame's data, or a gap that had to
// be left because the reservoir could not reach back far enough.
type segment struct {
	data  []byte // nil for a gap
	zeros int
}

func (s segment) size() int {
	if s.data == nil {
		return s.zeros
	}
	return len(s.data)
}

// reservoir is the new main-data stream described rather than built: the pieces
// in order, plus a read cursor for the second pass. A frame's slot is a window
// over the sequence and generally spans more than one piece, which is the whole
// point of the bit reservoir.
type reservoir struct {
	segs []segment
	len  int

	at, off int // read position: segment, and byte within it
}

func (r *reservoir) addGap(n int) {
	r.segs = append(r.segs, segment{zeros: n})
	r.len += n
}

func (r *reservoir) addData(b []byte) {
	r.segs = append(r.segs, segment{data: b})
	r.len += len(b)
}

// appendTo copies the next n bytes of the reservoir onto out.
func (r *reservoir) appendTo(out []byte, n int) []byte {
	for n > 0 && r.at < len(r.segs) {
		s := r.segs[r.at]
		take := min(n, s.size()-r.off)
		if s.data == nil {
			out = appendZeros(out, take)
		} else {
			out = append(out, s.data[r.off:r.off+take]...)
		}
		r.off += take
		n -= take
		if r.off == s.size() {
			r.at++
			r.off = 0
		}
	}
	return out
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
