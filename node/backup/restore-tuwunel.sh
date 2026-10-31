#!/usr/bin/env bash
# Version-pinned public adapter; R2 configuration plus B2 database data.
set -Eeuo pipefail
umask 077
source "$(dirname -- "${BASH_SOURCE[0]}")/local-config.sh"
host_defaults
CONFIG= BUNDLE= MODE= TARGET= RELEASE=
while (( $# )); do
    case "$1" in
        --config) CONFIG=$2; shift 2 ;;
        --bundle) BUNDLE=$2; shift 2 ;;
        --release) RELEASE=$2; shift 2 ;;
        --target) TARGET=$2; shift 2 ;;
        --isolated) MODE=isolated; shift ;;
        --production) printf 'Use controller admission to activate a restored generation\n' >&2; exit 1 ;;
        *) printf 'Unknown argument: %s\n' "$1" >&2; exit 1 ;;
    esac
done
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
[[ -f "$CONFIG" && -d "$BUNDLE" && "$MODE" == isolated && -d "$RELEASE" ]] ||
    die 'Usage: restore-tuwunel.sh --config FILE --bundle DIRECTORY --release VERIFIED_DIRECTORY --isolated [--target NEW_DIRECTORY]'
(( EUID == 0 )) || die 'Run as root to preserve database owners and configure Docker/nginx'
[[ $(stat -c %a "$CONFIG") =~ ^[046]00$ ]] || die 'The single recovery configuration must be private (0600)'
CONFIG=$(realpath -e -- "$CONFIG")
BUNDLE=$(realpath -e -- "$BUNDLE")
unset RESTIC_PASSWORD RESTIC_PASSWORD_FILE RESTIC_PASSWORD_COMMAND RESTIC_REPOSITORY_FILE
# Administrator-provided private shell configuration.
# shellcheck disable=SC1090
source "$CONFIG"
host_validate
[[ -n ${RESTIC_PASSWORD:-} ]] || die 'RESTIC_PASSWORD must be in this configuration, not a second file'
[[ -n ${S3_REGION:-${AWS_DEFAULT_REGION:-${B2_REGION:-}}} && -n ${S3_BUCKET:-${B2_BUCKET:-}} && -n ${AWS_ACCESS_KEY_ID:-} && -n ${AWS_SECRET_ACCESS_KEY:-} ]] || die 'B2 settings are incomplete'
[[ -n "$TARGET" ]] || TARGET=${RESTORE_ROOT:-/opt/matrix}
[[ "$TARGET" == /* && "$TARGET" != *[!a-zA-Z0-9_./-]* ]] || die 'Use an absolute target path without whitespace'
TARGET=$(realpath -m -- "$TARGET")
[[ "$TARGET" != / && "$TARGET" != /opt && "$TARGET" != /root && ! -e "$TARGET" ]] || die 'Target must be a new, nonexistent project directory'
[[ "$BUNDLE/" != "$TARGET/"* && "$TARGET/" != "$BUNDLE/"* ]] || die 'Bundle and destination must be separate'
for tool in "$DOCKER_BIN" jq sha256sum find sort xargs cmp curl timeout "$SQLITE_BIN"; do
    command -v "$tool" >/dev/null || die "Install prerequisite: $tool"
done
compose version >/dev/null
[[ $(jq -r '.format' "$BUNDLE/metadata.json") == tuwunel-checkpoint-v2 ]] || die 'Unsupported backup format'
[[ $(jq -r '.recovery_version // 0' "$BUNDLE/metadata.json") == 2 ]] || die 'Unsupported recovery format'
DOMAIN=$(jq -er .server_name "$BUNDLE/metadata.json")
[[ "$DOMAIN" =~ ^[a-zA-Z0-9.-]+$ ]] || die 'Invalid server identity in metadata'
PAYLOAD="$BUNDLE/payload"
source "$(dirname -- "$0")/payload-verify.sh"
payload_verify "$BUNDLE" full || die 'Invalid private data payload'
for item in database/CURRENT config/docker-compose.yml config/registration-token nginx/matrix.conf nginx/00-matrix-common.conf telegram/data/telegram.db telegram/data/config.yaml telegram/data/registration.yaml telegram/metadata.json config/s3/media.env; do
    [[ -s "$PAYLOAD/$item" ]] || die "Required recovery asset is missing: $item"
done
if jq -e '.instance_control != null' "$BUNDLE/metadata.json" >/dev/null; then
    for item in control/source-local.json; do
        [[ -s "$PAYLOAD/$item" ]] || die "Required controller asset is missing: $item"
    done
fi

source "$(dirname -- "$0")/release-common.sh"
release_verify "$RELEASE" || die 'Public release validation failed'
[[ $(sha256sum "$RELEASE/release-manifest.json" | cut -d' ' -f1) == $(jq -er .software.manifest_sha256 "$BUNDLE/metadata.json") ]] || die 'Adapter release differs from snapshot'
[[ ! -e "$PAYLOAD/recovery" ]] || die 'Executable recovery payloads are forbidden'

[[ ! -e "$BUNDLE/media" && $(jq -r .media.backup "$BUNDLE/metadata.json") == external-s3 ]] || die 'Expected database-only snapshot with external S3 media'

mkdir -p -- "$TARGET/deploy/tuwunel" "$TARGET/deploy/backup/bin" "$TARGET/deploy/nginx" "$TARGET/recovery-state"
REPORT="$TARGET/recovery-state/result.json"
TG_PROJECT="$TARGET/deploy/telegram"
log 'Reconstructing data/configuration with the exact public tool release'
cp -a -- "$PAYLOAD/config/." "$TARGET/deploy/tuwunel/"
cp -a --reflink=auto -- "$PAYLOAD/database" "$TARGET/deploy/tuwunel/data"
mkdir -p "$TG_PROJECT"
cp -a -- "$PAYLOAD/telegram/config/." "$TG_PROJECT/"
cp -a -- "$PAYLOAD/telegram/data" "$TG_PROJECT/data"
mkdir -m 700 "$TG_PROJECT/data/tmp"
chown --reference="$TG_PROJECT/data" "$TG_PROJECT/data/tmp"
SQLITE_BIN="$SQLITE_BIN" bash "$RELEASE/backup/telegram-backup.sh" verify "$TG_PROJECT/data/telegram.db" "$PAYLOAD/telegram/metadata.json"
cp -a -- "$PAYLOAD/nginx/." "$TARGET/deploy/nginx/"
release_copy "$RELEASE" "$TARGET/deploy"
if [[ -d "$PAYLOAD/control" ]]; then
    mkdir -p "$TARGET/deploy/control"
    cp -a -- "$PAYLOAD/control/." "$TARGET/deploy/control/"
    [[ -f "${R2_CONFIG:-}" ]] || die 'Current R2 input is required'
    install -m600 "$R2_CONFIG" "$TARGET/deploy/control/s3.env"
    # Never copy source-local.json to local.json, create managed, or restore the
    # source journal. A recovered data set has no business run authorization.
fi
install -m 600 -- "$CONFIG" "$TARGET/deploy/backup/restic.env"
# Configuration generated by the Agent, still from the one user-edited file.
printf '\nSTATE_DIR=%q\nDATA_DIR=%q\nPROJECT_DIR=%q\n' \
    "$TARGET/backup-state/tuwunel-restic" "$TARGET/deploy/tuwunel/data" "$TARGET/deploy/tuwunel" \
    >>"$TARGET/deploy/backup/restic.env"
printf '\nTELEGRAM_PROJECT_DIR=%q\n' "$TG_PROJECT" >>"$TARGET/deploy/backup/restic.env"
chmod 755 "$TARGET/deploy/backup/telegram-backup.sh" "$TARGET/deploy/backup/verify-telegram-runtime.sh" "$TARGET/deploy/backup/publish-recovery.sh" "$TARGET/deploy/backup/tuwunel-restic.sh" "$TARGET/deploy/backup/restore-tuwunel.sh"
RESTIC_TOOL=${RESTIC_BIN:-$(command -v restic || true)}
[[ -n "$RESTIC_TOOL" && -x "$RESTIC_TOOL" ]] || die 'Install verified restic and add it to PATH (or RESTIC_BIN in the same config)'
install -m 755 -- "$RESTIC_TOOL" "$TARGET/deploy/backup/bin/restic"
printf '\nRESTIC_BIN=%q\n' "$TARGET/deploy/backup/bin/restic" >>"$TARGET/deploy/backup/restic.env"
PROJECT="$TARGET/deploy/tuwunel"
[[ ${RECOVERY_IMAGE_REF:-} =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$ ]] || die 'Missing immutable Tuwunel image reference'
[[ $(docker image inspect --format '{{.Id}}' "$RECOVERY_IMAGE_REF") == "${RECOVERY_IMAGE_ID:-}" && "$RECOVERY_IMAGE_ID" == $(jq -er .image_id "$BUNDLE/metadata.json") ]] || die 'Tuwunel image identity mismatch'
# Preserve the fixed bridge IP and the matching trusted subnet on a fresh Docker host.
compose --project-directory "$PROJECT" -f "$PROJECT/docker-compose.yml" config --format json |
    jq --arg image "$RECOVERY_IMAGE_REF" --slurpfile t "$PAYLOAD/telegram/metadata.json" 'del(.services.tuwunel.build) | .services.tuwunel.image=$image | .services.tuwunel.restart="no" | .networks.default.name=$t[0].network.name | .networks.default.ipam.config=[{subnet:$t[0].network.subnet,gateway:$t[0].network.gateway}] | .services.tuwunel.environment |= with_entries(select(.key|startswith("TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__")|not)) | .services.tuwunel.env_file=["./s3/media.env"]' >"$PROJECT/docker-compose.json.tmp"
mv "$PROJECT/docker-compose.json.tmp" "$PROJECT/docker-compose.yml"
[[ ${RECOVERY_TELEGRAM_IMAGE_REF:-} =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$ ]] || die 'Missing immutable Telegram image reference'
[[ $(docker image inspect --format '{{.Id}}' "$RECOVERY_TELEGRAM_IMAGE_REF") == "${RECOVERY_TELEGRAM_IMAGE_ID:-}" && "$RECOVERY_TELEGRAM_IMAGE_ID" == $(jq -er .image_id "$PAYLOAD/telegram/metadata.json") ]] || die 'Telegram image identity mismatch'
compose --project-directory "$TG_PROJECT" -f "$TG_PROJECT/docker-compose.yml" config --format json |
    jq --arg image "$RECOVERY_TELEGRAM_IMAGE_REF" 'del(.services.telegram.build) | .services.telegram.image=$image | .services.telegram.restart="no"' >"$TG_PROJECT/docker-compose.json.tmp"
mv "$TG_PROJECT/docker-compose.json.tmp" "$TG_PROJECT/docker-compose.yml"
compose --project-directory "$TG_PROJECT" -f "$TG_PROJECT/docker-compose.yml" config --quiet
compose --project-directory "$PROJECT" -f "$PROJECT/docker-compose.yml" config --quiet
IMAGE=$(compose --project-directory "$PROJECT" -f "$PROJECT/docker-compose.yml" config --format json | jq -er '.services.tuwunel.image')
docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" >"$TARGET/recovery-state/image-pull.log" 2>&1

# Verify startup on a disposable copy, with all networking disabled. Production
# identity is never exposed by this test, even on a host with the original node.
PROBE=$(mktemp -d "$TARGET/recovery-state/validation.XXXXXX")
cp -a --reflink=auto -- "$PROJECT/data" "$PROBE/database"
NAME="tuwunel-recovery-check-$(cat /proc/sys/kernel/random/uuid)"
cleanup() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
log 'Checking recovered RocksDB using a network-isolated maintenance container'
if ! timeout --kill-after=10 180 docker run --rm --name "$NAME" --network none --memory 192m --memory-swap 384m \
    --pids-limit 128 --cpus 0.5 --read-only --tmpfs /tmp:size=16m \
    -v "$PROBE/database:/var/lib/tuwunel" -e "TUWUNEL_SERVER_NAME=$DOMAIN" \
    -e TUWUNEL_DATABASE_PATH=/var/lib/tuwunel -e TUWUNEL_ADDRESS=0.0.0.0 \
    -e TUWUNEL_DB_CACHE_CAPACITY_MB=16 -e TUWUNEL_DB_WRITE_BUFFER_CAPACITY_MB=8 \
    -e TUWUNEL_CACHE_CAPACITY_MODIFIER=0.1 -e TUWUNEL_DB_POOL_MAX_WORKERS=2 \
    -e TUWUNEL_ALLOW_FEDERATION=false "$IMAGE" --maintenance --execute 'server shutdown' \
    >"$PROBE/startup.log" 2>&1; then
    die 'Recovered database failed maintenance startup; inspect private recovery-state logs'
fi
rm -rf -- "$PROBE/database"
SQLITE_BIN="$SQLITE_BIN" DOCKER_BIN="$DOCKER_BIN" bash "$TARGET/deploy/backup/verify-telegram-runtime.sh" "$RECOVERY_TELEGRAM_IMAGE_REF" "$TG_PROJECT/data" "$PAYLOAD/telegram/metadata.json" "$TARGET/recovery-state/telegram-validation"
jq -n --slurpfile metadata "$BUNDLE/metadata.json" --arg domain "$DOMAIN" --arg mode "$MODE" --arg image "$IMAGE" \
    '{database_startup_verified:true,mode:$mode,server_name:$domain,image:$image,media_included:false,media_reused_from_s3:true,media:$metadata[0].media,telegram_included:true,telegram_database_integrity_verified:true,telegram_database_startup_verified:true,telegram_authentication_tested:false,consistency:$metadata[0].consistency,production_started:false}' >"$REPORT"
if [[ "$MODE" == isolated ]]; then
    log 'Isolated reconstruction verified. No ports, production container, nginx or timer were activated.'
    log "Report: $REPORT"
    exit 0
fi
