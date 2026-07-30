# go-mp3packer

Losslessly recompress MP3 files, in Go. Same audio, smaller file.

An MP3 stores its spectrum with Huffman code tables chosen from a fixed set, and
encoders pick those tables heuristically. `go-mp3packer` re-codes every granule
with the cheapest tables the format allows, then re-lays the result out across
frames so that as little padding as possible is left over. Nothing is
re-quantized and nothing is re-encoded in the lossy sense: every quantized
coefficient, every scalefactor and every gain and window field comes out the
other side bit for bit, so the file still means the same audio — the same
relationship a recompressed ZIP archive has to the original. What a *decoder*
then makes of it is bit-identical too for every decoder tested here bar one; see
[Losslessness, and one decoder that disagrees](#losslessness-and-one-decoder-that-disagrees).

This is a from-scratch Go implementation of the recompression half of Reed
Wilson's [mp3packer](https://github.com/hmage/mp3packer), scoped like the C++
port [mp3packercpp](https://github.com/Snesnopic/mp3packercpp): the Huffman
search and the bit-reservoir constraint solving, without the original's CBR
conversion, directory walking, tag stripping or reporting modes.

## Install

```sh
go install github.com/W-Floyd/go-mp3packer/cmd/mp3packer@latest
```

## Use

```sh
mp3packer in.mp3 out.mp3   # write a repacked copy
mp3packer in.mp3           # repack in place
```

```
  -n         skip the Huffman search; only repack the frame layout
  -no-crc    drop the optional frame CRC, freeing 2 bytes per frame
  -cbr       constant-bitrate output at the lowest bitrate that fits
  -b N       minimum bitrate in kbps; an exact one gives constant-bitrate output
  --ib       print the lowest constant bitrate this file fits in, and exit
  -j N       recompression workers (0 = one per CPU)
  -f         overwrite the output file if it exists
  -v         log per-frame details
  -q         print nothing on success
```

Output is written to a temporary file and renamed into place, so an interrupted
run cannot truncate your music, and in-place repacking is safe.

### Constant bitrate

Repacking normally makes every frame as small as its own audio needs, which is
variable bitrate by construction. `-cbr` gives up that saving for a stream a
player that cannot follow a varying bitrate will accept:

```sh
mp3packer -cbr in.mp3 out.mp3   # lowest constant bitrate this audio fits in
mp3packer --ib in.mp3           # just print that bitrate: 192
mp3packer -b 192 in.mp3 out.mp3 # or name one, as a floor
```

The bitrate `-cbr` picks is the lowest one every frame fits in *after* the search
has shrunk it, worked out from that same repack rather than from a first pass, so
it costs nothing beyond the repack itself. It is a real search, not the average
bitrate: a constant bitrate has to carry each frame's own payload out of one
frame's room plus whatever earlier frames banked in the bit reservoir, and only
the 511 bytes a back-reference can reach are ever available. `--ib` answers the
same question without writing anything; the answer moves with `-n` and `-no-crc`,
since both change how much has to fit.

`-b N` sets a floor instead of finding one, reading `N` the way the original
mp3packer's `-b` does — an exact bitrate gives that bitrate, one more than an
exact bitrate gives every frame padded, anything else rounds up. Combined with
`-cbr` the higher of the two wins. Padding follows the standard's cycle, so a
44.1 kHz stream is padded on the same frames a CBR encoder would pad, and the
file is exactly the bitrate it claims.

Frames only ever grow, and only into padding: the audio is untouched, and the
leading Xing/Info frame is grown to match so the whole file is one bitrate. That
frame is preserved rather than rebuilt, so if it arrived *larger* than the audio
needs, the floor rises to its bitrate rather than leaving one odd frame at the
front.

## As a library

```go
import mp3packer "github.com/W-Floyd/go-mp3packer"

stats, err := mp3packer.ProcessFile("in.mp3", "out.mp3", mp3packer.Options{
    Recompress: true,
})
```

`Process` does the same thing for data already in memory. Everything the command
does is an option, including constant bitrate:

```go
// The lowest constant bitrate this audio fits in, chosen from the repack itself.
out, stats, err := mp3packer.Process(data, mp3packer.Options{
    Recompress:      true,
    ConstantBitrate: true,
})
// stats.Bitrate is the bitrate it settled on. MinBitrate names one instead, and
// raises the floor if it is higher than ConstantBitrate would have picked.

// Or ask first, without writing anything — same answer, same search.
kbps, err := mp3packer.SmallestCBRBitrate(data, mp3packer.Options{Recompress: true})
```

The lower layers are exported and usable on their own:

- `mp3` — frame headers, side information, CRC, Xing/Info tags, and a parser that
  hands back every frame of a file with its main data.
- `huffman` — decode a granule's quantized spectrum, search for its cheapest
  legal coding, encode it again.

## What it saves

The test corpus repacked, sizes in bytes. `-n` is layout only; the default column
is the full search, the `-z` equivalent:

<!-- comparison:savings -->
| file              |                                      |  input |   `-n` | default |  saved |
|:------------------|:-------------------------------------|-------:|-------:|--------:|-------:|
| bench-vbr.mp3     | VBR, joint stereo, 44100 Hz          | 180098 | 179708 |  179295 |  0.45% |
| cbr-crc.mp3       | CBR 128, joint stereo, 44100 Hz, CRC |  20061 |  19914 |   19760 |  1.50% |
| cbr320-quiet.mp3  | CBR 320, joint stereo, 44100 Hz      | 322872 | 288541 |  288542 | 10.63% |
| cbr320-stereo.mp3 | CBR 320, stereo, 44100 Hz            |  50154 |  49903 |   49903 |  0.50% |
| lsf-22050.mp3     | VBR, joint stereo, 22050 Hz, MPEG2   |  15806 |  15599 |   15547 |  1.64% |
| vbr-joint.mp3     | VBR, joint stereo, 44100 Hz          |  28074 |  27866 |   27658 |  1.48% |
| vbr-mono.mp3      | VBR, mono, 44100 Hz                  |  10510 |  10093 |    9989 |  4.96% |
<!-- /comparison:savings -->

What a file gives up depends on how far its bitrate sits above what the audio
needs; quiet material at a high CBR is the extreme case. Whether any of that is
worth the rewrite is your call.

Recompression is not the whole story. `-n` alone removes padding, which is most
of what there is to remove on a dense file.

Where layout only comes out level with or smaller than the full search, that is
the format rather than a defect. Once every frame has shrunk to the minimum the
format allows, space freed by better compression cannot be given back — it becomes
padding instead, and the reservoir bookkeeping can spend more of it than it saves.
Compression and file size are not the same axis; `TestRecompressBeatsLayoutOnly`
insists on the payload shrinking rather than the file.

## Compared with the C++ port

On the same inputs, the coded audio comes out the same size as `mp3packercpp`'s
to the bit, and the files are byte-for-byte the same size except where the two
disagree about what to keep:

- **Frame CRCs.** We recompute and keep them; the C++ port discards them. Pass
  `-no-crc` to match it and reclaim two bytes per frame.
- **The Xing/Info header frame.** We keep it byte for byte and only update its
  stream size, seek table and checksum. The C++ port truncates it to the smallest
  frame that fits, which is up to one frame smaller but discards trailing
  extension fields.
- **MPEG-2 and 2.5.** We recompress low-sampling-frequency files too, which needs
  the LSF scalefactor tables to find where each granule's Huffman data starts;
  the C++ port copies those frames through untouched.

On speed, every test file, all three invoked as commands with every core
available, on an Apple M4 Max. `reference` is
[mp3packercpp](https://github.com/Snesnopic/mp3packercpp) as released; `fork` is
[a branch of it](https://github.com/W-Floyd/mp3packercpp/tree/perf/huffman-region-search)
carrying optimisation work that upstream does not have, which is the more
demanding comparison of the two:

<!-- comparison:comparison-files -->
| file              |    size |    ours |    fork | reference | vs fork | vs ref |
|:------------------|--------:|--------:|--------:|----------:|--------:|-------:|
| bench-vbr.mp3     |  180 kB | 4.52 ms | 3.98 ms |   22.7 ms |    0.9× |   5.0× |
| cbr-crc.mp3       |   20 kB | 3.75 ms | 2.89 ms |   7.14 ms |    0.8× |   1.9× |
| cbr320-quiet.mp3  |  322 kB | 4.17 ms | 3.76 ms |   8.41 ms |    0.9× |   2.0× |
| cbr320-stereo.mp3 |   50 kB | 3.52 ms | 2.97 ms |   3.51 ms |    0.8× |   1.0× |
| lsf-22050.mp3     |   15 kB | 3.50 ms | 2.36 ms |   2.64 ms |    0.7× |   0.8× |
| vbr-joint.mp3     |   28 kB | 3.67 ms | 3.20 ms |   6.73 ms |    0.9× |   1.8× |
| vbr-mono.mp3      |   10 kB | 3.35 ms | 2.66 ms |   4.51 ms |    0.8× |   1.3× |
| bards-tale.mp3    | 3810 kB | 23.2 ms | 22.4 ms |    206 ms |    1.0× |   8.9× |
<!-- /comparison:comparison-files -->

Three things in there are worth reading rather than skipping. The lead over the
released reference collapses as the files get smaller, and on the smallest it goes
the other way: on a small enough file every side is mostly paying to start a
process, and starting ours costs the larger share of that. `lsf-22050.mp3` is
quicker for both of them for a different reason — the C++ port declines to
recompress MPEG-2 at all, so it is not doing the same job.

The fork is the one to watch. It is within noise of us on long material and ahead
on the short files, for the startup reason above rather than anything about the
repack. Against a C++ implementation carrying comparable optimisation work, level
is the honest description; the order-of-magnitude figures belong to the comparison
with upstream, not to the language.

Across worker counts, on the longest file to hand:

<!-- comparison:comparison-threads -->
| threads |    ours |    fork | reference | vs fork | vs ref |
|--------:|--------:|--------:|----------:|--------:|-------:|
|       1 |  173 ms |  177 ms |   2251 ms |    1.0× |  13.0× |
|       2 | 92.1 ms | 96.1 ms |   1153 ms |    1.0× |  12.5× |
|       4 | 51.7 ms | 54.3 ms |    597 ms |    1.1× |  11.6× |
|     all | 23.2 ms | 22.4 ms |    206 ms |    1.0× |   8.9× |
<!-- /comparison:comparison-threads -->

Our lead over the reference is largest on one worker and narrows as cores are
added, because it scales better than we do. A fixed part of our wall clock is
parsing and laying frames back out, and no number of workers can share it — see
[PERFORMANCE.md](PERFORMANCE.md). The fork tracks us closely at every worker
count, which is what two implementations doing the same work in the same shape
look like.

`TestComparison` produces both tables and asserts what they claim, which is
narrowly that we are faster than the reference on the longest file at every worker
count. Nothing is asserted about the fork: which way that one goes is a fact to
report, not a property of this repository to defend. It times every side the same
way, as a subprocess over a file, so the exec and the disk are in every figure:

```sh
export MP3PACKER_REFERENCE=/path/to/mp3packercpp
export MP3PACKER_REFERENCE_FORK=/path/to/a/fork   # optional third column
export MP3PACKER_BENCH_FILE=/path/to/some/minutes/of/music.mp3   # optional
go test -run TestComparison -update-readme .
```

`TestReferenceCompression` is the other half: it repacks each file with both and
fails if our coded audio is larger. On most of the corpus the output is the same
size to the byte.

For measuring changes to the code rather than comparing implementations, see
[PERFORMANCE.md](PERFORMANCE.md).

## Performance

The one thing worth knowing before touching any of it: a fixed part of the wall
clock is parsing and laying the frames back out, and no number of workers can
share that. So a change is meaningless without a worker count attached. Folding
the frame CRC through a table lands almost entirely in that part and is worth many
times more across all cores than on one; the AVX2 row batching goes the other way,
4.6% of one worker and nothing at all of sixteen — a step no table here can
resolve, measured with `benchsteps ab`.

[PERFORMANCE.md](PERFORMANCE.md) has the measured history commit by commit, the
cross-architecture table, where the cores go, the assembly, and a list of things
tried and dropped. [CLAUDE.md](CLAUDE.md) has the measurement protocol.

## The compression ceiling

Within a fixed spectrum and block geometry, the format offers exactly five things
to re-choose: big_values, both region boundaries, the three code tables and the
count1 table. The search covers all of them exhaustively, and
`TestOptimizeMatchesExhaustiveSearch` holds it to a naive implementation, so there
is nothing left there.

The one degree of freedom this does *not* touch is the scalefactor domain:
scalefac_compress could be re-chosen for the same values, and scfsi could let the
second granule inherit the first's. Measured across the test files — 1908 granules
of LAME output — scalefac_compress is already minimal in every single one, and
scfsi is already set wherever the values permit it, so both would gain exactly
zero bits. Scalefactors are only 0.2–10% of the payload to begin with.

A cruder encoder might leave something there, but nothing available here produces
such files, so implementing it would be untestable speculation. It is written down
rather than built.

## Correctness

Losslessness is not taken on trust. Every re-coded granule is decoded again
before it is accepted, and any granule that does not reproduce its spectrum
exactly — or that cannot be decoded in the first place, as happens when a frame's
reservoir pointer refers to data the file does not contain — is passed through
untouched. Frames are only ever shrunk, never grown.

On top of that, `go test ./...`:

- re-derives every granule of every test file before and after a repack and
  requires the spectra to be identical;
- decodes both with `ffmpeg`, when available, and compares the waveforms;
- checks the fast Huffman search against a deliberately naive exhaustive one;
- round-trips every code table, every header bit pattern, and every side
  information block of the test files;
- verifies that repacking a repacked file changes nothing.

`FuzzProcess` drives the whole repack from arbitrary bytes, since `Process` is an
exported entry point taking untrusted input. Refusing an input is always allowed;
panicking is not, nor is returning success beside something that will not parse.
It found a real fault on its first run — in the parser, not the packer — and the
crashers under `testdata/fuzz/` are regression seeds that run with the ordinary
suite. `go test -fuzz FuzzProcess` to go looking for more; [CLAUDE.md](CLAUDE.md)
has what it caught and what it taught.

### Losslessness, and one decoder that disagrees

What is preserved exactly is everything the standard makes the audio a function
of: all 576 quantized coefficients of every granule, the scalefactors bit for
bit, and global_gain, scalefac_compress, scalefac_scale, preflag, block_type,
mixed_block_flag, subblock_gain and scfsi. What the search *does* change is how
those same values are split between the big-values and count1 regions, how far
the coded region runs before the implicit zero tail, and which tables code it —
none of which a decoder is supposed to be able to tell apart.

macOS `afconvert` (CoreAudio) can tell them apart, by one unit in the last place
of a 16-bit sample. Decoding a repack and the input it came from and comparing
sample by sample:

| decoder | differing samples |
|---|---|
| `ffmpeg` (mp3float) | 0 |
| `mpg123` | 0 |
| `afconvert -d LEI16` | 35 of 21,977,578 |

Those 35 are on a 2 MB, 9539-frame narration file; the rate is about 1.6 samples
per million, every difference is exactly ±1 at 16-bit scale, in both directions,
and the deviation sits around −90 dBFS. It is not particular to that file — the
long track already used for benchmarking here does it too, 3 samples of 14
million — and it is the boundary move itself rather than any particular coding
choice: constraining the search to keep each granule's `big_values` and coded
extent makes `afconvert` agree exactly, and costs almost all of the saving (0.45%
→ 0.04% on that file), which is why it is not offered as a mode. The likeliest
mechanism is that CoreAudio requantizes a ±1 coefficient down a slightly
different path depending on which region coded it, the two paths agreeing only to
within one float rounding step. The standard asks for limited-accuracy RMS
conformance rather than bit-exactness, so a decoder is entitled to this.

So: the bitstream means the same audio, and every decoder tested renders it
identically except CoreAudio, which renders it inaudibly differently.

## Not implemented

The original mp3packer does more than recompress. Out of scope here: the reservoir
placement switches (`-r`, `-R`), tag stripping (`-s`, `-t`), directory processing,
the rest of the `-i` information report, and error concealment for damaged frames
— broken frames are copied through as they are rather than silenced.

`-r`/`-R` is the nearest of those to mattering. mp3packer offers a choice of where
the slack sits: maximize the reservoir, so a frame's data starts as early as it
can, or minimize it, so each frame is as close to self-contained as the layout
allows. Neither changes the size of the file — the original's docs say so plainly —
and the only use given for minimizing is making CBR320 easier to split. We do what
`-R` does, which is also what mp3packer did before 1.16, and with a floor in play
that means `main_data_begin` sits pinned at its 511-byte maximum.

## Licence

MIT. See [LICENSE](LICENSE).

The code here is original, but the work it reimplements is not: mp3packer is
copyright © 2006-2012 Reed Wilson and is GPL-2.0-or-later.

