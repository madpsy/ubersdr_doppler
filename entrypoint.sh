#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_doppler flags
#
# Environment variables:
#   UBERSDR_URL          UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   DOPPLER_STATIONS     Comma-separated label:freq_hz pairs
#                        e.g. "WWV-10:10000000,WWV-15:15000000,WWVH-5:5000000"
#   DOPPLER_CALLSIGN     Your amateur radio callsign (included in CSV)
#   DOPPLER_GRID         Your Maidenhead grid locator (included in CSV)
#   DOPPLER_MIN_SNR      Minimum SNR in dB to accept a measurement (default: 10)
#   DOPPLER_MAX_DRIFT_HZ Maximum Doppler drift to search in Hz (default: 100)
#   DOPPLER_LOG_DIR      Directory for CSV log files (default: /data)
#   WEB_PORT             Port for the web UI server (default: 6096)

set -e

args=""

[ -n "$UBERSDR_URL"          ] && args="$args -url $UBERSDR_URL"
[ -n "$DOPPLER_STATIONS"     ] && args="$args -stations $DOPPLER_STATIONS"
[ -n "$DOPPLER_CALLSIGN"     ] && args="$args -callsign $DOPPLER_CALLSIGN"
[ -n "$DOPPLER_GRID"         ] && args="$args -grid $DOPPLER_GRID"
[ -n "$DOPPLER_MIN_SNR"      ] && args="$args -min-snr $DOPPLER_MIN_SNR"
[ -n "$DOPPLER_MAX_DRIFT_HZ" ] && args="$args -max-drift $DOPPLER_MAX_DRIFT_HZ"

DOPPLER_LOG_DIR="${DOPPLER_LOG_DIR:-/data}"
args="$args -log-dir $DOPPLER_LOG_DIR"

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6096"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_doppler $args "$@"
