#!/usr/bin/env bash
# start.sh — start the ubersdr_doppler service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/doppler"

cd "${INSTALL_DIR}"
echo "Starting ubersdr_doppler..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6096"
