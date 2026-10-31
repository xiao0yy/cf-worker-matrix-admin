#!/usr/bin/env bash
# Versioned bootstrap: small controller/config subset only. No business start.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
source "$BASE/local-config.sh"
host_defaults
CONFIG= BUNDLE= RELEASE= TARGET=
while (($#)); do
    case "$1" in
        --config) CONFIG=$2;shift 2;; --bundle) BUNDLE=$2;shift 2;;
        --release) RELEASE=$2;shift 2;; --target) TARGET=$2;shift 2;; *) exit 1;;
    esac
done
[[ $EUID == 0 && -f "$CONFIG" && $(stat -c %a "$CONFIG") == 600 ]] || exit 1
source "$CONFIG"
host_validate
host_bootstrap_target "$TARGET" || { echo 'Standby foundation must be new or contain only its private config/agent bootstrap envelope' >&2; exit 1; }
PAYLOAD="$BUNDLE/payload"
source "$BASE/release-common.sh"
release_verify "$RELEASE" || exit 1
[[ $(sha256sum "$RELEASE/release-manifest.json" | cut -d' ' -f1) == $(jq -er .software.manifest_sha256 "$BUNDLE/metadata.json") ]] || exit 1
source "$BASE/payload-verify.sh"
payload_verify "$BUNDLE" partial || { echo 'Invalid private bootstrap configuration' >&2; exit 1; }
"$RELEASE/control/bin/matrix-control" capabilities | jq -e '.schema==1 and .planned_handover' >/dev/null || exit 1
for name in tuwunel telegram-bridge; do
    if docker container inspect "$name" >/dev/null 2>&1; then echo 'Existing application container; standby bootstrap refused' >&2; exit 1; fi
done
for name in matrix.conf 00-matrix-common.conf; do
    [[ ! -e "$NGINX_HTTP_CONF_DIR/$name" ]] || { echo 'Existing nginx Matrix configuration; Agent must inspect it first' >&2; exit 1; }
done
mkdir -p "$TARGET/deploy/control" "$TARGET/deploy/backup" "$TARGET/deploy/nginx" "$TARGET/generations" "$TARGET/config" "$TARGET/agent"
cp -a "$PAYLOAD/control/." "$TARGET/deploy/control/"
release_copy "$RELEASE" "$TARGET/deploy"
CONTROL="$TARGET/deploy/control"
instance="matrix-$(cat /proc/sys/kernel/random/uuid)"
jq --arg root "$TARGET" --arg base "$CONTROL" --arg me "$instance" '.instance_id=$me | .s3_config=($base+"/s3.env") | .state_dir=($base+"/state") | .runtime_dir="/run/matrix-control" | .generations_dir=($root+"/generations") | .services[0].data_path=($root+"/deploy/tuwunel/data") | .services[1].data_path=($root+"/deploy/telegram/data")' "$CONTROL/source-local.json" >"$CONTROL/local.json"
RESTORE_ROOT=$TARGET
host_emit >"$CONTROL/host.env"
printf '\nWORK_ROOT=%q\nLOCK_FILE=%q\n' "$TARGET/generations" "$TARGET/agent/recovery.lock" >>"$CONTROL/host.env"
# The destination's user-supplied R2 input owns its credentials. The capsule
# never carries credentials for the source node's control storage.
[[ -f "${R2_CONFIG:-}" ]] || { echo 'Current R2 input is required' >&2; exit 1; }
install -m600 "$R2_CONFIG" "$CONTROL/s3.env"
chmod 700 "$TARGET/config" "$TARGET/agent"
chmod 600 "$TARGET/config/s3.env"
printf 'FOUNDATION=%q\nS3_CONFIG=%q\nLOCAL_CONFIG=%q\n' "$TARGET" "$TARGET/config/s3.env" "$CONTROL/host.env" >"$CONTROL/handover.env"
# Register the destination itself, never source registry or source identity.
jq -n --arg f "$TARGET" --arg s "$TARGET/config/s3.env" --arg h "$CONTROL/host.env" '{schema:1,foundation:$f,s3_config:$s,host_config:$h}' >"$TARGET/agent/registry.json"
mkdir -p "$CONTROL/nginx-server" "$CONTROL/nginx-location" "$CONTROL/state"
chmod -R go-rwx "$CONTROL"
cp "$CONTROL/nginx-server.conf" "$CONTROL/nginx-server/guard.conf"
cp "$CONTROL/nginx-location.conf" "$CONTROL/nginx-location/guard.conf"
old=$(dirname "$(jq -er .s3_config "$CONTROL/source-local.json")")
for name in matrix.conf 00-matrix-common.conf; do
    text=$(cat "$PAYLOAD/nginx/$name");text=${text//"$old"/"$CONTROL"}
    printf '%s\n' "$text" >"$TARGET/deploy/nginx/$name"
    install -m644 "$TARGET/deploy/nginx/$name" "$NGINX_HTTP_CONF_DIR/$name"
done
"$CONTROL/bin/matrix-control" prepare-standby --config "$CONTROL/local.json" --expected-instance "$instance"
printf '%s\n' '{"schema":1,"standby":true}' >"$CONTROL/managed"
text=$(cat "$CONTROL/matrix-control.service");text=${text//\/opt\/matrix/$TARGET}
printf '%s\n' "$text" >"$SYSTEMD_UNIT_DIR/matrix-control.service"
systemctl_cmd daemon-reload
systemctl_cmd enable --now matrix-control.service
nginx_cmd -t
systemctl_cmd enable --now "$NGINX_SERVICE"
systemctl_cmd reload "$NGINX_SERVICE"
bash "$CONTROL/handover-worker.sh" --install
# Install the management poller from the same verified public release.
if [[ -x "$CONTROL/bin/matrix-mailbox" ]]; then
    bash "$CONTROL/mailbox-install.sh" --install
fi
"$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json"
