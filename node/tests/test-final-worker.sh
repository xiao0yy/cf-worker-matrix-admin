#!/usr/bin/env bash
# Exercise real worker locking/error/retry flow with local command doubles only.
set -Eeuo pipefail
BASE=$(cd -- "$(dirname -- "$0")/../backup" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/backup" "$WORK/control/bin" "$WORK/state" "$WORK/data" "$WORK/mock"
cp "$BASE/final-backup.sh" "$WORK/backup/"
printf 'STATE_DIR=%q\nDATA_DIR=%q\n' "$WORK/state" "$WORK/data" >"$WORK/config.env"
chmod 600 "$WORK/config.env"
jq -n --arg state "$WORK/control/state" '{state_dir:$state}' >"$WORK/control/local.json"
touch "$WORK/timer-active" "$WORK/fail-upload"
jq -n '{instance_id:"source",bound_epoch:1,serving:true,blocked:false,final_backup:null}' >"$WORK/status.json"
cat >"$WORK/mock/systemctl" <<'MOCK'
#!/usr/bin/env bash
echo "systemctl $*" >>"$TEST_ROOT/trace"
case "$1" in
 is-active) [[ -e "$TEST_ROOT/timer-active" ]] ;;
 stop) rm -f "$TEST_ROOT/timer-active" ;;
 start) touch "$TEST_ROOT/timer-active" ;;
 show) echo inactive ;;
 *) exit 1 ;;
esac
MOCK
cat >"$WORK/control/bin/matrix-control" <<'MOCK'
#!/usr/bin/env bash
set -e
action=$1
echo "$action" >>"$TEST_ROOT/trace"
p="$TEST_ROOT/status.json"
case "$action" in
 status) cat "$p"; exit 0 ;;
 final-begin) jq '.serving=false | .blocked=true | .final_backup=(.final_backup // {id:"op-test",phase:"DRAINING",cutoff_time:"2026-10-25T00:00:00Z"})' "$p" >"$p.tmp" ;;
 final-stop) jq 'if .final_backup.phase=="DRAINING" then .final_backup.phase="STOPPING" else . end' "$p" >"$p.tmp" ;;
 final-frozen) jq 'if .final_backup.phase=="STOPPING" then .final_backup.phase="FROZEN" else . end' "$p" >"$p.tmp" ;;
 final-snapshot) jq 'if .final_backup.phase=="FROZEN" then .final_backup.phase="SNAPSHOTTING" else . end' "$p" >"$p.tmp" ;;
 final-verify) exit 0 ;;
 final-complete) [[ -f "$TEST_ROOT/control/state/final/op-test/receipt.json" ]]; jq '.final_backup.phase="READY"' "$p" >"$p.tmp" ;;
 final-resume) jq '.serving=true | .blocked=false | .final_backup=null' "$p" >"$p.tmp" ;;
 *) exit 1 ;;
esac
mv "$p.tmp" "$p"
MOCK
cat >"$WORK/backup/tuwunel-restic.sh" <<'MOCK'
#!/usr/bin/env bash
echo producer >>"$TEST_ROOT/trace"
[[ ! -e "$TEST_ROOT/fail-upload" ]] || exit 42
mkdir -p "$TEST_ROOT/state/finals/op-test"
echo '{"operation_id":"op-test"}' >"$TEST_ROOT/state/finals/op-test/published.json"
MOCK
chmod 700 "$WORK/mock/systemctl" "$WORK/control/bin/matrix-control"
export TEST_ROOT="$WORK" CONTROL_DIR="$WORK/control" MATRIX_BACKUP_CONFIG="$WORK/config.env" PATH="$WORK/mock:$PATH"
args=(--operation-id op-test --expected-instance source --expected-epoch 1)
if bash "$WORK/backup/final-backup.sh" "${args[@]}" >"$WORK/first.log" 2>&1; then echo 'FAIL: injected uploader failure accepted'; exit 1; fi
jq -e '.blocked and (.serving|not) and .final_backup.phase=="SNAPSHOTTING"' "$WORK/status.json" >/dev/null
[[ ! -e "$WORK/timer-active" && -s "$WORK/control/state/final/op-test/error.json" ]]
! grep -Eq '^final-resume$' "$WORK/trace"
echo 'PASS: upload failure leaves source frozen, timer paused, and records its phase'
rm "$WORK/fail-upload"
bash "$WORK/backup/final-backup.sh" "${args[@]}" >"$WORK/retry.log" 2>&1
jq -e '.blocked and (.serving|not) and .final_backup.phase=="READY"' "$WORK/status.json" >/dev/null
[[ ! -e "$WORK/control/state/final/op-test/error.json" ]]
count=$(grep -c '^producer$' "$WORK/trace")
bash "$WORK/backup/final-backup.sh" "${args[@]}" >"$WORK/ready-retry.log" 2>&1
[[ $(grep -c '^producer$' "$WORK/trace") == "$count" ]]
echo 'PASS: same operation retries to READY and does not re-upload a completed set'
bash "$WORK/backup/final-backup.sh" --resume "${args[@]}" >"$WORK/resume.log" 2>&1
jq -e '.serving and (.blocked|not) and .final_backup==null' "$WORK/status.json" >/dev/null
[[ -e "$WORK/timer-active" ]]
echo 'PASS: only explicit resume reopens the source and restores its prior timer'
