#!/usr/bin/env bash
# Shared helpers for the lossless-validation scripts.
#
# What these scripts add over `go test ./...`: the committed corpus is seven
# files, and the suite's losslessness check compares decoded *spectra* with our
# own decoder. That proves the coefficients survive, and cannot prove a real
# decoder makes the same samples of them, nor cover sample rates and channel
# modes no committed file uses. These drive the built CLI against corpora built
# on the spot with lame, and check the audio with decoders that are not ours.
#
# Written for bash 3.2, which is what macOS ships: no associative arrays, no
# `local -n`, no `${var^^}`.

set -uo pipefail

# resolve_bin [path] — sets BIN to an executable mp3packer. With no argument it
# builds one from the working tree into the workdir, so what gets validated is
# what you have edited rather than whatever was built last.
resolve_bin() {
    local repo
    repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    if [ -n "${1:-}" ]; then
        if [ ! -x "$1" ]; then
            echo "error: binary not found or not executable: $1" >&2
            exit 1
        fi
        BIN="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
        return
    fi
    BIN="$WORKDIR/mp3packer"
    echo "== building $BIN from $repo =="
    ( cd "$repo" && go build -o "$BIN" ./cmd/mp3packer ) || exit 1
}

# make_workdir — a temporary directory removed on exit, however we exit.
make_workdir() {
    WORKDIR="$(mktemp -d)"
    trap 'rm -rf "$WORKDIR"' EXIT
}

ncpu() { sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4; }

# require_tools tool... — every one must be present; anything optional is
# checked with have_tool at the point it is used instead.
require_tools() {
    local missing="" t
    for t in "$@"; do
        command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
    done
    if [ -n "$missing" ]; then
        echo "error: these are needed and not on PATH:$missing" >&2
        exit 1
    fi
}

have_tool() { command -v "$1" >/dev/null 2>&1; }

# Bookkeeping. TOTAL counts cases, FAILURES holds one line each.
TOTAL=0
FAIL=0
FAILURES=()

fail() {
    FAIL=$((FAIL + 1))
    FAILURES+=("$1")
    echo "FAIL  $1"
}

summary() { # summary UNIT
    local unit="${1:-cases}"
    echo ""
    echo "== summary: $((TOTAL - FAIL))/$TOTAL $unit passed =="
    if [ "$FAIL" -gt 0 ]; then
        echo ""
        echo "failures:"
        printf '  %s\n' "${FAILURES[@]}"
        exit 1
    fi
}

# decode DECODER IN OUT — raw s16le. Each decoder is applied to both the source
# and the packed file, so a decoder's own delay cancels out of the comparison.
decode() {
    local dec="$1" in="$2" out="$3"
    case "$dec" in
        ffmpeg)
            ffmpeg -y -v error -i "$in" -f s16le -acodec pcm_s16le "$out" 2>/dev/null ;;
        lame)
            lame --quiet --decode "$in" "$out.wav" 2>/dev/null &&
                ffmpeg -y -v error -i "$out.wav" -f s16le -acodec pcm_s16le "$out" 2>/dev/null ;;
        mpg123)
            mpg123 -q -w "$out.wav" "$in" 2>/dev/null &&
                ffmpeg -y -v error -i "$out.wav" -f s16le -acodec pcm_s16le "$out" 2>/dev/null ;;
        *)
            echo "internal error: unknown decoder $dec" >&2; return 1 ;;
    esac
}

# same_pcm A B — a repack is lossless, so the decoded bytes are not merely close
# but equal; `cmp` is both the stronger check and the one with no dependencies.
# Prints a description of the difference when there is one.
same_pcm() {
    if cmp -s "$1" "$2"; then
        return 0
    fi
    local sa sb where
    sa=$(wc -c <"$1" | tr -d ' ')
    sb=$(wc -c <"$2" | tr -d ' ')
    if [ "$sa" != "$sb" ]; then
        echo "decoded length $((sa / 2)) vs $((sb / 2)) samples"
    else
        where=$(cmp "$1" "$2" 2>/dev/null | head -1)
        echo "decoded samples differ (${where:-unknown offset})"
    fi
    return 1
}

# pack FLAGS... IN OUT — the CLI recompresses by default, where the C++ and
# OCaml tools need -z; -n is this side's opt-out.
pack() { "$BIN" -q -f "$@" >/dev/null 2>&1; }
