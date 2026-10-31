#!/usr/bin/env bash
# Also captured in backup: fresh standbys install the same polling contract.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
[[ ${1:-} == --install && $# == 1 && $EUID == 0 ]] || { echo 'Use --install as root';exit 1; }
source "$BASE/../backup/local-config.sh"
host_defaults
host_load "$BASE/host.env"
root=$(dirname "$(dirname "$BASE")")
[[ "$root" == /* && "$root" != *[!a-zA-Z0-9_./-]* && -x "$BASE/bin/matrix-mailbox" ]] || exit 1
for name in matrix-mailbox.service matrix-mailbox.timer; do
    value=$(cat "$BASE/$name");value=${value//\/opt\/matrix/$root}
    printf '%s\n' "$value" >"$SYSTEMD_UNIT_DIR/$name"
done
systemctl_cmd daemon-reload
systemctl_cmd enable --now matrix-mailbox.timer
