#!/usr/bin/env bash
# --check is the default. No package installation, SSH, CF calls or owner changes.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")/.." && pwd)
ROOT=${MATRIX_FOUNDATION:-/opt/matrix}
MODE=${1:---check}
[[ "$MODE" == --check || "$MODE" == --install ]] || { echo 'Use --check or --install';exit 1; }
[[ "$ROOT" == /* && "$ROOT" != *[!a-zA-Z0-9_./-]* && $(realpath -e "$ROOT") == "$ROOT" ]] || exit 1
CONTROL="$ROOT/deploy/control"
[[ -f "$CONTROL/managed" && -x "$CONTROL/bin/matrix-control" ]] || { echo 'Agent must first prepare this instance from S3';exit 1; }
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || { echo 'This binary release requires linux/amd64';exit 1; }
(cd "$BASE"; sha256sum -c bin/SHA256SUMS)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
"$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json" >"$work/status.json"
ACTIVE=$(jq -er --arg root "$ROOT" '.deployment_root | if .==null or .=="" then $root else . end' "$work/status.json")
[[ "$ACTIVE" == "$ROOT" || "$ACTIVE" == "$ROOT/generations/"* ]] || exit 1
[[ "$ACTIVE" != *[!a-zA-Z0-9_./-]* && $(realpath -e "$ACTIVE") == "$ACTIVE" ]] || exit 1
# Render both foundations before making any file change. No guessed patch.
i=0
for root in "$ROOT" "$ACTIVE"; do
    [[ $i == 0 || "$ACTIVE" != "$ROOT" ]] || continue
    backup="$root/deploy/backup"
    bash "$BASE/deploy/patch-file.sh" "$backup/tuwunel-restic.sh" producer >"$work/producer-$i"
    bash "$BASE/deploy/patch-file.sh" "$backup/prepare-standby.sh" standby >"$work/standby-$i"
    bash -n "$work/producer-$i" "$work/standby-$i"
    i=$((i+1))
done
if [[ "$MODE" == --check ]]; then
    echo 'CHECK OK: poller binary, existing identity and backup integration match. Nothing installed.'
    exit 0
fi
[[ $EUID == 0 ]] || { echo 'Run installer as root';exit 1; }
# Only install while the ordinary producer and final/target workers are idle.
source "$ACTIVE/deploy/backup/restic.env"
STATE_DIR=${STATE_DIR:-/opt/matrix/backup-state/tuwunel-restic}
exec 9>"$STATE_DIR/operation.lock"
flock -n 9 || { echo 'Backup producer busy; retry installation later';exit 1; }
local_state=$(jq -er .state_dir "$CONTROL/local.json")
exec 8>"$local_state/handover-worker.lock"
flock -n 8 || { echo 'Handover worker busy; retry installation later';exit 1; }
exec 7>"$local_state/mailbox.lock"
flock -n 7 || { echo 'Mailbox poller busy; retry installation later';exit 1; }
jq -e '.control.phase=="ONLINE" or (.bound_epoch==0 and .blocked)' "$work/status.json" >/dev/null || { echo 'Do not install during an active handover';exit 1; }
mkdir -p "$CONTROL/mailbox"
install -m600 "$BASE/poller/"*.go "$BASE/poller/go.mod" "$CONTROL/mailbox/"
install -m700 "$BASE/deploy/mailbox-install.sh" "$CONTROL/"
install -m600 "$BASE/deploy/matrix-mailbox.service" "$BASE/deploy/matrix-mailbox.timer" "$CONTROL/"
install -m700 "$BASE/bin/matrix-mailbox-linux-amd64" "$CONTROL/bin/matrix-mailbox.new"
mv "$CONTROL/bin/matrix-mailbox.new" "$CONTROL/bin/matrix-mailbox"
i=0
for root in "$ROOT" "$ACTIVE"; do
    [[ $i == 0 || "$ACTIVE" != "$ROOT" ]] || continue
    backup="$root/deploy/backup"
    for spec in 'producer:tuwunel-restic.sh' 'standby:prepare-standby.sh'; do
        type=${spec%%:*};file=${spec#*:}
        install -m700 "$work/$type-$i" "$backup/$file.mailbox-new"
        mv "$backup/$file.mailbox-new" "$backup/$file"
    done
    i=$((i+1))
done
bash "$CONTROL/mailbox-install.sh" --install
echo 'Installed command poller. Business services and control owner/epoch were not changed.'
echo 'Wait for the next ordinary backup to publish the poller and standby bootstrap together.'
