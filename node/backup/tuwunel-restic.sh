#!/usr/bin/env bash
# Online Tuwunel checkpoint -> encrypted, deduplicated restic repository.
set -Eeuo pipefail
umask 077

BASE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CONFIG=${MATRIX_BACKUP_CONFIG:-"$BASE/restic.env"}
ACTION=${1:-run}
STATE_DIR=/opt/matrix/backup-state/tuwunel-restic
DATA_DIR=/opt/matrix/deploy/tuwunel/data
PROJECT_DIR=/opt/matrix/deploy/tuwunel
CONTAINER=tuwunel
DOCKER_BIN=docker
RESTIC_BIN="$BASE/bin/restic"
CHECKPOINT_TIMEOUT=300
MIN_FREE_KIB=1048576
FULL_VERIFY=daily
TELEGRAM_PROJECT_DIR=/opt/matrix/deploy/telegram
TELEGRAM_CONTAINER=telegram-bridge
SQLITE_BIN=sqlite3

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
unset RESTIC_PASSWORD RESTIC_PASSWORD_COMMAND RESTIC_REPOSITORY_FILE RESTIC_PASSWORD_FILE
if [[ -f "$CONFIG" ]]; then
    [[ $(stat -c %a "$CONFIG") =~ ^[046]00$ ]] || die 'restic.env must have private permissions (0600)'
    # This is an administrator-owned shell configuration, not untrusted input.
    # shellcheck disable=SC1090
    source "$CONFIG"
elif [[ "$ACTION" != checkpoint && "$ACTION" != resume-checkpoint && "$ACTION" != status ]]; then
    die "Configuration missing: $CONFIG"
fi

[[ "$STATE_DIR" == /* && "$DATA_DIR" == /* ]] || die 'State/data paths must be absolute'
STATE_DIR=$(realpath -m -- "$STATE_DIR")
DATA_DIR=$(realpath -e -- "$DATA_DIR")
[[ "$STATE_DIR" != / && "$DATA_DIR" != / ]] || die 'Root filesystem cannot be used as state/data directory'
[[ "$STATE_DIR/" != "$DATA_DIR/"* && "$DATA_DIR/" != "$STATE_DIR/"* ]] || die 'State and live data directories must not overlap'
mkdir -p -- "$STATE_DIR"
chmod 700 -- "$STATE_DIR"
if [[ "$ACTION" == cold-run && ${FINAL_LOCK_HELD:-} == 1 && $(readlink /proc/$$/fd/9) == "$STATE_DIR/operation.lock" ]]; then
    flock -n 9 || die 'Inherited producer lock is not held'
else
    exec 9>"$STATE_DIR/operation.lock"
    flock -n 9 || die 'Another operation is already running'
fi
PENDING="$STATE_DIR/pending"
PREPARING="$STATE_DIR/preparing"
CHECKPOINT="$DATA_DIR/checkpoint-restic"
CONTAINER_CHECKPOINT=/var/lib/tuwunel/checkpoint-restic
EXPECTED_COMMAND="server checkpoint-database --path $CONTAINER_CHECKPOINT"
CONTROL_DIR=${CONTROL_DIR:-$BASE/../control}
source "$BASE/release-common.sh"
source "$BASE/payload-verify.sh"
source "$BASE/image-reference.sh"
FINAL_ID=
if [[ "$ACTION" == cold-run ]]; then
    FINAL_ID=${2:-}
    [[ "$FINAL_ID" =~ ^[a-zA-Z0-9_-]{1,100}$ ]] || die 'Use an explicit safe final operation ID'
    FINAL_WORK="$STATE_DIR/finals/$FINAL_ID"
    mkdir -p "$FINAL_WORK"
    PENDING="$FINAL_WORK/pending"
    PREPARING="$FINAL_WORK/preparing"
fi
SUCCESS_FILE="$STATE_DIR/last-success.json"
[[ -z "$FINAL_ID" ]] || SUCCESS_FILE="$FINAL_WORK/last-success.json"


for tool in jq sha256sum find sort xargs cmp timeout "$SQLITE_BIN"; do
    command -v "$tool" >/dev/null || die "Missing tool: $tool"
done
trap 'printf "ERROR: operation failed; previous successful backup is unchanged; inspect private logs in %s\n" "$STATE_DIR" >&2' ERR
trap 'exit 143' TERM
trap 'exit 130' INT

rest() {
    timeout --kill-after=30 90m "$RESTIC_BIN" --compression auto -o s3.connections=2 "$@"
}

remote_config() {
    [[ "$FULL_VERIFY" == 1 || "$FULL_VERIFY" == daily ]] || die 'FULL_VERIFY must be 1 or daily'
    [[ -x "$RESTIC_BIN" ]] || die 'restic binary is not installed'
    [[ ${RESTIC_REPOSITORY:-} == s3:https://* && ${RESTIC_REPOSITORY:-} != *REPLACE* ]] || die 'Configure the HTTPS S3 repository first'
    [[ -n ${AWS_ACCESS_KEY_ID:-} && -n ${AWS_SECRET_ACCESS_KEY:-} && ${AWS_ACCESS_KEY_ID:-} != *REPLACE* ]] || die 'Configure S3 access credentials first'
    [[ -n ${RESTIC_PASSWORD:-} ]] || die 'Internal repository key is missing'
    export RESTIC_PASSWORD
    export RESTIC_REPOSITORY AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
    export RESTIC_CACHE_DIR="$STATE_DIR/cache" GOMAXPROCS=1 RESTIC_READ_CONCURRENCY=1
    [[ -z ${AWS_DEFAULT_REGION:-} ]] || export AWS_DEFAULT_REGION
    [[ -z ${AWS_SESSION_TOKEN:-} ]] || export AWS_SESSION_TOKEN
    [[ -z ${RESTIC_CACERT:-} ]] || export RESTIC_CACERT
}

container_info() {
    "$DOCKER_BIN" inspect --format '{{.Id}} {{.State.Running}} {{.State.StartedAt}}' "$CONTAINER"
}

manifest() {
    local payload=$1
    [[ -z $(find "$payload" -type l -print -quit) ]] || die 'Payload contains symlinks; explicit handling is required'
    (cd -- "$payload"; find . -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum --)
}

control_permit() {
    local control="$CONTROL_DIR" current expected
    if [[ -n "$FINAL_ID" ]]; then
        current=$("$control/bin/matrix-control" status --config "$control/local.json") || die 'Controller unavailable'
        jq -e --arg op "$FINAL_ID" '.blocked and (.serving|not) and .final_backup.id==$op and .final_backup.phase=="SNAPSHOTTING" and .control.phase=="SNAPSHOTTING" and .control.operation.id==$op and .bound_epoch==.final_backup.source_epoch and .control.owner==.instance_id' <<<"$current" >/dev/null || die 'Final snapshot lacks frozen ownership'
        "$control/bin/matrix-control" final-verify --config "$control/local.json" --expected-instance "$(jq -er .instance_id <<<"$current")" --expected-epoch "$(jq -er .bound_epoch <<<"$current")" --operation-id "$FINAL_ID" >/dev/null
        return
    fi
    [[ -f "$control/managed" ]] || return 0
    current=$("$control/bin/matrix-control" status --config "$control/local.json") || die 'Instance controller is unavailable; backup publication refused'
    jq -e '.serving == true and .blocked == false' <<<"$current" >/dev/null || die 'This instance is not authorized and healthy; backup refused'
    if [[ -s "$PENDING/bundle/metadata.json" ]] && jq -e '.instance_control != null' "$PENDING/bundle/metadata.json" >/dev/null; then
        expected=$(jq -c '.instance_control | {cluster_id,instance_id:.source_instance_id,bound_epoch:.source_epoch}' "$PENDING/bundle/metadata.json")
        [[ $(jq -c '{cluster_id,instance_id,bound_epoch}' <<<"$current") == "$expected" ]] || die 'Staged backup belongs to a different instance epoch'
    fi
}

check_bundle() {
    local bundle=$1 manifest_tmp
    payload_verify "$bundle" full || die 'Invalid private data payload'
    [[ -s "$bundle/manifest.sha256" && -s "$bundle/metadata.json" ]] || die 'Incomplete bundle metadata'
    (cd -- "$bundle/payload"; sha256sum --quiet -c ../manifest.sha256)
    SQLITE_BIN="$SQLITE_BIN" bash "$BASE/telegram-backup.sh" verify "$bundle/payload/telegram/data/telegram.db" "$bundle/payload/telegram/metadata.json"
    manifest_tmp=$(mktemp "$STATE_DIR/manifest.XXXXXX")
    manifest "$bundle/payload" >"$manifest_tmp"
    cmp -s -- "$bundle/manifest.sha256" "$manifest_tmp" || { rm -f -- "$manifest_tmp"; die 'File set or content differs from the source manifest'; }
    rm -f -- "$manifest_tmp"
}

checkpoint() {
    control_permit
    [[ ! -e "$PENDING" && ! -e "$PREPARING" ]] || die 'A previous bundle exists; upload it or inspect the interrupted operation first'
    [[ ! -e "$CHECKPOINT" && ! -L "$CHECKPOINT" ]] || die 'Checkpoint path already exists; inspect the prior operation before retrying'
    [[ $(stat -c %d "$STATE_DIR") == "$(stat -c %d "$DATA_DIR")" ]] || die 'State and data must share a filesystem for atomic checkpoint staging'
    local available info id running started mountpoint start_time image
    available=$(df -Pk "$STATE_DIR" | awk 'END {print $4}')
    (( available >= MIN_FREE_KIB )) || die 'Not enough staging space'
    info=$(container_info); read -r id running started <<<"$info"
    [[ "$running" == true ]] || die 'Tuwunel is not running'
    local health_deadline=$((SECONDS + 60))
    until "$DOCKER_BIN" exec "$id" tuwunel --health-check >/dev/null 2>&1; do
        (( SECONDS < health_deadline )) || die 'Tuwunel did not become healthy before the checkpoint request'
        [[ $(container_info) == "$info" ]] || die 'Tuwunel restarted while waiting for health'
        sleep 1
    done
    mountpoint=$("$DOCKER_BIN" inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/tuwunel"}}{{.Source}}{{end}}{{end}}' "$id")
    [[ "$mountpoint" == "$DATA_DIR" ]] || die 'Configured data directory does not match the container mount'
    "$DOCKER_BIN" inspect --format '{{json .Config.Env}}' "$id" |
        jq -e --arg expected "$EXPECTED_COMMAND" 'map(select(startswith("TUWUNEL_ADMIN_SIGNAL_EXECUTE=")) | sub("^[^=]+="; "") | fromjson) == [[$expected]]' >/dev/null ||
        die 'Container checkpoint signal command is not configured as expected'
    mkdir -p -- "$PREPARING/bundle/payload"
    start_time=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
    printf '%s\n%s\n' "$start_time" "$info" >"$PREPARING/request.txt"
    log 'Requesting a checkpoint from the running Tuwunel process'
    "$DOCKER_BIN" kill --signal USR2 "$id" >/dev/null
    finish_checkpoint
}

finish_checkpoint() {
    [[ -d "$PREPARING" && ! -L "$PREPARING" && -s "$PREPARING/request.txt" && ! -e "$PENDING" ]] || die 'No safe unfinished checkpoint request exists'
    local -a request
    mapfile -t request <"$PREPARING/request.txt"
    (( ${#request[@]} == 2 )) || die 'Invalid checkpoint request record'
    local start_time=${request[0]} info=${request[1]} id running started image
    read -r id running started <<<"$info"
    local end=$((SECONDS + CHECKPOINT_TIMEOUT)) complete=0
    while (( SECONDS < end )); do
        [[ $(container_info) == "$info" ]] || die 'Tuwunel restarted during checkpoint creation'
        "$DOCKER_BIN" logs --since "$start_time" --tail 2000 "$id" >"$PREPARING/checkpoint.log" 2>&1
        # The console build renders Markdown code spans on a separate coloured
        # line. Strip ANSI colours and join lines before matching completion.
        if sed -E $'s/\033\\[[0-9;]*[[:alpha:]]//g' "$PREPARING/checkpoint.log" | tr '\r\n' '  ' |
            grep -E 'Created checkpoint at[[:space:]`]+/var/lib/tuwunel/checkpoint-restic[`]?\.' >/dev/null; then
            complete=1; break
        fi
        sleep 1
    done
    (( complete == 1 )) || die 'Checkpoint completion was not confirmed; request and logs retained'
    if [[ -d "$CHECKPOINT" && ! -e "$PREPARING/bundle/payload/database" ]]; then
        [[ -f "$CHECKPOINT/CURRENT" && ! -L "$CHECKPOINT" ]] || die 'Checkpoint CURRENT is missing'
        mv -- "$CHECKPOINT" "$PREPARING/bundle/payload/database"
    fi
    local current
    current=$(cat -- "$PREPARING/bundle/payload/database/CURRENT")
    [[ "$current" =~ ^MANIFEST-[0-9]+$ && -s "$PREPARING/bundle/payload/database/$current" ]] || die 'Checkpoint manifest is missing'
    package_application online "$start_time" "$id"
    log 'Online checkpoint staged; no service was stopped'
}

package_application() {
    local mode=$1 start_time=$2 id=$3 image
    find "$PREPARING/bundle/payload/database" -mindepth 1 -type d -empty -delete
    rm -rf -- "$PREPARING/bundle/payload/config"
    mkdir -p -- "$PREPARING/bundle/payload/config"
    cp -a -- "$PROJECT_DIR/docker-compose.yml" "$PROJECT_DIR/registration-token" "$PROJECT_DIR/appservices" "$PREPARING/bundle/payload/config/"
    if [[ -f "$PROJECT_DIR/.env" ]]; then cp -a -- "$PROJECT_DIR/.env" "$PREPARING/bundle/payload/config/"; fi
    if [[ -f "$PROJECT_DIR/s3/media.env" ]]; then
        mkdir -p -- "$PREPARING/bundle/payload/config/s3"
        cp -a -- "$PROJECT_DIR/s3/media.env" "$PREPARING/bundle/payload/config/s3/"
    fi
    mkdir -p -- "$PREPARING/bundle/payload/nginx"
    source "$BASE/release-common.sh"
    release_descriptor "$(release_root)" >"$PREPARING/software.json" || die 'Installed public release is incomplete or modified'
    cp -p -- "${NGINX_HTTP_CONF_DIR:-/etc/nginx/conf.d}/matrix.conf" "$PREPARING/bundle/payload/nginx/matrix.conf"
    cp -p -- "${NGINX_HTTP_CONF_DIR:-/etc/nginx/conf.d}/00-matrix-common.conf" "$PREPARING/bundle/payload/nginx/00-matrix-common.conf"
    local control="$CONTROL_DIR" control_binding=null
    if [[ -f "$control/managed" ]]; then
        control_permit
        mkdir -p "$PREPARING/bundle/payload/control"
        # Preserve host settings only as a source reference, never an instance ID
        # or run permission to reuse automatically on a different machine.
        cp -p "$control/local.json" "$PREPARING/bundle/payload/control/source-local.json"
        control_binding=$("$control/bin/matrix-control" status --config "$control/local.json" |
            jq -ce 'select((.serving and (.blocked|not)) or (.blocked and .final_backup.phase=="SNAPSHOTTING")) | {schema:1,cluster_id,source_instance_id:.instance_id,source_epoch:.bound_epoch,activation:"controller-admission"}')
    fi
    rm -rf -- "$PREPARING/bundle/payload/telegram"
    local telegram_mode=snapshot
    [[ "$mode" == online ]] || telegram_mode=cold-snapshot
    SQLITE_BIN="$SQLITE_BIN" DOCKER_BIN="$DOCKER_BIN" bash "$BASE/telegram-backup.sh" "$telegram_mode" "$TELEGRAM_PROJECT_DIR" "$TELEGRAM_CONTAINER" "$PREPARING/bundle/payload/telegram"
    image=$("$DOCKER_BIN" inspect --format '{{.Image}}' "$id")
    local domain media_prefix tuwunel_ref telegram_ref telegram_id
    domain=$("$DOCKER_BIN" inspect --format '{{json .Config.Env}}' "$id" | jq -er '[.[]|select(startswith("TUWUNEL_SERVER_NAME="))|sub("^[^=]+=";"")]|if length==1 then .[0] else error("server identity missing") end')
    [[ "$domain" =~ ^[a-zA-Z0-9.-]+$ ]] || die 'Invalid server identity'
    media_prefix=$("$DOCKER_BIN" inspect --format '{{json .Config.Env}}' "$id" | jq -er '[.[]|select(startswith("TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__BASE_PATH="))|sub("^[^=]+=";"")]|if length==1 then .[0] else error("media prefix missing") end')
    # Databases bind immutable public registry images. Local-only custom images
    # must be published and pulled by digest before admitting the first v2 point.
    tuwunel_ref=$(image_reference "$id" "$image")
    telegram_id=$(jq -er .image_id "$PREPARING/bundle/payload/telegram/metadata.json")
    telegram_ref=$(image_reference "$TELEGRAM_CONTAINER" "$telegram_id")
    jq -n --slurpfile telegram "$PREPARING/bundle/payload/telegram/metadata.json" --slurpfile software "$PREPARING/software.json" \
        --argjson control "$control_binding" --arg data_time "$start_time" --arg image "$image" --arg command "$EXPECTED_COMMAND" \
        --arg domain "$domain" --arg media_prefix "$media_prefix" --arg tr "$tuwunel_ref" --arg gr "$telegram_ref" \
        '{format:"tuwunel-checkpoint-v2",recovery_version:2,data_layout:"database-only-v1",data_time:$data_time,image_id:$image,server_name:$domain,checkpoint_command:$command,software:$software[0],images:{tuwunel:{ref:$tr,id:$image},telegram:{ref:$gr,id:$telegram[0].image_id}},media_included:false,media:{provider:"media_s3",prefix:$media_prefix,backup:"external-s3",restore:"reuse-existing"},telegram_included:true,consistency:"independent-online-snapshots",telegram:$telegram[0],instance_control:$control}' \
        >"$PREPARING/bundle/metadata.json"
    if [[ "$mode" == cold ]]; then
        "$control/bin/matrix-control" status --config "$control/local.json" >"$PREPARING/control.json"
        jq --slurpfile c "$PREPARING/control.json" '.consistency="globally-quiesced" | .data_time=$c[0].final_backup.cutoff_time | .operation_id=$c[0].final_backup.id | .stopped_proof=$c[0].final_backup.stopped_proof' "$PREPARING/bundle/metadata.json" >"$PREPARING/bundle/metadata.json.tmp"
        mv "$PREPARING/bundle/metadata.json.tmp" "$PREPARING/bundle/metadata.json"
    fi
    manifest "$PREPARING/bundle/payload" >"$PREPARING/bundle/manifest.sha256"
    payload_verify "$PREPARING/bundle" full || die 'Private backup contains an unsupported payload'
    (cd "$PREPARING/bundle/payload"; sha256sum --quiet -c ../manifest.sha256)
    "$BASE/bin/matrix-release" config-pack --bundle "$PREPARING/bundle" --output "$PREPARING/configuration.json"
    mv -- "$PREPARING" "$PENDING"
    log "Application bundle staged ($mode)"
}

upload() {
    control_permit
    [[ -d "$PENDING/bundle" ]] || die 'No staged checkpoint exists'
    payload_verify "$PENDING/bundle" full || die 'Invalid staged data payload'
    (cd "$PENDING/bundle/payload"; sha256sum --quiet -c ../manifest.sha256)
    local run_id snapshot restored verified data_time summary repository_id full_verification=0 verification
    run_id="checkpoint-$(date -u +%Y%m%dT%H%M%S)-$(cat /proc/sys/kernel/random/uuid)"
    log 'Uploading checkpoint with restic content deduplication'
    local recovery_tags=()
    [[ $(jq -er .recovery_version "$PENDING/bundle/metadata.json") == 2 ]] || die 'Unsupported recovery format'
    recovery_tags=(--tag recovery-v2)
    [[ -z "$FINAL_ID" ]] || recovery_tags+=(--tag "final-$FINAL_ID")

    existing=
    if [[ -n "$FINAL_ID" ]]; then
        rest snapshots --json --tag "final-$FINAL_ID" >"$PENDING/existing.json"
        [[ $(jq length "$PENDING/existing.json") -le 1 ]] || die 'Ambiguous final snapshot history'
        existing=$(jq -r '.[0].id // empty' "$PENDING/existing.json")
    fi
    if [[ -n "$existing" ]]; then
        snapshot=$existing
        summary='{}'
        run_id="final-$FINAL_ID"
    else
    if ! (cd -- "$PENDING"; rest backup --read-concurrency 1 --json --host "$(jq -er .server_name "$PENDING/bundle/metadata.json")" --tag tuwunel-checkpoint "${recovery_tags[@]}" --tag "$run_id" --tag pending bundle/payload/database bundle/payload/telegram/data/telegram.db) \
        >"$PENDING/backup.jsonl" 2>"$PENDING/backup.stderr"; then
        die 'restic backup did not succeed completely; pending checkpoint retained for retry'
    fi
    snapshot=$(jq -er 'select(.message_type == "summary") | .snapshot_id' "$PENDING/backup.jsonl")
    [[ "$snapshot" =~ ^[a-f0-9]{8,64}$ ]] || die 'No valid snapshot ID was returned'
    summary=$(jq -c 'select(.message_type == "summary") | del(.snapshot_id)' "$PENDING/backup.jsonl")
    fi
    repository_id=$(jq -er .id "$STATE_DIR/repository-config.json")
    if [[ "$FULL_VERIFY" == 1 ]] || [[ ! -s "$STATE_DIR/last-full-verify.json" ]] ||
        [[ $(jq -r '.media.backup // ""' "$STATE_DIR/last-full-verify.json") != external-s3 ]] ||
        [[ $(jq -r '.data_layout // ""' "$STATE_DIR/last-full-verify.json") != database-only-v1 ]] ||
        [[ $(jq -r '.verified_at[0:10]' "$STATE_DIR/last-full-verify.json") != "$(date -u +%F)" ]] ||
        [[ $(jq -r '.repository_id // ""' "$STATE_DIR/last-full-verify.json") != "$repository_id" ]]; then
        full_verification=1
    fi
    if (( full_verification == 1 )); then
        required_kib=$(( $(du -sk "$PENDING/bundle/payload" | cut -f1) + 524288 ))
        (( $(df -Pk "$STATE_DIR" | awk 'END {print $4}') >= required_kib )) || die 'Insufficient space for full database restore verification; previous recovery point retained'
        restored=$(mktemp -d "$STATE_DIR/restore.XXXXXX")
        log 'Restoring the exact snapshot and comparing full file hashes'
        if ! rest restore "$snapshot" --target "$restored/download" --verify >"$PENDING/restore.log" 2>&1; then
            rm -rf -- "$restored"; die 'Remote restore failed; staged checkpoint retained'
        fi
        "$BASE/bin/matrix-release" config-unpack --input "$PENDING/configuration.json" --bundle "$restored/combined"
        "$BASE/bin/matrix-release" data-merge --input "$restored/download" --bundle "$restored/combined"
        rm -rf -- "$restored/download"
        mv "$restored/combined" "$restored/bundle"
        if ! (check_bundle "$restored/bundle"); then
            rm -rf -- "$restored"; die 'Remote restored files failed verification'
        fi
        if [[ -n "$FINAL_ID" ]]; then
            log 'Verifying both restored databases in network-isolated maintenance containers'
            cp "$CONFIG" "$restored/probe.env"
            printf '\nR2_CONFIG=%q\n' "$CONTROL_DIR/s3.env" >>"$restored/probe.env"
            image_id=$(jq -er .image_id "$restored/bundle/metadata.json")
            telegram_id=$(jq -er .telegram.image_id "$restored/bundle/metadata.json")
            image_ref=$(jq -er .images.tuwunel.ref "$restored/bundle/metadata.json")
            telegram_ref=$(jq -er .images.telegram.ref "$restored/bundle/metadata.json")
            printf '\nRESTIC_BIN=%q\nRECOVERY_IMAGE_REF=%q\nRECOVERY_IMAGE_ID=%q\nRECOVERY_TELEGRAM_IMAGE_REF=%q\nRECOVERY_TELEGRAM_IMAGE_ID=%q\n' "$RESTIC_BIN" "$image_ref" "$image_id" "$telegram_ref" "$telegram_id" >>"$restored/probe.env"
            bash "$BASE/restore-tuwunel.sh" --config "$restored/probe.env" --bundle "$restored/bundle" --release "$(release_root)" --isolated --target "$restored/checked" >"$PENDING/database-startup.log" 2>&1 || die 'Cold database maintenance startup failed; source remains frozen'
            database_startup_verified=true
            cp "$PENDING/database-startup.log" "$FINAL_WORK/database-startup.log"
        fi
        rm -rf -- "$restored"
        verification=full-restore
    else
        log 'Verifying the database file inventory; full restore is checked daily'
        rest ls --json --recursive "$snapshot" /bundle >"$PENDING/remote-tree.jsonl" 2>"$PENDING/remote-tree.stderr"
        jq -sc '[.[] | select(.struct_type=="node" and .type=="file") | {path,size}] | sort_by(.path)' \
            "$PENDING/remote-tree.jsonl" >"$PENDING/remote-files.json"
        (
            cd -- "$PENDING/bundle/payload"
            find ./database ./telegram/data/telegram.db -type f -print0 | while IFS= read -r -d '' file; do
                jq -cn --arg path "/bundle/payload/${file#./}" --argjson size "$(stat -c %s -- "$file")" '{path:$path,size:$size}'
            done
        ) | jq -sc 'sort_by(.path)' >"$PENDING/source-files.json"
        cmp -s -- "$PENDING/source-files.json" "$PENDING/remote-files.json" || die 'Remote file tree is incomplete or sizes differ'
        verification=database-tree
    fi
    rest tag --remove pending --add verified "$snapshot" >"$PENDING/tag.log" 2>&1
    rest snapshots --json --tag "$run_id,verified" >"$PENDING/snapshots.json" 2>"$PENDING/snapshots.stderr"
    verified=$(jq -er 'if length == 1 then .[0].id else error("ambiguous snapshot") end' "$PENDING/snapshots.json")
    data_time=$(jq -er .data_time "$PENDING/bundle/metadata.json")
    jq -n --slurpfile metadata "$PENDING/bundle/metadata.json" --arg id "$verified" --arg data_time "$data_time" --arg verified_at "$(date -u +%FT%TZ)" \
        --argjson startup "${database_startup_verified:-false}" --arg verification "$verification" --arg repository_id "$repository_id" --argjson summary "$summary" \
        '{snapshot_id:$id,data_time:$data_time,verified_at:$verified_at,verification:$verification,repository_id:$repository_id,backup_summary:$summary,database_startup_verified:$startup,format:"tuwunel-checkpoint-v2",data_layout:"database-only-v1",media_included:false,media:$metadata[0].media,telegram_included:true,consistency:$metadata[0].consistency,telegram:$metadata[0].telegram,instance_control:$metadata[0].instance_control}' >"$SUCCESS_FILE.tmp"
    # A database-only verified snapshot is not yet a completed recovery point.
    # Publish its immutable dependencies and S3 index before advancing success.
    control_permit
    RECOVERY_BUNDLE="$PENDING/bundle" RECOVERY_CONFIG_FILE="$PENDING/configuration.json" RECOVERY_COMMIT_MODE=${FINAL_ID:+final} RECOVERY_POINT_FILE="$SUCCESS_FILE.tmp" "$BASE/publish-recovery.sh"
    mv -- "$SUCCESS_FILE.tmp" "$SUCCESS_FILE"
    if (( full_verification == 1 )); then
        cp -- "$SUCCESS_FILE" "$STATE_DIR/last-full-verify.json.tmp"
        mv -- "$STATE_DIR/last-full-verify.json.tmp" "$STATE_DIR/last-full-verify.json"
    fi
    if [[ -n "$FINAL_ID" ]]; then
        cp "$STATE_DIR/final-published-$FINAL_ID.json" "$FINAL_WORK/published.json"
    fi
    rm -rf -- "$PENDING"
    log "Verified remote checkpoint: $verified"
}

cold_checkpoint() {
    control_permit
    [[ ! -e "$PENDING" ]] || die 'Cold stage already completed'
    if [[ -e "$PREPARING" ]]; then mv "$PREPARING" "$FINAL_WORK/interrupted-$(cat /proc/sys/kernel/random/uuid)"; fi
    [[ ! -e "$CHECKPOINT" ]] || die 'Unfinished daily checkpoint needs inspection before final backup'
    [[ ! -d "$DATA_DIR/media" || -z $(find "$DATA_DIR/media" -type f -print -quit) ]] || die 'Unexpected local media bodies; classify them before database-only final backup'
    mkdir -p "$PREPARING/bundle/payload"
    manifest "$DATA_DIR" >"$PREPARING/source.sha256"
    cp -a --reflink=auto "$DATA_DIR" "$PREPARING/bundle/payload/database"
    (cd "$PREPARING/bundle/payload/database"; sha256sum --quiet -c "$PREPARING/source.sha256")
    manifest "$DATA_DIR" >"$PREPARING/source-after.sha256"
    cmp "$PREPARING/source.sha256" "$PREPARING/source-after.sha256" || die 'Source changed during frozen copy'
    control_permit
    local id cutoff
    id=$("$DOCKER_BIN" inspect --format '{{.Id}}' "$CONTAINER")
    cutoff=$("$CONTROL_DIR/bin/matrix-control" status --config "$CONTROL_DIR/local.json" | jq -er .final_backup.cutoff_time)
    package_application cold "$cutoff" "$id"
}

case "$ACTION" in
    cold-run)
        FULL_VERIFY=1
        remote_config
        rest cat config >"$STATE_DIR/repository-config.json"
        if [[ -f "$SUCCESS_FILE" && -f "$FINAL_WORK/published.json" ]]; then control_permit; exit 0; fi
        [[ -d "$PENDING" ]] || cold_checkpoint
        upload
        ;;
    checkpoint) checkpoint ;;
    resume-checkpoint) finish_checkpoint ;;
    init)
        remote_config
        rest init >"$STATE_DIR/init.log" 2>&1
        log 'Repository initialized; publish its private recovery key with the first verified point'
        ;;
    check)
        remote_config
        rest cat config >"$STATE_DIR/repository-config.json" 2>"$STATE_DIR/check.stderr"
        log 'Remote repository and password verified'
        ;;
    run|upload)
        remote_config
        rest cat config >"$STATE_DIR/repository-config.json" 2>"$STATE_DIR/check.stderr"
        if [[ "$ACTION" == run && ! -d "$PENDING" ]]; then checkpoint; fi
        upload
        ;;
    snapshots)
        remote_config
        rest snapshots --tag tuwunel-checkpoint
        ;;
    status)
        [[ -s "$STATE_DIR/last-success.json" ]] || die 'No completed remote backup exists yet'
        jq . "$STATE_DIR/last-success.json"
        data_epoch=$(date -d "$(jq -er .data_time "$STATE_DIR/last-success.json")" +%s)
        (( $(date +%s) - data_epoch < 2700 )) || die 'Newest remote recovery point is older than 45 minutes'
        ;;
    *) die 'Usage: tuwunel-restic.sh {checkpoint|resume-checkpoint|init|check|run|upload|snapshots|status}' ;;
esac
