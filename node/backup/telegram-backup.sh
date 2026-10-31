#!/usr/bin/env bash
# SQLite's backup API makes a standalone consistent copy, including committed WAL.
set -Eeuo pipefail
umask 077
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
SQLITE_BIN=${SQLITE_BIN:-sqlite3}
DOCKER_BIN=${DOCKER_BIN:-docker}

db_counts() {
    "$SQLITE_BIN" -readonly -batch -bail -json "$1" \
        "SELECT 'user_login' AS name,count(*) AS count FROM user_login UNION ALL SELECT 'crypto_account',count(*) FROM crypto_account UNION ALL SELECT 'crypto_olm_session',count(*) FROM crypto_olm_session UNION ALL SELECT 'crypto_megolm_inbound_session',count(*) FROM crypto_megolm_inbound_session UNION ALL SELECT 'crypto_megolm_outbound_session',count(*) FROM crypto_megolm_outbound_session UNION ALL SELECT 'portal',count(*) FROM portal UNION ALL SELECT 'message',count(*) FROM message;" |
        jq 'map({key:.name,value:.count}) | from_entries'
}
verify_db() {
    local db=$1 metadata=$2 result actual
    [[ -s "$db" && -f "$metadata" && ! -L "$db" ]] || die 'Missing Telegram database/metadata'
    result=$("$SQLITE_BIN" -readonly -batch -bail "$db" 'PRAGMA integrity_check;')
    [[ "$result" == ok ]] || die 'Telegram database integrity check failed'
    result=$("$SQLITE_BIN" -readonly -batch -bail "$db" 'PRAGMA foreign_key_check;')
    [[ -z "$result" ]] || die 'Telegram database foreign-key check failed'
    actual=$(db_counts "$db")
    jq -e --argjson actual "$actual" '.schema==1 and (.method=="sqlite-online-backup" or .method=="sqlite-cold-backup") and .state_counts==$actual' "$metadata" >/dev/null || die 'Telegram database state counts differ from the recorded snapshot'
}

case ${1:-} in
    snapshot|cold-snapshot)
        mode=$1
        [[ $# == 4 ]] || die 'Usage: telegram-backup.sh snapshot PROJECT CONTAINER NEW_DIRECTORY'
        project=$(realpath -e -- "$2"); container=$3; destination=$(realpath -m -- "$4")
        source_data="$project/data"
        [[ "$destination" == /* && "$destination" != *[!a-zA-Z0-9_./-]* && ! -e "$destination" ]] || die 'Use a new safe destination directory'
        [[ "$destination/" != "$source_data/"* && "$source_data/" != "$destination/"* ]] || die 'Live and backup paths must be separate'
        for name in config.yaml registration.yaml telegram.db; do
            [[ -s "$source_data/$name" && ! -L "$source_data/$name" ]] || die "Missing persistent Telegram file: $name"
        done
        # Fail if the application starts storing new persistent files. tmp is
        # explicitly transient media conversion scratch, not account state.
        while IFS= read -r -d '' entry; do
            case "${entry##*/}" in
                config.yaml|registration.yaml|telegram.db|telegram.db-wal|telegram.db-shm) [[ -f "$entry" && ! -L "$entry" ]] || die 'Unexpected Telegram file type' ;;
                tmp) [[ -d "$entry" && ! -L "$entry" ]] || die 'Unexpected Telegram tmp directory' ;;
                *) die "Unclassified persistent Telegram path: ${entry##*/}" ;;
            esac
        done < <(find "$source_data" -mindepth 1 -maxdepth 1 -print0)
        before=$("$DOCKER_BIN" inspect --format '{{.Id}} {{.State.Running}} {{.State.StartedAt}}' "$container")
        if [[ "$mode" == snapshot ]]; then
            [[ "$before" == *' true '* ]] || die 'Telegram bridge is not running'
        else
            [[ "$before" == *' false '* ]] || die 'Cold snapshot requires stopped bridge'
            "$DOCKER_BIN" inspect --format '{{json .State}}' "$container" | jq -e '.Running==false and .Restarting==false and .ExitCode==0 and .OOMKilled==false and .Pid==0' >/dev/null || die 'Bridge did not stop cleanly'
        fi
        mount=$("$DOCKER_BIN" inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}' "$container")
        [[ "$mount" == "$source_data" ]] || die 'Telegram container mount differs from backup source'
        if [[ "$mode" == snapshot ]]; then
        "$DOCKER_BIN" exec "$container" sh -c 'for fd in /proc/1/fd/*; do [ "$(readlink "$fd")" = /data/telegram.db ] && exit 0; done; exit 1' || die 'The running bridge does not have the expected SQLite database open'
        fi
        started=$(date -u +%FT%TZ)
        mkdir -p "$destination/data" "$destination/config"
        cp -a -- "$source_data/config.yaml" "$source_data/registration.yaml" "$destination/data/"
        cp -p -- "$project/docker-compose.yml" "$destination/config/"
        timeout --kill-after=10 120 "$SQLITE_BIN" -readonly -batch -bail -cmd '.timeout 15000' \
            "$source_data/telegram.db" ".backup '$destination/data/telegram.db'" >"$destination/sqlite-backup.log" 2>&1
        # Normalize only the copied database. Do not alter the source WAL mode.
        [[ $("$SQLITE_BIN" -batch -bail "$destination/data/telegram.db" 'PRAGMA journal_mode=DELETE;') == delete ]] || die 'Could not finalize standalone Telegram database'
        chmod 600 "$destination/data/telegram.db"
        chown --reference="$source_data/telegram.db" "$destination/data/telegram.db"
        chown --reference="$source_data" "$destination/data"
        chmod 700 "$destination/data"
        for name in config.yaml registration.yaml; do
            cmp -s -- "$source_data/$name" "$destination/data/$name" || die 'Telegram configuration changed during backup'
        done
        [[ $("$DOCKER_BIN" inspect --format '{{.Id}} {{.State.Running}} {{.State.StartedAt}}' "$container") == "$before" ]] || die 'Telegram bridge restarted during backup'
        image=$("$DOCKER_BIN" inspect --format '{{.Image}}' "$container")
        networks=$("$DOCKER_BIN" inspect --format '{{json .NetworkSettings.Networks}}' "$container")
        [[ $(jq length <<<"$networks") == 1 ]] || die 'Expected exactly one Telegram application network'
        network_name=$(jq -er 'keys[0]' <<<"$networks")
        address=$(jq -er '.[].IPAMConfig.IPv4Address // .[].IPAddress | select(length>0)' <<<"$networks")
        network=$("$DOCKER_BIN" network inspect "$network_name" | jq -e --arg name "$network_name" --arg ip "$address" \
            '.[0] | select(.Driver=="bridge" and (.IPAM.Config|length)==1) | {name:$name,address:$ip,subnet:.IPAM.Config[0].Subnet,gateway:.IPAM.Config[0].Gateway}') || die 'Unsupported Telegram network layout'
        counts=$(db_counts "$destination/data/telegram.db")
        method=sqlite-online-backup
        [[ "$mode" == snapshot ]] || method=sqlite-cold-backup
        jq -n --arg method "$method" --arg started "$started" --arg finished "$(date -u +%FT%TZ)" --arg image "$image" --argjson counts "$counts" --argjson network "$network" \
            '{schema:1,method:$method,started_at:$started,finished_at:$finished,image_id:$image,network:$network,state_counts:$counts,source_service_restarted:false,excluded_paths:["data/tmp","data/telegram.db-wal","data/telegram.db-shm"]}' >"$destination/metadata.json"
        verify_db "$destination/data/telegram.db" "$destination/metadata.json"
        rm -f "$destination/sqlite-backup.log"
        ;;
    verify)
        [[ $# == 3 ]] || die 'Usage: telegram-backup.sh verify DATABASE METADATA'
        verify_db "$2" "$3"
        ;;
    *) die 'Usage: telegram-backup.sh {snapshot|verify} ...' ;;
esac
