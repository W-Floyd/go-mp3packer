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
| bits written through an accumulator, decode positions kept in registers | 14.3 |
| memoised region costs no longer called once per candidate | 13.6 |
| prefix covers batched, losing candidates rejected before the call | 13.5 |
| window-switched geometry searched in its own loop | 13.0 |
| encoder's accumulator held in registers | 12.6 |

Each row from `boundary tests` downwards was measured against the row above it in
the same sitting. Between sittings the same code moves by a few percent — the
`prefix rows stored pre-scaled` row measured 21.7 rather than 20.5 when it was
re-run, and `memoised region costs` measured 13.9 — so read these as steps
relative to their neighbours rather than as one continuous scale. The last row is
an example: its sitting read 13.81 for the row above it and 13.38 for itself, and
it is the ratio between those two that is carried onto the 13.0 anchor.

Interleave in a rotating order, not a fixed one. Running A, B, C and then A, B, C
again gives whichever binary always goes last a systematic penalty on a machine
that is warming up — the encoder row first read as a 1.3% *regression* measured
that way, and came out level once the order rotated.

The table is arm64. Every step since the kernels became architecture-specific has
been re-measured on a Xeon E5-2698 v4, on two and a half minutes of music as well
as on the file above, and all of them hold there:

| | arm64 | x86-64 |
| --- | --- | --- |
| tail of the bit window read from a pad | −3.4% | −2.0% |
| bits through an accumulator, decode positions in registers | −7.5% | −4.2% |
| memoised region costs not called per candidate | −1.1% | −1.8% |
| prefix covers batched, candidates rejected before the call | −3.6% | −5.8% |
| window-switched geometry in its own loop | −2.0% | −0.9% |
| encoder's accumulator in registers | −4.8% | −5.3% |

These are one worker on the two-and-a-half-minute track. The window-switched row
is the weakest of them: it wins four of six rotated pairs there, against a clean
−3.3% on the dense eight-second file, which is the gap between real music and
material that over-represents the search. It is also the row with least to win —
only one to four granules in a hundred switch — so a smaller number on real music
is what it should read, not a disappointment.

The pad is the one worth checking rather than assuming: it doubles a Reader from
32 bytes to 64, and a 32 kB L1 is less forgiving than a 128 kB one. It pays on
both anyway, because a Reader lives on its caller's stack and there is one per
frame, not one per granule — so the cost is a second cache line on a hot frame,
not traffic. The two calls it removes per symbol are worth more than that
everywhere, though noticeably more on the wider core.

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

The same accounting applies to a function that does nothing. The head and prefix
costs are memoised on how far they have already been computed, but that test sat
inside them, and both hold loops, so neither can inline: every big_values
candidate paid a real call to be told there was nothing to do, tens of thousands
of times per file. Moving the test to the call site — only a candidate that
reached a new band boundary has anything to add — is worth 5% of the repack for
two lines.

None of that accounting is something the language promises. The budget of 80 and
the 57 a call costs are internals of `cmd/compile`, free to move in any release,
and if they move the reader quietly goes back to calling per symbol with every
test still green. `TestHotPathsStillInline` asks the compiler directly, by
building the package with `-gcflags=-m` and requiring the functions that are
called per pair and per field to appear as inlined. It builds a copy under a
module path unique to the run, because a cached build does not compile and so
reports nothing, and it fails rather than passes if it sees no inlining decisions
at all.

**Not going back to memory.** Measured on real music rather than the eight-second
file, the coder was the whole cost: `Decode` a third of the profile and `Encode`
a fifth, with the search behind them. Neither was doing much arithmetic. The
writer merged every field into the buffer with a read-modify-write — load eight
bytes, byte-swap, or, byte-swap, store — for each of the two or three fields a
coefficient pair emits. Pending bits now sit in a register and reach the buffer
eight bytes at a time, so a field is a shift and an or, and the buffer is never
read at all: a flush stores a whole word and commits only the bytes it filled, so
the next flush overwrites the rest. That took the writer from 12% of the profile to
under 5%, and means the output buffer no longer has to arrive zeroed.

The decoder's problem was the same shape. Its bit position lived in the Reader, so
every symbol loaded it three times and stored it once, and the store-to-load
turnaround sat on the loop's critical path — each symbol waiting on the previous
one's bookkeeping rather than on its own work. The coefficient index had the same
trouble for a different reason: it was captured by the closure that decoded a
region, which forces it to memory. Passing both through plain functions and
peeking at an explicit position keeps them in registers, and takes the big-values
loop from 32% of the profile to 25%. Together the two are worth 4% of a repack on
x86 and 7% on arm64.

**Batching the other endpoint.** The tail costs were batched long ago because
every span they cost shares its upper endpoint. The prefix search has exactly the
same shape and had been missed: for a given boundary it costs up to eight spans
that all *end* there, one kernel call each, and at three nanoseconds a call most
of that was getting there. It needs no second kernel, only the totals it already
had: the batched kernel scales its shared endpoint itself, so keeping an unscaled
copy of each boundary's row alongside the pre-scaled one lets the prefix search
hand it straight over. Three kilobytes of scratch for a fifth of the prefix cost.

Two smaller things in the same pass. The routine that keeps the best candidate
cannot inline — it holds the tie-break and every winner field, and reaching it
spills six arguments — yet it rejects almost everything handed to it on a single
comparison; making that comparison at the call sites leaves the call for
candidates that might actually win. And the count1 tail costs were built from the
top of the spectrum every granule, though nothing above twice the largest
big_values is ever read: starting two positions above the last non-zero
coefficient, which is all the recurrence needs to seed itself, skips a walk that
wrote zeros nobody looked at.

### Where the cores go

A worker per core gets 9.7× out of sixteen, and it is worth knowing why it is not
sixteen, because the answer is not in the search at all. Timing the stages of a
repack of two and a half minutes of music, one worker against sixteen, each the
median of fifteen runs:

| | −j 1 | −j 16 |
| --- | --- | --- |
| parse and build the reservoir view | 1.54 ms | 1.69 ms |
| recompress | 184.05 | 15.82 |
| lay the frames back out | 1.78 | 1.79 |
| **serial share of the total** | **1.8%** | **18.0%** |

The ratio has been falling as the parallel stage gets faster: it was 10.4× when
the search was slower, and every step that only speeds up recompression pushes it
down. That is not a regression, it is the serial floor becoming a larger share of
a smaller total, and it is the argument for spending effort on the two stages
either side of it rather than on the search.

Build with `-tags mp3timing` for those three figures: `Stats` carries them and `-v`
prints them. They are compiled out by default, because reading the clock four times
costs about a hundred nanoseconds a file — nothing against a repack, but a
measurable fraction of the layout-only benchmark, which is one of the benchmarks
used to judge the layout pass.

The recompression itself scales 11.6× across twelve performance cores and four
efficiency ones, which is about what four half-speed cores predict. What caps the
total is the sixth of the wall clock that never shrinks. That is also why a kernel
win measured on one worker mostly vanishes across all of them — the AVX2 row
batching is worth 4.6% of one worker and nothing at all of sixteen — and it cuts
the other way too: the frame CRC below was worth 1% of one worker and a sixth of
the run on sixteen.

Three likelier-sounding explanations are not the cause. Garbage collection is not:
the collector runs eight or nine times whatever the worker count, pausing about
35 µs, and forcing `GOGC=off` leaves the curve unchanged. Memory bandwidth is not:
the eight-second file, with a twentieth of the data, scales better rather than
worse. Nor is it the tail of uneven frames, since six thousand frames across
sixteen workers leaves nothing much to straggle. The runtime lock contention and
idle spinning that show up in a parallel profile — `findRunnable` taking
`sched.lock`, then `osyield` — are fifteen processors with nothing to do during the
serial stages, which is a symptom of the same thing.

So the lever on parallel throughput is the layout pass, not the coder:
`BenchmarkRecompressWorkers` measures the curve, and `BenchmarkLayoutOnly` measures
the thing that bounds it.

**The serial stage was mostly a CRC.** A protected frame stores a checksum over its
side information, and rewriting the side info means recomputing it. Written from
the definition that is eight branches a bit, about thirty bytes a frame, and on a
CRC-protected file it was 40% of the layout pass — the largest single cost in the
one stage that no number of workers can share. Folding a byte at a time through a
256-entry table takes the layout of a two-and-a-half-minute track from 5.6 ms to
2.1 ms. On one worker that is worth 1%; across sixteen it is 17%, because it comes
entirely out of the fifth of the wall clock that was serial.

It is also invisible on most of the test corpus, which is why it survived this
long: only two of the eight files carry CRCs, and the file the layout benchmark
used was not one of them. Real encodes do — the track measured above was
downloaded rather than made for the tests.

So the benchmark was rebuilt around that. `BenchmarkLayoutOnly` now runs on the
one protected corpus file repeated sixteen times, about 320 KB and 770 frames,
which is both long enough to settle to a couple of percent and protected enough
to do the CRC work at all. On the old 28 KB unprotected input the CRC fold above
was unmeasurable; on this one it is 762 µs against 452 µs, a clear 1.7×.
`TestLayoutBenchInput` pins the two properties the measurement depends on, so a
corpus change cannot quietly take them away again. Note that the benchmark times
the whole serial path, and layout is only about a quarter of it — parsing is the
rest — so build with `-tags mp3timing` for the two stages reported separately.
Read those for attribution and the total for A/B: the per-call stage clocks are
much noisier than the wall clock.

**The reservoir was built twice.** `layout` placed every frame's data into a
`stream` buffer and then copied that buffer into the output — a second copy of
the whole audio, and an allocation to hold it. The first of its two passes never
needed the bytes: choosing a frame's size and its reservoir offset takes lengths
only. It now records the pieces instead — each frame's data, and the gap runs
left where the reservoir cannot reach back far enough — and the emitting pass
reads them straight out of the frames' own buffers. A frame's slot is a window
over that sequence and generally spans more than one piece, which is the whole
point of a bit reservoir, so the read side is a cursor rather than a slice.

That is 3.27 ms to 3.11 ms on the serial path of the long track, winning all five
interleaved pairs, and 26.9 MB to 19.4 MB of allocation per repack. The
allocation saved is twice the size of the audio, because the gap padding pushed
`stream` past the capacity it had been given and it doubled. Across all cores it
is worth about 0.9%, which does not clear the noise there. It earns no row in the
step table above, which is one worker on the eight-second file and cannot see a
serial change at all; where it shows is the stage table under *Where the cores
go*, as layout falling from 2.48 ms to 1.78.

Caching `Frame.MainDataBits` was the other half of this item and was dropped.
Recomputing it — a loop over granules and channels summing `part2_3_length`, from
four call sites — was 30 ms of a 440 ms profile when it was first noted. On the
current code it is 1.4% of the serial-path profile and does not appear in the
one-worker profile at all, which is around 0.3% of an all-cores repack for a
cache that has to be threaded through four callers. Everything around it got
faster and it stopped being worth the plumbing.

**The search that was not one.** A window-switched granule has no split to
enumerate: the standard fixes where region0 ends, at the ninth long band or the
third short band across all three windows, and there is no third region. Two spans
and no choice of geometry. It had been sharing the long-block loop anyway, which
batches every band boundary's span before looking at any of them, so each
candidate costed up to two dozen spans in order to read one — and for a short
block, whose boundary is not one of the band boundaries at all, the span it needed
was not even in the batch, so a second call fetched it separately. Giving that
geometry its own loop, asking for the one span that moves and settling the one that
does not on first use, more than halves the search for those granules.

Sharing the loop was costing something in the other direction too: with both cases
in it, the long-block path carried a test and a page of unrelated code between the
loop head and its own candidate enumeration, worth 3% until the two were separated.
The gain end to end is larger than the 1% to 4% of granules that switch, because
under the old arrangement those were the most expensive granules in the file rather
than the cheapest.

**The encoder was writing through memory.** Appending a field went through
`Writer.put`, which the inliner will not take, so every coefficient pair cost a
load of the accumulator and its bit count, a store of both, and the store-to-load
turnaround between one pair and the next. That is the same round trip `PeekAt`
exists to spare the reader, and the write side never had an answer to it: the
field group was already assembled in a register, and then handed to a call that
put it straight back in memory. `Encode` now takes the accumulator with
`Pending`, appends to it in two locals for the whole granule, spills with `Store`
only when the next field would not fit — every three or four pairs — and hands it
back with `Resume`. Nothing about the bits changes; all 24 corpus outputs are
byte-identical. `BenchmarkEncodeGranule` goes from 827 ns to 618 ns, a quarter
off, and `Write64` at 51% of `Encode` becomes `Store` at 12%. End to end on the
two-and-a-half-minute track that is 197 ms to 187 ms on one worker, 4.8%, which
is what `Encode` being 18.8% of a worker predicts. The Xeon agrees: 5135 ns to
4079 on the granule, 723 ms to 685 on the track.

This is work that parallelises, so unlike the frame CRC it is worth *less* across
all cores rather than more, and by different amounts on the two machines: 20.1 ms
to 19.6 on sixteen, 2.7%, against 51.1 ms to 48.7 on forty, 4.7%. The serial floor
explains the gap. It is close to a fifth of the arm64 all-core run and much less
of the Xeon's, so the same saving in the parallel part is diluted more on the machine
that has less parallel part left. Both were measured as eight interleaved pairs of
twenty runs, which is the least that separates them: at three pairs the arm64
figure read 1.9% one way and 3.5% the other depending on whether the runs were
summarised by median or by mean.

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

Giving the encoder's pair loop the treatment that worked on the decoder was tried
twice and dropped both times. The decode side got 5% from resolving a symbol
through `pairDecode` and appending signs branchlessly, and the write side looked
like it had the same shape: `abs`, two clamps and two escape tests before the
table lookup. Neither is where the time is. Folding the sign counts and the
linbits width into the codeword entry, so nothing in the loop branches or tests
for zero, cost 2%: the escapes are rare and perfectly predicted, and the masking
that replaced them is not free. Folding just the `linbits > 0` half of the escape
test into an out-of-range sentinel, as `decodeRegion` does, measured flat — the
compiler was already hoisting it. A profile of `BenchmarkEncodeGranule` says why
both were beside the point: `Write64` is 51% of `Encode` and `abs` is 0.9%. The
coefficient arithmetic was never the problem, which is what the next section is
about.

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

`FuzzProcess` drives the whole repack from arbitrary bytes, since `Process` is an
exported entry point taking untrusted input and the code that indexes its way
through it moves in most commits. Refusing an input is always allowed; panicking
is not, nor is returning success beside something that will not parse.

It found a real fault on its first run, and in the parser rather than the packer.
`findSync` would only lock onto a run of fewer than three frames within 64 bytes
of the end of the data, but a frame is 26 bytes at the bottom of MPEG-2 and 1440
at the top of MPEG-1, so a lone frame bigger than that window could not be found
at all — and layout grows a frame whose payload needs the room. A 27-byte frame
declaring 3442 bits came back out as a perfectly good 470-byte one that our own
parser then rejected. Room is now measured by scoring each candidate rather than
against a fixed count of bytes: a longer chain wins, a chain that fits in the
data beats one that overruns it, and between two that fit, the one leaving less
of the file unexplained wins. A real file still returns at its first candidate,
so nothing about the normal path changed.

Three of the target's own assertions turned out to be wrong rather than the code,
and telling those apart from faults was most of the work:

- A leading Xing/Info frame is copied byte for byte, so `-no-crc` does not reach
  it and it keeps whatever CRC it arrived with.
- Junk has to be checked against the output bytes, not against a re-parse of
  them. Parsing is a search, and where the junk holds something that reads as a
  frame header the result has two readings; the repack changes which one scores
  best by changing the length and position of what follows. No encoder produces
  such a file and the fuzzer produces little else.
- A codeword is read whole once it has started, so one straddling
  `part2_3_length` pulls in the bytes after the granule — which are ancillary
  data, which a repack is free to drop, and does. Comparing decoded spectra had
  to stop reading there, or two files that decode to the same audio compare as
  different. A granule needing those bytes is reported as such, and our own
  output is held to never being one.

Run it with `go test -fuzz FuzzProcess`. The saved crashers under
`testdata/fuzz/` are regression seeds and run as part of the ordinary suite.

`BenchmarkOptimizeGranule`, `BenchmarkDecodeGranule` and `BenchmarkEncodeGranule`
time the three halves of the work over a fixed spread of granules. The end-to-end
benchmarks cannot say which of the three a change moved without a profile, which
makes a one-percent step hard to judge; these can be read directly.

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
