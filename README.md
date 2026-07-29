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

On speed we are well ahead: 1.6 ms versus 22.4 ms to repack an 8-second VBR file
(Apple M4 Max, both multi-threaded; see below).

To reproduce, point the benchmarks at any other implementation that accepts
`-z in out`:

```sh
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run TestReference -v .
MP3PACKER_REFERENCE=/path/to/mp3packercpp go test -run XXX -bench 'Recompress$|Reference$' .
```

## Performance

Repacking an 8-second VBR file, one worker, so the numbers reflect the search
itself rather than the core count (Apple M4 Max, best of six runs):

| | ms |
| --- | --- |
| first working version | 190 |
| cost tables laid out pair-major | 132 |
| memo packed, winner tracked as scalars | 121 |
| NEON / SSE2 kernels | 101 |
| region search factored by shape | 45.6 |
| decode from a 64-bit window, no 4.6 kB spectrum copies | 38.4 |
| batched encoder writes | 36.2 |
| tail costs batched into one kernel call | 25.1 |
| bit reader and writer down to one 64-bit access each | 25.1 |
| eight-bit lookup tables for Huffman decode | 23.1 |
| count1 costs built only over the range searched | 22.6 |
| winner comparison hoisted, head costs precomputed | 21.3 |
| prefix rows stored pre-scaled | 20.5 |
| boundary tests reduced to arithmetic on one counter | 18.3 |
| dead two-region candidates never entered | 17.7 |
| count1 deltas tabulated, region covers hoisted out of the bv loop | 17.1 |
| tail reduction batched four rows at a time (NEON) | 17.0 |
| count1 quadruples and pair signs decoded without branching | 16.3 |
| frame list and per-frame output preallocated, encoder invariants hoisted | 15.1 |
| peeking the bit window inlined into its callers | 14.9 |

The last six rows were measured in one sitting, in which the row above them came
out at 21.7 rather than the 20.5 recorded when it was new; treat the steps as
relative to each other rather than to the older figures.

With all cores that file takes 1.6 ms. Every step was verified by comparing
output byte for byte against the previous one, so none of this changed a single
bit of any result.

Three of those steps carried the work; the rest were ordinary tuning.

**Layout.** The search costs a region by subtracting two per-table prefix sums.
Keeping those sums table-major meant every query strided across 32 separate rows;
pair-major makes it two cache lines. Only 24 prefixes are ever needed — the
scalefactor band boundaries plus wherever big_values currently sits — so the
accumulator walks the pairs once and snapshots as it goes, instead of
materialising a row per pair.

**Factoring.** The obvious search enumerates every (region0_count, region1_count)
pair for every big_values: measured at 11,000 inner iterations and 24,000 region
lookups per granule, against only 2,400 that reached the vector kernel. But
regions 0 and 1 do not move as big_values does. Their best combination is a
property of the boundary they end at, so it is computed once per boundary and
reused, leaving only the tail to recompute. Same answers, a third of the work.

**Batching.** What remained was one kernel call per region per candidate, and at
three nanoseconds a call two thirds of that was call overhead. Every span that
moves with big_values shares its upper endpoint, so they are now computed
together: one call per candidate that keeps the shared endpoint in registers and
walks the rows. That alone took 36 ms to 25.

**Counting instead of searching.** With the kernels that fast, what was left was
the enumeration around them: for every candidate big_values the search walked the
band table to find where the regions could fall, four separate times, one of them
nested. But the band boundaries are sorted, so every one of those questions is the
same question — how many boundaries lie below big_values — and `boundary[i] >= bv`
is just `i >= nTail`. Counting that once, and carrying it across candidates since
big_values only grows, replaces all of it with arithmetic: the innermost search
for the smallest region1_count that reaches the tail becomes one subtraction. The
two-region loop went from 630 ms to 40 ms of a 5.2 s profile, and the best
two-region cover below each boundary, which was being re-checked for every
candidate after being computed once, is now settled the same way the head costs
already were.

Repacking allocated more than it needed to. `Capacities` built a 30-element slice
per frame, twice, once only to read its last element; the side-info writers and the
reservoir grew from nil; and a `Frame` is over a kilobyte, so several loops were
copying one per iteration by ranging over values. None of that is on the
recompression path, but it is most of the layout-only path, which dropped by a
third, and it cut allocations per repack from 2032 to 676.

What was left of it was sized wrong rather than unnecessary. The parser grew its
frame list from nil, and a `Frame` is over a kilobyte, so a file of any length
spent hundreds of kilobytes being copied forward; every frame in a stream shares a
sample rate and hence a duration, so the first frame's size predicts the rest,
exactly for CBR and closely enough for VBR that the slice grows once. The
per-frame stage allocated a buffer per frame, but each frame's output is bounded
by its input, so the sizes are all known before any of it runs: it now writes into
disjoint slots of one buffer, which is also what makes it allocation-free with a
worker per CPU. Together with copying a frame's reservoir span in one `copy`
instead of a bounds-checked loop per byte, that is 707 allocations per repack down
to 310, the layout-only path down 19%, and — because the workers are no longer
competing with the collector — 7% to 15% off recompression across the test files,
more than the 4% it saves on a single worker.

Two things in the encoder were paying per pair for facts that hold per region: the
table's linbits, the address of its code table, and whether the region codes
anything at all were re-read for every pair, when a region has one table by
definition. The sign bits went the way the decoder's already had — appended by
shifting the word by zero or one rather than branching on data that cannot be
predicted — and the search's two backward bounds scans were fused, since no pair
above the last non-zero coefficient can hold a magnitude over 1, so the second
scan can start where the first one ended instead of at the top of the spectrum.

**Not branching on the data.** What a decoder branches on is mostly the audio:
whether a coefficient is zero, and which way its sign points. Neither is
predictable, so each such branch costs a misprediction about half the time. None
of them are needed. A sign applies arithmetically — `x^-1 + 1` is `-x`, `x^0 + 0`
is `x` — and that expression leaves zero alone whichever way the sign bit falls,
so the only thing that still depends on the value is how far to advance the bit
window: a shift by the sign count, which a table supplies as 0 or 1. The count1
quadruples went the same way. Their four values and four signs are now one lookup
keyed by the symbol and the next four bits, tabulated over the bits that belong to
the following codeword so that they cannot matter. Whether a table escapes to
linbits is fixed per region, so instead of testing it per pair the escape trigger
is set out of range for the tables that have none. Together this cut the decoder's
own work by about a third and the whole repack by 5% on both architectures.

**What the inliner is really counting.** `Peek64` runs once per coefficient pair
and was not being inlined; the comment above it blamed the 64-bit load, and that
was wrong. The load scores 10 against a budget of 80. A probe containing nothing
but the bounds test and the out-of-line call for the zero-filled tail scores 82: a
call the inliner cannot see through costs a flat 57, so the `//go:noinline` put on
the tail path to keep the hot path lean was itself what kept the hot path out of
line. Letting the tail inline instead does not fit either, at 92, and it would drag
its loop into the caller. What fits is having no call in the function at all. The
reader now carries a sixteen-byte zero-padded copy of its own tail, and a peek near
the end reads from that; every index at or past the end of the data clamps into the
pad's zero region, so the far tail needs no bounds handling beyond that clamp. The
whole function becomes a branch and a load, at a cost of 44. `Read` came along
free, having scored 81 — one point over — and that is thirty-odd calls per frame of
side info, so the layout-only path dropped another 10% on top of the search's 6%.
It costs 40 bytes of `Reader`, which is stack-allocated in every hot path, and it
means a reader has to be built through `NewReader` rather than as a bare literal.

Two things were tried and dropped. A lower bound on each big_values, to skip
candidates that cannot win, prunes almost nothing — moving a coefficient pair
between the big-values and count1 regions barely changes the total — and it cost
5% as a monotone loop break. It was tried a second time in its tightest honest
form, the exact cheapest coding of each pair with every pair free to choose its
own table, tabulated over the twelve key bits and summed as a prefix so that a
candidate could be dismissed before a single region was costed. A bound that loose
cannot compete with what three regions actually achieve: it fired on 707 of 206,885
candidates, 0.34%, and the 16 kB table it needs made recompression 6% slower.
Rewriting the bit reader and writer around single
64-bit accesses is clearly faster in isolation but did not move the total; it is
kept because it is also simpler than the byte-at-a-time version it replaced.

Narrowing `Spectrum` from `int` to `int32` was tried and reverted. It halves the
4.6 kB a granule occupies and every copy, clear and compare of it, which looked
worth having on a 32 kB L1; measured on both an M4 Max and a Xeon E5-2698 v4 it
was worth nothing at all, and it changes an exported type, so it went back.

### Assembly

Three kernels have hand-written arm64 (NEON) and amd64 (SSE2/SSE4.1/AVX2)
implementations, with portable Go equivalents for every other architecture:

| | asm | Go | |
| --- | --- | --- | --- |
| `accumulate` — add 288 pairs' costs across all 32 tables | 252 ns | 3122 ns | 12.4× |
| `bestTails` — cheapest table for 22 spans sharing an endpoint | 24 ns | 247 ns | 10.1× |
| `bestTable` — cheapest of 32 tables for one span | 2.9 ns | 13.0 ns | 4.4× |

All three are 32 lanes wide with no gathers, which is the whole reason they
vectorise: a pair's cost for every table is one row of a precomputed table,
indexed by the pair's clamped magnitudes, and the escape penalty is a second row
indexed by how many linbits it needs.

The cost is packed above the table index so that a single unsigned minimum yields
both the cost and the winning table, ties included. Prefix rows are therefore
stored pre-scaled by 32, which leaves the low bits free: subtracting a scaled row
from a scaled-and-labelled endpoint produces the packed answer directly, with no
shift per row. `TestKernelsMatchPortable` compares the assembly against the Go
implementations on randomised input, including rows that make a span
unrepresentable, so the fallbacks stay honest.

`bestTails` reduces four rows at once. Folding a row's last four lanes on its own
needs a shuffle-and-minimum chain and then a lane-to-register move, all serial and
all per row; transposing four partial results instead turns four of those folds
into three minimums and lets the four answers leave as one 16-byte store. On arm64
that is worth about 17%. It would be shorter still with `UMINV`, a single
horizontal minimum, but Go's arm64 assembler cannot yet emit one — it is in the
`simd` package's arm64 generator input, so if that lands these kernels could
collapse into one Go source instead of two `.s` files.

On amd64 each kernel has up to three forms, chosen once by `CPUID`. SSE2 is the
baseline and has no 32-bit minimum at all, so every one costs six instructions to
emulate — and the reductions are almost nothing but minimums, which is what earns
the first feature check: SSE4.1's `PMINUD` does it in one.

AVX2 then halves the register count, because 32 `int32` lanes are four 256-bit
registers rather than eight 128-bit ones, and its three-operand encoding takes a
memory source directly. That second part matters more than the width: SSE is
two-operand and destructive, so almost every instruction needs a register copy
first, and those disappear. A row of `bestTails` becomes four subtracts straight
out of memory and three minimums, against eight loads, eight copies, eight
subtracts and seven minimums. `accumulate` gains most, having had no SSE4.1 form
at all — its eight load-and-add pairs collapse to four adds with memory operands.
Measured on a Xeon E5-2698 v4 at a locked 2.2 GHz:

The AVX2 `bestTails` also batches four rows and transposes, as the NEON one does.
Once the subtracts came free that fold was most of what was left: six serial
instructions and a 4-byte store per row, against 3.5 amortised instructions and a
quarter of a store. The unpacks work inside each 128-bit half, so both halves
transpose at once and one cross-half minimum finishes all eight lanes of all four
rows.

| | SSE2 | SSE4.1 | AVX2 | |
| --- | --- | --- | --- | --- |
| `accumulate` | 1004 ns | — | 574 ns | 1.7× |
| `bestTails` | 255 ns | 116 ns | 65 ns | 3.9× |
| `bestTable` | 16.0 ns | 11.7 ns | 8.6 ns | 1.9× |

End to end, each step against the one before it on the 8-second file above, one
worker: SSE4.1 to AVX2 took 66.3 ms to 61.5, and batching the rows took 61.1 to
58.3. Both are far less than the kernel figures suggest, for the ordinary reason —
the three kernels are a quarter of that file's x86 profile, so a third off them is
under a tenth off the total.

That file also flatters them. On two and a half minutes of 192 kbps CBR music,
`bestTails` is 10% of the profile rather than 19%, and the row batching is worth
1.8% rather than 4.6%; `Decode` and `Encode` are 22% each, which is where the time
actually goes on ordinary material. Nothing here is measurable at all across every
core, where the work is bound by memory rather than by arithmetic. The eight-second
file is a fair yardstick for the search itself, which is what it was chosen for,
but it is not a fair guide to what a kernel change is worth in practice — hence
`BenchmarkRecompressFile`, which takes a path from `MP3PACKER_BENCH_FILE` so a real
track can be measured without carrying one in the repository.

Detection asks for more than the AVX2 feature bit: the YMM registers are only
usable if the operating system has enabled saving them, so `OSXSAVE` and `XGETBV`
are checked too, and every AVX2 path ends in `VZEROUPPER` — returning to Go's SSE
code with the upper halves live costs far more than the kernel saves. Every x86
since about 2013 takes the AVX2 path and everything since 2008 takes SSE4.1, so
the narrower forms would otherwise never run anywhere:
`TestKernelsDispatchPaths` forces each one in turn and holds it to the portable Go.

The figures in the first table are arm64. x86-64 is tested in CI, and all three
amd64 paths were verified and benchmarked on hardware.

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
