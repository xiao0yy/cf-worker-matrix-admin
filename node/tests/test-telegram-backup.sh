#!/usr/bin/env bash
# Exercise real SQLite WAL backup, corruption detection and session retention.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")/../backup" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf -- "$WORK"' EXIT
mkdir -p "$WORK/project/data/tmp"
printf 'local test config\n' >"$WORK/project/data/config.yaml"
printf 'local test registration\n' >"$WORK/project/data/registration.yaml"
printf 'services: {}\n' >"$WORK/project/docker-compose.yml"
DB="$WORK/project/data/telegram.db"
# A live connection keeps committed rows in WAL while the helper backs up.
mkfifo "$WORK/sql-input"
sqlite3 -batch -bail "$DB" <"$WORK/sql-input" >"$WORK/sql-output" 2>&1 &
SQL_PID=$!
trap 'exec 8>&-; kill "$SQL_PID" 2>/dev/null || true; wait "$SQL_PID" 2>/dev/null || true; rm -rf -- "$WORK"' EXIT
exec 8>"$WORK/sql-input"
cat >&8 <<SQL
PRAGMA journal_mode=WAL;
PRAGMA wal_autocheckpoint=0;
CREATE TABLE user_login(id INTEGER PRIMARY KEY,secret TEXT);
CREATE TABLE crypto_account(secret TEXT);
CREATE TABLE crypto_olm_session(secret TEXT);
CREATE TABLE crypto_megolm_inbound_session(secret TEXT);
CREATE TABLE crypto_megolm_outbound_session(secret TEXT);
CREATE TABLE portal(id INTEGER);
CREATE TABLE message(id INTEGER);
INSERT INTO user_login VALUES(1,'test-session');
INSERT INTO crypto_account VALUES('test-key');
.shell touch '$WORK/ready'
SQL
for ((i=0;i<100;i++)); do [[ -f "$WORK/ready" ]] && break; sleep 0.02; done
[[ -f "$WORK/ready" && -s "$DB-wal" ]]
export TEST_SOURCE="$WORK/project/data"
cat >"$WORK/docker" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
 inspect)
  case "$3" in
   *Mounts*) echo "$TEST_SOURCE" ;;
   *NetworkSettings*) echo '{"test-network":{"IPAddress":"172.20.0.3"}}' ;;
   '{{json .State}}') echo '{"Running":false,"Restarting":false,"ExitCode":0,"OOMKilled":false,"Pid":0}' ;;
   *Image*) printf 'sha256:%064d\n' 1 ;;
   *) echo "test-container ${TEST_RUNNING:-true} 2026-10-25T00:00:00Z" ;;
  esac ;;
 exec) exit 0 ;;
 network) echo '[{"Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.20.0.0/16","Gateway":"172.20.0.1"}]}}]' ;;
 *) exit 1 ;;
esac
MOCK
chmod 700 "$WORK/docker"
DOCKER_BIN="$WORK/docker" bash "$BASE/telegram-backup.sh" snapshot "$WORK/project" bridge "$WORK/backup"
bash "$BASE/telegram-backup.sh" verify "$WORK/backup/data/telegram.db" "$WORK/backup/metadata.json"
[[ $(sqlite3 -readonly "$WORK/backup/data/telegram.db" 'SELECT secret FROM user_login') == test-session ]]
[[ $(sqlite3 -readonly "$WORK/backup/data/telegram.db" 'SELECT secret FROM crypto_account') == test-key ]]
[[ ! -e "$WORK/backup/data/telegram.db-wal" && ! -e "$WORK/backup/data/telegram.db-shm" ]]
echo 'PASS: committed WAL session/key data captured in a standalone database'
[[ $(sqlite3 -readonly "$DB" 'PRAGMA journal_mode') == wal ]]
echo 'PASS: source stays in WAL mode and its connection remains running'
sqlite3 "$WORK/backup/data/telegram.db" 'DELETE FROM user_login;'
if bash "$BASE/telegram-backup.sh" verify "$WORK/backup/data/telegram.db" "$WORK/backup/metadata.json" >"$WORK/altered.log" 2>&1; then
    echo 'FAIL: state loss accepted'; exit 1
fi
echo 'PASS: lost login state rejected'
printf 'unexpected durable state' >"$WORK/project/data/new-session"
if DOCKER_BIN="$WORK/docker" bash "$BASE/telegram-backup.sh" snapshot "$WORK/project" bridge "$WORK/second" >"$WORK/unknown.log" 2>&1; then
    echo 'FAIL: unclassified persistent data skipped'; exit 1
fi
echo 'PASS: unclassified persistent state rejected instead of silently omitted'

rm "$WORK/project/data/new-session"
exec 8>&-
wait "$SQL_PID"
TEST_RUNNING=false DOCKER_BIN="$WORK/docker" bash "$BASE/telegram-backup.sh" cold-snapshot "$WORK/project" bridge "$WORK/cold"
bash "$BASE/telegram-backup.sh" verify "$WORK/cold/data/telegram.db" "$WORK/cold/metadata.json"
[[ $(sqlite3 -readonly "$WORK/cold/data/telegram.db" 'SELECT secret FROM user_login') == test-session ]]
jq -e '.method=="sqlite-cold-backup"' "$WORK/cold/metadata.json" >/dev/null
echo 'PASS: cleanly stopped bridge retains session/key data via native SQLite backup'
if TEST_RUNNING=true DOCKER_BIN="$WORK/docker" bash "$BASE/telegram-backup.sh" cold-snapshot "$WORK/project" bridge "$WORK/running-cold" >"$WORK/running-cold.log" 2>&1; then
    echo 'FAIL: cold snapshot accepted a running writer'; exit 1
fi
echo 'PASS: cold SQLite snapshot rejects running source'
