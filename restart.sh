#!/usr/bin/env bash
# restart.sh — restart the ubersdr_doppler service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/doppler"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_doppler..."
docker compose down
echo "Starting ubersdr_doppler..."
docker compose up -d --remove-orphans
echo "Done."
echo "  View logs : docker compose logs -f"
echo "  Web UI    : http://localhost:6096"
