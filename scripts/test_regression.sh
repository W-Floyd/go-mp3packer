#!/usr/bin/env bash
# Deep regression check for the mp3packer CLI.
#
# Where test_matrix.sh and test_random.sh go wide — many parameter combinations,
# one decoder — this goes deep on a narrower corpus:
#
#   - cross-decoder losslessness: ffmpeg, lame --decode and mpg123 must each
#     decode the packed file to the same PCM they decode the source to. One
#     decoder agreeing proves less than it looks: a wrong Xing/LAME tag, for
#     instance, changes gapless trimming in ffmpeg but not in mpg123.
#   - decoded length: a frame silently dropped or added shows up here even when
#     every overlapping sample matches. Quiet material is where that happens —
#     a granule can compress down to a back-reference with nothing of its own,
#     which is not the same thing as a frame that need not be written.
#   - all nine MPEG-1/2/2.5 sample rates, which no committed test file covers.
#   - structural validity via mp3val, if it is installed.
#   - idempotence: packing an already-packed file must not change it further.
#   - thread determinism: one worker and every core, byte for byte.
#
# Usage: scripts/test_regression.sh [path/to/mp3packer]
# With no argument, builds the CLI from the working tree.
#
# MP3PACKER_REGRESSION_EXTRA may name extra real-world files (colon-separated)
# to run the same checks over; bards-tale.mp3 in the repo root is picked up
# automatically when it is there.

. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_tools ffmpeg lame mpg123 go
make_workdir
resolve_bin "${1:-}"
NCPU="$(ncpu)"

DECODERS="ffmpeg lame mpg123"
have_tool mp3val || echo "note: mp3val not installed — skipping the structural check"

check_file() { # check_file LABEL SRC
    local label="$1" src="$2"
    TOTAL=$((TOTAL + 1))
    echo "== $label"

    local plain="$WORKDIR/o_plain.mp3" t1="$WORKDIR/o_t1.mp3" tn="$WORKDIR/o_tn.mp3"
    pack -n     "$src" "$plain" || { fail "$label: -n pack failed"; return; }
    pack -j 1   "$src" "$t1"    || { fail "$label: one-worker pack failed"; return; }
    pack -j "$NCPU" "$src" "$tn" || { fail "$label: all-cores pack failed"; return; }

    cmp -s "$t1" "$tn" || fail "$label: one worker and $NCPU workers differ"

    if have_tool mp3val; then
        local f w
        for f in "$plain" "$t1"; do
            # mp3val's notes about missing tags and VBR headers are cosmetic and
            # say nothing about the frames.
            w=$(mp3val "$f" 2>&1 | grep -E "WARNING|ERROR" |
                grep -v "No supported tags" | grep -v "VBR detected" | wc -l | tr -d ' ')
            [ "$w" = "0" ] || fail "$label: mp3val reported $w issue(s) on $(basename "$f")"
        done
    fi

    local dec f ref got msg
    for dec in $DECODERS; do
        ref="$WORKDIR/ref_$dec.raw"
        decode "$dec" "$src" "$ref" || { fail "$label: $dec could not decode the source"; continue; }
        for f in "$plain" "$t1"; do
            got="$WORKDIR/got_$dec.raw"
            if ! decode "$dec" "$f" "$got"; then
                fail "$label: $dec could not decode $(basename "$f")"; continue
            fi
            msg="$(same_pcm "$ref" "$got")" ||
                fail "$label: $dec $(basename "$f") $msg"
        done
    done

    local again="$WORKDIR/o_again.mp3"
    pack "$t1" "$again" || { fail "$label: repack failed"; return; }
    cmp -s "$t1" "$again" || fail "$label: not idempotent (repacking changed bytes)"
}

echo "== building corpus in $WORKDIR =="
for sr in 44100 48000 32000 22050 24000 16000 11025 12000 8000; do
    for ch in 1 2; do
        wav="$WORKDIR/s_${sr}_${ch}.wav"
        ffmpeg -y -v error -f lavfi \
               -i "anoisesrc=color=pink:seed=11:duration=3:sample_rate=$sr" \
               -ar "$sr" -ac "$ch" "$wav" 2>/dev/null || continue
        mono=""; [ "$ch" = "1" ] && mono="-m m"
        for crc in "" "-c"; do
            tag="nocrc"; [ -n "$crc" ] && tag="crc"
            mp3="$WORKDIR/c_${sr}_${ch}_${tag}.mp3"
            lame $mono $crc -V 4 "$wav" "$mp3" >/dev/null 2>&1 || continue
            check_file "${sr}Hz/ch${ch}/${tag}" "$mp3"
        done
    done
done

# Near-silence is the case a frame-dropping bug hides in: the material still
# occupies frames, and every one of them has to be written.
for sr in 44100 22050; do
    wav="$WORKDIR/quiet_${sr}.wav"
    ffmpeg -y -v error -f lavfi -i "anullsrc=r=$sr:cl=stereo:d=3" -ar "$sr" "$wav" 2>/dev/null || continue
    mp3="$WORKDIR/quiet_${sr}.mp3"
    lame -b 320 --cbr "$wav" "$mp3" >/dev/null 2>&1 || continue
    check_file "silence/${sr}Hz" "$mp3"
done

# Real material, where it is available: bards-tale.mp3 is gitignored but is the
# only long input with meaningful CRC and switched-granule coverage.
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
extra="${MP3PACKER_REGRESSION_EXTRA:-}"
[ -f "$repo/bards-tale.mp3" ] && extra="$repo/bards-tale.mp3:$extra"
saved_ifs="$IFS"; IFS=:
for real in $extra; do
    IFS="$saved_ifs"
    [ -n "$real" ] && [ -f "$real" ] && check_file "real/$(basename "$real")" "$real"
    IFS=:
done
IFS="$saved_ifs"

summary files
