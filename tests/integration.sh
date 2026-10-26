#!/usr/bin/env bash
set -Eeuo pipefail
BASE=$(cd -- "$(dirname -- "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
for kind in producer standby; do
    bash "$BASE/deploy/patch-file.sh" "$BASE/tests/fixtures/$kind.sh" "$kind" >"$WORK/$kind"
    bash -n "$WORK/$kind"
    bash "$BASE/deploy/patch-file.sh" "$WORK/$kind" "$kind" >"$WORK/$kind.again"
    cmp "$WORK/$kind" "$WORK/$kind.again"
done
echo 'PASS: backup/standby integration patches are idempotent and valid Bash'
printf '%s\n' '# unknown layout' >"$WORK/unknown"
if bash "$BASE/deploy/patch-file.sh" "$WORK/unknown" producer >"$WORK/out" 2>/dev/null; then exit 1; fi
printf '%s\n' '# MATRIX_MAILBOX_V1: partial' >"$WORK/partial"
if bash "$BASE/deploy/patch-file.sh" "$WORK/partial" producer >"$WORK/out" 2>/dev/null; then exit 1; fi
echo 'PASS: unknown layout and partial patch are rejected'
control="$WORK/control"
PREPARING="$WORK/preparing"
mkdir -p "$control/bin" "$control/mailbox" "$PREPARING/bundle/payload/control/bin"
printf test >"$control/bin/matrix-mailbox"
chmod 700 "$control/bin/matrix-mailbox"
printf test >"$control/mailbox/main.go"
cp "$BASE/deploy/mailbox-install.sh" "$BASE/deploy/matrix-mailbox.service" "$BASE/deploy/matrix-mailbox.timer" "$control/"
source "$BASE/deploy/producer-insert.txt"
cmp "$control/bin/matrix-mailbox" "$PREPARING/bundle/payload/control/bin/matrix-mailbox"
cmp "$control/mailbox/main.go" "$PREPARING/bundle/payload/control/mailbox/main.go"
echo 'PASS: future backup payload carries the poller, source and bootstrap units'
REPO="$WORK/repo"
ROOT="$WORK/foundation"
mkdir -p "$REPO/bin" "$ROOT/deploy/control/bin" "$ROOT/deploy/backup"
cp -a "$BASE/deploy" "$REPO/"
printf '#!/bin/sh\nexit 0\n' >"$REPO/bin/matrix-mailbox-linux-amd64"
(cd "$REPO"; sha256sum bin/matrix-mailbox-linux-amd64 >bin/SHA256SUMS)
printf '%s\n' '{"schema":1}' >"$ROOT/deploy/control/managed"
cat >"$ROOT/deploy/control/bin/matrix-control" <<'MOCK'
#!/bin/sh
printf '%s\n' '{"schema":1,"deployment_root":"","bound_epoch":0,"blocked":true,"control":{"phase":"ONLINE"}}'
MOCK
chmod 700 "$ROOT/deploy/control/bin/matrix-control"
cp "$BASE/tests/fixtures/producer.sh" "$ROOT/deploy/backup/tuwunel-restic.sh"
cp "$BASE/tests/fixtures/standby.sh" "$ROOT/deploy/backup/prepare-standby.sh"
MATRIX_FOUNDATION="$ROOT" bash "$REPO/deploy/install-instance.sh" --check
[[ ! -e "$ROOT/deploy/control/bin/matrix-mailbox" && ! -e "$ROOT/deploy/control/mailbox" ]]
cmp "$BASE/tests/fixtures/producer.sh" "$ROOT/deploy/backup/tuwunel-restic.sh"
echo 'PASS: installer --check does not install, rewrite backup scripts, or start services'
