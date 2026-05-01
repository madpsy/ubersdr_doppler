#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_doppler flags
#
# Environment variables:
#   UBERSDR_URL   UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   DOPPLER_DATA  Data directory for stations.json, settings.json and CSV logs (default: /data)
#   UI_PASSWORD   Password required for write actions in the web UI (empty = write actions disabled)
#   WEB_PORT      Port for the web UI server (default: 6096)

set -e

args=""

[ -n "$UBERSDR_URL"  ] && args="$args -url $UBERSDR_URL"
[ -n "$UI_PASSWORD"  ] && args="$args -ui-password $UI_PASSWORD"

DOPPLER_DATA="${DOPPLER_DATA:-/data}"
args="$args -data $DOPPLER_DATA"

# WEB_PORT → -listen :<port>
if [ -n "$WEB_PORT" ]; then
    args="$args -listen :$WEB_PORT"
else
    args="$args -listen :6096"
fi

# Append any CLI args passed directly to the container
# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_doppler $args "$@"
