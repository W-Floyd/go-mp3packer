package mp3packer

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/W-Floyd/go-mp3packer/mp3"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/renderer"
	"github.com/olekukonko/tablewriter/tw"
)

// markdownTable renders a table for the README. Columns are aligned by align,
// which is also what markdown's own alignment row will say, so a column of
// numbers lines up on its digits wherever the file is rendered.
//
// Header formatting is switched off: the default upper-cases them, which reads as
// shouting in prose and loses the case of things like `-n`.
func markdownTable(header []string, align []tw.Align, rows [][]string) string {
	var buf bytes.Buffer
	cfg := tw.CellConfig{Alignment: tw.CellAlignment{PerColumn: align}}
	t := tablewriter.NewTable(&buf,
		tablewriter.WithRenderer(renderer.NewMarkdown()),
		tablewriter.WithConfig(tablewriter.Config{
			Header: tw.CellConfig{
				Formatting: tw.CellFormatting{AutoFormat: tw.Off},
				Alignment:  cfg.Alignment,
			},
			Row: cfg,
		}))
	t.Header(header)
	for _, r := range rows {
		if err := t.Append(r); err != nil {
			panic(err)
		}
	}
	if err := t.Render(); err != nil {
		panic(err)
	}
	return buf.String()
}

var (
	alignL = tw.AlignLeft
	alignR = tw.AlignRight
)

var updateReadme = flag.Bool("update-readme", false,
	"rewrite the generated tables in README.md from a fresh measurement")

// comparisonRuns is how many times each command is timed. The median of five is
// steady enough for a table quoted to three figures, and the whole test stays
// inside a minute.
const comparisonRuns = 5

// comparisonThreads are the worker counts both implementations are measured at.
// Zero means "as many as you like", which is -j0 here and no -t at all for the
// reference.
var comparisonThreads = []int{1, 2, 4, 0}

// TestComparison times this implementation against the reference.
//
// Ordinarily it asserts the claim the README makes, and with -update-readme it
// also rewrites the tables there, so those numbers are generated from a
// measurement rather than typed in from one.
//
// The claim is deliberately narrow: faster on the longest file available, at every
// worker count. It is not "faster on every file", which is false and which this
// test is how we found out. Below about 50 kB both sides are mostly paying to
// start a process, and ours costs 2.9 ms of that, so the corpus's small files come
// out level or worse — and on the MPEG-2 file the reference is quicker because it
// declines to recompress MPEG-2 at all, which is not the same job.
//
// Both sides are timed the same way, as a subprocess over a file, so the exec and
// the disk are in every figure.
func TestComparison(t *testing.T) {
	ref := referenceBinary(t) // skips unless MP3PACKER_REFERENCE is set
	ours := mp3packerBinary(t)
	dir := t.TempDir()

	files := benchFiles(t)
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
			t.Fatal(err)
		}
		r := row{
			file:    filepath.Base(path),
			bytes:   int(info.Size()),
			ourMs:   map[int]float64{},
			theirMs: map[int]float64{},
		}
		out := filepath.Join(dir, r.file)
		for _, n := range comparisonThreads {
			r.ourMs[n] = timeCommand(t, ours, ourArgs(n, path, out)...)
			r.theirMs[n] = timeCommand(t, ref, theirArgs(n, path, out)...)
		}
		rows = append(rows, r)
	}

	// The longest file is the only one where the work outweighs starting the
	// process on both sides, so it is the only one worth asserting on.
	longest := slices.MaxFunc(rows, func(a, b row) int { return a.bytes - b.bytes })
	for _, n := range comparisonThreads {
		if longest.ourMs[n] >= longest.theirMs[n] {
			t.Errorf("%s at %d threads: %.1f ms against the reference's %.1f",
				longest.file, n, longest.ourMs[n], longest.theirMs[n])
		}
	}
	for _, r := range rows {
		if r.ourMs[0] >= r.theirMs[0] {
			t.Logf("%s (%d kB): %.2f ms against %.2f, which is startup on both sides",
				r.file, r.bytes/1000, r.ourMs[0], r.theirMs[0])
		}
	}

	if !*updateReadme {
		t.Log("pass -update-readme to rewrite the tables in README.md")
		return
	}

	byFile := make([][]string, 0, len(rows))
	for _, r := range rows {
		byFile = append(byFile, []string{
			r.file, fmt.Sprintf("%d kB", r.bytes/1000),
			threeFigures(r.ourMs[0]) + " ms", threeFigures(r.theirMs[0]) + " ms",
			fmt.Sprintf("%.1f×", r.theirMs[0]/r.ourMs[0]),
		})
	}
	filesTable := markdownTable(
		[]string{"file", "size", "ours", "reference", ""},
		[]tw.Align{alignL, alignR, alignR, alignR, alignR}, byFile)

	// Thread scaling is shown on the longest file available: on a short one the
	// fixed costs swamp what the workers are doing.
	last := longest
	byThreads := make([][]string, 0, len(comparisonThreads))
	for _, n := range comparisonThreads {
		label := strconv.Itoa(n)
		if n == 0 {
			label = "all"
		}
		byThreads = append(byThreads, []string{
			label, threeFigures(last.ourMs[n]) + " ms", threeFigures(last.theirMs[n]) + " ms",
			fmt.Sprintf("%.1f×", last.theirMs[n]/last.ourMs[n]),
		})
	}
	threadsTable := markdownTable(
		[]string{"threads", "ours", "reference", ""},
		[]tw.Align{alignR, alignR, alignR, alignR}, byThreads)

	body, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, b := range []struct{ id, table string }{
		{"comparison-files", filesTable},
		{"comparison-threads", threadsTable},
	} {
		text, err = replaceMarked(text, b.id, b.table)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile("README.md", []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrote README.md from %d files at %d thread counts, %s",
		len(rows), len(comparisonThreads), last.file)
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
func timeCommand(t *testing.T, bin string, args ...string) float64 {
	t.Helper()
	if err := exec.Command(bin, args...).Run(); err != nil {
		t.Fatalf("%s %s: %v", filepath.Base(bin), strings.Join(args, " "), err)
	}
	ms := make([]float64, 0, comparisonRuns)
	for range comparisonRuns {
		start := time.Now()
		if err := exec.Command(bin, args...).Run(); err != nil {
			t.Fatalf("%s %s: %v", filepath.Base(bin), strings.Join(args, " "), err)
		}
		ms = append(ms, float64(time.Since(start).Microseconds())/1000)
	}
	slices.Sort(ms)
	return ms[len(ms)/2]
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

// replaceMarked swaps the text between a pair of HTML comments. Their absence is
// an error rather than something to fix by appending: a table quietly added to the
// end of the document is never what was wanted.
func replaceMarked(text, id, body string) (string, error) {
	open, shut := "<!-- comparison:"+id+" -->", "<!-- /comparison:"+id+" -->"
	i, j := strings.Index(text, open), strings.Index(text, shut)
	if i < 0 || j < 0 || j < i {
		return "", fmt.Errorf("markers for %q not found in the document", id)
	}
	return text[:i+len(open)] + "\n" + body + text[j:], nil
}

// TestSavings writes the table of what a repack actually saves.
//
// It needs no reference implementation and no timing, the sizes being exact and
// deterministic, so it regenerates from the corpus every time it is asked. The
// table it replaced had been left behind by the corpus: of its seven input sizes
// only two still matched a file in the repository, and it described three files
// that no longer existed.
//
// Each row's description comes from the frames rather than from the file's name,
// so a file cannot be mislabelled by renaming it.
func TestSavings(t *testing.T) {
	var rows [][]string
	for _, path := range testFiles(t) {
		in := read(t, path)
		layout, _, err := Process(in, Options{})
		if err != nil {
			t.Fatal(err)
		}
		packed, _, err := Process(in, Options{Recompress: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(packed) > len(in) {
			t.Errorf("%s grew: %d -> %d", filepath.Base(path), len(in), len(packed))
		}
		rows = append(rows, []string{
			filepath.Base(path), describe(t, in),
			strconv.Itoa(len(in)), strconv.Itoa(len(layout)), strconv.Itoa(len(packed)),
			fmt.Sprintf("%.2f%%", 100*float64(len(in)-len(packed))/float64(len(in))),
		})
	}
	table := markdownTable(
		[]string{"file", "", "input", "`-n`", "default", "saved"},
		[]tw.Align{alignL, alignL, alignR, alignR, alignR, alignR}, rows)

	if !*updateReadme {
		t.Log("pass -update-readme to rewrite the table in README.md")
		return
	}
	body, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text, err := replaceMarked(string(body), "savings", table)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("README.md", []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// describe names a file by what its frames say it is: the MPEG version where it is
// not the usual one, the bitrate if every frame shares it, and the channel mode.
func describe(t *testing.T, data []byte) string {
	t.Helper()
	f, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
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
	return strings.Join(parts, ", ")
}
