#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
for tool in go node jq sqlite3 bash; do command -v "$tool" >/dev/null || { echo "Missing test dependency: $tool" >&2; exit 1; }; done
[[ $EUID == 0 ]] || { echo 'Run node integration tests as root in an isolated Linux test environment; production root guards remain enabled.' >&2; exit 1; }
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
export MATRIX_RELEASE_BINARY="$WORK/matrix-release"
(cd node/release; go build -buildvcs=false -trimpath -o "$MATRIX_RELEASE_BINARY" .)
node --test node/tests/*.test.mjs
for test in test-agent-state.sh test-final-worker.sh test-telegram-backup.sh; do bash "node/tests/$test"; done
export RESTIC_TEST_BINARY=${RESTIC_TEST_BINARY:-$(command -v restic || true)}
[[ -x "$RESTIC_TEST_BINARY" ]] || { echo 'Provide RESTIC_TEST_BINARY (restic 0.19.1) for the real repository round-trip test' >&2; exit 1; }
bash node/tests/test-cold-producer.sh
