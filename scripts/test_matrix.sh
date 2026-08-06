#!/usr/bin/env bash
# Broad lossless-validation matrix for the mp3packer CLI.
#
# Generates a corpus spanning MPEG version (via samplerate), channel mode,
# CBR/VBR, CRC on/off and signal type (stationary / transient-heavy / noisy),
# then verifies for each file that:
#   - -n output decodes to identical PCM as the source
#   - the default repack, at one worker, decodes to identical PCM
#   - the default repack, at every core, decodes to identical PCM
#   - -no-crc output decodes to identical PCM
#   - one worker and every core agree byte for byte — the search must not
#     depend on how the work was divided
#
# Usage: scripts/test_matrix.sh [path/to/mp3packer]
# With no argument, builds the CLI from the working tree.

. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_tools ffmpeg lame go
make_workdir
resolve_bin "${1:-}"
NCPU="$(ncpu)"

# check_case LABEL SRC — one source file through every variant.
check_case() {
    local label="$1" src="$2"
    TOTAL=$((TOTAL + 1))

    local ref="$WORKDIR/ref.raw"
    if ! decode ffmpeg "$src" "$ref"; then
        fail "$label :: source failed to decode"; return
    fi

    # variant label -> flags, as two parallel lists (bash 3.2 has no maps).
    local names="-n one-worker all-cores -no-crc"
    local i=0 name
    local outs=""
    for name in $names; do
        local flags out="$WORKDIR/out_$i.mp3"
        case "$name" in
            -n)          flags="-n" ;;
            one-worker)  flags="-j 1" ;;
            all-cores)   flags="-j $NCPU" ;;
            -no-crc)     flags="-no-crc" ;;
        esac
        if ! pack $flags "$src" "$out"; then
            fail "$label :: $name pack failed"; return
        fi
        outs="$outs $out"
        i=$((i + 1))
    done

    local ok=1 msg
    i=0
    for name in $names; do
        local out="$WORKDIR/out_$i.mp3" got="$WORKDIR/got_$i.raw"
        if ! decode ffmpeg "$out" "$got"; then
            fail "$label :: $name output failed to decode"; ok=0; i=$((i + 1)); continue
        fi
        if ! msg="$(same_pcm "$ref" "$got")"; then
            fail "$label :: $name $msg"; ok=0
        fi
        i=$((i + 1))
    done

    # one-worker is index 1, all-cores index 2.
    if ! cmp -s "$WORKDIR/out_1.mp3" "$WORKDIR/out_2.mp3"; then
        fail "$label :: one worker and $NCPU workers produced different bytes"; ok=0
    fi

    [ "$ok" = "1" ] && echo "ok    $label"
}

signal_filter() {
    case "$1" in
        sine)   echo "sine=frequency=440:duration=4" ;;
        clicks) echo "aevalsrc=exprs='if(eq(mod(n\,2205)\,0)\,1\,0)':d=4" ;;
        noise)  echo "anoisesrc=color=white:duration=4" ;;
    esac
}

echo "== generating corpus in $WORKDIR =="

for signal in sine clicks noise; do
    filter="$(signal_filter "$signal")"
    for samplerate in 44100 22050 11025; do
        for channels_label in mono stereo; do
            ch=2; mono_flag=""
            if [ "$channels_label" = "mono" ]; then ch=1; mono_flag="-m m"; fi
            wav="$WORKDIR/${signal}_${samplerate}_${channels_label}.wav"
            ffmpeg -y -v error -f lavfi -i "${filter}:sample_rate=${samplerate}" \
                   -ar "$samplerate" -ac "$ch" "$wav" 2>/dev/null || continue

            for bitrate_mode in cbr vbr; do
                lame_br="-V 2"
                [ "$bitrate_mode" = "cbr" ] && lame_br="-b 128 --cbr"
                for crc_mode in crc nocrc; do
                    lame_crc=""
                    [ "$crc_mode" = "crc" ] && lame_crc="-c"

                    mp3="$WORKDIR/${signal}_${samplerate}_${channels_label}_${bitrate_mode}_${crc_mode}.mp3"
                    lame $mono_flag $lame_crc $lame_br "$wav" "$mp3" >/dev/null 2>&1 || continue

                    check_case "${signal}/${samplerate}Hz/${channels_label}/${bitrate_mode}/${crc_mode}" "$mp3"
                done
            done
        done
    done
done

summary cases
