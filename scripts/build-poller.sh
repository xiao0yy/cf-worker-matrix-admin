#!/usr/bin/env bash
set -Eeuo pipefail
BASE=$(cd -- "$(dirname -- "$0")/.." && pwd)
command -v go >/dev/null || { echo 'Agent must prepare Go 1.23+ to build the instance poller';exit 1; }
mkdir -p "$BASE/bin"
(cd "$BASE/poller"; CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$BASE/bin/matrix-mailbox-linux-amd64" .)
(cd "$BASE"; sha256sum bin/matrix-mailbox-linux-amd64 >bin/SHA256SUMS)
echo 'Built linux/amd64 instance poller. No service installed.'
