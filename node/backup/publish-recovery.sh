#!/usr/bin/env bash
# Publish private data/configuration recovery points, with a public software descriptor.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
source "$BASE/s3-common.sh"
source "$BASE/local-config.sh"
host_defaults
CONFIG=${MATRIX_BACKUP_CONFIG:-$BASE/restic.env}
STATE_DIR=/opt/matrix/backup-state/tuwunel-restic
RESTIC_BIN="$BASE/bin/restic"
source "$CONFIG"
exec 8>"$STATE_DIR/publication.lock"
flock -n 8 || fail 'Another recovery publication is running'
[[ -n ${RESTIC_PASSWORD:-} ]] || fail 'Publisher needs the existing internal restic password'
PASSWORD=$RESTIC_PASSWORD
WORK=$(mktemp -d "$STATE_DIR/publish.XXXXXXXX")
AUTH=
cleanup_publish() {
    local result=$?
    [[ -z "$AUTH" ]] || rm -f -- "$AUTH"
    if (( result == 0 )); then
        rm -rf -- "$WORK"
    else
        rm -f -- "$WORK/repository.json" "$WORK/compare"
        printf 'Recovery publication failed; private diagnostics: %s\n' "$WORK" >&2
    fi
}
trap cleanup_publish EXIT
# B2 credentials describe database storage; R2 transport is loaded separately.
S3_REGION=${S3_REGION:-${AWS_DEFAULT_REGION:-${B2_REGION:-}}}
S3_BUCKET=${S3_BUCKET:-${B2_BUCKET:-}}
S3_ENDPOINT=${S3_ENDPOINT:-https://s3.${S3_REGION}.backblazeb2.com}
S3_ENDPOINT=${S3_ENDPOINT%/}
[[ ${RESTIC_REPOSITORY:-} == "s3:$S3_ENDPOINT/$S3_BUCKET/"* ]] || fail 'Repository must use the configured database bucket'
REPOSITORY_PATH=${RESTIC_REPOSITORY#"s3:$S3_ENDPOINT/$S3_BUCKET/"}
[[ "$REPOSITORY_PATH" =~ ^[a-zA-Z0-9/_-]+$ ]] || fail 'Invalid repository path'
POINT=${RECOVERY_POINT_FILE:-$STATE_DIR/last-success.json}
[[ -f "$POINT" ]] || fail 'No verified backup to publish'
SNAPSHOT=$(jq -er .snapshot_id "$POINT")
REPOSITORY_ID=$(jq -er .repository_id "$POINT")
[[ "$SNAPSHOT" =~ ^[a-f0-9]{64}$ && "$REPOSITORY_ID" =~ ^[a-zA-Z0-9_-]+$ ]] || fail 'Invalid database snapshot/repository identity'
BUNDLE=${RECOVERY_BUNDLE:?Publisher requires the verified staged bundle}
CAPSULE=${RECOVERY_CONFIG_FILE:?Publisher requires the staged configuration capsule}
source "$BASE/payload-verify.sh"
payload_verify "$BUNDLE" full || fail 'Invalid staged payload'
jq -e '.recovery_version==2 and .data_layout=="database-only-v1" and .format=="tuwunel-checkpoint-v2" and .media_included==false and .media.backup=="external-s3" and .telegram_included==true and .instance_control.schema==1' "$BUNDLE/metadata.json" >/dev/null || fail 'Unsupported data-only recovery point'
# Never execute any remotely stored configuration or pass it to a shell.
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY RESTIC_PASSWORD
jq -n --arg id "$REPOSITORY_ID" --arg endpoint "$S3_ENDPOINT" --arg region "$S3_REGION" --arg bucket "$S3_BUCKET" --arg path "$REPOSITORY_PATH" \
    '{schema:1,repository_id:$id,endpoint:$endpoint,region:$region,bucket:$bucket,path:$path,access_key_id:env.AWS_ACCESS_KEY_ID,secret_access_key:env.AWS_SECRET_ACCESS_KEY,password:env.RESTIC_PASSWORD}' >"$WORK/repository.json"
control=${CONTROL_DIR:-$BASE/../control}
[[ -f "$control/managed" ]] || fail 'Recovery publication requires an enrolled controller'
CLUSTER_ID=$(jq -er .cluster_id "$control/local.json")
load_s3 "$control/s3.env"
jq -e --arg cluster "$CLUSTER_ID" '.instance_control.cluster_id==$cluster' "$BUNDLE/metadata.json" >/dev/null || fail 'R2 cluster differs from snapshot'
capture_latest
source "$BASE/release-common.sh"
jq -e '.software' "$BUNDLE/metadata.json" >"$WORK/software.json"
# Prove the public release can be fetched before the first point references it.
# The cache is content-addressed, and verified again before each publication.
release_hash=$(jq -er .manifest_sha256 "$WORK/software.json")
[[ "$release_hash" =~ ^[a-f0-9]{64}$ ]] || fail 'Invalid release digest'
mkdir -p "$STATE_DIR/public-releases"
release="$STATE_DIR/public-releases/$release_hash"
[[ -d "$release" ]] || release_fetch "$WORK/software.json" "$release"
release_verify "$release" || fail 'Public release verification failed'
verify_hash "$release/release-manifest.json" "$release_hash"
# R2 read permission intentionally grants access to the recovery credentials.
REPOSITORY_HASH=$(hash_file "$WORK/repository.json")
CONFIG_HASH=$(hash_file "$CAPSULE")
REPOSITORY_OBJECT="$RECOVERY_PREFIX/repositories/$REPOSITORY_ID-$REPOSITORY_HASH.json"
CONFIG_OBJECT="$RECOVERY_PREFIX/configurations/$CONFIG_HASH.json"
# Verify the capsule against the staged bundle before referencing it remotely.
"$BASE/bin/matrix-release" config-unpack --input "$CAPSULE" --bundle "$WORK/configuration"
cmp -s "$WORK/configuration/manifest.sha256" "$BUNDLE/manifest.sha256" || fail 'Configuration capsule manifest differs'
jq -e --slurpfile m "$BUNDLE/metadata.json" '.==$m[0]' "$WORK/configuration/metadata.json" >/dev/null || fail 'Configuration capsule metadata differs'
put_verified "$REPOSITORY_OBJECT" "$WORK/repository.json"
put_verified "$CONFIG_OBJECT" "$CAPSULE"
jq -n --slurpfile p "$POINT" --slurpfile m "$BUNDLE/metadata.json" --arg repo "$REPOSITORY_OBJECT" --arg rh "$REPOSITORY_HASH" --arg conf "$CONFIG_OBJECT" --arg ch "$CONFIG_HASH" \
    '{protocol:2,point_schema:2,catalog_schema:1,data_layout:"database-only-v1",backup_format:$m[0].format,recovery_version:2,
    platform:"linux/amd64",snapshot_id:$p[0].snapshot_id,repository_id:$p[0].repository_id,
    repository_object:$repo,repository_sha256:$rh,configuration_object:$conf,configuration_sha256:$ch,
    software:$m[0].software,server_name:$m[0].server_name,
    data_time:$p[0].data_time,verification:$p[0].verification,database_startup_verified:$p[0].database_startup_verified,
    image_id:$m[0].image_id,images:$m[0].images,consistency:$m[0].consistency,telegram:$m[0].telegram,
    operation_id:$m[0].operation_id,stopped_proof:$m[0].stopped_proof,media:$m[0].media,instance_control:$m[0].instance_control,
    scope:{media:false,telegram:true}}' >"$WORK/point.json"
POINT_KEY="$RECOVERY_PREFIX/points/$SNAPSHOT.json"
put_verified "$POINT_KEY" "$WORK/point.json"
jq -n --arg key "$POINT_KEY" --arg sha "$(hash_file "$WORK/point.json")" \
    '{protocol:2,point_schema:2,point_key:$key,point_sha256:$sha}' >"$WORK/latest.json"
if [[ ${RECOVERY_COMMIT_MODE:-} == final ]]; then
    operation=$(jq -er .operation_id "$BUNDLE/metadata.json")
    [[ "$operation" =~ ^[a-zA-Z0-9_-]{1,100}$ ]] || fail 'Invalid final operation ID'
    control=${CONTROL_DIR:-$BASE/../control}
    "$control/bin/matrix-control" status --config "$control/local.json" >"$WORK/control-status.json"
    jq -e --arg op "$operation" --slurpfile point "$WORK/point.json" '.blocked and (.serving|not) and .final_backup.id==$op and .final_backup.phase=="SNAPSHOTTING" and .control.phase=="SNAPSHOTTING" and .control.operation.id==$op and .instance_id==$point[0].instance_control.source_instance_id and .bound_epoch==$point[0].instance_control.source_epoch and $point[0].consistency=="globally-quiesced" and $point[0].verification=="full-restore" and $point[0].database_startup_verified==true' "$WORK/control-status.json" >/dev/null || fail 'Final source ownership or verification mismatch'
    "$control/bin/matrix-control" final-verify --config "$control/local.json" --expected-instance "$(jq -er .instance_id "$WORK/control-status.json")" --expected-epoch "$(jq -er .bound_epoch "$WORK/control-status.json")" --operation-id "$operation" >/dev/null
    jq -n --arg key "$POINT_KEY" --arg sha "$(hash_file "$WORK/point.json")" --arg op "$operation" --slurpfile p "$WORK/point.json" '{schema:1,operation_id:$op,cluster_id:$p[0].instance_control.cluster_id,source_instance_id:$p[0].instance_control.source_instance_id,source_epoch:$p[0].instance_control.source_epoch,cutoff_time:$p[0].data_time,snapshot_id:$p[0].snapshot_id,point_key:$key,point_sha256:$sha,full_restore_verified:true,database_startup_verified:$p[0].database_startup_verified,verified_at:(now|todateiso8601)}' >"$STATE_DIR/final-published-$operation.json.tmp"
    mv "$STATE_DIR/final-published-$operation.json.tmp" "$STATE_DIR/final-published-$operation.json"
    note "Immutable final recovery point published (latest unchanged): $SNAPSHOT"
    exit 0
fi
# Recheck current authority before committing the mutable data index.
control=${CONTROL_DIR:-$BASE/../control}
if [[ -f "$control/managed" ]]; then
    "$control/bin/matrix-control" status --config "$control/local.json" >"$WORK/control-status.json"
    jq -e --slurpfile point "$WORK/point.json" \
        '.serving and (.blocked|not) and .cluster_id==$point[0].instance_control.cluster_id and .instance_id==$point[0].instance_control.source_instance_id and .bound_epoch==$point[0].instance_control.source_epoch' \
        "$WORK/control-status.json" >/dev/null || fail 'Instance lost backup publication permission'
fi
commit_latest
cp "$WORK/latest.json" "$STATE_DIR/last-published.json.tmp"
mv "$STATE_DIR/last-published.json.tmp" "$STATE_DIR/last-published.json"
note "Data-only recovery point published: $SNAPSHOT"
