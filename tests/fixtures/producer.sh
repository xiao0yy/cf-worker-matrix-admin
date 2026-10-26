#!/usr/bin/env bash
set -Eeuo pipefail
if [[ -f "$control/managed" ]]; then
        # Preserve host settings only as a source reference, never an instance ID
        # or run permission to reuse automatically on a different machine.
        :
fi
