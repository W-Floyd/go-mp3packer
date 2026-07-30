# go-mp3packer — working notes

Hard-won context for anyone (human or agent) changing this code. Remaining work
lives in [TODO.md](TODO.md); [PERFORMANCE.md](PERFORMANCE.md) carries the measured
history, the cross-architecture table, the scaling decomposition, the assembly and
a tried-and-dropped list.

## Verification protocol — please pass this on

Every commit in this series holds to it, and two changes would have shipped
wrong without it:

- Byte-identical output against the previous commit: 24 outputs (8 files ×
  `""`/`-n`/`-no-crc`, counting the long track `bards-tale.mp3`), on both arches
  and across them. This is the backstop that matters.
- The remote box has no Go toolchain or package sources, so two suites need care
  there: `mp3`'s tests want `../testdata`, so run that binary from a `mp3/`
  subdirectory, and `TestHotPathsStillInline` shells out to `go build`, so it
  cannot run cross-compiled at all. Both look like failures if you just run the
  binaries in one directory.
- For anything touching `Optimize`: compare the chosen `Config`, not just the bit
  count. `TestOptimizeMatchesExhaustiveSearch` compares costs only, so a tie
  broken differently passes it and silently changes output bytes. The scaffold
  used dumps `fmt.Fprintf(out, "%d %d %v %d", n, rate, cfg, bits)` over 400
  spectra × 6 sample rates × 6 geometries (long, short, short+mixed, start, stop,
  short with count1 table 33) = 14,400 searches, run in both trees and diffed.
- `benchCorpus` searches every granule with `WindowSwitching: false`.
  `BenchmarkOptimizeGranule` cannot see any change to the switched path — that's
  why `BenchmarkOptimizeGranuleSwitched` exists. Check which path a change
  touches before trusting a flat result.
- New amd64 asm must not fall through between dispatch paths. An early AVX2 draft
  let the SSE2 store block run into the AVX2 block with the counter at zero —
  `DECQ` wrapped to 2⁶⁴−1 and walked off the heap. Assembles clean, passes vet,
  impossible on arm64. `TestKernelsDispatchPaths` is what catches it.

### Fuzzing

`FuzzProcess` covers the three option combinations, seeded from `testdata/`.
Crashers are committed under `testdata/fuzz/` and run in the ordinary suite.

What it found on its first run was in the parser. `findSync` would only lock onto
a run of fewer than three frames within 64 bytes of the end of the data, but a
frame is 26 bytes at the bottom of MPEG-2 and 1440 at the top of MPEG-1, so a lone
frame bigger than that window could not be found at all — and layout grows a frame
whose payload needs the room. A 27-byte frame declaring 3442 bits came back out as
a perfectly good 470-byte one our own parser then rejected. Candidates are now
scored: a longer chain wins, one that fits the data beats one that overruns it, and
between two that fit, the one leaving less of the file unexplained wins.

Three of the target's own assertions were wrong rather than the code, and telling
those apart from faults was most of the work. Before adding assertions:

- **Never assert that a re-parse of the output agrees with the parse of the
  input.** Parsing is a search; junk and payload can both hold plausible headers,
  and the repack changes which reading wins by changing what follows. Assert
  against the *bytes*, and carve the frame region out by the lengths the input
  parse gave you.
- A leading Xing/Info frame is copied byte for byte, so `-no-crc` does not reach
  it and it keeps whatever CRC it arrived with.
- A codeword is read whole once it has started, so one straddling
  `part2_3_length` pulls in the bytes after the granule — ancillary data, which a
  repack is free to drop, and does. Comparing decoded spectra has to stop at the
  declared length, or two files that decode to the same audio compare as
  different. `decodeSpectra` reports a granule that needs those bytes, and our own
  output is held to never being one.

## How to A/B a change

Use the tool. Every hand-rolled harness in this project's history got something
wrong — a fixed run order, two halves built from the same tree, three pairs
mistaken for a sample — and each mistake produced a plausible number rather than
an error.

**Before committing**, which is when the answer decides something:

```sh
export MP3PACKER_BENCH_FILE=$PWD/bards-tale.mp3
go run ./cmd/benchsteps ab -input serial HEAD WORK
```

`WORK` is the working tree as it stands — modifications, new files and deletions
alike, everything git would not ignore. It is recorded as a dangling commit
through a temporary index, so your tree and your index are left exactly as they
were: no branch, no commit, nothing to undo, and nothing lost if the run is
interrupted, which these runs are. The build comes from a clean checkout of that
commit, which is also what keeps the generated harness files out of your tree.

Pick the input that can see the change — `serial` for anything in parsing or
layout, `short` for the search, `long1`/`longall` for a whole repack — and leave
`-input` off to get all four.

`WORK` runs are not stored: the code they measured stops existing the moment you
edit again, and the commit holding it is dangling and will be collected, so
pooling it later would be meaningless. The committed side's runs are kept.

**After committing**, name both:

```sh
go run ./cmd/benchsteps ab b15d4c7 212628e
```

It builds both, alternates which runs first, measures until each median's
standard error is under 0.5%, and prints the step with its error bar:

```
serial         2.799 ->     2.501 ms  -10.66% ±0.73  −10.7%
```

`≈` in the last column means the measurement cannot support a direction — see
*Reading these tables* in PERFORMANCE.md for what the thresholds are. These runs are stored and are
pooled into the step tables in PERFORMANCE.md, so a pair measured here is one `inject`
can draw on.

**Then put it in the history.** Add the commit to `bench/steps.json`, then
`run -sweep` and `inject`. Do not hand-edit the tables.

**Run nothing else while a sweep is going.** Not `go test`, not `go vet`, not
`gofmt`, not a build in another checkout. The lock in `results.json.lock` stops a
second `benchsteps` and can do nothing about ordinary development, which competes
for the same cores just as hard. A full sweep is ten to thirty minutes of the
machine being unavailable; that is the cost of the tables, and working through it
poisons them. It has happened: seventeen passes were thrown away because a
`go vet` and a `go test` were run between them.

Every run records its pass as well as its session, which is what makes that
detectable afterwards. A sweep measures every cell once per pass, so dividing each
timing by its own cell's median leaves the machine's conditions with the code
divided out — a pass 15% slow across all twenty commits at once was a busy
machine, not twenty simultaneous regressions. `run` warns when it sees one, and
the remedy keeps the rest of the session:

```sh
go run ./cmd/benchsteps prune -contaminated 1.05 -dry-run   # what would go
go run ./cmd/benchsteps prune -contaminated 1.05            # drop those passes
go run ./cmd/benchsteps prune -session 3                    # or the whole sitting
```

**What the tables are not.** They are the shape of twenty steps at once. A step
of a few per cent is buried in a twenty-row sweep and wants `ab`, which settles
the same question in a couple of minutes. Claims in the README that the tables
cannot resolve say so and cite the `ab` that produced them.

## Traps that cost time — worth knowing up front

- **`bench-vbr.mp3` overstates search work ~2×.** Dense 8-second VBR.
  `bestTails` is 19% of profile there, 10% on real music. Always confirm via
  `MP3PACKER_BENCH_FILE`.
- **Only 2 of 8 test files carry CRCs** (`cbr-crc.mp3` and `bards-tale.mp3`).
  Layout work is invisible without one. `BenchmarkLayoutOnly` builds its own
  input from the first of them for exactly this reason; any *new* layout
  benchmark has the same problem.
- **Interleave A/B runs in a rotating order, not a fixed one.** A fixed A, B, C
  penalises whichever binary always goes last on a warming machine. The encoder
  step first read as a 1.3% regression that way and came out level once rotated.
  `benchsteps` rotates; a loop you write by hand will not unless you make it.
- **1-worker and all-core diverge sharply.** Serial is 2% of one worker, 20% of
  sixteen. AVX2 batching: 4.6% / 0%. CRC table: 1% / 17%. Always say which.
- **Don't trust profile deltas between runs.** `Writer.put` read 4.6% and 8.9% on
  identical code. Use interleaved A/B wall-clock; profiles for *where*, not *how
  much*. Noise: `BenchmarkOptimizeGranule*` ±1–3%, `BenchmarkLayoutOnly` ±2%
  since `9754913`, end-to-end ±1–5%. Use `-count 10` and `benchstat` with
  p-values; `~` is a real answer.
- **Three interleaved pairs is not enough for an all-core number.** The encoder
  change read 1.9% by median and 3.5% by mean off the same three pairs; eight
  pairs of `-benchtime 20x` put it at 2.7% and won every pair. Sample until the
  sign is unanimous, not until the means differ.
- **Verify which tree each half of an A/B came from** — or better, let
  `benchsteps ab` do it, which is why it exists. Running both sides from the same
  clone once produced a fabricated -2.9%.
- **`BenchmarkLayoutOnly` times the whole serial path** and layout is only ~a
  quarter of it (parsing is the rest). Build with `-tags mp3timing` for the two
  stages separately — and read those for attribution only, since the per-call
  stage clocks swing ±20% where the total swings 2%. `TestLayoutBenchInput` pins
  the properties the benchmark depends on.

- **Any speed claim against another implementation must be CLI against CLI.**
  `BenchmarkReference` execs a subprocess that reads and writes files; `Process`
  does neither, and on the 8-second file the difference is larger than the repack
  — 1.36 ms against 4.76, of which 2.88 is starting the process. Pairing `Process`
  against `BenchmarkReference` overstated the lead by about 3×. Use
  `BenchmarkCLI`, and prefer `MP3PACKER_BENCH_FILE` over the 8-second file, where
  fixed costs dominate. `TestComparison` generates the README's tables and asserts
  the claim; `-update-comparison` rewrites them. Two results from it to keep in
  mind: **below about 50 kB we are level with or slower than the C++ port**,
  because 2.9 ms of startup outweighs the work, and **our lead narrows as cores
  are added** — 11.4× on one thread against 8.2× on sixteen, the reference scaling
  11.2× where we manage 8.0×. Adding cores is not where the remaining win is; the
  serial floor is.

## Tooling

- **`.env`** holds the local machine details — `MP3PACKER_X86_HOST` and
  `MP3PACKER_REFERENCE`. It is gitignored; `set -a; . ./.env; set +a` to load it.
  Nothing that identifies a machine belongs in a tracked file.
- **x86 box:** a Xeon E5-2698 v4, 2.2 GHz locked (no turbo — don't assume 3.6),
  40 threads, Go 1.26.5, reachable over ssh at `$MP3PACKER_X86_HOST`. Cross-compile and `scp` test
  binaries; don't sync source. Locally, Rosetta runs amd64 test binaries for
  correctness only.
- **`-tags mp3timing`** for `Stats.Prepare`/`Recompress`/`Layout` + `Serial()`,
  printed by `-v`. Compiled out by default (verified zero-cost).
- **Attribution:** `BenchmarkOptimizeGranule` / `DecodeGranule` /
  `EncodeGranule` for the three halves, `BenchmarkRecompressWorkers` for the `-j`
  curve, `MP3PACKER_BENCH_FILE=bards-tale.mp3` for long material. `benchstat` is
  at `$(go env GOPATH)/bin/benchstat`.
- **Generated tables: never hand-edit one.** Every table in README.md and
  PERFORMANCE.md sits between HTML-comment markers and is written by code —
  `TestComparison` and `TestSavings` with `-update-readme` for the README's,
  `benchsteps inject` for PERFORMANCE.md's. All of them render through
  `olekukonko/tablewriter`, which is why the columns line up. That dependency is
  test-and-tool only: `go list -deps .` does not reach it, so nothing a consumer
  of the library builds does either.
- **`cmd/benchsteps` owns the step tables in PERFORMANCE.md**, and `ab` is how a single
  change is measured — see *How to A/B a change* above. Do not hand-edit the
  tables; add a commit to `bench/steps.json`, `run -sweep`, then `inject`. Runs are cached in
  `bench/results.json` and only unsettled cells are re-measured, so adding one
  step costs one step, and re-running unchanged costs nothing. The cache is keyed
  on machine, CPU, OS version, Go toolchain, harness digest and per-input digest
  — and on a hand-bumped `measurementVersion` const, which **must** be
  incremented if you change how a run is produced, since nothing else will
  notice. Presentation (labels, order, headers, table membership) comes from
  `steps.json` at inject time, so changing it needs no re-measurement.
- **Killing a `run` mid-sweep is safe and normal.** Results are written after
  every pass, and the lock is taken over automatically once its owning pid is
  gone. Tightening the tolerance costs runs as its inverse square: all-core cells
  went 8 to 156 runs between 1.5% and 0.75%, and the all-core column needs an
  order of magnitude more runs than single-worker for the same confidence.
- **This machine drifts ~7% between sittings**, which is larger than most steps
  in the tables. That is why runs carry a session and `inject` will only publish
  from one: plain `run` is for iterating, `run -sweep` is what makes a table.
  Chasing a tight tolerance across sittings buys nothing — prefer 1.5% in one
  session over 0.75% spread over five. A cross-sitting median invented a 3.1%
  one-worker win for the CRC fold that a same-sitting A/B put at 0.2%. Steps are
  pooled across sessions by *ratio*, which is drift-free, so old sessions keep
  earning their keep even though old absolutes cannot be compared.
- **Sanity-check a step against arithmetic before believing it.** The CRC fold
  saves 23 ns on each of 6071 frames; that is 0.14 ms, and on a 183 ms one-worker
  run it cannot be 3%. Any end-to-end figure much larger than the routine's own
  saving is measurement error, not a discovery.
- **`bards-tale.mp3`** is gitignored, real 6071-frame material (2m38s, 192 kbps
  CBR), and the only input with meaningful CRC and switched-granule coverage — a
  new session won't have it unless you say so.

## Settled — don't re-attempt without new evidence

- **Caching `Frame.MainDataBits` per frame.** The 30 ms of 440 ms was real when
  first written; on current code it is 1.4% of the serial-path profile and does
  not appear in the one-worker profile at all — about 0.3% of an all-cores
  repack, for a cache threaded through four callers. Everything around it got
  faster and it stopped being worth it.
- **Both forms of the `pairDecode` treatment** measured flat or worse (README's
  tried-and-dropped list). Note the diagnosis that motivated them was wrong:
  `abs` is 0.9% of `Encode`, not 3.5% — the 3.5% in the whole-program profile is
  `pairCost` in the search, not the encoder. What paid instead was the bit
  writer: `put` will not inline, so every pair went through memory;
  `Pending`/`Store`/`Resume` let `Encode` hold the accumulator in two locals for
  the whole granule (−25% on the granule, −4.8% / −5.3% on one worker,
  −2.7% / −4.7% all cores, arm64 / x86-64).
- **PGO.** Measured `~` on one worker, all cores and all four granule
  benchmarks; the command itself came out 1–2% slower on one profile and level on
  another, so the sign is build luck. It does apply — forty-five extra inline
  decisions, all in the hot path — but the inlining that matters here is already
  forced by hand and pinned by `TestHotPathsStillInline`, the rest is assembly,
  and there is no interface call in `huffman` or `bitio` to devirtualise. Note
  where a profile is looked for before re-testing: only
  `cmd/mp3packer/default.pgo` is auto-applied, `go test` discovers nothing, and
  `benchsteps` builds with a plain `go test -c` — so the step tables cannot see
  PGO without a harness change and a `measurementVersion` bump. PERFORMANCE.md
  has the numbers.
- Also dropped, per the README: `big_values` pruning, int32 `Spectrum`,
  bit-reader rewrite.
