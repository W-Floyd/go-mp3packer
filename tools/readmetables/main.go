// Command readmetables writes the generated tables in README.md: what a repack
// saves on the test corpus, and how long this implementation takes against
// another one invoked the same way.
//
// These were tests once, run with -update-readme. Two things pushed them out.
// Neither asserts anything any more — the size property they used to defend is
// covered by TestRecompressBeatsLayoutOnly over the same corpus, and the timings
// stopped being a claim to defend when upstream mp3packercpp merged the
// optimisation work and the two implementations came out level. What was left was
// a generator wearing a test's clothes, run by hand behind a flag.
//
// The other reason is the dependency. Rendering markdown needs tablewriter; a
// module that requires it puts it in every consumer's go.sum whether or not a
// single line of it is ever compiled. Living in the tools module, it reaches
// nobody: the root module requires nothing at all.
//
// Run it from anywhere in the checkout:
//
//	go run ./tools/readmetables            # every table it owns
//	go run ./tools/readmetables -savings   # just the one needing no binaries
//	go run ./tools/readmetables -dry-run   # print them instead of writing
//
// The comparison tables need $MP3PACKER_REFERENCE pointing at an implementation
// that accepts "-z <in> <out>", which both the original OCaml mp3packer and the
// C++ port do. $MP3PACKER_BENCH_FILE adds a file to the corpus, and since the
// thread-scaling table is built from the longest file available, that is how the
// table comes to describe real-length material rather than eight seconds of it.
package main

import (
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	mp3packer "github.com/W-Floyd/go-mp3packer"
	"github.com/W-Floyd/go-mp3packer/mp3"
	"github.com/W-Floyd/go-mp3packer/tools/internal/mdtable"
)

// comparisonRuns is how many times each command is timed. The median of five is
// steady enough for a table quoted to three figures, and the whole run stays
// inside a minute.
const comparisonRuns = 5

// comparisonThreads are the worker counts both implementations are measured at.
// Zero means "as many as you like", which is -j0 here and no -t at all for the
// reference.
var comparisonThreads = []int{1, 2, 4, 0}

func main() {
	log.SetFlags(0)
	log.SetPrefix("readmetables: ")
	savingsOnly := flag.Bool("savings", false,
		"write only the savings table, which needs no reference implementation")
	dryRun := flag.Bool("dry-run", false, "print the tables instead of writing README.md")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		log.Fatal(err)
	}

	tables := map[string]string{}
	savings, err := savingsTable()
	if err != nil {
		log.Fatal(err)
	}
	tables["savings"] = savings

	if !*savingsOnly {
		files, threads, err := comparisonTables()
		if err != nil {
			log.Fatal(err)
		}
		tables["comparison-files"] = files
		tables["comparison-threads"] = threads
	}

	if *dryRun {
		for _, id := range slices.Sorted(maps.Keys(tables)) {
			fmt.Printf("<!-- comparison:%s -->\n%s\n", id, tables[id])
		}
		return
	}
	if err := writeTables(tables); err != nil {
		log.Fatal(err)
	}
	for _, id := range slices.Sorted(maps.Keys(tables)) {
		log.Printf("wrote %s", id)
	}
}

// savingsTable is the corpus repacked, in bytes. It needs no reference
// implementation and no timing, the sizes being exact and deterministic, so it
// regenerates every time it is asked.
//
// Each row's description comes from the frames rather than from the file's name,
// so a file cannot be mislabelled by renaming it.
func savingsTable() (string, error) {
	files, err := testFiles()
	if err != nil {
		return "", err
	}
	var rows [][]string
	for _, path := range files {
		in, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		layout, _, err := mp3packer.Process(in, mp3packer.Options{})
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		packed, _, err := mp3packer.Process(in, mp3packer.Options{Recompress: true})
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		what, err := describe(in)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		rows = append(rows, []string{
			filepath.Base(path), what,
			strconv.Itoa(len(in)), strconv.Itoa(len(layout)), strconv.Itoa(len(packed)),
			fmt.Sprintf("%.2f%%", 100*float64(len(in)-len(packed))/float64(len(in))),
		})
	}
	return mdtable.Render(
		[]string{"file", "", "input", "`-n`", "default", "saved"},
		[]mdtable.Align{mdtable.Left, mdtable.Left, mdtable.Right, mdtable.Right, mdtable.Right, mdtable.Right},
		rows)
}

// comparisonTables times this implementation against the reference and returns
// the by-file and by-thread-count tables.
//
// Nothing is asserted about what comes back. Which way a given measurement falls
// is a fact to report, not a property of this repository to defend — see the
// README section these tables sit in. Both sides are timed the same way, as a
// subprocess over a file, so the exec and the disk are in every figure, and on a
// small enough file that is most of what is being measured.
func comparisonTables() (string, string, error) {
	ref, err := referenceBinary()
	if err != nil {
		return "", "", err
	}
	ours, cleanup, err := mp3packerBinary()
	if err != nil {
		return "", "", err
	}
	defer cleanup()

	dir, err := os.MkdirTemp("", "readmetables")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)

	files, err := benchFiles()
	if err != nil {
		return "", "", err
	}
	type row struct {
		file    string
		bytes   int
		ourMs   map[int]float64
		theirMs map[int]float64
	}
	rows := make([]row, 0, len(files))
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return "", "", err
		}
		r := row{
			file:    filepath.Base(path),
			bytes:   int(info.Size()),
			ourMs:   map[int]float64{},
			theirMs: map[int]float64{},
		}
		out := filepath.Join(dir, r.file)
		for _, n := range comparisonThreads {
			if r.ourMs[n], err = timeCommand(ours, ourArgs(n, path, out)...); err != nil {
				return "", "", err
			}
			if r.theirMs[n], err = timeCommand(ref, theirArgs(n, path, out)...); err != nil {
				return "", "", err
			}
		}
		rows = append(rows, r)
	}

	byFile := make([][]string, 0, len(rows))
	for _, r := range rows {
		byFile = append(byFile, []string{
			r.file, fmt.Sprintf("%d kB", r.bytes/1000),
			threeFigures(r.ourMs[0]) + " ms",
			threeFigures(r.theirMs[0]) + " ms",
			fmt.Sprintf("%.1f×", r.theirMs[0]/r.ourMs[0]),
		})
	}
	filesTable, err := mdtable.Render(
		[]string{"file", "size", "ours", "reference", "vs ref"},
		[]mdtable.Align{mdtable.Left, mdtable.Right, mdtable.Right, mdtable.Right, mdtable.Right},
		byFile)
	if err != nil {
		return "", "", err
	}

	// Thread scaling is shown on the longest file available: on a short one the
	// fixed costs swamp what the workers are doing.
	longest := slices.MaxFunc(rows, func(a, b row) int { return a.bytes - b.bytes })
	byThreads := make([][]string, 0, len(comparisonThreads))
	for _, n := range comparisonThreads {
		label := strconv.Itoa(n)
		if n == 0 {
			label = "all"
		}
		byThreads = append(byThreads, []string{
			label, threeFigures(longest.ourMs[n]) + " ms",
			threeFigures(longest.theirMs[n]) + " ms",
			fmt.Sprintf("%.1f×", longest.theirMs[n]/longest.ourMs[n]),
		})
		log.Printf("%s at %s threads: %.1f ms against the reference's %.1f",
			longest.file, label, longest.ourMs[n], longest.theirMs[n])
	}
	threadsTable, err := mdtable.Render(
		[]string{"threads", "ours", "reference", "vs ref"},
		[]mdtable.Align{mdtable.Right, mdtable.Right, mdtable.Right, mdtable.Right},
		byThreads)
	if err != nil {
		return "", "", err
	}
	return filesTable, threadsTable, nil
}

// writeTables swaps every table into README.md in one write, so a failure to find
// one document's markers cannot leave the file half rewritten.
func writeTables(tables map[string]string) error {
	body, err := os.ReadFile("README.md")
	if err != nil {
		return err
	}
	text := string(body)
	for _, id := range slices.Sorted(maps.Keys(tables)) {
		if text, err = mdtable.Replace(text, "comparison", id, tables[id]); err != nil {
			return err
		}
	}
	return os.WriteFile("README.md", []byte(text), 0o644)
}

func ourArgs(threads int, in, out string) []string {
	return []string{"-q", "-f", "-j", strconv.Itoa(threads), in, out}
}

// theirArgs mirrors ourArgs. -z is the reference's recompression switch, which is
// on by default here; passing no -t leaves it choosing for itself, which is what
// -j0 does for us.
func theirArgs(threads int, in, out string) []string {
	args := []string{"-z"}
	if threads > 0 {
		args = append(args, "-t", strconv.Itoa(threads))
	}
	return append(args, in, out)
}

// timeCommand runs a command comparisonRuns times and returns the median
// wall-clock in milliseconds. The first run is discarded, so that one side is not
// charged for pulling the input into the page cache.
func timeCommand(bin string, args ...string) (float64, error) {
	fail := func(err error) error {
		return fmt.Errorf("%s %s: %w", filepath.Base(bin), strings.Join(args, " "), err)
	}
	if err := exec.Command(bin, args...).Run(); err != nil {
		return 0, fail(err)
	}
	ms := make([]float64, 0, comparisonRuns)
	for range comparisonRuns {
		start := time.Now()
		if err := exec.Command(bin, args...).Run(); err != nil {
			return 0, fail(err)
		}
		ms = append(ms, float64(time.Since(start).Microseconds())/1000)
	}
	slices.Sort(ms)
	return ms[len(ms)/2], nil
}

// threeFigures keeps the tables from claiming a precision five runs cannot
// support, without dropping to exponents on the millisecond scale.
func threeFigures(v float64) string {
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

// describe names a file by what its frames say it is: the MPEG version where it is
// not the usual one, the bitrate if every frame shares it, and the channel mode.
func describe(data []byte) (string, error) {
	f, err := mp3.Parse(data)
	if err != nil {
		return "", err
	}
	first := f.Frames[0].Header
	rate := "VBR"
	if slices.IndexFunc(f.Frames, func(fr mp3.Frame) bool {
		return fr.Header.BitrateIndex != first.BitrateIndex
	}) < 0 {
		rate = fmt.Sprintf("CBR %d", first.Bitrate())
	}
	mode := map[mp3.ChannelMode]string{
		mp3.Stereo: "stereo", mp3.JointStereo: "joint stereo",
		mp3.DualChannel: "dual channel", mp3.Mono: "mono",
	}[first.Mode]

	parts := []string{rate, mode, fmt.Sprintf("%d Hz", first.SampleRate)}
	if first.Version != mp3.MPEG1 {
		parts = append(parts, first.Version.String())
	}
	if first.CRC {
		parts = append(parts, "CRC")
	}
	return strings.Join(parts, ", "), nil
}

func testFiles() ([]string, error) {
	files, err := filepath.Glob("testdata/*.mp3")
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no test files in testdata/: %v", err)
	}
	return files, nil
}

// benchFiles is the corpus plus whatever MP3PACKER_BENCH_FILE points at, so that
// the tables can describe real-length material as well. Eight seconds is short
// enough that fixed costs dominate, which is the opposite of how anyone uses this.
func benchFiles() ([]string, error) {
	files, err := testFiles()
	if err != nil {
		return nil, err
	}
	if path := os.Getenv("MP3PACKER_BENCH_FILE"); path != "" {
		files = append(files, path)
	}
	return files, nil
}

// mp3packerBinary builds the command, so that it can be timed the way a user
// actually invokes it.
func mp3packerBinary() (string, func(), error) {
	dir, err := os.MkdirTemp("", "readmetables-bin")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	bin := filepath.Join(dir, "mp3packer")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/mp3packer").CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("building the command: %v: %s", err, out)
	}
	return bin, cleanup, nil
}

// referenceBinary returns the path to another mp3packer implementation to compare
// against, taken from $MP3PACKER_REFERENCE.
func referenceBinary() (string, error) {
	path := os.Getenv("MP3PACKER_REFERENCE")
	if path == "" {
		return "", fmt.Errorf("set MP3PACKER_REFERENCE to an implementation to compare " +
			"against, or pass -savings for the table that does not need one")
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", fmt.Errorf("MP3PACKER_REFERENCE=%q: %w", path, err)
	}
	return resolved, nil
}

// repoRoot lets the command be run from anywhere in the checkout: every path it
// touches — testdata, README.md, ./cmd/mp3packer — is relative to the top.
func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("finding the repository root: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
