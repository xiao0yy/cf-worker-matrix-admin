#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")/../backup" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
ROOT="$WORK/foundation"
mkdir -p "$ROOT/deploy/control/bin" "$ROOT/deploy/control/state" "$WORK/mock" "$WORK/units"
echo sensitive-credential-sentinel >"$WORK/s3.env"
echo nginx-fixture >"$WORK/nginx.conf"
printf 'LOCAL_CONFIG_SCHEMA=1\nDOCKER_BIN=%q\nNGINX_BIN=%q\nSYSTEMCTL_BIN=%q\nNGINX_MAIN_CONF=%q\nNGINX_HTTP_CONF_DIR=%q\nSYSTEMD_UNIT_DIR=%q\nWORK_ROOT=%q\nLOCK_FILE=%q\n' "$WORK/mock/docker" "$WORK/mock/nginx" "$WORK/mock/systemctl" "$WORK/nginx.conf" "$WORK" "$WORK/units" "$WORK" "$WORK/recovery.lock" >"$ROOT/deploy/control/host.env"
echo '{"schema":1,"instance_id":"local-node","cluster_id":"fixture"}' >"$ROOT/deploy/control/local.json"
echo '{"bound_epoch":0}' >"$ROOT/deploy/control/state/journal.json"
touch "$ROOT/deploy/control/managed"
echo '{"instance_id":"local-node","cluster_id":"fixture","bound_epoch":0,"blocked":true,"serving":false,"control":{"phase":"ONLINE","owner":"other-node","epoch":1,"operation":null}}' >"$WORK/status.json"
cat >"$ROOT/deploy/control/bin/matrix-control" <<'MOCK'
#!/usr/bin/env bash
[[ "$1" == status ]] || exit 99
cat "$TEST_ROOT/status.json"
MOCK
cat >"$WORK/mock/docker" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
 --version) echo 'Docker fixture version' ;;
 compose) echo 'Docker Compose fixture version' ;;
 info) [[ ! -f "$TEST_ROOT/daemon-down" ]] ;;
 *) echo forbidden-operation >>"$TEST_ROOT/trace"; exit 99 ;;
esac
MOCK
cat >"$WORK/mock/nginx" <<'MOCK'
#!/usr/bin/env bash
case "$1" in -v|-t) echo 'nginx fixture' ;; *) exit 99 ;; esac
MOCK
cat >"$WORK/mock/systemctl" <<'MOCK'
#!/usr/bin/env bash
case "$1" in --version) echo 'systemd fixture' ;; is-active) exit 0 ;; *) echo forbidden-operation >>"$TEST_ROOT/trace";exit 99;; esac
MOCK
cat >"$WORK/mock/curl" <<'MOCK'
#!/usr/bin/env bash
[[ "$1" == --version ]] || { echo forbidden-network >>"$TEST_ROOT/trace";exit 99; }
echo 'curl fixture'
MOCK
chmod 700 "$WORK/mock/"* "$ROOT/deploy/control/bin/matrix-control"
export TEST_ROOT="$WORK" PATH="$WORK/mock:$PATH"
args=(--state-dir "$WORK/agent" --foundation "$ROOT" --config "$WORK/s3.env" --local "$ROOT/deploy/control/host.env")
bash "$BASE/inspect-host.sh" "${args[@]}" >"$WORK/first.json"
jq -e --arg docker "$WORK/mock/docker" '.schema==1 and .authority=="observation-only" and .phase=="STANDBY" and .dependencies.docker.resolved_path==$docker and .checks.foundation_ready' "$WORK/first.json" >/dev/null
registry_hash=$(sha256sum "$WORK/agent/registry.json")
echo '{"phase":"ONLINE"}' >"$WORK/agent/state.json"
bash "$BASE/inspect-host.sh" --state-dir "$WORK/agent" >"$WORK/repeated.json"
jq -e '.phase=="STANDBY"' "$WORK/repeated.json" >/dev/null
[[ $(sha256sum "$WORK/agent/registry.json") == "$registry_hash" ]]
! grep -Eq sensitive-credential-sentinel "$WORK/agent/state.json" "$WORK/agent/registry.json"
echo 'PASS: local state rediscovers installed paths, preserves registry, ignores stale observations, and contains no credentials'
touch "$WORK/daemon-down"
bash "$BASE/inspect-host.sh" --state-dir "$WORK/agent" >"$WORK/down.json"
jq -e '.dependencies.docker.state=="present" and (.checks.docker_daemon.ready|not) and (.checks.foundation_ready|not)' "$WORK/down.json" >/dev/null
rm "$WORK/daemon-down"
echo 'PASS: an installed Docker with failed daemon is recorded for repair, not classified as missing'
if bash "$BASE/inspect-host.sh" --state-dir "$WORK/agent" --foundation "$WORK/other" >"$WORK/wrong-path.log" 2>&1;then exit 1;fi
[[ $(sha256sum "$WORK/agent/registry.json") == "$registry_hash" ]]
echo 'PASS: conflicting registered paths rejected without replacing the local registry'
before=$(sha256sum "$ROOT/deploy/control/local.json" "$ROOT/deploy/control/state/journal.json")
bash "$BASE/recover.sh" --config "$WORK/s3.env" --prepare-standby --target "$ROOT" >"$WORK/reused.json"
jq -e '.already_prepared and .instance_id=="local-node" and (.assets_downloaded|not) and (.identity_recreated|not) and (.services_changed|not)' "$WORK/reused.json" >/dev/null
[[ $(sha256sum "$ROOT/deploy/control/local.json" "$ROOT/deploy/control/state/journal.json") == "$before" ]]
[[ ! -e "$WORK/trace" ]]
echo 'PASS: repeat standby preparation returns the existing identity without network, service changes, or new data'
mkdir "$WORK/partial"
if bash "$BASE/recover.sh" --config "$WORK/s3.env" --prepare-standby --target "$WORK/partial" --local "$ROOT/deploy/control/host.env" >"$WORK/partial.log" 2>&1;then exit 1;fi
grep -Eq 'incomplete' "$WORK/partial.log"
[[ -d "$WORK/partial" && ! -e "$WORK/trace" ]]
echo 'PASS: incomplete foundation is preserved for repair instead of resetting it'

new="$WORK/new-node";mkdir -p "$new/config" "$new/generations";cp "$WORK/s3.env" "$new/config/s3.env"
bash "$BASE/inspect-host.sh" --foundation "$new" --local "$ROOT/deploy/control/host.env" >"$WORK/new-node.json"
jq -e --arg root "$new" '.phase=="READY_TO_PREPARE" and .paths.agent_dir==($root+"/agent") and .paths.s3_config==($root+"/config/s3.env")' "$WORK/new-node.json" >/dev/null
[[ ! -e "$WORK/trace" ]]
echo 'PASS: fresh canonical config/agent envelope is ready to prepare, not misclassified as a partial deployment'
