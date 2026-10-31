#!/usr/bin/env bash
# Start only a disposable bridge copy. No network, published ports or live APIs.
set -Eeuo pipefail
umask 077
[[ $# == 4 ]] || { echo 'Usage: verify-telegram-runtime.sh IMAGE DATA METADATA NEW_WORKDIR' >&2; exit 1; }
IMAGE=$1; DATA=$(realpath -e "$2"); METADATA=$(realpath -e "$3"); WORK=$(realpath -m "$4")
DOCKER_BIN=${DOCKER_BIN:-docker}
SQLITE_BIN=${SQLITE_BIN:-sqlite3}
[[ ! -e "$WORK" && "$WORK/" != "$DATA/"* && "$DATA/" != "$WORK/"* ]] || exit 1
[[ $("$DOCKER_BIN" image inspect --format '{{.Id}}' "$IMAGE") == $(jq -er .image_id "$METADATA") ]] || { echo 'Telegram image identity mismatch' >&2; exit 1; }
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
bash "$BASE/telegram-backup.sh" verify "$DATA/telegram.db" "$METADATA"
mkdir -p "$WORK"
cp -a --reflink=auto "$DATA" "$WORK/data"
NAME="telegram-recovery-check-$(cat /proc/sys/kernel/random/uuid)"
trap '"$DOCKER_BIN" rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
"$DOCKER_BIN" run -d --name "$NAME" --network none --read-only --memory 128m --memory-swap 192m \
    --pids-limit 96 --cpus 0.5 --tmpfs /tmp:size=16m -v "$WORK/data:/data" "$IMAGE" >"$WORK/container-id"
deadline=$((SECONDS + 45))
while true; do
    "$DOCKER_BIN" logs "$NAME" >"$WORK/startup.log" 2>&1
    # A successful migration logs "Upgrading database", not "Database is up to
    # date". Starting the Matrix connector proves the main DB upgrade returned;
    # require the state DB check and HTTP startup as separate milestones.
    if jq -se 'any(.[];.message=="Starting Matrix connector") and any(.[];.message=="Database is up to date" and .db_section=="matrix_state") and any(.[];.message=="Starting HTTP listener")' "$WORK/startup.log" >/dev/null 2>&1; then break; fi
    [[ $("$DOCKER_BIN" inspect --format '{{.State.Running}}' "$NAME") == true ]] || { echo 'Recovered Telegram bridge exited during initialization' >&2; exit 1; }
    (( SECONDS < deadline )) || { echo 'Recovered Telegram bridge database initialization timed out' >&2; exit 1; }
    sleep 1
done
"$DOCKER_BIN" stop --time 10 "$NAME" >/dev/null
exit_code=$("$DOCKER_BIN" inspect --format '{{.State.ExitCode}}' "$NAME")
# The offline probe may be waiting for the absent homeserver before installing
# its signal handler. Terminating this disposable copy is not a live shutdown test.
[[ "$exit_code" =~ ^(0|143|137)$ ]] || { echo 'Recovered Telegram probe failed unexpectedly' >&2; exit 1; }
[[ $("$SQLITE_BIN" -readonly -batch -bail "$WORK/data/telegram.db" 'PRAGMA integrity_check;') == ok ]] || exit 1
bash "$BASE/telegram-backup.sh" verify "$WORK/data/telegram.db" "$METADATA"
jq -n --argjson exit_code "$exit_code" --slurpfile m "$METADATA" '{database_integrity_verified:true,state_counts:$m[0].state_counts,bridge_database_startup_verified:true,crypto_state_counts_verified:true,crypto_runtime_startup_verified:false,probe_shutdown_exit_code:$exit_code,clean_shutdown_verified:($exit_code==0),network:"none",published_ports:false,telegram_authentication_tested:false}' >"$WORK/result.json"
rm -rf -- "$WORK/data"
