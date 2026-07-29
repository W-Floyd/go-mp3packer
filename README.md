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

On speed we are ahead. Both invoked as commands, on an Apple M4 Max:

| | `go-mp3packer` | `mp3packercpp` |
| --- | --- | --- |
| 8-second VBR, 180 kB | 4.8 ms | 22.4 ms |
| 2m38s, 192 kbps CBR, 3.8 MB | 24.4 ms | — |

`BenchmarkCLI` and `BenchmarkReference` are the pair, both timing a subprocess
over a file, so the exec and the disk are in each. To reproduce, and to fill in
the figure this machine has no binary for:

```sh
export MP3PACKER_BENCH_FILE=/path/to/some/minutes/of/music.mp3
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run TestReference -v .
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run XXX -bench 'CLI|Reference' .
```

For measuring changes to the code rather than comparing implementations, see
[PERFORMANCE.md](PERFORMANCE.md).

## Performance

Repacking two and a half minutes of music on an Apple M4 Max: 197 ms on one
worker, 17.9 ms across sixteen. From the first working version that is a little
over tenfold either way, by quite different routes.

The one thing worth knowing before touching any of it: a fifth of the wall clock
is parsing and laying the frames back out, and no number of workers can share
that. So a change is meaningless without a worker count attached — folding the
frame CRC through a table is worth 2% on one worker and 18% across sixteen, and
the AVX2 row batching is worth 4.6% of one worker and nothing at all of sixteen.

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

