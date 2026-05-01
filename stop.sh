#!/usr/bin/env bash
# stop.sh — stop the ubersdr_doppler service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/doppler"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_doppler..."
docker compose down
echo "Done."
