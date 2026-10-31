#!/usr/bin/env bash
# Real local restic/SQLite, fixture Docker/control/maintenance endpoints; no S3.
set -Eeuo pipefail
umask 077
SOURCE=$(cd -- "$(dirname -- "$0")/../backup" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/backup" "$WORK/control/bin" "$WORK/state" "$WORK/db" "$WORK/project/appservices" "$WORK/project/s3" "$WORK/telegram/data" "$WORK/nginx" "$WORK/mock"
node --input-type=module - "$WORK" <<'JS'
import {release} from './node/tests/fixtures.mjs';
release(process.argv[2]);
JS
echo MANIFEST-1 >"$WORK/db/CURRENT"; echo fixture >"$WORK/db/MANIFEST-1"
echo 'services: {}' >"$WORK/project/docker-compose.yml"
echo token >"$WORK/project/registration-token"; echo media >"$WORK/project/s3/media.env"
echo 'services: {}' >"$WORK/telegram/docker-compose.yml"
echo config >"$WORK/telegram/data/config.yaml"; echo registration >"$WORK/telegram/data/registration.yaml"
for name in matrix.conf 00-matrix-common.conf; do echo fixture >"$WORK/nginx/$name"; done
sqlite3 "$WORK/telegram/data/telegram.db" <<'SQL'
CREATE TABLE user_login(id INTEGER);
CREATE TABLE crypto_account(id INTEGER);
CREATE TABLE crypto_olm_session(id INTEGER);
CREATE TABLE crypto_megolm_inbound_session(id INTEGER);
CREATE TABLE crypto_megolm_outbound_session(id INTEGER);
CREATE TABLE portal(id INTEGER);
CREATE TABLE message(id INTEGER);
INSERT INTO user_login VALUES(1);
INSERT INTO crypto_account VALUES(1);
SQL
cat >"$WORK/control/bin/matrix-control" <<'MOCK'
#!/usr/bin/env bash
[[ "$1" != final-verify ]] || exit 0
[[ "$1" == status ]] || exit 1
if [[ -f "$TEST_ROOT/daily-mode" ]];then
 echo '{"instance_id":"fixture","cluster_id":"test","bound_epoch":1,"serving":true,"blocked":false}'
 exit
fi
echo '{"instance_id":"fixture","cluster_id":"test","bound_epoch":1,"blocked":true,"serving":false,"control":{"phase":"SNAPSHOTTING","owner":"fixture","operation":{"id":"op-fixture"}},"final_backup":{"id":"op-fixture","phase":"SNAPSHOTTING","source_epoch":1,"cutoff_time":"2026-10-25T00:00:00Z","stopped_proof":[{},{}]}}'
MOCK
cat >"$WORK/mock/docker" <<'MOCK'
#!/usr/bin/env bash
set -e
echo "$1" >>"$TEST_ROOT/docker-calls"
case "$1" in
 inspect)
  case "$3" in
   '{{.Id}}') echo fixture ;;
   '{{json .}}') printf '{"Image":"sha256:%064d","Config":{"Image":"registry.example/matrix@sha256:%064d"}}\n' 1 1 ;;
   '{{json .State}}') echo '{"Running":false,"Restarting":false,"ExitCode":0,"OOMKilled":false,"Pid":0}' ;;
   *Mounts*) echo "$TEST_ROOT/telegram/data" ;;
   *NetworkSettings*) echo '{"test-network":{"IPAddress":"","IPAMConfig":{"IPv4Address":"172.20.0.3"}}}' ;;
   *Config.Env*) echo '["TUWUNEL_SERVER_NAME=matrix.example","TUWUNEL_STORAGE_PROVIDER__MEDIA_S3__S3__BASE_PATH=media"]' ;;
   *Image*) printf 'sha256:%064d\n' 1 ;;
   *) echo 'fixture false 2026-10-25T00:00:00Z' ;;
  esac ;;
 image)
  if [[ "$3" == --format ]];then printf 'sha256:%064d\n' 1;else printf '[{"RepoDigests":["registry.example/matrix@sha256:%064d"]}]\n' 1;fi ;;
 network) echo '[{"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.20.0.0/16","Gateway":"172.20.0.1"}]}}]' ;;
 tag) exit 0 ;;
 *) exit 99 ;;
esac
MOCK
cat >"$WORK/mock/restic" <<'MOCK'
#!/usr/bin/env bash
unset RESTIC_REPOSITORY
exec "$RESTIC_TEST_BINARY" --repo "$TEST_ROOT/repo" "$@"
MOCK
cat >"$WORK/backup/restore-tuwunel.sh" <<'MOCK'
#!/usr/bin/env bash
echo maintenance >"$TEST_ROOT/maintenance-called"
MOCK
cat >"$WORK/backup/publish-recovery.sh" <<'MOCK'
#!/usr/bin/env bash
set -e
if [[ "$RECOVERY_COMMIT_MODE" != final ]];then
 jq -e '.verification=="database-tree" and .data_layout=="database-only-v1"' "$RECOVERY_POINT_FILE" >/dev/null
 exit
fi
cp "$RECOVERY_CONFIG_FILE" "$TEST_ROOT/recorded-configuration.json"
jq -e '.consistency=="globally-quiesced" and .verification=="full-restore" and .database_startup_verified' "$RECOVERY_POINT_FILE" >/dev/null
echo '{"operation_id":"op-fixture"}' >"$TEST_ROOT/state/final-published-op-fixture.json"
MOCK
chmod 700 "$WORK/control/bin/matrix-control" "$WORK/mock/"* "$WORK/backup/"*.sh
node --input-type=module - "$WORK" <<'JS'
import {sealRelease} from './node/tests/fixtures.mjs';
sealRelease(process.argv[2]);
JS
touch "$WORK/control/managed"
echo private-fixture >"$WORK/control/s3.env"
echo '{"cluster_id":"test","instance_id":"fixture"}' >"$WORK/control/local.json"
export TEST_ROOT="$WORK" PATH="$WORK/mock:$PATH" RESTIC_PASSWORD=local-test-only RESTIC_CACHE_DIR="$WORK/cache"
"$WORK/mock/restic" init >"$WORK/init.log"
printf 'STATE_DIR=%q\nDATA_DIR=%q\nPROJECT_DIR=%q\nTELEGRAM_PROJECT_DIR=%q\nRESTIC_BIN=%q\nDOCKER_BIN=%q\nCONTROL_DIR=%q\nNGINX_HTTP_CONF_DIR=%q\nRESTIC_PASSWORD=local-test-only\nRESTIC_REPOSITORY=s3:https://test.invalid/test/repo\nAWS_ACCESS_KEY_ID=test\nAWS_SECRET_ACCESS_KEY=test\n' \
 "$WORK/state" "$WORK/db" "$WORK/project" "$WORK/telegram" "$WORK/mock/restic" "$WORK/mock/docker" "$WORK/control" "$WORK/nginx" >"$WORK/config.env"
echo '{"sentinel":"daily"}' >"$WORK/state/last-success.json"
MATRIX_BACKUP_CONFIG="$WORK/config.env" bash "$WORK/backup/tuwunel-restic.sh" cold-run op-fixture >"$WORK/first.log" 2>&1 || { cat "$WORK/first.log"; exit 1; }
[[ -e "$WORK/maintenance-called" && -e "$WORK/state/finals/op-fixture/published.json" ]]
jq -e '.sentinel=="daily"' "$WORK/state/last-success.json" >/dev/null
! grep -Eq '^kill$|^exec$' "$WORK/docker-calls"
count=$("$WORK/mock/restic" snapshots --json | jq length)
MATRIX_BACKUP_CONFIG="$WORK/config.env" bash "$WORK/backup/tuwunel-restic.sh" cold-run op-fixture >"$WORK/second.log" 2>&1
[[ $("$WORK/mock/restic" snapshots --json | jq length) == "$count" && "$count" == 1 ]]
snapshot=$("$WORK/mock/restic" snapshots --json | jq -er '.[0].id')
"$WORK/mock/restic" restore "$snapshot" --target "$WORK/inspect" >/dev/null
[[ ! -e "$WORK/inspect/bundle/payload/recovery" && ! -e "$WORK/inspect/bundle/payload/control/bin" && ! -e "$WORK/inspect/bundle/payload/database/media" ]]
[[ ! -e "$WORK/inspect/bundle/metadata.json" && ! -e "$WORK/inspect/bundle/manifest.sha256" && ! -e "$WORK/inspect/bundle/payload/config" && ! -e "$WORK/inspect/bundle/payload/control" ]]
jq -e '.metadata.recovery_version==2 and .metadata.data_layout=="database-only-v1" and .metadata.software.project=="matrix-admin" and .metadata.images.tuwunel.ref!=null and ([.files[].path]|all(startswith("database/")|not)) and ([.files[].path]|index("control/s3.env")==null)' "$WORK/recorded-configuration.json" >/dev/null
echo 'PASS: real restic payload has databases only; R2 capsule has configuration only, public version and image digest pinned'
echo 'PASS: cold producer verifies a real local restic/SQLite restore without invoking online checkpoint'
echo 'PASS: cold producer keeps daily success untouched and reuses a completed final snapshot'

# Exercise the lighter daily verification against the same real restic repository.
# Reconstitute a staged bundle from the independently stored configuration/data.
touch "$WORK/daily-mode"
mkdir -p "$WORK/state/pending"
"$MATRIX_RELEASE_BINARY" config-unpack --input "$WORK/recorded-configuration.json" --bundle "$WORK/state/pending/bundle"
"$MATRIX_RELEASE_BINARY" data-merge --input "$WORK/inspect" --bundle "$WORK/state/pending/bundle"
cp "$WORK/recorded-configuration.json" "$WORK/state/pending/configuration.json"
echo FULL_VERIFY=daily >>"$WORK/config.env"
MATRIX_BACKUP_CONFIG="$WORK/config.env" bash "$WORK/backup/tuwunel-restic.sh" upload >"$WORK/daily.log" 2>&1 || { cat "$WORK/daily.log";exit 1; }
jq -e '.verification=="database-tree" and .data_layout=="database-only-v1"' "$WORK/state/last-success.json" >/dev/null
[[ ! -d "$WORK/state/pending" ]]
echo 'PASS: daily database-only file-tree verification works with real restic'
