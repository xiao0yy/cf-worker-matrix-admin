# Loaded by the verified public release entry.
host_defaults
CONFIG= LOCAL_CONFIG= MODE=isolated TARGET= SELECTED_POINT= PREFLIGHT=no RESULT_FILE= POINT_HASH=
while (( $# )); do
    case "$1" in
        --config) CONFIG=${2:?}; shift 2 ;;
        --local) LOCAL_CONFIG=${2:?}; shift 2 ;;
        --preflight) PREFLIGHT=yes; shift ;;
        --target) TARGET=${2:?}; shift 2 ;;
        --activate|--old-server-isolated) fail 'Standalone activation is forbidden; prepare a standby and use controller admission' ;;
        --point) SELECTED_POINT=${2:?}; shift 2 ;;
        --point-sha256) POINT_HASH=${2:?}; shift 2 ;;
        --result-file) RESULT_FILE=${2:?}; shift 2 ;;
        --prepare-standby) MODE=standby; shift ;;
        --check-foundation) MODE=check; shift ;;
        --update-foundation) MODE=update; shift ;;
        *) fail 'Usage: recover.sh --config s3.env [--local HOST.env] [--preflight] [--target NEW_DIRECTORY] [--point SNAPSHOT_ID]' ;;
    esac
done
(( EUID == 0 )) || fail 'Run as root'
# All first-boot inputs and local Agent observations live in the foundation.
if [[ -n "$TARGET" ]]; then RESTORE_ROOT=$TARGET; WORK_ROOT=$TARGET/generations; LOCK_FILE=$TARGET/agent/recovery.lock; fi
CONFIG=${CONFIG:-$RESTORE_ROOT/config/s3.env}
if [[ -z "$LOCAL_CONFIG" && -f "$RESTORE_ROOT/agent/local.env" ]]; then LOCAL_CONFIG=$RESTORE_ROOT/agent/local.env; fi
# An existing foundation owns its actual host paths; do not rediscover defaults
# or download another bootstrap over it when an Agent continues the same task.
if [[ "$MODE" =~ ^(standby|check|update)$ && -z "$LOCAL_CONFIG" ]]; then
    existing_root=${TARGET:-$RESTORE_ROOT}
    [[ ! -f "$existing_root/deploy/control/host.env" ]] || LOCAL_CONFIG="$existing_root/deploy/control/host.env"
fi
[[ -z "$LOCAL_CONFIG" ]] || host_load "$LOCAL_CONFIG"
host_validate
if [[ -n "$RESULT_FILE" ]]; then
    [[ "$RESULT_FILE" == /* && "$RESULT_FILE" != *[!a-zA-Z0-9_./-]* && ! -e "$RESULT_FILE" && ! -L "$RESULT_FILE" && -d $(dirname "$RESULT_FILE") ]] || fail 'Result file must be a new local absolute path with an existing parent'
fi
[[ -z "$POINT_HASH" || ( -n "$SELECTED_POINT" && "$POINT_HASH" =~ ^[a-f0-9]{64}$ ) ]] || fail 'Point SHA256 requires an explicitly selected snapshot'
[[ -n "$TARGET" ]] || TARGET=$RESTORE_ROOT
# Resolve every prerequisite without installing packages or changing services.
missing=()
for tool in curl jq sha256sum gzip timeout flock find sort xargs cmp "$DOCKER_BIN" "$SQLITE_BIN"; do
    type -P "$tool" >/dev/null || missing+=("$tool")
done
if [[ -n "$COMPOSE_BIN" ]]; then type -P "$COMPOSE_BIN" >/dev/null || missing+=("$COMPOSE_BIN"); fi
if [[ "$MODE" =~ ^(standby|check|update)$ ]]; then
    for tool in "$NGINX_BIN" "$SYSTEMCTL_BIN"; do type -P "$tool" >/dev/null || missing+=("$tool"); done
    [[ -f "$NGINX_MAIN_CONF" && -d "$NGINX_HTTP_CONF_DIR" && -d "$SYSTEMD_UNIT_DIR" ]] || fail 'Host nginx/systemd paths are not ready; ask the Agent to prepare them or adjust --local'
fi
(( ${#missing[@]} == 0 )) || fail "Missing prerequisites (Agent must install/configure): ${missing[*]}"
compose version >/dev/null || fail 'Compose is unavailable; Agent must configure Docker Compose or COMPOSE_BIN'
docker info >/dev/null 2>&1 || fail 'Docker daemon is unavailable; Agent must prepare it'
[[ $(uname -m) == x86_64 ]] || fail 'This recovery release requires an amd64 VPS'
[[ -d "$WORK_ROOT" && -d $(dirname "$LOCK_FILE") ]] || fail 'Host working/lock directories must already exist'
if [[ "$PREFLIGHT" == yes ]]; then
    jq -n --arg mode "$MODE" --arg root "$TARGET" --arg work "$WORK_ROOT" \
        --arg main "$NGINX_MAIN_CONF" --arg dir "$NGINX_HTTP_CONF_DIR" \
        '{preflight_ok:true,mode:$mode,restore_root:$root,work_root:$work,nginx_main_conf:$main,nginx_http_conf_dir:$dir,packages_installed:false,services_changed:false}'
    exit 0
fi
if [[ "$MODE" == standby && -e "$TARGET/deploy" ]]; then
    [[ -f "$CONFIG" && $(stat -c %a "$CONFIG") =~ ^[046]00$ ]] || fail 'Use the private S3 connection configuration'
    existing="$TARGET/deploy/control"
    [[ -f "$existing/managed" && -f "$existing/local.json" && -f "$existing/state/journal.json" && -x "$existing/bin/matrix-control" ]] || fail 'Existing foundation is incomplete; inspect local agent state and repair missing steps without deleting or reinitializing it'
    observed=$(mktemp)
    if ! "$existing/bin/matrix-control" status --config "$existing/local.json" >"$observed" || ! jq -e --slurpfile cfg "$existing/local.json" --slurpfile journal "$existing/state/journal.json" '.instance_id==$cfg[0].instance_id and .cluster_id==$cfg[0].cluster_id and .bound_epoch==$journal[0].bound_epoch' "$observed" >/dev/null; then
        rm -f "$observed"
        fail 'Existing controller cannot be verified; repair it using its local journal and operation logs, do not reinitialize'
    fi
    jq --arg root "$TARGET" '{already_prepared:true,foundation:$root,instance_id,cluster_id,bound_epoch,serving,blocked,control,final_backup,handover,assets_downloaded:false,identity_recreated:false,services_changed:false}' "$observed"
    rm -f "$observed"
    exit 0
fi
if [[ "$MODE" == standby ]]; then
    host_bootstrap_target "$TARGET" || fail 'Existing foundation is incomplete or contains unknown data; inspect it without deleting or reinitializing it'
fi
if [[ "$MODE" =~ ^(standby|check|update)$ ]]; then
    mkdir -p "$TARGET/config" "$TARGET/agent/work" "$TARGET/generations"
    if [[ ! -e "$TARGET/config/s3.env" ]]; then
        install -m600 "$CONFIG" "$TARGET/config/s3.env"
    else
        cmp -s "$CONFIG" "$TARGET/config/s3.env" || fail 'S3 input differs from the canonical foundation config; inspect config/s3.env first'
    fi
    CONFIG="$TARGET/config/s3.env"
    WORK=$(mktemp -d "$TARGET/agent/work/matrix-s3-recovery.XXXXXXXX")
else
    WORK=$(mktemp -d "$WORK_ROOT/matrix-s3-recovery.XXXXXXXX")
fi
note "Recovery workspace: $WORK"
(( $(df -Pk "$WORK" | awk 'END {print $4}') >= 1048576 )) || fail 'Recovery needs at least 1 GiB free working space'
AUTH=
trap '[[ -z "$AUTH" ]] || rm -f -- "$AUTH"' EXIT
load_s3 "$CONFIG"
# Never accept an out-of-band repository password or an inherited selector.
unset RESTIC_PASSWORD RESTIC_PASSWORD_FILE RESTIC_PASSWORD_COMMAND RESTIC_REPOSITORY_FILE
exec 9>"$LOCK_FILE"
flock -n 9 || fail 'Recovery already running'
note 'Reading the committed R2 recovery index'
if [[ -n "$SELECTED_POINT" ]]; then
    [[ "$SELECTED_POINT" =~ ^[a-f0-9]{64}$ ]] || fail 'Use a full snapshot ID to select a recovery point'
    POINT_KEY="$RECOVERY_PREFIX/points/$SELECTED_POINT.json"
else
    s3 GET "$RECOVERY_PREFIX/latest.json" "$WORK/latest.json" || fail 'Cannot download recovery index; existing deployments were not touched'
    jq -e '.protocol == 2 and .point_schema == 2 and (.point_key|type=="string")' "$WORK/latest.json" >/dev/null || fail 'Unsupported recovery index version'
    POINT_KEY=$(jq -er .point_key "$WORK/latest.json")
fi
[[ "$POINT_KEY" == "$RECOVERY_PREFIX/points/"* ]] || fail 'Point key outside recovery namespace'
s3 GET "$POINT_KEY" "$WORK/point.json" || fail 'Committed recovery point is missing'
[[ -z "$POINT_HASH" ]] || verify_hash "$WORK/point.json" "$POINT_HASH"
if [[ -z "$SELECTED_POINT" ]]; then verify_hash "$WORK/point.json" "$(jq -er .point_sha256 "$WORK/latest.json")"; fi
jq -e --arg cluster "$CLUSTER_ID" '.catalog_schema==1 and .data_layout=="database-only-v1" and .instance_control.cluster_id==$cluster and .protocol==2 and .point_schema==2 and .backup_format=="tuwunel-checkpoint-v2" and .recovery_version==2 and .platform=="linux/amd64" and .scope.media==false and .scope.telegram==true and .media.backup=="external-s3"' "$WORK/point.json" >/dev/null || fail 'Unsupported recovery point/schema/platform/scope'
[[ $(uname -m) == x86_64 ]] || fail 'This backup requires an amd64 VPS; do not run incompatible database images'
SNAPSHOT=$(jq -er .snapshot_id "$WORK/point.json")
[[ "$SNAPSHOT" =~ ^[a-f0-9]{64}$ ]] || fail 'Invalid snapshot ID'
[[ "$POINT_KEY" == "$RECOVERY_PREFIX/points/$SNAPSHOT.json" && ( -z "$SELECTED_POINT" || "$SELECTED_POINT" == "$SNAPSHOT" ) ]] || fail 'Selected point identity mismatch'
jq -e '.software' "$WORK/point.json" >"$WORK/software.json"
RELEASE=$(jq -er .manifest_sha256 "$WORK/software.json")
[[ "$RELEASE" =~ ^[a-f0-9]{64}$ ]] || fail 'Invalid public release descriptor'
RELEASE_ROOT=$(release_root)
if [[ $(hash_file "$RELEASE_ROOT/release-manifest.json") != "$RELEASE" ]] || ! release_verify "$RELEASE_ROOT"; then
    release_fetch "$WORK/software.json" "$WORK/release"
    # Resume through the exact pinned reader, retaining the immutable point even
    # if latest.json changes. No data was restored and no host service changed.
    rm -f -- "$AUTH"; AUTH=
    exec 9>&-
    exec bash "$WORK/release/backup/recover.sh" "${RECOVER_ARGS[@]}" --point "$SNAPSHOT" --point-sha256 "$(hash_file "$WORK/point.json")"
fi
RESTIC_BIN="$RELEASE_ROOT/backup/bin/restic"
expected_restic=$(jq -er .restic_version "$RELEASE_ROOT/release.json")
[[ $("$RESTIC_BIN" version) == "restic $expected_restic "* ]] || fail 'Incorrect restic binary'
# Configuration is small, immutable and in R2. Standby never opens B2.
configuration_hash=$(jq -er .configuration_sha256 "$WORK/point.json")
configuration_object=$(jq -er .configuration_object "$WORK/point.json")
[[ "$configuration_hash" =~ ^[a-f0-9]{64}$ && "$configuration_object" == "$RECOVERY_PREFIX/configurations/$configuration_hash.json" ]] || fail 'Invalid R2 configuration reference'
s3 GET "$configuration_object" "$WORK/configuration.json" || fail 'R2 configuration capsule is missing'
verify_hash "$WORK/configuration.json" "$configuration_hash"
mkdir -p "$WORK/download"
"$RELEASE_ROOT/backup/bin/matrix-release" config-unpack --input "$WORK/configuration.json" --bundle "$WORK/download/bundle" || fail 'Invalid R2 configuration capsule'
BUNDLE="$WORK/download/bundle"
jq -e --slurpfile p "$WORK/point.json" '.format==$p[0].backup_format and .recovery_version==$p[0].recovery_version and .data_layout==$p[0].data_layout and .software==$p[0].software and .instance_control==$p[0].instance_control' "$BUNDLE/metadata.json" >/dev/null || fail 'R2 configuration metadata differs from recovery point'
jq -e --arg cluster "$CLUSTER_ID" '.cluster_id==$cluster' "$BUNDLE/payload/control/source-local.json" >/dev/null || fail 'Source controller configuration belongs to another cluster'
if [[ "$MODE" =~ ^(standby|check|update)$ ]]; then
    jq -e '.instance_control.schema==1' "$WORK/point.json" >/dev/null || fail 'Standby preparation requires a controller-managed recovery point'
    jq -e --slurpfile p "$WORK/point.json" '.format==$p[0].backup_format and .recovery_version==$p[0].recovery_version and .software==$p[0].software and .instance_control==$p[0].instance_control' "$WORK/download/bundle/metadata.json" >/dev/null || fail 'Private bootstrap metadata differs from recovery point'
    [[ -f "$RELEASE_ROOT/backup/prepare-standby.sh" ]] || fail 'This release predates standby preparation; update the source and publish a new backup'
    cp -- "$CONFIG" "$WORK/internal.env"
    printf '\nR2_CONFIG=%q\n' "$CONFIG" >>"$WORK/internal.env"
    host_emit >>"$WORK/internal.env"
    if [[ "$MODE" == check || "$MODE" == update ]]; then
        [[ -f "$RELEASE_ROOT/backup/foundation-software.sh" ]] || fail 'Recovery point lacks the software update contract; publish a new complete source backup'
        bash "$RELEASE_ROOT/backup/foundation-software.sh" "$MODE" "$WORK/internal.env" "$WORK/download/bundle" "$RELEASE_ROOT" "$TARGET" "$SNAPSHOT" "$RELEASE"
        rm -f -- "$WORK/internal.env" "$WORK/repository-key.json"
        unset RESTIC_PASSWORD
        if [[ -n "$RESULT_FILE" ]]; then
            jq -n --arg s "$SNAPSHOT" --arg h "$(hash_file "$WORK/point.json")" '{schema:1,snapshot_id:$s,point_sha256:$h,software_matches:true}' >"$RESULT_FILE.tmp"
            mv "$RESULT_FILE.tmp" "$RESULT_FILE"
        fi
        exit 0
    fi
    bash "$RELEASE_ROOT/backup/prepare-standby.sh" --config "$WORK/internal.env" --bundle "$WORK/download/bundle" --release "$RELEASE_ROOT" --target "$TARGET"
    rm -f -- "$WORK/internal.env" "$WORK/repository-key.json"
    unset RESTIC_PASSWORD
    note "Standby prepared at $TARGET. No database or media bodies, application images, or run permission were restored."
    exit 0
fi
repository_hash=$(jq -er .repository_sha256 "$WORK/point.json")
repository_id=$(jq -er .repository_id "$WORK/point.json")
repository_object=$(jq -er .repository_object "$WORK/point.json")
[[ "$repository_hash" =~ ^[a-f0-9]{64}$ && "$repository_id" =~ ^[a-zA-Z0-9_-]+$ && "$repository_object" == "$RECOVERY_PREFIX/repositories/$repository_id-$repository_hash.json" ]] || fail 'Invalid R2 repository reference'
s3 GET "$repository_object" "$WORK/repository-key.json" || fail 'Cannot retrieve B2 repository configuration from R2'
verify_hash "$WORK/repository-key.json" "$repository_hash"
jq -e --arg id "$repository_id" '.schema==1 and .repository_id==$id and ([.password,.access_key_id,.secret_access_key,.region,.bucket,.path,.endpoint]|all(type=="string" and length>0))' "$WORK/repository-key.json" >/dev/null || fail 'Invalid database repository configuration'
DB_ENDPOINT=$(jq -er .endpoint "$WORK/repository-key.json")
DB_REGION=$(jq -er .region "$WORK/repository-key.json")
DB_BUCKET=$(jq -er .bucket "$WORK/repository-key.json")
repo_path=$(jq -er .path "$WORK/repository-key.json")
[[ "$DB_ENDPOINT" =~ ^https://[a-zA-Z0-9.-]+$ && "$DB_REGION" =~ ^[a-zA-Z0-9-]+$ && "$DB_BUCKET" =~ ^[a-zA-Z0-9.-]+$ && "$repo_path" =~ ^[a-zA-Z0-9/_-]+$ && "$repo_path" != *..* ]] || fail 'Invalid database endpoint or repository path'
export AWS_ACCESS_KEY_ID=$(jq -er .access_key_id "$WORK/repository-key.json")
export AWS_SECRET_ACCESS_KEY=$(jq -er .secret_access_key "$WORK/repository-key.json")
export AWS_DEFAULT_REGION="$DB_REGION"
export RESTIC_PASSWORD=$(jq -er .password "$WORK/repository-key.json")
export RESTIC_REPOSITORY="s3:$DB_ENDPOINT/$DB_BUCKET/$repo_path"
export RESTIC_CACHE_DIR="$WORK/cache" GOMAXPROCS=1 RESTIC_READ_CONCURRENCY=1
RESTIC=("$RESTIC_BIN" -o s3.connections=2)
"${RESTIC[@]}" cat config >"$WORK/repository.json" 2>"$WORK/repository.log"
[[ $(jq -er .id "$WORK/repository.json") == "$repository_id" ]] || fail 'Restic repository identity mismatch'
"${RESTIC[@]}" snapshots --json "$SNAPSHOT" >"$WORK/snapshot.json" 2>"$WORK/snapshot.log"
jq -e --arg id "$SNAPSHOT" --arg tag recovery-v2 'length==1 and .[0].id==$id and (.[0].tags|index("verified")!=null and index($tag)!=null and index("tuwunel-checkpoint")!=null)' "$WORK/snapshot.json" >/dev/null || fail 'Indexed snapshot is not a verified recovery snapshot'
note 'Restoring database data from B2 and matching it to the R2 configuration'
"${RESTIC[@]}" restore "$SNAPSHOT" --target "$WORK/database-download" --verify >"$WORK/restore.log" 2>&1
"$RELEASE_ROOT/backup/bin/matrix-release" data-merge --input "$WORK/database-download" --bundle "$BUNDLE" || fail 'Database snapshot differs from the R2 manifest'
source "$BASE/payload-verify.sh"
payload_verify "$BUNDLE" full || fail 'Restored data/configuration inventory differs'
BUNDLE="$WORK/download/bundle"
jq -e --slurpfile p "$WORK/point.json" '.format==$p[0].backup_format and .recovery_version==$p[0].recovery_version and .server_name==$p[0].server_name and .media_included==false and .telegram_included==true and .telegram.image_id==$p[0].telegram.image_id and .software==$p[0].software and .images==$p[0].images and .instance_control==$p[0].instance_control' "$BUNDLE/metadata.json" >/dev/null || fail 'Snapshot metadata differs from recovery point'
(cd "$BUNDLE/payload"; sha256sum --quiet -c ../manifest.sha256)
# Image software belongs in an immutable registry, never the private data bucket.
image_pull() {
    local kind=$1 ref expected
    ref=$(jq -er --arg kind "$kind" '.images[$kind].ref' "$WORK/point.json")
    expected=$(jq -er --arg kind "$kind" '.images[$kind].id' "$WORK/point.json")
    [[ "$ref" =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$ && "$expected" =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'Invalid immutable image descriptor'
    docker image inspect "$ref" >/dev/null 2>&1 || docker pull "$ref" >"$WORK/$kind-image-pull.log" 2>&1 || fail "Cannot retrieve pinned $kind image"
    [[ $(docker image inspect --format '{{.Id}}' "$ref") == "$expected" ]] || fail 'Container image identity mismatch'
    [[ $(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$ref") == linux/amd64 ]] || fail 'Container image platform mismatch'
    printf '%s\n' "$ref"
}
IMAGE=$(image_pull tuwunel)
TELEGRAM_IMAGE=$(image_pull telegram)
# Private generated configuration is an implementation detail, not user input.
printf 'S3_ENDPOINT=%q\nS3_REGION=%q\nS3_BUCKET=%q\nAWS_ACCESS_KEY_ID=%q\nAWS_SECRET_ACCESS_KEY=%q\nAWS_DEFAULT_REGION=%q\nRESTIC_REPOSITORY=%q\nRESTIC_PASSWORD=%q\nRESTIC_BIN=%q\nR2_CONFIG=%q\n' \
    "$DB_ENDPOINT" "$DB_REGION" "$DB_BUCKET" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" "$DB_REGION" "$RESTIC_REPOSITORY" "$RESTIC_PASSWORD" "$RESTIC_BIN" "$CONFIG" >"$WORK/internal.env"
printf '\nRECOVERY_IMAGE_REF=%q\nRECOVERY_IMAGE_ID=%q\n' "$IMAGE" "$(jq -er .image_id "$WORK/point.json")" >>"$WORK/internal.env"
printf '\nRECOVERY_TELEGRAM_IMAGE_REF=%q\nRECOVERY_TELEGRAM_IMAGE_ID=%q\n' "$TELEGRAM_IMAGE" "$(jq -er .telegram.image_id "$WORK/point.json")" >>"$WORK/internal.env"
host_emit >>"$WORK/internal.env"
TEST_TARGET="$WORK/rebuilt"
note 'Running the versioned recovery adapter in network isolation'
bash "$RELEASE_ROOT/backup/restore-tuwunel.sh" --config "$WORK/internal.env" --bundle "$BUNDLE" --release "$RELEASE_ROOT" --isolated --target "$TEST_TARGET" >"$WORK/validation.log" 2>&1
jq -n --slurpfile point "$WORK/point.json" --arg snapshot "$SNAPSHOT" --arg release "$RELEASE" --arg mode "$MODE" \
    '{protocol:2,snapshot_id:$snapshot,release:$release,mode:$mode,s3_only_input:true,consistency:$point[0].consistency,operation_id:$point[0].operation_id,database_startup_verified:true,media_included:false,media_reused_from_s3:true,media:$point[0].media,telegram_included:true,telegram_database_startup_verified:true,telegram_authentication_tested:false}' >"$WORK/result.json"
unset RESTIC_PASSWORD
rm -f -- "$WORK/internal.env" "$WORK/repository-key.json"
note "Recovery succeeded ($MODE). Report and private logs: $WORK"
consistency=$(jq -er .consistency "$WORK/point.json")
if [[ -n "$RESULT_FILE" ]]; then
    jq --arg work "$WORK" --arg root "$TEST_TARGET" --arg hash "$(hash_file "$WORK/point.json")" '.+{work_dir:$work,deployment_root:$root,point_sha256:$hash}' "$WORK/result.json" >"$RESULT_FILE.tmp"
    mv "$RESULT_FILE.tmp" "$RESULT_FILE"
fi
note "Scope: Tuwunel and Telegram database/config; consistency=$consistency. Media remains in native S3 and is reused; no media content was downloaded or restored. DNS was not changed."
