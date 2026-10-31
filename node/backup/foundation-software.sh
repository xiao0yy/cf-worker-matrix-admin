#!/usr/bin/env bash
# Code-only update contract. Never copies identity, credentials, journal or data.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
source "$BASE/local-config.sh"
die() { printf 'Foundation update: %s\n' "$*" >&2; exit 1; }
MODE=${1:-}; shift || true
[[ "$MODE" == check || "$MODE" == update ]] || die 'Use check|update CONFIG BUNDLE RELEASE TARGET SNAPSHOT RELEASE_KEY'
[[ $# == 6 ]] || die 'Invalid versioned update arguments'
CONFIG=$1 BUNDLE=$2 RELEASE=$3 TARGET=$4 SNAPSHOT=$5 RELEASE_KEY=$6
[[ -f "$CONFIG" && ! -L "$CONFIG" && $(stat -c %a "$CONFIG") =~ ^[046]00$ ]] || die 'Private local configuration required'
host_defaults; source "$CONFIG"; host_validate
CONTROL="$TARGET/deploy/control"
[[ -f "$CONTROL/managed" && -f "$CONTROL/host.env" ]] || die 'Not an existing managed foundation'
host_load "$CONTROL/host.env"
jq -e --slurpfile cfg "$CONTROL/local.json" '.cluster_id==$cfg[0].cluster_id' "$BUNDLE/payload/control/source-local.json" >/dev/null || die 'Recovery point belongs to a different cluster'
source "$BASE/release-common.sh"
release_verify "$RELEASE" || die 'Public release verification failed'
[[ $(sha256sum "$RELEASE/release-manifest.json" | cut -d' ' -f1) == "$RELEASE_KEY" ]] || die 'Pinned release checksum differs'
[[ $(jq -er .software.manifest_sha256 "$BUNDLE/metadata.json") == "$RELEASE_KEY" ]] || die 'Snapshot references a different tool release'
source "$BASE/payload-verify.sh"
payload_verify "$BUNDLE" partial || die 'Invalid private configuration'
PAYLOAD="$RELEASE/control"
"$PAYLOAD/bin/matrix-control" capabilities | jq -e '.schema==1 and .planned_handover' >/dev/null || die 'Unsupported controller capabilities'
identity=$(sha256sum "$CONTROL/local.json" | cut -d' ' -f1)
state=$(jq -er .state_dir "$CONTROL/local.json")
pending="$TARGET/agent/pending-update.json"
status_file="$BUNDLE/update-status.json"
read_update_status() {
if ! "$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json" >"$status_file"; then
    [[ "$MODE" == update && -f "$pending" ]] || die 'Controller unavailable; no update authorization'
    jq -e --arg s "$SNAPSHOT" --arg r "$RELEASE_KEY" --arg i "$identity" '.schema==1 and .snapshot_id==$s and .release==$r and .identity_sha256==$i and .offline==true' "$pending" >/dev/null || die 'Resume requires the same immutable recovery point and identity'
    jq -e --slurpfile p "$pending" '.blocked and .bound_epoch==$p[0].bound_epoch and (.deployment_root // null)==$p[0].deployment_root and (.handover.phase // null)==$p[0].handover_phase and (.handover.may_have_written // false)==$p[0].may_have_written and (.handover.phase!="STARTING" and .handover.phase!="COMMITTING")' "$state/journal.json" >/dev/null || die 'Local journal no longer permits offline update'
    jq '{blocked,serving:false,bound_epoch,deployment_root,handover,control:{phase:"UPDATE_RESUME"}}' "$state/journal.json" >"$status_file"
fi
if [[ -f "$pending" ]]; then
    jq -e --slurpfile p "$pending" '.bound_epoch==$p[0].bound_epoch and (.deployment_root // null)==$p[0].deployment_root and (.handover.phase // null)==$p[0].handover_phase and (.handover.may_have_written // false)==$p[0].may_have_written and (if $p[0].offline then .blocked and (.serving|not) else true end)' "$status_file" >/dev/null || die 'Instance state changed since the interrupted update; preserve pending intent for inspection'
fi
}
status_guard() {
    jq -Sc '{bound_epoch,deployment_root:(.deployment_root // null),blocked,serving,handover_phase:(.handover.phase // null),may_have_written:(.handover.may_have_written // false),control_phase:.control.phase,owner:(.control.owner // null),operation:(.control.operation.id // null),final_backup:(.final_backup // null)}' "$status_file"
}
read_update_status
initial_guard=$(status_guard)
active=$(jq -r '.deployment_root // empty' "$status_file")
if [[ -n "$active" ]]; then
    [[ "$active" == "$TARGET/generations/"* && $(realpath -e "$active") == "$active" ]] || die 'Invalid active generation'
fi
matches=true
"$RELEASE/backup/bin/matrix-release" verify --manifest "$RELEASE/release-manifest.json" --root "$TARGET/deploy" >/dev/null 2>&1 || matches=false
if [[ -n "$active" ]]; then
    "$RELEASE/backup/bin/matrix-release" verify --manifest "$RELEASE/release-manifest.json" --root "$active/deploy" >/dev/null 2>&1 || matches=false
fi
if [[ "$MODE" == check ]]; then
    jq -n --argjson ok "$matches" --arg s "$SNAPSHOT" --arg r "$RELEASE_KEY" '{schema:1,software_matches:$ok,snapshot_id:$s,release:$r,services_changed:false}'
    [[ "$matches" == true ]] || die 'UPDATE_REQUIRED: use the public release reader --update-foundation with this exact --point (before STARTING)'
    exit 0
fi
mkdir -p "$TARGET/agent/updates"
exec 5>"$state/handover-worker.lock"; flock -n 5 || die 'Handover worker busy; retry later'
exec 6>"$state/mailbox.lock"; flock -n 6 || die 'Management poller busy; retry later'
# Serialize with the producer of the active generation, without exposing config.
backup_root=${active:-$TARGET}/deploy/backup
if [[ -f "$backup_root/restic.env" ]]; then
    backup_state=$( (STATE_DIR=/opt/matrix/backup-state/tuwunel-restic; source "$backup_root/restic.env"; printf '%s' "$STATE_DIR") )
    [[ "$backup_state" == /* && -d "$backup_state" ]] || die 'Invalid backup state directory'
    exec 7>"$backup_state/operation.lock"; flock -n 7 || die 'Backup producer busy; retry later'
fi
# The selected generation and backup lock must still describe the same instance.
# Retry from the beginning if anything changed while acquiring worker locks.
read_update_status
[[ $(status_guard) == "$initial_guard" ]] || die 'Instance state changed while acquiring update locks; retry after inspecting the operation'
offline=false
if jq -e '.blocked and (.serving|not) and ((.control.phase=="ONLINE" and (.final_backup==null or .final_backup.phase=="RETIRED")) or (((.handover.may_have_written // false)|not) and ( .control.phase=="RELEASED" or .control.phase=="RESTORING" or .control.phase=="VALIDATING" or .control.phase=="UPDATE_RESUME")))' "$status_file" >/dev/null; then
    offline=true
else
    jq -e '.serving and (.blocked|not) and .control.phase=="ONLINE" and .control.owner==.instance_id' "$status_file" >/dev/null || die 'Update refused during this operation; STARTING data must never be replaced'
    cmp -s "$CONTROL/bin/matrix-control" "$PAYLOAD/bin/matrix-control" || die 'Controller binary update requires a fenced standby; active controller will not be restarted'
    rendered=$(cat "$PAYLOAD/matrix-control.service"); rendered=${rendered//\/opt\/matrix/$TARGET}
    cmp -s <(printf '%s\n' "$rendered") "$SYSTEMD_UNIT_DIR/matrix-control.service" || die 'Controller unit update requires a fenced standby'
fi
if [[ -f "$pending" ]]; then
    jq -e --arg s "$SNAPSHOT" --arg r "$RELEASE_KEY" --arg i "$identity" '.snapshot_id==$s and .release==$r and .identity_sha256==$i' "$pending" >/dev/null || die 'Different update in progress; resume its pinned snapshot'
fi
# Persist intent before touching installed code. Re-running the same point repairs
# partial file replacement. Never roll back identity/journal or a data generation.
jq -n --slurpfile status "$status_file" --arg s "$SNAPSHOT" --arg r "$RELEASE_KEY" --arg i "$identity" --argjson o "$offline" '{schema:1,snapshot_id:$s,release:$r,identity_sha256:$i,offline:$o,bound_epoch:$status[0].bound_epoch,deployment_root:($status[0].deployment_root // null),handover_phase:($status[0].handover.phase // null),may_have_written:($status[0].handover.may_have_written // false),started_at:(now|todateiso8601)}' >"$pending.tmp"
mv "$pending.tmp" "$pending"
sync -f "$pending"
if [[ "$offline" == true ]]; then systemctl_cmd stop matrix-control.service; fi
# Only public inventory members are replaced. Config/identity/journal are never
# restored by an update. Partial replacement is repairable using pinned intent.
release_copy "$RELEASE" "$TARGET/deploy"
if [[ -n "$active" ]]; then release_copy "$RELEASE" "$active/deploy"; fi
for name in matrix-control.service matrix-handover.service matrix-handover.timer matrix-mailbox.service matrix-mailbox.timer; do
    text=$(cat "$CONTROL/$name"); text=${text//\/opt\/matrix/$TARGET}
    printf '%s\n' "$text" >"$SYSTEMD_UNIT_DIR/$name"
done
# A standby has no backup credentials/data yet. On an existing deployment,
# rewrite the producer unit against its own active generation, never the source.
if [[ -f "$backup_root/restic.env" ]]; then
    app_root=${active:-$TARGET}
    for name in tuwunel-restic.service tuwunel-restic.timer; do
        [[ -f "$RELEASE/backup/$name" ]] || die 'Incomplete backup service templates'
        text=$(cat "$RELEASE/backup/$name"); text=${text//\/opt\/matrix/$app_root}
        printf '%s\n' "$text" >"$SYSTEMD_UNIT_DIR/$name"
    done
fi
systemctl_cmd daemon-reload
if [[ "$offline" == true ]]; then systemctl_cmd start matrix-control.service; fi
"$CONTROL/bin/matrix-control" status --config "$CONTROL/local.json" >"$BUNDLE/after-update.json"
[[ $(sha256sum "$CONTROL/local.json" | cut -d' ' -f1) == "$identity" ]] || die 'Instance identity changed'
if [[ "$offline" == true ]]; then jq -e '.blocked and (.serving|not)' "$BUNDLE/after-update.json" >/dev/null || die 'Unexpected run authorization'; fi
release_verify "$TARGET/deploy" || die 'Installed release verification failed'
if [[ -n "$active" ]]; then release_verify "$active/deploy" || die 'Installed generation verification failed'; fi
jq --arg h "$(sha256sum "$PAYLOAD/software.sha256" | cut -d' ' -f1)" '.+{completed_at:(now|todateiso8601),control_sha256:$h}' "$pending" >"$TARGET/agent/software.json.tmp"
mv "$TARGET/agent/software.json.tmp" "$TARGET/agent/software.json"
sync -f "$TARGET/agent/software.json"
cp "$TARGET/agent/software.json" "$TARGET/agent/updates/$SNAPSHOT.json"
rm "$pending"
systemctl_cmd enable --now matrix-handover.timer matrix-mailbox.timer
cat "$TARGET/agent/software.json"
