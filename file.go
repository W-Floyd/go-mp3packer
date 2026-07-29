package mp3packer

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProcessFile repacks inPath and writes the result to outPath. The output is
// written to a temporary file in the same directory and renamed into place, so
// an interrupted run cannot leave a half-written file behind, and outPath may
// safely be the same as inPath.
func ProcessFile(inPath, outPath string, opt Options) (Stats, error) {
	in, err := os.ReadFile(inPath)
	if err != nil {
		return Stats{}, err
	}
	out, stats, err := Process(in, opt)
	if err != nil {
		return stats, fmt.Errorf("%s: %w", inPath, err)
	}

	info, err := os.Stat(inPath)
	if err != nil {
		return stats, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".mp3packer-*")
	if err != nil {
		return stats, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return stats, err
	}
	if err := tmp.Close(); err != nil {
		return stats, err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return stats, err
	}
	return stats, os.Rename(tmp.Name(), outPath)
}
