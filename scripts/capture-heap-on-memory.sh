#!/usr/bin/env bash

set -euo pipefail

CONTAINER="${CONTAINER:-doco-cd}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-10}"
THRESHOLD_MIB="${THRESHOLD_MIB:-60}"
OUTPUT_FILE="${OUTPUT_FILE:-after.pb.gz}"

usage() {
    cat <<'EOF'
Usage: scripts/capture-heap-on-memory.sh

Polls a Docker container's memory usage and captures a GC-forced Go heap profile
when usage exceeds the configured threshold. The script exits after one capture.

Environment variables:
  CONTAINER         Container name or ID (default: doco-cd)
  INTERVAL_SECONDS  Polling interval in seconds (default: 10)
  THRESHOLD_MIB     Memory threshold in MiB (default: 60)
  OUTPUT_FILE       Heap profile destination (default: after.pb.gz)
EOF
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    usage
    exit 0
fi

if ! [[ "$INTERVAL_SECONDS" =~ ^[1-9][0-9]*$ ]]; then
    printf 'INTERVAL_SECONDS must be a positive integer; got %q\n' "$INTERVAL_SECONDS" >&2
    exit 1
fi

if ! awk -v threshold="$THRESHOLD_MIB" 'BEGIN { exit !(threshold > 0) }'; then
    printf 'THRESHOLD_MIB must be a positive number; got %q\n' "$THRESHOLD_MIB" >&2
    exit 1
fi

memory_to_mib() {
    local memory="$1"
    local number
    local unit
    local multiplier

    if [[ ! "$memory" =~ ^([0-9]+([.][0-9]+)?)([[:alpha:]]+)$ ]]; then
        return 1
    fi

    number="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[3]}"

    case "$unit" in
        B)
            multiplier="0.00000095367431640625"
            ;;
        KiB)
            multiplier="0.0009765625"
            ;;
        MiB)
            multiplier="1"
            ;;
        GiB)
            multiplier="1024"
            ;;
        kB)
            multiplier="0.00095367431640625"
            ;;
        MB)
            multiplier="0.95367431640625"
            ;;
        GB)
            multiplier="953.67431640625"
            ;;
        *)
            return 1
            ;;
    esac

    awk -v number="$number" -v multiplier="$multiplier" 'BEGIN { print number * multiplier }'
}

printf 'Watching container %q every %ss; capturing when memory exceeds %s MiB.\n' \
    "$CONTAINER" "$INTERVAL_SECONDS" "$THRESHOLD_MIB"

while true; do
    if ! memory_usage="$(docker stats --no-stream --format '{{.MemUsage}}' "$CONTAINER" 2>&1)"; then
        printf 'Unable to read memory usage for container %q: %s\n' "$CONTAINER" "$memory_usage" >&2
        sleep "$INTERVAL_SECONDS"
        continue
    fi

    memory_value="${memory_usage%% *}"
    if ! memory_mib="$(memory_to_mib "$memory_value")"; then
        printf 'Unable to parse Docker memory usage %q.\n' "$memory_usage" >&2
        sleep "$INTERVAL_SECONDS"
        continue
    fi

    printf '%s %s (%s MiB)\n' "$(date --iso-8601=seconds)" "$memory_usage" "$memory_mib"

    if awk -v memory="$memory_mib" -v threshold="$THRESHOLD_MIB" 'BEGIN { exit !(memory > threshold) }'; then
        temporary_output="$(mktemp "${OUTPUT_FILE}.tmp.XXXXXX")"
        trap 'rm -f -- "$temporary_output"' EXIT

        printf 'Memory threshold exceeded; capturing heap profile in %s.\n' "$OUTPUT_FILE"
        docker run --rm --network "container:${CONTAINER}" curlimages/curl:latest \
            -sSf "http://127.0.0.1:6060/debug/pprof/heap?gc=1" > "$temporary_output"
        mv -- "$temporary_output" "$OUTPUT_FILE"
        trap - EXIT

        printf 'Heap profile saved to %s.\n' "$OUTPUT_FILE"
        exit 0
    fi

    sleep "$INTERVAL_SECONDS"
done
