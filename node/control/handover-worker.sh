#!/usr/bin/env bash
# Root worker; controller polls independently and owns every authorization CAS.
set -Eeuo pipefail
umask 077
BASE=$(cd -- "$(dirname -- "$0")" && pwd)
CONFIG=${HANDOVER_CONFIG:-$BASE/handover.env}
[[ -f "$CONFIG" && $(stat -c %a "$CONFIG") == 600 ]] || { echo 'Agent must prepare private handover.env' >&2; exit 1; }
source "$BASE/../backup/local-config.sh"
host_defaults
source "$CONFIG"
[[ -n ${FOUNDATION:-} && -n ${S3_CONFIG:-} && -n ${LOCAL_CONFIG:-} ]] || exit 1
host_load "$LOCAL_CONFIG"
[[ $(realpath -e "$FOUNDATION/deploy/control") == "$BASE" ]] || exit 1
CTL="$BASE/bin/matrix-control"
if [[ ${1:-} == --install ]]; then
    for name in matrix-handover.service matrix-handover.timer; do
        text=$(cat "$BASE/$name"); text=${text//\/opt\/matrix/$FOUNDATION}
        printf '%s\n' "$text" >"$SYSTEMD_UNIT_DIR/$name"
    done
    systemctl_cmd daemon-reload
    systemctl_cmd enable --now matrix-handover.timer
    exit 0
fi
[[ $# == 0 ]] || exit 1
state=$(jq -er .state_dir "$BASE/local.json")
exec 6>"$state/handover-worker.lock"
flock -n 6 || exit 0
status() { "$CTL" status --config "$BASE/local.json"; }
status >"$state/worker-status.json"
S="$state/worker-status.json"
me=$(jq -er .instance_id "$S")
phase=$(jq -r '.control.phase // "UNKNOWN"' "$S")
op=$(jq -r '.control.operation.id // .handover.id // .final_backup.id // empty' "$S")
mode=$(jq -r '.control.operation.mode // empty' "$S")
if jq -e '.control.phase=="ONLINE" and .final_backup.mode=="planned" and .final_backup.phase!="RETIRED" and .control.last_completed.id==.final_backup.id' "$S" >/dev/null; then
    mode=planned
    op=$(jq -er .final_backup.id "$S")
fi
if [[ "$mode" != planned ]]; then
    jq -e '.control.phase=="ONLINE" and .control.owner==.instance_id and .control.epoch==.handover.next_epoch and .control.last_completed.id==.handover.id and (.handover.phase=="COMMITTING" or .handover.phase=="ONLINE")' "$S" >/dev/null || exit 0
fi
[[ "$op" =~ ^[a-zA-Z0-9_-]{1,100}$ ]] || exit 1
epoch=$(jq -er '.control.operation.source_epoch // .final_backup.source_epoch // .handover.source_epoch' "$S")
dir="$state/handover/$op"
mkdir -p "$dir"
step=inspect
trap 'code=$?; if ((code)); then jq -n --arg op "$op" --arg step "$step" --argjson code "$code" '\''{operation_id:$op,step:$step,exit_code:$code,at:(now|todateiso8601),error:"Worker stopped safely; retry the same operation. After STARTING, never restore over this generation."}'\'' >"$dir/error.json.tmp"; mv "$dir/error.json.tmp" "$dir/error.json"; fi' EXIT
ctl() { "$CTL" "$1" --config "$BASE/local.json" --operation-id "$op" --expected-instance "$me" --expected-epoch "$epoch"; }
source_id=$(jq -r '.control.operation.source // .final_backup.source // empty' "$S")
target_id=$(jq -r '.control.operation.target // .handover.target // empty' "$S")
if [[ "$source_id" == "$me" ]]; then
    case "$phase" in REQUESTED|QUIESCING|QUIESCED|SNAPSHOTTING)
        active=$(jq -r '.deployment_root // empty' "$S")
        [[ -n "$active" ]] || active=$FOUNDATION
        step=final-backup
        CONTROL_DIR="$BASE" MATRIX_BACKUP_CONFIG="$active/deploy/backup/restic.env" bash "$active/deploy/backup/final-backup.sh" --operation-id "$op" --expected-instance "$me" --expected-epoch "$epoch" >"$dir/final-worker.log" 2>&1
        step=release
        ctl handover-release
        rm -f "$dir/error.json"
        ;;
        RELEASED|RESTORING|VALIDATING|STARTING|ONLINE) step=reconcile-release; ctl handover-release ;;
    esac
    exit 0
fi
[[ "$target_id" == "$me" ]] || exit 0
case "$phase" in
    RELEASED)
        # An old prepared host must match the final source software before it
        # obtains the next epoch. Agent can update this fenced instance using
        # the same pinned point; retries keep the operation and identity.
        step=software-version-check
        snapshot=$(jq -er '.control.operation.recovery_set.snapshot_id' "$S")
        hash=$(jq -er '.control.operation.recovery_set.point_sha256' "$S")
        if [[ ! -f "$dir/software-checked.json" ]]; then
            bash "$BASE/../backup/recover.sh" --config "$S3_CONFIG" --local "$LOCAL_CONFIG" --target "$FOUNDATION" --point "$snapshot" --point-sha256 "$hash" --check-foundation --result-file "$dir/software-checked.json" >"$dir/software-check.log" 2>&1
        fi
        jq -e --arg s "$snapshot" --arg h "$hash" '.snapshot_id==$s and .point_sha256==$h and .software_matches' "$dir/software-checked.json" >/dev/null
        step=claim; ctl handover-claim ;;

    RESTORING|VALIDATING)
        [[ $(jq -r '.handover.may_have_written' "$S") == false ]] || { echo 'Refusing to restore a generation that may have written' >&2; exit 1; }
        step=validate-intent
        ctl handover-validating
        status >"$S"
        snapshot=$(jq -er '.control.operation.recovery_set.snapshot_id' "$S")
        hash=$(jq -er '.control.operation.recovery_set.point_sha256' "$S")
        step=download-and-isolated-restore
        if [[ ! -f "$dir/recovery.json" ]]; then
            bash "$BASE/../backup/recover.sh" --config "$S3_CONFIG" --local "$LOCAL_CONFIG" --point "$snapshot" --point-sha256 "$hash" --result-file "$dir/recovery.json" >"$dir/recovery.log" 2>&1
        fi
        jq -e --arg s "$snapshot" --arg h "$hash" '.snapshot_id==$s and .point_sha256==$h and .mode=="isolated" and .database_startup_verified and .telegram_database_startup_verified' "$dir/recovery.json" >/dev/null
        root=$(jq -er .deployment_root "$dir/recovery.json")
        work=$(jq -er .work_dir "$dir/recovery.json")
        [[ "$root" == "$FOUNDATION/generations/"* && "$root" == "$work/rebuilt" && $(realpath -e "$root") == "$root" ]] || exit 1
        cp "$work/point.json" "$dir/point.json"
        next=$(jq -er '.control.operation.next_epoch' "$S")
        step=verify-restored-software
        source "$BASE/../backup/release-common.sh"
        "$root/deploy/backup/bin/matrix-release" verify --root "$FOUNDATION/deploy" --manifest "$root/deploy/release-manifest.json" || { echo 'UPDATE_REQUIRED: restored controller software differs; never start mismatched foundation' >&2; exit 1; }
        step=bind-generation
        # Remove only stopped, previously pinned containers; keep their data.
        jq -c --slurpfile local "$BASE/local.json" '.active_services // $local[0].services | .[]' "$state/journal.json" | while IFS= read -r service; do
            name=$(jq -er .name <<<"$service")
            if docker container inspect "$name" >"$dir/old-container.json" 2>/dev/null; then
                jq -e --argjson s "$service" '.[0] | .State.Running==false and .State.Restarting==false and .Image==$s.image_id and any(.Mounts[];.Source==$s.data_path and .Destination==$s.mount_path)' "$dir/old-container.json" >/dev/null
                docker rm "$name" >/dev/null
            fi
        done
        jq -n --arg root "$root" --arg op "$op" --arg me "$me" --argjson epoch "$next" '{schema:1,deployment_root:$root,operation_id:$op,instance_id:$me,epoch:$epoch}' >"$dir/deployment.json.tmp"
        mv "$dir/deployment.json.tmp" "$dir/deployment.json"
        ctl handover-bind
        step=prepare-stopped-containers
        printf '\nCONTROL_DIR=%q\n' "$BASE" >>"$root/deploy/backup/restic.env"
        for project in tuwunel telegram; do
            file="$root/deploy/$project/docker-compose.yml"
            jq -e '.services|all(.restart=="no")' "$file" >/dev/null
            compose --project-directory "$root/deploy/$project" -f "$file" create --no-build >"$dir/create-$project.log" 2>&1
        done
        # Foundation guard stays fixed while database generations change.
        old=$(dirname "$(jq -er .s3_config "$root/deploy/control/source-local.json")")
        for name in matrix.conf 00-matrix-common.conf; do
            text=$(cat "$root/deploy/nginx/$name");text=${text//"$old"/"$BASE"}
            printf '%s\n' "$text" >"$root/deploy/nginx/$name"
            if [[ -f "$NGINX_HTTP_CONF_DIR/$name" ]]; then cmp "$root/deploy/nginx/$name" "$NGINX_HTTP_CONF_DIR/$name"; else install -m644 "$root/deploy/nginx/$name" "$NGINX_HTTP_CONF_DIR/$name"; fi
        done
        nginx_cmd -t >"$dir/nginx-check.log" 2>&1
        systemctl_cmd reload "$NGINX_SERVICE"
        for name in tuwunel-restic.service tuwunel-restic.timer; do
            text=$(cat "$root/deploy/backup/$name");text=${text//\/opt\/matrix/$root}
            printf '%s\n' "$text" >"$SYSTEMD_UNIT_DIR/$name"
        done
        systemctl_cmd daemon-reload
        step=start-intent
        ctl handover-start
        ;;
    STARTING)
        # No download, file replacement, container recreation, or automatic
        # unblocking is permitted here, including after a crash or stale R2.
        step=health-and-online
        domain=$(jq -er .server_name "$dir/point.json")
        curl --noproxy '*' --fail --silent --show-error http://127.0.0.1:8008/_matrix/key/v2/server | jq -e --arg d "$domain" '.server_name==$d' >/dev/null
        ctl handover-online
        ;;
    ONLINE)
        step=reconcile-online-ack
        if [[ $(jq -r .handover.phase "$S") == COMMITTING ]]; then ctl handover-online; fi
        ;;
esac
status >"$S"
if jq -e --arg id "$op" '.serving and .handover.id==$id and .handover.phase=="ONLINE"' "$S" >/dev/null; then
    step=enable-routine-backup
    if [[ ! -f "$dir/activation-complete.json" ]]; then
        systemctl_cmd enable --now tuwunel-restic.timer
        jq -n '{online_verified_at:(now|todateiso8601),backup_timer_enabled:true}' >"$dir/activation-complete.json.tmp"
        mv "$dir/activation-complete.json.tmp" "$dir/activation-complete.json"
    fi
    rm -f "$dir/error.json"
fi
