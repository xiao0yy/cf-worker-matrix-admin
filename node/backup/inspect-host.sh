#!/usr/bin/env bash
# Local observations only. Never installs, restores, changes services, or grants ownership.
set -Eeuo pipefail
umask 077
source "$(dirname -- "${BASH_SOURCE[0]}")/local-config.sh"
STATE_DIR=
FOUNDATION= S3_CONFIG= HOST_CONFIG=
while (($#)); do
    case "$1" in
        --state-dir) STATE_DIR=${2:?};shift 2;;
        --foundation) FOUNDATION=${2:?};shift 2;;
        --config) S3_CONFIG=${2:?};shift 2;;
        --local) HOST_CONFIG=${2:?};shift 2;;
        *) echo 'Use [--state-dir DIR] [--foundation DIR] [--config FILE] [--local FILE]' >&2;exit 1;;
    esac
done
STATE_DIR=${STATE_DIR:-${FOUNDATION:-/opt/matrix}/agent}
for tool in jq flock timeout sha256sum; do command -v "$tool" >/dev/null || { echo "Agent must prepare missing probe prerequisite: $tool" >&2;exit 1; };done
for path in "$STATE_DIR" "$FOUNDATION" "$S3_CONFIG" "$HOST_CONFIG"; do
    [[ -z "$path" || ( "$path" == /* && "$path" != / && "$path" != *[!a-zA-Z0-9_./-]* ) ]] || exit 1
done
mkdir -p "$STATE_DIR/downloads" "$STATE_DIR/logs"
chmod 700 "$STATE_DIR"
exec 9>"$STATE_DIR/inspect.lock"
flock -n 9 || { echo 'Another local inspection is running' >&2;exit 1; }
REGISTRY="$STATE_DIR/registry.json"
if [[ -e "$REGISTRY" ]]; then
    [[ ! -L "$REGISTRY" && $(stat -c %a "$REGISTRY") == 600 ]] || exit 1
    jq -e '.schema==1 and (.foundation|type=="string") and (.s3_config|type=="string") and (.host_config|type=="string")' "$REGISTRY" >/dev/null || { echo 'Unknown or malformed local registry; Agent must inspect it' >&2;exit 1; }
    for pair in FOUNDATION:foundation S3_CONFIG:s3_config HOST_CONFIG:host_config; do
        var=${pair%%:*};key=${pair#*:};saved=$(jq -er --arg k "$key" '.[$k]' "$REGISTRY")
        [[ -z ${!var} || ${!var} == "$saved" ]] || { echo "Registered $key differs; inspect the local registry before changing paths" >&2;exit 1; }
        printf -v "$var" '%s' "$saved"
    done
else
    FOUNDATION=${FOUNDATION:-/opt/matrix}
    S3_CONFIG=${S3_CONFIG:-$FOUNDATION/config/s3.env}
    HOST_CONFIG=${HOST_CONFIG:-$STATE_DIR/local.env}
    jq -n --arg f "$FOUNDATION" --arg s "$S3_CONFIG" --arg h "$HOST_CONFIG" '{schema:1,foundation:$f,s3_config:$s,host_config:$h}' >"$REGISTRY.tmp"
    mv "$REGISTRY.tmp" "$REGISTRY"
fi
for path in "$FOUNDATION" "$S3_CONFIG" "$HOST_CONFIG"; do [[ "$path" == /* && "$path" != / && "$path" != *[!a-zA-Z0-9_./-]* ]] || exit 1;done
DOCKER_BIN=docker COMPOSE_BIN= NGINX_BIN=nginx SQLITE_BIN=sqlite3 SYSTEMCTL_BIN=systemctl
NGINX_MAIN_CONF=/etc/nginx/nginx.conf NGINX_SERVICE=nginx
actual_host=$HOST_CONFIG
[[ ! -f "$FOUNDATION/deploy/control/host.env" ]] || actual_host="$FOUNDATION/deploy/control/host.env"
if [[ -f "$actual_host" ]]; then
    [[ ! -L "$actual_host" && $(stat -c %a "$actual_host") =~ ^[046]00$ ]] || { echo 'Host config must be a private regular local file' >&2;exit 1; }
    source "$actual_host"
    [[ ${LOCAL_CONFIG_SCHEMA:-} == 1 ]] || exit 1
fi
TMP=$(mktemp -d "$STATE_DIR/.inspect.XXXXXXXX")
trap 'rm -rf -- "$TMP"' EXIT
: >"$TMP/dependencies.jsonl"
probe() {
    local name=$1 exe=$2 resolved state version
    shift 2
    resolved=$(type -P "$exe" || true)
    state=missing;version=
    if [[ -n "$resolved" ]]; then
        state=broken
        if timeout 8 "$resolved" "$@" >"$TMP/$name.output" 2>&1; then state=present;fi
        if [[ "$state" == present ]]; then IFS= read -r version <"$TMP/$name.output" || true;fi
    fi
    jq -n --arg name "$name" --arg state "$state" --arg path "$resolved" --arg version "$version" '{key:$name,value:{state:$state,resolved_path:(if $path=="" then null else $path end),version:(if $version=="" then null else $version end)}}' >>"$TMP/dependencies.jsonl"
}
probe curl curl --version
probe jq jq --version
probe docker "$DOCKER_BIN" --version
if [[ -n "$COMPOSE_BIN" ]]; then probe compose "$COMPOSE_BIN" version;else probe compose "$DOCKER_BIN" compose version;fi
probe nginx "$NGINX_BIN" -v
probe sqlite3 "$SQLITE_BIN" --version
probe gzip gzip --version
probe sha256sum sha256sum --version
probe flock flock --version
probe timeout timeout --version
probe systemctl "$SYSTEMCTL_BIN" --version
jq -s 'from_entries' "$TMP/dependencies.jsonl" >"$TMP/dependencies.json"
docker_ready=false nginx_ready=false
timeout 8 "$DOCKER_BIN" info >"$TMP/docker-info.log" 2>&1 && docker_ready=true
if [[ -f "$NGINX_MAIN_CONF" ]]; then timeout 8 "$NGINX_BIN" -t -c "$NGINX_MAIN_CONF" >"$TMP/nginx-check.log" 2>&1 && nginx_ready=true;fi
control="$FOUNDATION/deploy/control"
cfg="$control/local.json"
echo null >"$TMP/instance.json"
phase=
if [[ -f "$cfg" && -f "$control/state/journal.json" && -x "$control/bin/matrix-control" ]]; then
    if timeout 22 "$control/bin/matrix-control" status --config "$cfg" >"$TMP/status.json" 2>"$TMP/controller.log" && jq -e --slurpfile cfg "$cfg" --slurpfile journal "$control/state/journal.json" '.instance_id==$cfg[0].instance_id and .cluster_id==$cfg[0].cluster_id and .bound_epoch==$journal[0].bound_epoch' "$TMP/status.json" >/dev/null; then
        jq '{instance_id,cluster_id,bound_epoch,serving,blocked,control,final_backup,handover,deployment_root,reason,error}' "$TMP/status.json" >"$TMP/instance.json"
        phase=$(jq -r 'if .serving then "ONLINE" elif .final_backup.phase=="RETIRED" then "RETIRED" elif (.control.operation!=null and (.control.operation.target==.instance_id or .control.operation.source==.instance_id)) then "IN_PROGRESS" elif .bound_epoch==0 and .blocked and .control!=null then "STANDBY" else "BLOCKED" end' "$TMP/status.json")
    else phase=NEEDS_ATTENTION;fi
elif [[ -e "$FOUNDATION" ]] && ! host_bootstrap_target "$FOUNDATION"; then phase=PARTIAL
fi
if [[ -z "$phase" ]]; then
    if jq -e 'any(.[];.state=="missing")' "$TMP/dependencies.json" >/dev/null;then phase=NEEDS_DEPENDENCIES
    elif jq -e 'any(.[];.state=="broken")' "$TMP/dependencies.json" >/dev/null || [[ "$docker_ready" != true || "$nginx_ready" != true ]];then phase=NEEDS_ATTENTION
    else phase=READY_TO_PREPARE;fi
fi
s3_exists=false s3_private=false
if [[ -f "$S3_CONFIG" ]];then s3_exists=true;[[ $(stat -c %a "$S3_CONFIG") =~ ^[046]00$ ]] && s3_private=true;fi
: >"$TMP/units.jsonl"
for pair in controller:matrix-control.service handover_timer:matrix-handover.timer backup_timer:tuwunel-restic.timer mailbox_timer:matrix-mailbox.timer nginx:"$NGINX_SERVICE";do
    key=${pair%%:*};unit=${pair#*:};active=false
    timeout 5 "$SYSTEMCTL_BIN" is-active --quiet "$unit" >/dev/null 2>&1 && active=true
    jq -cn --arg k "$key" --arg unit "$unit" --argjson active "$active" '{key:$k,value:{unit:$unit,active:$active}}' >>"$TMP/units.jsonl"
done
jq -s 'from_entries' "$TMP/units.jsonl" >"$TMP/units.json"
jq -n --slurpfile registry "$REGISTRY" --slurpfile deps "$TMP/dependencies.json" --slurpfile instance "$TMP/instance.json" --slurpfile units "$TMP/units.json" --arg phase "$phase" --arg dir "$STATE_DIR" --arg actual "$actual_host" --arg nginx "$NGINX_MAIN_CONF" --argjson docker "$docker_ready" --argjson nginx_ready "$nginx_ready" --argjson s3_exists "$s3_exists" --argjson s3_private "$s3_private" '{schema:1,authority:"observation-only",observed_at:(now|todateiso8601),phase:$phase,paths:($registry[0]|del(.schema)|.+{agent_dir:$dir,actual_host_config:$actual,nginx_main_conf:$nginx}),dependencies:$deps[0],checks:{docker_daemon:{ready:$docker},nginx_config:{ready:$nginx_ready},units:$units[0],foundation_ready:($instance[0]!=null and $docker and $nginx_ready and $units[0].controller.active and $units[0].handover_timer.active and $units[0].mailbox_timer.active and $units[0].nginx.active),s3_config:{exists:$s3_exists,private:$s3_private,credentials_tested:false}},instance:$instance[0]}' >"$STATE_DIR/state.json.tmp"
mv "$STATE_DIR/state.json.tmp" "$STATE_DIR/state.json"
cat "$STATE_DIR/state.json"
