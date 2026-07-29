# go-mp3packer

Losslessly recompress MP3 files, in Go. Same audio, smaller file.

An MP3 stores its spectrum with Huffman code tables chosen from a fixed set, and
encoders pick those tables heuristically. `go-mp3packer` re-codes every granule
with the cheapest tables the format allows, then re-lays the result out across
frames so that as little padding as possible is left over. Nothing is
re-quantized and nothing is re-encoded in the lossy sense, so the decoded audio
is bit-for-bit identical to the input — the same relationship a recompressed ZIP
archive has to the original.

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
  -j N       recompression workers (0 = one per CPU)
  -f         overwrite the output file if it exists
  -v         log per-frame details
  -q         print nothing on success
```

Output is written to a temporary file and renamed into place, so an interrupted
run cannot truncate your music, and in-place repacking is safe.

## As a library

```go
import mp3packer "github.com/W-Floyd/go-mp3packer"

stats, err := mp3packer.ProcessFile("in.mp3", "out.mp3", mp3packer.Options{
    Recompress: true,
})
```

`Process` does the same thing for data already in memory. The lower layers are
exported and usable on their own:

- `mp3` — frame headers, side information, CRC, Xing/Info tags, and a parser that
  hands back every frame of a file with its main data.
- `huffman` — decode a granule's quantized spectrum, search for its cheapest
  legal coding, encode it again.

## What it saves

Eight seconds of test audio, encoded by LAME, then repacked:

| file | input | `-n` (layout only) | default (`-z` equivalent) |
| --- | --- | --- | --- |
| VBR, joint stereo | 180098 | 179708 | 179295 |
| CBR 192 | 193723 | 193149 | 193102 |
| CBR 320 | 322872 | 322142 | 322139 |
| CBR 320, quiet audio | 322872 | 322142 | 288542 |
| VBR mono | 129148 | 128580 | 128583 |
| MPEG-2, 22050 Hz | 96484 | 96391 | 96388 |
| VBR with ID3v2 tags | 49523 | 49081 | 48365 |

Typical savings on real encodes run from a few tenths of a percent to a few
percent; files whose bitrate is far above what the audio needs — CBR 320 of quiet
material being the extreme case — lose much more. Whether any of that is worth
the rewrite is your call.

Two rows deserve a note. Recompression is not the whole story: `-n` alone removes
padding, which is why it gets most of the way on dense files. And on the mono row
the layout-only output is three bytes *smaller*, because once every frame has
already shrunk to the minimum frame size, the space freed up by better
compression cannot be given back — it becomes padding, and the reservoir bookkeeping
can spend a byte or two more of it.

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

On speed we are now well ahead: 4.3 ms versus 22.6 ms to repack an 8-second VBR
file (Apple M4 Max, both multi-threaded; see below).

To reproduce, point the benchmarks at any other implementation that accepts
`-z in out`:

```sh
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run TestReference -v .
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run XXX -bench 'Recompress$|Reference$' .
```

## Performance

Repacking an 8-second VBR file, one worker, so that the numbers reflect the
search itself rather than the core count (Apple M4 Max):

| | ms |
| --- | --- |
| first working version | 190 |
| cost tables laid out pair-major | 132 |
| memo packed, winner tracked as scalars | 121 |
| NEON / SSE2 kernels | 101 |
| region search factored by shape | 45.6 |
| decode from a 64-bit window, no 4.6 kB spectrum copies | 38.4 |
| batched encoder writes | 36.2 |

With all cores it is 4.3 ms for the same file. Every step was verified by
comparing output byte for byte against the previous one, so none of this changed
a single bit of any result.

Two of those steps carried the work; the rest were ordinary tuning:

**Layout.** The search costs a region by subtracting two per-table prefix sums.
Keeping those sums table-major meant every query strided across 32 separate rows;
pair-major makes it two cache lines. Only 24 prefixes are ever needed — the
scalefactor band boundaries plus wherever big_values currently sits — so the
accumulator walks the pairs once and snapshots as it goes, instead of
materialising a row per pair.

**Factoring.** The obvious search enumerates every (region0_count, region1_count)
pair for every big_values: measured at 11,000 inner iterations and 24,000 region
lookups per granule, against only 2,400 that actually reached the vector kernel.
But regions 0 and 1 do not move as big_values does. Their best combination is a
property of the boundary they end at, so it is computed once per boundary and
reused, leaving only the tail to recompute. Same answers, a third of the work.

A lower bound on each big_values, to skip candidates that cannot win, was tried
and removed: it prunes almost nothing, because moving a coefficient pair between
the big-values and count1 regions barely changes the total.

### Assembly

Two kernels have hand-written arm64 (NEON) and amd64 (SSE2) implementations, with
portable Go equivalents for everything else:

| | asm | Go | |
| --- | --- | --- | --- |
| `accumulate` — add 288 pairs' costs across all 32 tables | 262 ns | 3323 ns | 12.7× |
| `bestTable` — cheapest of 32 tables for one region | 3.0 ns | 10.9 ns | 3.7× |

Both are 32 lanes wide with no gathers, which is the whole reason they vectorise:
a pair's cost for every table is one row of a precomputed table, indexed by the
pair's clamped magnitudes, and the escape penalty is a second row indexed by how
many linbits it needs. `bestTable` packs each cost above the table index so that
one unsigned minimum yields both the cost and the winning table, ties included.
`TestKernelsMatchPortable` compares the assembly against the Go implementations
on randomised input, so the fallbacks stay honest.

The SSE2 path sticks to the amd64 baseline: `PMINSD` would shorten `bestTable`,
but not enough to justify a runtime feature check. It is tested on x86-64 in CI;
the figures above are arm64.

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

## Not implemented

The original mp3packer does more than recompress. Out of scope here: CBR/VBR
conversion and minimum-bitrate control (`-b`), the reservoir placement switches
(`-r`, `-R`), tag stripping (`-s`, `-t`), directory processing, the `-i`
information report, and error concealment for damaged frames — broken frames are
copied through as they are rather than silenced.

## Licence

MIT. See [LICENSE](LICENSE).

The code here is original, but the work it reimplements is not: mp3packer is
copyright © 2006-2012 Reed Wilson and is GPL-2.0-or-later, as is the C++ port.
The Huffman code tables and scalefactor band tables are the constants defined by
ISO/IEC 11172-3 and 13818-3 and appear identically in every layer III
implementation.
