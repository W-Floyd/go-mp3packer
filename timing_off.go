//go:build !mp3timing

package mp3packer

// Stage timings are off unless built with -tags mp3timing. Reading the clock four
// times costs about a hundred nanoseconds a file, which is nothing next to a
// repack but is a measurable fraction of the layout-only benchmark, and that is
// one of the benchmarks used to judge the layout pass. A constant rather than a
// variable, so that the reads and the arithmetic behind them compile away.
const timingEnabled = false
