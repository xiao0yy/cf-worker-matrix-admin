#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
for module in node/release node/control poller; do
    (cd "$ROOT/$module"; go test ./...; go vet ./...)
done
