// Command mp3packer losslessly recompresses MP3 files.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	mp3packer "github.com/W-Floyd/go-mp3packer"
)

const version = "0.1.0"

func usage() {
	fmt.Fprint(os.Stderr, `mp3packer - losslessly recompress MP3 files

usage: mp3packer [options] in.mp3 [out.mp3]

Rewrites in.mp3 with the smallest legal Huffman coding of the same audio and the
tightest frame layout that coding allows. The decoded audio is unchanged.

If out.mp3 is omitted, the input is replaced in place. Tags and any leading
Xing/Info header are preserved.

options:
`)
	flag.PrintDefaults()
}

func main() {
	var (
		noRecompress = flag.Bool("n", false, "skip the Huffman search; only repack the frame layout")
		stripCRC     = flag.Bool("no-crc", false, "drop the optional frame CRC, freeing 2 bytes per frame")
		workers      = flag.Int("j", 0, "recompression workers (0 = one per CPU)")
		verbose      = flag.Bool("v", false, "log per-frame details")
		quiet        = flag.Bool("q", false, "print nothing on success")
		force        = flag.Bool("f", false, "overwrite the output file if it exists")
		showVersion  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("mp3packer", version)
		return
	}
	args := flag.Args()
	if len(args) < 1 || len(args) > 2 {
		usage()
		os.Exit(2)
	}
	in := args[0]
	out := in
	if len(args) == 2 {
		out = args[1]
		if info, err := os.Stat(out); err == nil {
			if info.IsDir() {
				out = filepath.Join(out, filepath.Base(in))
			} else if !*force {
				fatal(fmt.Errorf("%s exists (use -f to overwrite)", out))
			}
		}
	}

	opt := mp3packer.Options{
		Recompress: !*noRecompress,
		StripCRC:   *stripCRC,
		Workers:    *workers,
	}
	if *verbose {
		opt.Log = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}

	stats, err := mp3packer.ProcessFile(in, out, opt)
	if err != nil {
		fatal(err)
	}
	if !*quiet {
		pct := 0.0
		if stats.InputSize > 0 {
			pct = 100 * float64(stats.Saved()) / float64(stats.InputSize)
		}
		fmt.Printf("%s: %d -> %d bytes (%.2f%% smaller), %d frames, %d recompressed",
			in, stats.InputSize, stats.OutputSize, pct, stats.Frames, stats.Recompressed)
		if stats.Abandoned > 0 {
			fmt.Printf(", %d left as-is", stats.Abandoned)
		}
		if stats.SyncErrors > 0 {
			fmt.Printf(", %d sync errors", stats.SyncErrors)
		}
		fmt.Println()
		if *verbose && stats.Serial() > 0 {
			// Zero unless built with -tags mp3timing. Only the recompression stage
			// is parallel, so the other two are what stop more workers helping.
			fmt.Printf("  prepare %v, recompress %v, layout %v (%v serial)\n",
				stats.Prepare.Round(time.Microsecond), stats.Recompress.Round(time.Microsecond),
				stats.Layout.Round(time.Microsecond), stats.Serial().Round(time.Microsecond))
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mp3packer:", err)
	os.Exit(1)
}
