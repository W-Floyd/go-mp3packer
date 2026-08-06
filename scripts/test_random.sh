#!/usr/bin/env bash
# Randomized lossless validation for the mp3packer CLI.
#
# Complements test_matrix.sh's fixed grid: instead of a handful of representative
# points it draws N random cases from the whole parameter space — all nine
# MPEG-1/2/2.5 sample rates, random durations, random sine frequencies, noise
# seeds and click periods, random bitrates, every channel mode, CRC on and off —
# and runs the same lossless and thread-determinism checks on each.
#
# Usage: scripts/test_random.sh [N] [seed] [path/to/mp3packer]
#   N     number of random cases (default 50)
#   seed  RANDOM seed, for reproducibility (default: $RANDOM)
# With no binary path, builds the CLI from the working tree.

. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

N="${1:-50}"
SEED="${2:-$RANDOM}"

require_tools ffmpeg lame go
make_workdir
resolve_bin "${3:-}"
NCPU="$(ncpu)"

RANDOM=$SEED
echo "== seed: $SEED (rerun with the same seed to reproduce) =="

# Both helpers assign into a caller-named variable through eval rather than
# echoing into a command substitution: `$(...)` forks a subshell, and $RANDOM's
# state advanced inside one never reaches the parent — every such call would
# silently restart from the same state and the seed would stop reproducing
# anything. (No `local -n` either: macOS ships bash 3.2, which predates it.)
rand_range() { # rand_range VARNAME MIN MAX
    local _rr_val=$(( $2 + RANDOM % ($3 - $2 + 1) ))
    eval "$1=\$_rr_val"
}

rand_choice() { # rand_choice VARNAME item1 item2 ...
    local _rc_var="$1"; shift
    local _rc_arr=("$@")
    local _rc_val="${_rc_arr[$((RANDOM % $#))]}"
    eval "$_rc_var=\$_rc_val"
}

SAMPLERATES=(44100 48000 32000 22050 24000 16000 11025 12000 8000)

# CBR bitrates valid for each version's table (kbps). Asking lame for a bitrate
# outside the chosen version's table does not error — it silently resamples and
# switches version to make the request satisfiable (-b 8 on a 44100 Hz source
# comes back as an 8000 Hz MPEG-2.5 file), which would quietly defeat the point
# of drawing samplerate and bitrate independently.
MPEG1_BITRATES=(32 40 48 56 64 80 96 112 128 160 192 224 256 320)
MPEG2_BITRATES=(8 16 24 32 40 48 56 64 80 96 112 128 144 160)

is_mpeg1() {
    case "$1" in
        44100|48000|32000) return 0 ;;
        *) return 1 ;;
    esac
}

check_case() { # check_case LABEL SRC
    local label="$1" src="$2"
    TOTAL=$((TOTAL + 1))

    local ref="$WORKDIR/ref.raw"
    if ! decode ffmpeg "$src" "$ref"; then
        fail "$label :: source failed to decode"; return
    fi

    local plain="$WORKDIR/o_plain.mp3" t1="$WORKDIR/o_t1.mp3" tn="$WORKDIR/o_tn.mp3"
    pack -n       "$src" "$plain" || { fail "$label :: -n pack failed"; return; }
    pack -j 1     "$src" "$t1"    || { fail "$label :: one-worker pack failed"; return; }
    pack -j "$NCPU" "$src" "$tn"  || { fail "$label :: all-cores pack failed"; return; }

    local ok=1 f got msg i=0
    for f in "$plain" "$t1" "$tn"; do
        got="$WORKDIR/got_$i.raw"
        if ! decode ffmpeg "$f" "$got"; then
            fail "$label :: $(basename "$f") failed to decode"; ok=0; i=$((i + 1)); continue
        fi
        if ! msg="$(same_pcm "$ref" "$got")"; then
            fail "$label :: $(basename "$f") $msg"; ok=0
        fi
        i=$((i + 1))
    done

    if ! cmp -s "$t1" "$tn"; then
        fail "$label :: one worker and $NCPU workers produced different bytes"; ok=0
    fi

    [ "$ok" = "1" ] && echo "ok    $label"
}

echo "== drawing $N cases into $WORKDIR =="

case_n=0
while [ "$case_n" -lt "$N" ]; do
    case_n=$((case_n + 1))

    rand_choice samplerate "${SAMPLERATES[@]}"
    rand_choice channels mono stereo joint
    rand_choice signal sine clicks noise
    rand_choice bitrate_mode cbr vbr
    rand_choice crc_mode crc nocrc
    rand_range duration 1 6

    case "$signal" in
        sine)
            rand_range freq 100 8000
            # A tone above the Nyquist frequency is not a tone; keep it inside.
            [ $((freq * 2)) -gt "$samplerate" ] && freq=$((samplerate / 4))
            filter="sine=frequency=${freq}:duration=${duration}"
            detail="${freq}Hz" ;;
        clicks)
            rand_range period 512 8192
            filter="aevalsrc=exprs='if(eq(mod(n\,${period})\,0)\,1\,0)':d=${duration}"
            detail="every${period}" ;;
        noise)
            rand_range seed 1 9999
            rand_choice color white pink brown
            filter="anoisesrc=color=${color}:seed=${seed}:duration=${duration}"
            detail="${color}${seed}" ;;
    esac

    ch=2; mono_flag=""
    case "$channels" in
        mono)   ch=1; mono_flag="-m m" ;;
        stereo) mono_flag="-m s" ;;
        joint)  mono_flag="-m j" ;;
    esac

    wav="$WORKDIR/case${case_n}.wav"
    ffmpeg -y -v error -f lavfi -i "${filter}:sample_rate=${samplerate}" \
           -ar "$samplerate" -ac "$ch" "$wav" 2>/dev/null || {
        fail "case${case_n} :: could not synthesise the source"; TOTAL=$((TOTAL + 1)); continue
    }

    if [ "$bitrate_mode" = "cbr" ]; then
        if is_mpeg1 "$samplerate"; then
            rand_choice br "${MPEG1_BITRATES[@]}"
        else
            rand_choice br "${MPEG2_BITRATES[@]}"
        fi
        lame_br="-b $br --cbr"
        rate_label="${br}k"
    else
        rand_range v 0 6
        lame_br="-V $v"
        rate_label="V$v"
    fi
    lame_crc=""
    [ "$crc_mode" = "crc" ] && lame_crc="-c"

    mp3="$WORKDIR/case${case_n}.mp3"
    lame $mono_flag $lame_crc $lame_br "$wav" "$mp3" >/dev/null 2>&1 || {
        fail "case${case_n} :: lame refused the parameters ($mono_flag $lame_crc $lame_br)"
        TOTAL=$((TOTAL + 1)); continue
    }

    check_case "case${case_n} ${signal}/${detail}/${samplerate}Hz/${channels}/${rate_label}/${crc_mode}/${duration}s" "$mp3"
    rm -f "$wav" "$mp3"
done

summary cases
