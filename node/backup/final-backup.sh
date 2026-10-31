#!/usr/bin/env bash
# Local, resumable final-backup primitive. Never transfers ownership or reopens
# ingress on success/failure. A separate explicit final-resume ends the freeze.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
CONTROL=${CONTROL_DIR:-$BASE/../control}
CONFIG=${MATRIX_BACKUP_CONFIG:-$BASE/restic.env}
OP= INSTANCE= EPOCH= RESUME=no
while (( $# )); do
    case "$1" in
        --operation-id) OP=$2; shift 2 ;;
        --expected-instance) INSTANCE=$2; shift 2 ;;
        --expected-epoch) EPOCH=$2; shift 2 ;;
        --resume) RESUME=yes; shift ;;
        *) echo 'Use --operation-id ID --expected-instance ID --expected-epoch N' >&2; exit 1 ;;
    esac
done
[[ "$OP" =~ ^[a-zA-Z0-9_-]{1,100}$ && "$INSTANCE" =~ ^[a-zA-Z0-9_.-]+$ && "$EPOCH" =~ ^[1-9][0-9]*$ ]] || exit 1
[[ $(stat -c %a "$CONFIG") =~ ^[046]00$ ]] || exit 1
# Trusted local host configuration, not data downloaded from S3.
STATE_DIR=/opt/matrix/backup-state/tuwunel-restic
DATA_DIR=/opt/matrix/deploy/tuwunel/data
source "$CONFIG"
CONTROL_STATE=$(jq -er .state_dir "$CONTROL/local.json")
DIR="$CONTROL_STATE/final/$OP"
mkdir -p "$DIR"
if [[ "$RESUME" == yes ]]; then
    "$CONTROL/bin/matrix-control" final-resume --config "$CONTROL/local.json" --expected-instance "$INSTANCE" --expected-epoch "$EPOCH" --operation-id "$OP"
    deadline=$((SECONDS+180))
    until "$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json" | jq -e --arg i "$INSTANCE" --argjson e "$EPOCH" '.serving and (.blocked|not) and .instance_id==$i and .bound_epoch==$e and .final_backup==null' >/dev/null; do
        (( SECONDS < deadline )) || { echo 'Source did not become healthy; timer remains paused' >&2; exit 1; }
        sleep 1
    done
    if [[ -f "$DIR/timer.json" ]] && jq -e .timer_was_active "$DIR/timer.json" >/dev/null; then systemctl start tuwunel-restic.timer; fi
    echo 'Original instance resumed. The closed final recovery set is no longer valid for a future handover.'
    exit 0
fi
# Keep fd 7 inherited by every uploader. fd 8 belongs to the publication lock,
# fd 9 to the producer lock; replacing either must not release this lifetime lock.
exec 7>"$DIR/worker.lock"
flock -n 7 || { echo 'This final worker is already running' >&2; exit 1; }
step=preflight
finish() {
    code=$?
    if (( code != 0 )); then
        jq -n --arg op "$OP" --arg step "$step" --argjson code "$code" \
            '{operation_id:$op,step:$step,exit_code:$code,error:"Final worker stopped; source remains frozen. Retry the same operation or explicitly final-resume.",at:(now|todateiso8601)}' >"$DIR/error.json.tmp"
        mv "$DIR/error.json.tmp" "$DIR/error.json"
    fi
}
trap finish EXIT
ctl() { "$CONTROL/bin/matrix-control" "$1" --config "$CONTROL/local.json" --expected-instance "$INSTANCE" --expected-epoch "$EPOCH" --operation-id "$OP"; }
status() { "$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json"; }
if [[ ! -f "$DIR/timer.json" ]]; then
    active=false
    systemctl is-active --quiet tuwunel-restic.timer && active=true
    jq -n --argjson active "$active" '{timer_was_active:$active}' >"$DIR/timer.json.tmp"
    mv "$DIR/timer.json.tmp" "$DIR/timer.json"
fi
step=close-ingress
ctl final-begin
step=pause-producer
systemctl stop tuwunel-restic.timer
deadline=$((SECONDS+900))
while :; do
    state=$(systemctl show tuwunel-restic.service --property=ActiveState --value)
    case "$state" in active|activating|deactivating) ;; *) break ;; esac
    (( SECONDS < deadline )) || { echo 'Ordinary backup did not finish; source remains fenced' >&2; exit 1; }
    sleep 2
done
exec 9>"$STATE_DIR/operation.lock"
flock -w 900 9 || { echo 'Ordinary producer lock is busy' >&2; exit 1; }
step=stop-writers
ctl final-stop
deadline=$((SECONDS+4500))
until ctl final-frozen >"$DIR/frozen-call.log" 2>&1; do
    (( SECONDS < deadline )) || { echo 'Clean stop not proven; no final snapshot is allowed' >&2; exit 1; }
    sleep 2
done
step=snapshotting
ctl final-snapshot
ctl final-verify
# A checkpoint left by an interrupted ordinary job is staging, not live DB.
# Preserve it for inspection instead of copying a nested snapshot into the DB.
if [[ -e "$DATA_DIR/checkpoint-restic" ]]; then
    [[ -f "$STATE_DIR/preparing/request.txt" && -d "$DATA_DIR/checkpoint-restic" && ! -L "$DATA_DIR/checkpoint-restic" && -f "$DATA_DIR/checkpoint-restic/CURRENT" ]] || { echo 'Unclassified checkpoint directory; manual inspection required' >&2; exit 1; }
    mkdir -p "$STATE_DIR/finals/$OP"
    mv "$DATA_DIR/checkpoint-restic" "$STATE_DIR/finals/$OP/interrupted-daily-checkpoint"
fi
phase=$(status | jq -er .final_backup.phase)
if [[ "$phase" != READY ]]; then
    step=upload-and-verify
    FINAL_LOCK_HELD=1 CONTROL_DIR="$CONTROL" MATRIX_BACKUP_CONFIG="$CONFIG" bash "$BASE/tuwunel-restic.sh" cold-run "$OP"
    ctl final-verify
    cp "$STATE_DIR/finals/$OP/published.json" "$DIR/receipt.json.tmp"
    mv "$DIR/receipt.json.tmp" "$DIR/receipt.json"
    step=publish-frozen-result
    ctl final-complete
fi
rm -f "$DIR/error.json"
status
echo 'Final recovery set verified. Source is still frozen; ownership unchanged. Use explicit final-resume only after this worker exits.'
