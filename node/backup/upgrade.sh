#!/usr/bin/env bash
# Explicit software upgrade, independent of data restore. The next successful
# backup records this public release; old recovery points retain their versions.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd -P)
source "$BASE/local-config.sh"
source "$BASE/release-common.sh"
host_defaults
TARGET=/opt/matrix RELEASE= MODE=check
while (($#)); do
    case "$1" in
        --target) TARGET=${2:?}; shift 2 ;;
        --release) RELEASE=${2:?}; shift 2 ;;
        --apply) MODE=update; shift ;;
        *) echo 'Use upgrade.sh --release VERIFIED_PUBLIC_RELEASE [--target FOUNDATION] [--apply]' >&2; exit 1 ;;
    esac
done
[[ $EUID == 0 && "$TARGET" == /* && "$TARGET" != / && -d "$RELEASE" ]] || exit 1
TARGET=$(realpath -e "$TARGET")
host_load "$TARGET/deploy/control/host.env"
[[ -f "$TARGET/deploy/control/managed" ]] || { echo 'An existing managed foundation is required' >&2; exit 1; }
release_verify "$RELEASE"
exec 9>"$LOCK_FILE"
flock -n 9 || { echo 'Recovery or another software update is running' >&2; exit 1; }
WORK=$(mktemp -d "$TARGET/agent/upgrade.XXXXXXXX")
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/bundle/payload/control"
cp "$TARGET/deploy/control/local.json" "$WORK/bundle/payload/control/source-local.json"
release_descriptor "$RELEASE" >"$WORK/software.json"
hash=$(jq -er .manifest_sha256 "$WORK/software.json")
jq -n --slurpfile software "$WORK/software.json" '{software:$software[0]}' >"$WORK/bundle/metadata.json"
(cd "$WORK/bundle/payload"; sha256sum ./control/source-local.json) >"$WORK/bundle/manifest.sha256"
host_emit >"$WORK/host.env"
bash "$RELEASE/backup/foundation-software.sh" "$MODE" "$WORK/host.env" "$WORK/bundle" "$RELEASE" "$TARGET" "software-$hash" "$hash"
