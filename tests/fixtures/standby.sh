#!/usr/bin/env bash
set -Eeuo pipefail
bash "$CONTROL/handover-worker.sh" --install
"$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json"
